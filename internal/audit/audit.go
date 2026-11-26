// Package audit reads the owner's prose in the fleet file (the standing
// orders and each worker's brief) for what costs tokens on every turn and
// says nothing a worker needs: what shed already writes, what another brief
// or the orders already say, and names the fleet file no longer has. It
// changes nothing.
package audit

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/sources"
)

// What a finding is.
const (
	ShedWrites  = "shed writes it"             // shed already writes it into every brief
	InOrders    = "in the orders"              // a brief repeats the standing orders
	Repeated    = "the same in several briefs" // the same text is in more than one brief
	Stale       = "stale"                      // it names what the fleet file does not have
	InBrief     = "in its brief"               // a start-up file repeats the worker's brief
	SaidTwice   = "said twice"                 // two start-up files of a worker say the same
	SettingDoes = "a setting does it"          // the model: a setting that is on does it
	SettingOff  = "a setting is off"           // the model: it describes a setting that is off
	Contradicts = "contradicts"                // the model: it contradicts a setting
)

// OrdersPath names the standing orders in a finding.
const OrdersPath = "standing_orders"

// RepoOrdersPath names a repository's standing orders in a finding.
func RepoOrdersPath(repo string) string { return "standing_orders of " + repo }

// isOrders reports whether a path names standing orders, the fleet's or a
// repository's.
func isOrders(path string) bool {
	return path == OrdersPath || strings.HasPrefix(path, "standing_orders of ")
}

// BriefPath names a worker's brief in a finding.
func BriefPath(m config.Machine, w config.Worker) string {
	return "brief of " + m.Name + "/" + w.Name
}

// Finding is one piece of the owner's prose that costs tokens and need not.
type Finding struct {
	Kind  string           `json:"kind"`
	Where sources.Location `json:"where"`
	Text  string           `json:"text"`
	// Why says what makes it unneeded, and what to do.
	Why string `json:"why"`
	// Tokens estimates what it costs every worker that reads it, on every
	// turn.
	Tokens int `json:"tokens"`
}

// Startup is what one worker's session loads before its first item, as
// its machine read it.
type Startup struct {
	Machine, Worker string
	Sources         []sources.Source
	Stale           []sources.Stale
}

// WorkerLoad is the size of what a worker loads before its first item.
type WorkerLoad struct {
	Machine string           `json:"machine"`
	Worker  string           `json:"worker"`
	Total   int              `json:"total"`
	Sources []sources.Source `json:"sources"`
}

// Report is the audit of a fleet file's prose.
type Report struct {
	// Workers is what each worker loads before its first item, when the
	// workers' machines were asked.
	Workers []WorkerLoad `json:"workers,omitempty"`
	// Prose is the owner's prose that was read, with its size.
	Prose    []sources.Source `json:"prose"`
	Findings []Finding        `json:"findings"`
	Problems []string         `json:"problems,omitempty"`
}

// ShedBrief returns what shed writes into a worker's brief by itself.
type ShedBrief func(m config.Machine, w config.Worker) string

// Prose collects the owner's prose: the standing orders, then each
// worker's brief.
func Prose(cfg *config.Config) []sources.Source {
	var prose []sources.Source
	add := func(path, kind, text string) {
		if strings.TrimSpace(text) != "" {
			prose = append(prose, sources.Source{Path: path, Kind: kind, Tokens: len(text) / 4, Text: text})
		}
	}
	add(OrdersPath, "orders", cfg.Defaults.StandingOrders)
	for _, r := range cfg.Repos {
		add(RepoOrdersPath(r.Name), "orders", r.StandingOrders)
	}
	for _, m := range cfg.Machines {
		for _, w := range m.Workers {
			add(BriefPath(m, w), "brief", w.Brief)
		}
	}
	return prose
}

// Run finds, with no model and no network, the owner's prose that shed
// already writes, that the orders or another brief already say, and the
// names in it that the fleet file does not have. commands are shed's
// commands.
func Run(cfg *config.Config, shed ShedBrief, commands []string) Report {
	prose := Prose(cfg)
	r := Report{Prose: prose, Findings: []Finding{}}
	// What shed writes is the same in many briefs: once each.
	var written []string
	for _, m := range cfg.Machines {
		for _, w := range m.Workers {
			if text := shed(m, w); !slices.Contains(written, text) {
				written = append(written, text)
			}
		}
	}
	shedLines := lines(strings.Join(written, "\n"))
	for i, s := range prose {
		places := []place{{ShedWrites, shedWhy, shedLines}}
		for _, earlier := range prose[:i] {
			p := place{Repeated, "also in the " + earlier.Path + ". A worker reads only its own brief, so this costs no tokens, but an edit of one copy leaves the other saying the old thing: keep them in step, or give the briefs one text with a YAML anchor (&name, then *name) where they can be the same", lines(earlier.Text)}
			if isOrders(earlier.Path) {
				p.kind, p.why = InOrders, ordersWhy
			}
			places = append(places, p)
		}
		r.Findings = append(r.Findings, lineFindings(s, places)...)
	}
	r.Findings = append(r.Findings, stale(cfg, prose, commands)...)
	sortFindings(r.Findings)
	return r
}

const (
	shedWhy   = "shed writes them into every worker's brief by itself: delete them"
	ordersWhy = "the standing orders say them, and every worker reads those: delete them here"
)

// place is somewhere a line may already be said.
type place struct {
	kind, why string
	lines     []line
}

// lineFindings counts each line of a source once, against the first place
// that already says it, and gives one finding per place. A repeat between
// briefs costs nothing a turn: a worker reads only its own.
func lineFindings(s sources.Source, places []place) []Finding {
	own := lines(s.Text)
	hits := make([][]line, len(places))
	for _, l := range own {
		for n, p := range places {
			if slices.ContainsFunc(p.lines, func(o line) bool { return same(l, o) }) {
				hits[n] = append(hits[n], l)
				break
			}
		}
	}
	var out []Finding
	for n, p := range places {
		if f, ok := overlap(s, len(own), hits[n], p.kind, p.why); ok {
			if p.kind == Repeated {
				f.Tokens = 0
			}
			out = append(out, f)
		}
	}
	return out
}

// minLineWords is how many words a line needs to be compared: a heading
// or a short phrase says too little to be a repeat.
const minLineWords = 6

// line is one line of prose, with its word triples.
type line struct {
	n       int
	text    string
	triples map[string]bool
}

var wordRE = regexp.MustCompile(`[a-z0-9]+`)

// lines splits prose into the lines worth comparing.
func lines(text string) []line {
	var out []line
	for n, l := range strings.Split(text, "\n") {
		words := wordRE.FindAllString(strings.ToLower(l), -1)
		if len(words) < minLineWords {
			continue
		}
		set := map[string]bool{}
		for i := 0; i+3 <= len(words); i++ {
			set[strings.Join(words[i:i+3], " ")] = true
		}
		out = append(out, line{n: n + 1, text: strings.TrimSpace(l), triples: set})
	}
	return out
}

// same reports whether two lines say the same thing: most of their word
// triples are shared.
func same(a, b line) bool {
	shared := 0
	for t := range a.triples {
		if b.triples[t] {
			shared++
		}
	}
	return shared > 0 && float64(shared) >= 0.6*float64(min(len(a.triples), len(b.triples)))
}

// overlap is one finding for the lines of a source that another text
// already has: where the first is, how many, and what they cost.
func overlap(s sources.Source, total int, hit []line, kind, why string) (Finding, bool) {
	if len(hit) == 0 {
		return Finding{}, false
	}
	tokens := 0
	for _, l := range hit {
		tokens += len(l.text) / 4
	}
	text := hit[0].text
	if len(text) > 80 {
		text = text[:79] + "…"
	}
	return Finding{Kind: kind, Where: sources.Location{Path: s.Path, Line: hit[0].n}, Text: text, Tokens: tokens,
		Why: fmt.Sprintf("%d of its %d lines are like this one, from line %d: %s", len(hit), total, hit[0].n, why)}, true
}

var (
	labelRE   = regexp.MustCompile(`\b([a-z][a-z-]*):([a-z0-9][a-z0-9-]*)\b`)
	commandRE = regexp.MustCompile("`shed ([a-z][a-z-]+)")
	sessionRE = regexp.MustCompile(`\b([a-z0-9]+)-([a-z0-9][a-z0-9-]*)\b`)
)

// stale finds the names in the prose that the fleet file does not have: a
// queue label of a kind the fleet uses, a session of a known machine with
// no such worker, and a shed command that does not exist.
func stale(cfg *config.Config, prose []sources.Source, commands []string) []Finding {
	labels := map[string]bool{}
	prefixes := map[string]bool{}
	for _, l := range cfg.QueueLabels() {
		labels[l] = true
		if p, _, ok := strings.Cut(l, ":"); ok {
			prefixes[p] = true
		}
	}
	for _, a := range cfg.Accounts {
		if a.Overflow != "" {
			labels[a.Overflow] = true
		}
	}
	workers := map[string]map[string]bool{}
	for _, m := range cfg.Machines {
		workers[m.Name] = map[string]bool{}
		for _, w := range m.Workers {
			workers[m.Name][w.Name] = true
		}
	}
	var out []Finding
	for _, s := range prose {
		for n, line := range strings.Split(s.Text, "\n") {
			at := sources.Location{Path: s.Path, Line: n + 1}
			for _, m := range labelRE.FindAllStringSubmatch(line, -1) {
				if prefixes[m[1]] && !labels[m[0]] {
					out = append(out, Finding{Kind: Stale, Where: at, Text: m[0], Why: fmt.Sprintf("no queue of the fleet file has the label %s", m[0])})
				}
			}
			for _, m := range sessionRE.FindAllStringSubmatch(line, -1) {
				if known, ok := workers[m[1]]; ok && !known[m[2]] && !strings.Contains(m[2], "-") {
					out = append(out, Finding{Kind: Stale, Where: at, Text: m[0], Why: fmt.Sprintf("machine %s has no worker %s", m[1], m[2])})
				}
			}
			for _, m := range commandRE.FindAllStringSubmatch(line, -1) {
				if !slices.Contains(commands, m[1]) {
					out = append(out, Finding{Kind: Stale, Where: at, Text: m[0], Why: fmt.Sprintf("shed has no command %q", m[1])})
				}
			}
		}
	}
	return out
}

// sortFindings puts the costliest first, then by where they are.
func sortFindings(f []Finding) {
	slices.SortStableFunc(f, func(a, b Finding) int {
		if a.Tokens != b.Tokens {
			return b.Tokens - a.Tokens
		}
		if a.Where.Path != b.Where.Path {
			return strings.Compare(a.Where.Path, b.Where.Path)
		}
		return a.Where.Line - b.Where.Line
	})
}

// RunWorkers is Run with each worker's start-up files: the global, parent
// and repository instructions, their imports, and the memory index. A line
// of one of them that shed writes, that the standing orders or the
// worker's own brief say, or that an earlier file says, is read again on
// every turn. A file that several workers load is reported once.
func RunWorkers(cfg *config.Config, shed ShedBrief, commands []string, startups []Startup) Report {
	r := Run(cfg, shed, commands)
	reported := map[string]bool{}
	for _, st := range startups {
		load := WorkerLoad{Machine: st.Machine, Worker: st.Worker}
		for _, s := range st.Sources {
			load.Total += s.Tokens
			load.Sources = append(load.Sources, sources.Source{Path: s.Path, Kind: s.Kind, Tokens: s.Tokens})
		}
		r.Workers = append(r.Workers, load)
		m, ok := cfg.Machine(st.Machine)
		if !ok {
			continue
		}
		w := slices.IndexFunc(m.Workers, func(w config.Worker) bool { return w.Name == st.Worker })
		if w < 0 {
			continue
		}
		places := []place{
			{ShedWrites, shedWhy, lines(shed(m, m.Workers[w]))},
			{InOrders, ordersWhy, lines(cfg.Defaults.StandingOrders + "\n" + repoOrdersOf(cfg, m, m.Workers[w]))},
			{InBrief, "the worker's brief in the fleet file says them, and it reads both: keep them in one place", lines(m.Workers[w].Brief)},
		}
		for _, s := range st.Sources {
			// The brief file is what shed writes, the orders and the brief.
			if s.Kind == "brief" {
				continue
			}
			key := st.Machine + ":" + s.Path
			file := sources.Source{Path: key, Kind: s.Kind, Tokens: s.Tokens, Text: s.Text}
			if !reported[key] {
				reported[key] = true
				r.Prose = append(r.Prose, file)
				r.Findings = append(r.Findings, lineFindings(file, places)...)
			}
			places = append(places, place{SaidTwice, "also in " + key + ", which the worker loads too: keep it in one of them", lines(s.Text)})
		}
		for _, sf := range st.Stale {
			key := st.Machine + ":" + sf.Path
			if reported[key+"\x00"+sf.Ref] {
				continue
			}
			reported[key+"\x00"+sf.Ref] = true
			r.Findings = append(r.Findings, Finding{Kind: Stale, Where: sources.Location{Path: key, Line: sf.Line}, Text: sf.Ref, Why: sf.Why})
		}
	}
	sortFindings(r.Findings)
	return r
}

// repoOrdersOf is the standing orders of the repositories a worker's queue
// takes issues from.
func repoOrdersOf(cfg *config.Config, m config.Machine, w config.Worker) string {
	var parts []string
	for _, src := range m.SourcesFor(w) {
		for _, r := range cfg.Repos {
			if r.Name == src.Repo && r.StandingOrders != "" && !slices.Contains(parts, r.StandingOrders) {
				parts = append(parts, r.StandingOrders)
			}
		}
	}
	return strings.Join(parts, "\n")
}
