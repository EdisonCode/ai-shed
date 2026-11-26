package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/deploy"
	"github.com/edisoncode/ai-shed/internal/runlog"
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
	// Machine is where a bump is made; empty for a comment.
	Machine string `json:"machine,omitempty"`
	// Does says what an action that posts no comment does.
	Does string `json:"does,omitempty"`
	// Quick marks an answer a worker teed up: the page lists it first.
	Quick bool `json:"quick,omitempty"`
	// Warn is said before the owner confirms an action that is easy to
	// get wrong.
	Warn string `json:"warn,omitempty"`
	// For is the issue whose card offers the action, when that is not the
	// issue it acts on: a held issue offers the bump of its blocker.
	For string `json:"for,omitempty"`
	// Goal and Mode are, for a goal action, the goal's label and the mode
	// it is set to on every machine; "" clears it.
	Goal string `json:"goal,omitempty"`
	Mode string `json:"mode,omitempty"`
	// Worker is the worker a recycle gives a fresh session.
	Worker string `json:"worker,omitempty"`
	// Remove is the queue label a cancel takes off the issue.
	Remove string `json:"remove,omitempty"`
	// Into is the goal a merge moves the issues to, and Repos the
	// repositories a goal's labels are changed in.
	Into  string   `json:"into,omitempty"`
	Repos []string `json:"repos,omitempty"`
	// Ask is set on an action that needs a name from the owner.
	Ask string `json:"ask,omitempty"`
}

// GoalLabels changes a goal's labels across a repository, as the owner.
type GoalLabels interface {
	Labeler
	Labeled(ctx context.Context, repo, label string, closedToo bool) ([]int, error)
	HasLabel(ctx context.Context, repo, label string) (bool, error)
	RenameLabel(ctx context.Context, repo, from, to string) error
	DeleteLabel(ctx context.Context, repo, label string) error
}

// Labeler changes an issue's labels, as the owner.
type Labeler interface {
	Relabel(ctx context.Context, repo string, issue int, add, remove string) error
}

// Action kinds.
const (
	ActAccept   = "accept"      // accept the recommendation of a decision
	ActReady    = "ready"       // say an issue that was not ready is ready now
	ActEyes     = "eyes"        // say an eye check passed
	ActUndepend = "undepend"    // end a dependency the issue's text named
	ActBump     = "bump"        // put a queued issue first in its queue
	ActUnbump   = "unbump"      // put a bumped issue back in its place
	ActGoal     = "goal"        // focus on a goal, table it, or clear either
	ActPause    = "pause"       // pause the machine a card's issue is queued on
	ActRecycle  = "recycle"     // give the worker on a card a fresh session
	ActReshape  = "reshape"     // send an issue back to the owner as too big
	ActCancel   = "cancel"      // take an issue off a machine's queue
	ActGoalEnd  = "goal-end"    // take a goal's label off its open issues
	ActRename   = "goal-rename" // rename a goal's label in each repository
	ActMerge    = "goal-merge"  // move a goal's issues to another goal
)

// goalLabelActions works out what the owner can do with each goal's label:
// end the goal, rename it, or merge it into another. They need the deck's
// counts, to say first how many issues change.
func goalLabelActions(r status.Report, deck *status.Digest) []Action {
	if deck == nil {
		return nil
	}
	modes := map[string]string{}
	for _, m := range r.Machines {
		if m.Probe != nil {
			maps.Copy(modes, m.Probe.Goals)
		}
	}
	type counts struct {
		repos       []string
		open, total int
	}
	byLabel := map[string]*counts{}
	for _, g := range deck.Goals {
		c := byLabel[g.Label]
		if c == nil {
			c = &counts{}
			byLabel[g.Label] = c
		}
		c.repos = append(c.repos, g.Repo)
		c.open += g.Pace.Open
		c.total += g.Total
	}
	labels := slices.Sorted(maps.Keys(byLabel))
	var out []Action
	for _, label := range labels {
		c := byLabel[label]
		where := fmt.Sprintf("%d repositor%s (%s)", len(c.repos), map[bool]string{true: "y", false: "ies"}[len(c.repos) == 1], strings.Join(c.repos, ", "))
		out = append(out, Action{ID: ActGoalEnd + ":" + label, Kind: ActGoalEnd, Label: "End this goal", Goal: label, Mode: modes[label], Repos: c.repos,
			Does: fmt.Sprintf("Takes %s off its %d open issue(s) in %s: it stops counting as a goal and its card goes away. No issue is closed, and closed issues keep the label.", label, c.open, where),
			Warn: fmt.Sprintf("Relabels %d open issue(s) in %s.", c.open, where)})
		out = append(out, Action{ID: ActRename + ":" + label, Kind: ActRename, Label: "Rename this goal…", Goal: label, Mode: modes[label], Repos: c.repos,
			Ask:  "The new label, with the goal prefix",
			Does: fmt.Sprintf("Renames the label %s in %s: all %d of its issues, closed ones too, keep it under the new name.", label, where, c.total)})
		for _, into := range labels {
			if into == label {
				continue
			}
			out = append(out, Action{ID: ActMerge + ":" + label + ":" + into, Kind: ActMerge, Label: "Merge into " + into, Goal: label, Into: into, Mode: modes[label], Repos: c.repos,
				Does: fmt.Sprintf("Moves all %d issue(s) of %s, closed ones too, to %s in %s, then deletes the label %s there. For one outcome that ended up under two labels.", c.total, label, into, where, label),
				Warn: fmt.Sprintf("Relabels %d issue(s) in %s and deletes a label: the page cannot undo it.", c.total, where)})
		}
	}
	return out
}

// goalName checks a new goal label from the owner: it has the goal prefix,
// fits a label, and is not the old one.
func goalName(name, prefix, old string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "" || name == old:
		return "", fmt.Errorf("give a new name for %s", old)
	case prefix != "" && !strings.HasPrefix(name, prefix):
		return "", fmt.Errorf("%q does not start with the goal prefix %q, so it would not count as a goal", name, prefix)
	case len(name) > 50 || strings.ContainsAny(name, ",\r\n\""):
		return "", fmt.Errorf("%q is not a label: at most 50 characters, with no comma, quote or line break", name)
	}
	return name, nil
}

// doGoalLabels ends, renames or merges a goal, repository by repository, and
// moves its focus or table to its new label.
func (s *Server) doGoalLabels(ctx context.Context, a Action, name string) error {
	if s.GoalLabels == nil {
		return fmt.Errorf("the page cannot change labels")
	}
	g := s.GoalLabels
	to := a.Into
	if a.Kind == ActRename {
		prefix := ""
		if s.Config != nil {
			prefix = s.Config.Supervisor.GoalLabels
		}
		var err error
		if to, err = goalName(name, prefix, a.Goal); err != nil {
			return err
		}
		for _, repo := range a.Repos {
			if has, err := g.HasLabel(ctx, repo, to); err != nil {
				return err
			} else if has {
				return fmt.Errorf("%s already has the label %s: merge into it instead", repo, to)
			}
		}
	}
	for _, repo := range a.Repos {
		var err error
		switch a.Kind {
		case ActGoalEnd:
			err = relabelAll(ctx, g, repo, a.Goal, "", false)
		case ActRename:
			err = g.RenameLabel(ctx, repo, a.Goal, to)
		case ActMerge:
			var has bool
			if has, err = g.HasLabel(ctx, repo, to); err == nil && !has {
				err = g.RenameLabel(ctx, repo, a.Goal, to)
			} else if err == nil {
				if err = relabelAll(ctx, g, repo, a.Goal, to, true); err == nil {
					err = g.DeleteLabel(ctx, repo, a.Goal)
				}
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", repo, err)
		}
	}
	if a.Mode == "" {
		return nil
	}
	if to != "" {
		if _, err := deploy.SetFleetGoalMode(ctx, s.Runner, s.Config, to, a.Mode); err != nil {
			return err
		}
	}
	_, err := deploy.SetFleetGoalMode(ctx, s.Runner, s.Config, a.Goal, "")
	return err
}

// relabelAll moves the repository's issues with a label to another, or
// takes it off them when to is empty.
func relabelAll(ctx context.Context, g GoalLabels, repo, from, to string, closedToo bool) error {
	numbers, err := g.Labeled(ctx, repo, from, closedToo)
	if err != nil {
		return err
	}
	for _, n := range numbers {
		if err := g.Relabel(ctx, repo, n, to, from); err != nil {
			return err
		}
	}
	return nil
}

// cardPause is how long a pause from a card lasts, as shed pause's default.
const cardPause = 30 * time.Minute

// cancelWarning is said before an issue is taken off a queue from the page.
const cancelWarning = "The issue stays open, with no queue label: no worker starts it until you label it for a machine again. A branch or pull request a worker already has for it stays where it is."

// cardActions works out what the owner can do with the machine and worker
// behind each card: pause the machine, recycle the worker, send the issue
// back as too big, or take it off the queue. Each is offered on the card of
// the issue it came from.
func cardActions(r status.Report, cfg *config.Config, signals []config.Signal, relabel bool) []Action {
	ready := ""
	for _, s := range signals {
		if s.Name == config.ReadySignal {
			ready = s.Ask
		}
	}
	var out []Action
	seen := map[string]bool{}
	add := func(a Action) {
		a.ID = fmt.Sprintf("%s:%s#%d:%s:%s:%s%s", a.Kind, a.Repo, a.Number, a.Machine, a.Worker, a.Remove, a.Comment)
		if !seen[a.ID] {
			seen[a.ID] = true
			out = append(out, a)
		}
	}
	for _, m := range r.Machines {
		machine, known := config.Machine{}, false
		if cfg != nil {
			machine, known = cfg.Machine(m.Name)
		}
		for _, i := range m.Issues {
			if i.Worker != "" {
				add(Action{Kind: ActRecycle, Label: "Recycle worker " + i.Worker, Repo: i.Repo, Number: i.Number, Machine: m.Name, Worker: i.Worker,
					Does: fmt.Sprintf("Gives worker %s on %s a fresh session as soon as it has no issue in progress, as shed recycle does. Nothing it has in hand is lost.", i.Worker, m.Name)})
			}
			if known && len(machine.Workers) > 0 {
				add(Action{Kind: ActPause, Label: "Pause " + m.Name + " for 30m", Repo: i.Repo, Number: i.Number, Machine: m.Name,
					Does: fmt.Sprintf("Hands out nothing on %s and nudges nobody for 30 minutes, as shed pause does. A worker finishes the turn it is in; shed resume %s ends it early.", m.Name, m.Name)})
			}
			if i.State != status.Queued && i.State != status.Held {
				continue
			}
			if ready != "" {
				add(Action{Kind: ActReshape, Label: "Too big: send it back to shape", Repo: i.Repo, Number: i.Number,
					Comment: ready + " too big: split it into slices that a worker can each finish in one pull request, each with its own acceptance."})
			}
			if label := queueLabel(machine, i.Repo); relabel && known && label != "" {
				add(Action{Kind: ActCancel, Label: "Take it off " + m.Name + "'s queue", Repo: i.Repo, Number: i.Number, Machine: m.Name, Remove: label, Warn: cancelWarning,
					Does: fmt.Sprintf("Takes the label %s off #%d, so no worker on %s starts it.", label, i.Number, m.Name)})
			}
		}
	}
	return out
}

// queueLabel is the label that puts the repository's issues in the
// machine's queue; "" when a query or an assignee selects them too, or more
// than one label does: then no single label takes an issue off.
func queueLabel(m config.Machine, repo string) string {
	label := ""
	for _, src := range m.AllSources() {
		if src.Repo != repo {
			continue
		}
		if src.Label == "" || src.Query != "" || src.Assignee != "" || (label != "" && label != src.Label) {
			return ""
		}
		label = src.Label
	}
	return label
}

// focusKept is how many goals focus works for: past it, focus spreads the
// fleet as thin as no focus does.
const focusKept = 2

// goalActions works out what the owner can do with each goal: focus on it,
// table it, or clear either, on every machine. The goals are those of the
// deck and of the report's issues; each goal's mode is the one the machines
// report.
func goalActions(r status.Report, deck *status.Digest) []Action {
	modes := map[string]string{}
	labels := map[string]bool{}
	for _, m := range r.Machines {
		if m.Probe != nil {
			for label, mode := range m.Probe.Goals {
				modes[label] = mode
				labels[label] = true
			}
		}
		for _, i := range m.Issues {
			if i.Goal != "" {
				labels[i.Goal] = true
			}
		}
	}
	if deck != nil {
		for _, g := range deck.Goals {
			labels[g.Label] = true
		}
	}
	var focused []string
	for label, mode := range modes {
		if mode == runlog.GoalFocus {
			focused = append(focused, label)
		}
	}
	slices.Sort(focused)
	sorted := slices.Sorted(maps.Keys(labels))
	var out []Action
	add := func(label, mode, name, does, warn string) {
		out = append(out, Action{ID: "goal:" + label + ":" + mode, Kind: ActGoal, Label: name, Goal: label, Mode: mode, Does: does, Warn: warn})
	}
	for _, label := range sorted {
		mode := modes[label]
		if mode != runlog.GoalFocus {
			warn := ""
			if len(focused) >= focusKept {
				warn = fmt.Sprintf("%d goals are in focus already (%s). Focus finishes goals when it is one or two; take one out of focus first, or this one spreads the fleet as thin as before.", len(focused), strings.Join(focused, ", "))
			}
			add(label, runlog.GoalFocus, "Focus: finish this goal first",
				fmt.Sprintf("Puts the issues of %s ahead of every other goal's in each queue, on every machine, until you take it out of focus. Nobody is interrupted: each worker takes it up when it finishes what it has in hand.", label), warn)
		}
		if mode != runlog.GoalTabled {
			add(label, runlog.GoalTabled, "Table this goal",
				fmt.Sprintf("Holds the issues of %s that nobody has started, on every machine. Work already started on it (in hand, rework, verification, eye checks) goes on to its end.", label), "")
		}
		switch mode {
		case runlog.GoalFocus:
			add(label, "", "Take it out of focus", fmt.Sprintf("Puts the issues of %s back in their usual order, on every machine.", label), "")
		case runlog.GoalTabled:
			add(label, "", "Take it off the table", fmt.Sprintf("Lets workers start the issues of %s again, on every machine.", label), "")
		}
	}
	return out
}

// dropWarning is said before a dependency is dropped from the page.
const dropWarning = "Drop a dependency only when it is not real. Once dropped, the issue goes to a worker at once, and a worker that builds on a part that is not there yet hands back rework or files a follow-up. For a real build order, bump the blocker instead."

// actionsFor works out the answers the owner can give to the report's
// issues. Each issue counts once, however many queues it is in.
func actionsFor(r status.Report, signals []config.Signal) []Action {
	byName := map[string]config.Signal{}
	for _, s := range signals {
		byName[s.Name] = s
	}
	var out []Action
	seen := map[string]bool{}
	addAction := func(a Action) {
		a.ID = fmt.Sprintf("%s:%s#%d:%s%s", a.Kind, a.Repo, a.Number, a.Machine, a.Comment)
		if a.For != "" {
			a.ID += ":for " + a.For
		}
		if !seen[a.ID] {
			seen[a.ID] = true
			out = append(out, a)
		}
	}
	add := func(i status.IssueStatus, kind, label, comment, context string) {
		addAction(Action{Kind: kind, Label: label, Repo: i.Repo, Number: i.Number, Comment: comment, Context: context, Quick: kind != ActUndepend})
	}
	// Where each queued issue can be bumped: the first machine whose queue
	// has it waiting its turn.
	queued := map[string]string{}
	for _, m := range r.Machines {
		for _, i := range m.Issues {
			if key := fmt.Sprintf("%s#%d", i.Repo, i.Number); i.State == status.Queued && queued[key] == "" {
				queued[key] = m.Name
			}
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
			key := fmt.Sprintf("%s#%d", i.Repo, i.Number)
			switch {
			case i.State == status.Queued && !i.Bumped && queued[key] == m.Name:
				addAction(Action{Kind: ActBump, Label: "Bump to the front", Repo: i.Repo, Number: i.Number, Machine: m.Name,
					Does: fmt.Sprintf("Puts #%d first in its queue on %s. It is handed over when a worker has nothing in progress; nobody is interrupted.", i.Number, m.Name)})
			case i.Bumped:
				addAction(Action{Kind: ActUnbump, Label: "Put back in its place", Repo: i.Repo, Number: i.Number, Machine: m.Name,
					Does: fmt.Sprintf("Takes back the bump of #%d on %s.", i.Number, m.Name)})
			}
			if i.State != status.Held {
				continue
			}
			for _, b := range i.Blockers {
				full := b
				if len(b) > 0 && b[0] == '#' {
					full = i.Repo + b
				}
				on := queued[full]
				if on == "" {
					continue
				}
				repo, number := splitKey(full)
				addAction(Action{Kind: ActBump, Label: "Bump its blocker " + b, Repo: repo, Number: number, Machine: on, For: key, Context: i.Held,
					Does: fmt.Sprintf("Puts %s first in its queue on %s, so the build order goes on. It is handed over when a worker has nothing in progress; nobody is interrupted.", b, on)})
			}
			for _, d := range i.Droppable {
				a := Action{Kind: ActUndepend, Label: "Drop the dependency on " + d, Repo: i.Repo, Number: i.Number, Comment: "No longer depends on " + d, Context: i.Held, Warn: dropWarning}
				addAction(a)
			}
		}
	}
	return out
}

// splitKey splits "org/app#12" into its repository and number.
func splitKey(key string) (string, int) {
	at := strings.LastIndex(key, "#")
	n, _ := strconv.Atoi(key[at+1:])
	return key[:at], n
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
		ID   string `json:"id"`
		Name string `json:"name"`
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
	if err := s.do(ctx, *found, req.Name); err != nil {
		http.Error(w, fmt.Sprintf("%s on %s: %v", found.Label, found.target(), err), http.StatusBadGateway)
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
	json.NewEncoder(w).Encode(map[string]string{"done": found.Label, "on": found.target()})
}

// target names what an action acts on: its issue, or its goal.
func (a Action) target() string {
	if a.Goal != "" {
		return a.Goal
	}
	return fmt.Sprintf("%s#%d", a.Repo, a.Number)
}

// do carries out an action: a comment through the owner's gh, or a bump on
// the machine through the owner's ssh, as shed bump makes it.
func (s *Server) do(ctx context.Context, a Action, name string) error {
	switch a.Kind {
	case ActGoalEnd, ActRename, ActMerge:
		return s.doGoalLabels(ctx, a, name)
	case ActGoal:
		_, err := deploy.SetFleetGoalMode(ctx, s.Runner, s.Config, a.Goal, a.Mode)
		return err
	case ActPause, ActRecycle:
		m, ok := s.Config.Machine(a.Machine)
		if !ok {
			return fmt.Errorf("machine %q is not in the fleet file", a.Machine)
		}
		if a.Kind == ActPause {
			_, err := deploy.Pause(ctx, s.Runner, m, cardPause)
			return err
		}
		return deploy.Recycle(ctx, s.Runner, m, a.Worker, false)
	case ActCancel:
		if s.Labeler == nil {
			return fmt.Errorf("the page cannot change labels")
		}
		return s.Labeler.Relabel(ctx, a.Repo, a.Number, "", a.Remove)
	case ActBump, ActUnbump:
		m, ok := s.Config.Machine(a.Machine)
		if !ok {
			return fmt.Errorf("machine %q is not in the fleet file", a.Machine)
		}
		if a.Kind == ActUnbump {
			_, err := deploy.Unbump(ctx, s.Runner, m, a.Number)
			return err
		}
		return deploy.Bump(ctx, s.Runner, m, a.Number, "")
	}
	return s.Commenter.Comment(ctx, a.Repo, a.Number, a.Comment)
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

var (
	_ Commenter  = backlog.GH{}
	_ Labeler    = backlog.GH{}
	_ GoalLabels = backlog.GH{}
)
