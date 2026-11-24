package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/status"
)

// Commenter posts a comment on an issue, as the owner.
type Commenter interface {
	Comment(ctx context.Context, repo string, issue int, body string) error
}

// Action is one answer the owner can give from the page: a comment, in a
// phrase shed already reads, on one issue.
type Action struct {
	// ID names the action; the page sends only this back.
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	// Comment is exactly what is posted, shown before it is.
	Comment string `json:"comment"`
	// Context is what the answer is to: the worker's question and its
	// recommendation, or what to look at.
	Context string `json:"context,omitempty"`
}

// Action kinds.
const (
	ActAccept   = "accept"   // accept the recommendation of a decision
	ActReady    = "ready"    // say an issue that was not ready is ready now
	ActEyes     = "eyes"     // say an eye check passed
	ActUndepend = "undepend" // end a dependency the issue's text named
)

// actionsFor works out the answers the owner can give to the report's
// issues. Each issue counts once, however many queues it is in.
func actionsFor(r status.Report, signals []config.Signal) []Action {
	byName := map[string]config.Signal{}
	for _, s := range signals {
		byName[s.Name] = s
	}
	var out []Action
	seen := map[string]bool{}
	add := func(i status.IssueStatus, kind, label, comment, context string) {
		a := Action{Kind: kind, Label: label, Repo: i.Repo, Number: i.Number, Comment: comment, Context: context}
		a.ID = fmt.Sprintf("%s:%s#%d:%s", kind, i.Repo, i.Number, comment)
		if !seen[a.ID] {
			seen[a.ID] = true
			out = append(out, a)
		}
	}
	for _, m := range r.Machines {
		for _, i := range m.Issues {
			for _, ask := range i.Asks {
				s := byName[ask.Name]
				switch {
				case s.AnsweredBy == "":
				case s.Name == config.ReadySignal:
					add(i, ActReady, "Ready now", s.AnsweredBy, ask.Text)
				case s.Name == config.EyesSignal:
					add(i, ActEyes, "Looked: it passes", s.AnsweredBy+": passed on staging.", ask.Text)
				case s.SendsBack && len(s.Accept) > 0:
					add(i, ActAccept, "Accept the recommendation", s.AnsweredBy+": "+s.Accept[0], ask.Text)
				}
			}
			if i.State == status.Held {
				for _, d := range i.Droppable {
					add(i, ActUndepend, "Drop the dependency on "+d, "No longer depends on "+d, i.Held)
				}
			}
		}
	}
	return out
}

// NewToken makes the per-run token a page must send with an action.
func NewToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("make the action token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// loopbackHost reports whether a request's Host names this machine. A page
// on another site that points its own name at 127.0.0.1 sends that name,
// and is refused.
func loopbackHost(host string) bool {
	name, _, err := net.SplitHostPort(host)
	if err != nil {
		name = host
	}
	if name == "localhost" {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}

// act posts the answer an action names, when the request may: it comes from
// the page on this machine, with this run's token, and names an action of
// the latest report.
func (s *Server) act(w http.ResponseWriter, r *http.Request) {
	if !s.actionsOn() {
		http.Error(w, "actions are off: the page listens on an address other than loopback", http.StatusForbidden)
		return
	}
	if !loopbackHost(r.Host) || r.Header.Get("Origin") != "http://"+r.Host {
		http.Error(w, "an action must come from the shed page on this machine", http.StatusForbidden)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Shed-Token")), []byte(s.Token)) != 1 {
		http.Error(w, "the page's token is not this run's: reload the page", http.StatusForbidden)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "the request names no action", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	var found *Action
	if s.snap != nil {
		for _, a := range s.snap.Actions {
			if a.ID == req.ID {
				found = &a
				break
			}
		}
	}
	s.mu.Unlock()
	if found == nil {
		http.Error(w, "that answer no longer applies: the issue changed since the page was drawn", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), askTimeout)
	defer cancel()
	if err := s.Commenter.Comment(ctx, found.Repo, found.Number, found.Comment); err != nil {
		http.Error(w, fmt.Sprintf("post on %s#%d: %v", found.Repo, found.Number, err), http.StatusBadGateway)
		return
	}
	s.mu.Lock()
	if s.posted == nil {
		s.posted = map[string]time.Time{}
	}
	s.posted[found.ID] = time.Now()
	s.mu.Unlock()
	s.publish()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"posted": found.Comment, "on": fmt.Sprintf("%s#%d", found.Repo, found.Number)})
}

// actionsOn reports whether the page may act: it has a way to post and a
// token to check.
func (s *Server) actionsOn() bool {
	return s.Commenter != nil && s.Token != ""
}

// signals are the fleet's signals, or shed's defaults.
func (s *Server) signals() []config.Signal {
	if s.Config != nil && len(s.Config.Signals) > 0 {
		return s.Config.Signals
	}
	return config.DefaultSignals()
}

var _ Commenter = backlog.GH{}
