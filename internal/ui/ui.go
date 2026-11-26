// Package ui serves a live, read-only view of the fleet in the browser.
// It reads what shed status reads, from the machine it runs on: nothing is
// installed on the fleet, and the fleet does not depend on it.
package ui

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/runlog"
	"github.com/edisoncode/ai-shed/internal/status"
)

// DefaultListen keeps the page on this machine unless another address is
// given.
const DefaultListen = "127.0.0.1:7777"

const (
	screenLines = 80
	logLines    = 60
	// askTimeout bounds a screen or log read over ssh.
	askTimeout = 20 * time.Second
	// keepAlive is how often an idle event stream gets a comment, so a
	// proxy or the browser does not close it.
	keepAlive = 25 * time.Second
)

//go:embed page.html
var page []byte

// Snapshot is one report of the fleet, as the page receives it.
type Snapshot struct {
	At       time.Time `json:"at"`
	Took     string    `json:"took"`
	NeedsYou int       `json:"needs_you"`
	// Every is how often the fleet is read, so the page can tell a report
	// that is late.
	Every string `json:"every"`
	// RepoAttention is what the repositories' release state needs from
	// the owner, one line each.
	RepoAttention []string      `json:"repo_attention,omitempty"`
	Report        status.Report `json:"report"`
	// Deck is the slower part of the page: goals and their pace, flow,
	// idle hours and spend, as the digest reads them over the last day.
	// Its own report is left out; Report is the current one. Nil until
	// the first is read.
	Deck *status.Digest `json:"deck,omitempty"`
	// Actions are the answers the owner can give from the page; empty when
	// actions are off.
	Actions []Action `json:"actions,omitempty"`
	// Held says why the page is not reading the fleet now: the account's
	// GitHub quota is low, and what is left is the supervisors'.
	Held string `json:"held,omitempty"`
	// readStart is when the read of this report began: an answer posted
	// after it is not in the report yet.
	readStart time.Time
}

// Server keeps the latest report of the fleet and sends each new one to the
// open pages.
type Server struct {
	Config  *config.Config
	Collect func(context.Context) status.Report
	Runner  probe.Runner
	Every   time.Duration
	// Digest reads the deck from a report; nil leaves the deck out. It is
	// read at most every DeckEvery, apart from the report, so a slow read
	// of goals and flow does not hold the report back.
	Digest    func(context.Context, status.Report) status.Digest
	DeckEvery time.Duration
	// Commenter posts the owner's answers, and Token is this run's secret
	// that a page must send with one. Either empty turns actions off.
	Commenter Commenter
	Token     string
	// Quota says how much of the account's hourly GitHub quota is left and
	// when it resets; nil reads without asking. Below QuotaReserve the page
	// holds its reads, so the supervisors on the same account keep theirs.
	Quota func(context.Context) (int, time.Time, error)
	// Labeler takes an issue off a queue, and GoalLabels ends, renames and
	// merges goals; nil leaves those actions out.
	Labeler    Labeler
	GoalLabels GoalLabels

	mu      sync.Mutex
	latest  []byte
	subs    map[chan []byte]struct{}
	snap    *Snapshot
	deckAt  time.Time
	reading bool
	// posted is when each action was posted from the page.
	posted map[string]time.Time
}

// Run reads the fleet at once and then every Every, until ctx ends.
func (s *Server) Run(ctx context.Context) {
	for {
		s.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.Every):
		}
	}
}

// QuotaReserve is the part of the account's hourly GitHub quota the page
// leaves to the supervisors: below it, the page does not read.
const QuotaReserve = 1000

// held says why the page must not read GitHub now; "" when it may.
func (s *Server) held(ctx context.Context) string {
	if s.Quota == nil {
		return ""
	}
	left, reset, err := s.Quota(ctx)
	if err != nil || left >= QuotaReserve {
		// A quota that cannot be asked for is no reason to stop reading.
		return ""
	}
	return fmt.Sprintf("reads held: %d of the account's GitHub quota left until %s, kept for the supervisors", left, reset.Local().Format("15:04"))
}

// refresh reads the fleet once and sends the report to every open page.
// When the deck is due, it is read after, in the background. With the
// GitHub quota low, it reads nothing and the page says so.
func (s *Server) refresh(ctx context.Context) {
	// The first read goes ahead: a page with nothing to show helps nobody.
	s.mu.Lock()
	first := s.snap == nil
	s.mu.Unlock()
	if why := ""; !first {
		if why = s.held(ctx); why != "" {
			s.mu.Lock()
			s.snap.Held = why
			s.mu.Unlock()
			s.publish()
			return
		}
	}
	start := time.Now()
	r := s.Collect(ctx)
	snap := Snapshot{At: time.Now(), Took: time.Since(start).Round(100 * time.Millisecond).String(), Every: s.Every.String(), Report: r, readStart: start}
	for _, m := range r.Machines {
		snap.NeedsYou += len(m.Attention)
	}
	snap.RepoAttention = status.RepoAttention(r)
	snap.NeedsYou += len(snap.RepoAttention)
	s.mu.Lock()
	if s.snap != nil {
		snap.Deck = s.snap.Deck
	}
	snap.Actions = s.actions(snap.Report, snap.Deck)
	s.snap = &snap
	due := s.Digest != nil && !s.reading && (snap.Deck == nil || time.Since(s.deckAt) >= s.DeckEvery)
	if due {
		s.reading = true
	}
	s.mu.Unlock()
	s.publish()
	if due {
		go s.readDeck(ctx, r)
	}
}

// actions are what the owner can do from the page with the report's issues
// and the goals; none when the page may not act.
func (s *Server) actions(r status.Report, deck *status.Digest) []Action {
	if !s.actionsOn() {
		return nil
	}
	out := append(actionsFor(r, s.signals()), goalActions(r, deck)...)
	out = append(out, cardActions(r, s.Config, s.signals(), s.Labeler != nil)...)
	if s.GoalLabels != nil {
		out = append(out, goalLabelActions(r, deck)...)
	}
	return out
}

// readDeck reads the deck from a report and sends it to every open page
// with the latest report.
func (s *Server) readDeck(ctx context.Context, r status.Report) {
	d := s.Digest(ctx, r)
	// What the deck read from the report is the snapshot's to show, from
	// the current report.
	d.Report, d.Waiting, d.RepoAttention = status.Report{}, nil, nil
	s.mu.Lock()
	s.reading = false
	s.deckAt = time.Now()
	if s.snap != nil {
		s.snap.Deck = &d
		// The deck names goals the report may not: offer what can be done
		// with each.
		s.snap.Actions = s.actions(s.snap.Report, s.snap.Deck)
	}
	s.mu.Unlock()
	s.publish()
}

// publish encodes the latest snapshot and sends it to every open page.
func (s *Server) publish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap == nil {
		return
	}
	// An answer posted since the read began is not in the report yet: the
	// page does not offer it again.
	shown := *s.snap
	shown.Actions = nil
	for _, a := range s.snap.Actions {
		if at, ok := s.posted[a.ID]; !ok || at.Before(s.snap.readStart) {
			shown.Actions = append(shown.Actions, a)
		}
	}
	data, err := json.Marshal(shown)
	if err != nil {
		// A report that cannot be encoded is a bug in shed: say so in its
		// place, so the page does not go on showing an old one as current.
		data, _ = json.Marshal(map[string]string{"error": fmt.Sprintf("encode the report: %v", err)})
	}
	s.latest = data
	for ch := range s.subs {
		select {
		case ch <- data:
		default:
			// A page that has not taken the last report gets the next one.
		}
	}
}

func (s *Server) snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest
}

// Handler serves the page and its read-only API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		token := ""
		if s.actionsOn() {
			token = s.Token
		}
		w.Write(bytes.Replace(page, []byte(`<meta name="shed-token" content="">`), []byte(`<meta name="shed-token" content="`+token+`">`), 1))
	})
	mux.HandleFunc("GET /api/report", func(w http.ResponseWriter, r *http.Request) {
		data := s.snapshot()
		if data == nil {
			http.Error(w, "the first report of the fleet is still being read", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/screen", s.screen)
	mux.HandleFunc("GET /api/log", s.log)
	mux.HandleFunc("POST /api/act", s.act)
	return readOnly(mux)
}

// readOnly answers every method but GET and HEAD with 405, apart from the
// one way the page acts: posting an answer the server worked out itself.
func readOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/act" {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET")
			http.Error(w, "the shed page is read-only", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan []byte, 1)
	s.mu.Lock()
	if s.subs == nil {
		s.subs = map[chan []byte]struct{}{}
	}
	s.subs[ch] = struct{}{}
	latest := s.latest
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()
	send := func(data []byte) bool {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if latest != nil && !send(latest) {
		return
	}
	flusher.Flush()
	tick := time.NewTicker(keepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-ch:
			if !send(data) {
				return
			}
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// worker finds a worker the fleet file names. The query never reaches a
// command: only names read from the fleet file do.
func (s *Server) worker(machine, worker string) (config.Machine, config.Worker, bool) {
	m, ok := s.Config.Machine(machine)
	if !ok {
		return config.Machine{}, config.Worker{}, false
	}
	i := slices.IndexFunc(m.Workers, func(w config.Worker) bool { return w.Name == worker })
	if i < 0 {
		return config.Machine{}, config.Worker{}, false
	}
	return m, m.Workers[i], true
}

func (s *Server) screen(w http.ResponseWriter, r *http.Request) {
	m, wk, ok := s.worker(r.URL.Query().Get("machine"), r.URL.Query().Get("worker"))
	if !ok {
		http.Error(w, "no such worker in the fleet file", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), askTimeout)
	defer cancel()
	command := fmt.Sprintf("tmux capture-pane -p -J -S -%d -t %s", screenLines, shellQuote("=shed:="+wk.Name))
	out, err := s.Runner.Run(ctx, m, command, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("read the screen of %s on %s: %v", wk.Name, m.Name, err), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(out)
}

func (s *Server) log(w http.ResponseWriter, r *http.Request) {
	m, ok := s.Config.Machine(r.URL.Query().Get("machine"))
	if !ok {
		http.Error(w, "no such machine in the fleet file", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), askTimeout)
	defer cancel()
	command := fmt.Sprintf(`f="$HOME/%s/%s"; [ -f "$f" ] && tail -n %d "$f"; true`, runlog.StateDir, runlog.CheckinsFile, logLines)
	out, err := s.Runner.Run(ctx, m, command, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("read the log of %s: %v", m.Name, err), http.StatusBadGateway)
		return
	}
	checkins := []runlog.Checkin{}
	for line := range strings.SplitSeq(string(bytes.TrimSpace(out)), "\n") {
		if c, err := runlog.ParseCheckin(line); err == nil {
			checkins = append(checkins, c)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(checkins)
}

// Loopback reports whether a listen address reaches only this machine.
func Loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
