// Package supervisor keeps a machine's workers going while the owner is
// away. It starts each worker's session, checks in when a worker goes quiet
// or has worked a while, and types a short message when that gets work
// moving again.
package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

const (
	// settle is how long a window must be silent before the worker counts as
	// idle. It is far below any cache lifetime, so the first check-in on an
	// idle worker always lands while its prompt cache is warm.
	settle = 90 * time.Second
	// queueRecheck is how often a resting worker's queue is looked at for
	// something new. This asks GitHub, not the reviewer.
	queueRecheck = 5 * time.Minute
	// A worker that takes maxNudges nudges or maxStarts starts inside
	// loopWindow is not being helped by more of them.
	loopWindow = 30 * time.Minute
	maxNudges  = 3
	maxStarts  = 3
	// retryAfter spaces out check-ins after one fails, so a rate limit or a
	// broken reviewer is not hit every tick.
	retryAfter = 10 * time.Minute

	terminalLines = 80
	recentKept    = 3

	// BriefFile is the worker's brief, inside its directory so reading it
	// needs no permission.
	BriefFile = ".shed/BRIEF.md"

	startPrompt   = "You are an unattended worker. Read " + BriefFile + " in full and carry it out."
	briefChanged  = "Your brief changed. Read " + BriefFile + " again and follow it."
	reorient      = "Your context was cleared. Read " + BriefFile + " first, then check git status, git log and the issue comments to see where the work stands. Then: "
	clearSettling = 2 * time.Second
)

// Check-in kinds: why the supervisor looked.
const (
	KindStart   = "start"   // the worker had no window
	KindRestart = "restart" // its session had exited to the shell
	KindIdle    = "idle"    // it went quiet
	KindScope   = "scope"   // it has worked a while; is it still in its brief?
	KindQueue   = "queue"   // it was resting and its queue changed
	KindBrief   = "brief"   // its brief was edited
	KindError   = "error"
)

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true, "-zsh": true, "-bash": true}

type Supervisor struct {
	Machine  config.Machine
	Settings config.Supervisor
	Signals  []config.Signal
	// StandingOrders are the owner's rules for every worker.
	StandingOrders string
	Terminal       Terminal
	Reviewer       Reviewer
	Lister         backlog.Lister
	StateDir       string
	Now            func() time.Time
	// Sleep waits for the worker's terminal to take a command.
	Sleep func(time.Duration)
	Logf  func(format string, args ...any)

	workers map[string]*workerState
}

// workerState is what the supervisor remembers between ticks. It is lost on
// an agent restart; the cost is one extra check-in per worker.
type workerState struct {
	briefChecked bool
	lastReview   time.Time
	// reviewedActivity is the window's activity time at the last review. An
	// idle worker is reviewed once per silence, not once per tick.
	reviewedActivity time.Time
	lastQueueCheck   time.Time
	queueSeen        string
	nudges           []time.Time
	starts           []time.Time
	gaveUpStarting   bool
	notBefore        time.Time
	lastError        string
	// model is the model the worker was last switched to; empty when unknown.
	model  string
	recent []runlog.Checkin
}

// Tick checks every worker once. A failure on one worker is recorded and
// does not stop the others.
func (s *Supervisor) Tick(ctx context.Context) {
	if s.workers == nil {
		s.workers = make(map[string]*workerState)
	}
	for _, w := range s.Machine.Workers {
		st := s.workers[w.Name]
		if st == nil {
			st = &workerState{}
			s.workers[w.Name] = st
		}
		now := s.Now()
		if now.Before(st.notBefore) {
			continue
		}
		err := s.check(ctx, w, st, now)
		if err == nil {
			st.lastError = ""
			continue
		}
		st.notBefore = now.Add(retryAfter)
		// The same failure is recorded once, not every retry.
		if err.Error() != st.lastError {
			st.lastError = err.Error()
			s.record(st, runlog.Checkin{Time: now, Worker: w.Name, Kind: KindError, Verdict: runlog.VerdictError, Reason: err.Error()})
		}
	}
}

func (s *Supervisor) check(ctx context.Context, w config.Worker, st *workerState, now time.Time) error {
	obs, err := s.Terminal.Observe(w.Name)
	if err != nil {
		return err
	}
	if !obs.Exists || shells[obs.Command] {
		return s.start(w, st, obs.Exists, now)
	}
	st.gaveUpStarting = false

	if !st.briefChecked {
		changed, err := s.writeBrief(w)
		if err != nil {
			return err
		}
		st.briefChecked = true
		if changed {
			if err := s.Terminal.Send(w.Name, briefChanged); err != nil {
				return err
			}
			s.record(st, runlog.Checkin{Time: now, Worker: w.Name, Kind: KindBrief, Verdict: runlog.VerdictNudge, Reason: "the brief was edited", Message: briefChanged, Sent: true})
			return nil
		}
	}

	idle := now.Sub(obs.LastActivity)
	switch {
	case idle < settle:
		if now.Sub(st.lastReview) < s.Settings.ScopeEveryOrDefault() {
			return nil
		}
		return s.review(ctx, w, st, obs, now, KindScope, nil)
	case !obs.LastActivity.Equal(st.reviewedActivity):
		return s.review(ctx, w, st, obs, now, KindIdle, nil)
	case now.Sub(st.lastQueueCheck) >= queueRecheck:
		// The worker is resting after a review. Only new work wakes it.
		queue, err := s.queue(ctx)
		if err != nil {
			return err
		}
		st.lastQueueCheck = now
		if fingerprint(queue) == st.queueSeen {
			return nil
		}
		return s.review(ctx, w, st, obs, now, KindQueue, queue)
	}
	return nil
}

// start opens the worker's window if needed and starts its session.
func (s *Supervisor) start(w config.Worker, st *workerState, windowExists bool, now time.Time) error {
	st.starts = within(st.starts, now)
	if len(st.starts) >= maxStarts {
		if !st.gaveUpStarting {
			st.gaveUpStarting = true
			s.record(st, runlog.Checkin{Time: now, Worker: w.Name, Kind: KindRestart, Verdict: runlog.VerdictStuck,
				Reason: fmt.Sprintf("its session ended %d times in %s; not starting it again", maxStarts, loopWindow)})
		}
		return nil
	}
	if _, err := s.writeBrief(w); err != nil {
		return err
	}
	st.briefChecked = true
	kind := KindRestart
	if !windowExists {
		kind = KindStart
		if err := s.Terminal.Open(w.Name, expandHome(w.Dir)); err != nil {
			return err
		}
	}
	command := w.CommandOrDefault() + " " + shellQuote(startPrompt)
	if err := s.Terminal.Send(w.Name, command); err != nil {
		return err
	}
	st.starts = append(st.starts, now)
	st.lastReview, st.model = now, ""
	s.record(st, runlog.Checkin{Time: now, Worker: w.Name, Kind: kind, Verdict: runlog.VerdictStarted, Message: command, Sent: true})
	return nil
}

// review asks the reviewer about one worker and acts on the verdict.
func (s *Supervisor) review(ctx context.Context, w config.Worker, st *workerState, obs Observation, now time.Time, kind string, queue []QueueItem) error {
	if queue == nil {
		var err error
		if queue, err = s.queue(ctx); err != nil {
			return err
		}
	}
	terminal, err := s.Terminal.Capture(w.Name, terminalLines)
	if err != nil {
		return err
	}
	idle := now.Sub(obs.LastActivity)
	in := ReviewInput{Worker: w.Name, Brief: s.scope(w), Queue: queue, Recent: st.recent, Terminal: terminal}
	if idle >= settle {
		in.Idle = idle
	}
	verdict, err := s.Reviewer.Review(ctx, Prompt(in))
	if err != nil {
		return err
	}
	st.lastReview, st.reviewedActivity = now, obs.LastActivity
	st.lastQueueCheck, st.queueSeen = now, fingerprint(queue)

	c := runlog.Checkin{Time: now, Worker: w.Name, Kind: kind, Verdict: verdict.Verdict, Reason: verdict.Reason}
	if verdict.Verdict == runlog.VerdictNudge {
		st.nudges = within(st.nudges, now)
		if len(st.nudges) >= maxNudges {
			c.Verdict = runlog.VerdictStuck
			c.Reason = fmt.Sprintf("%d nudges in %s did not get it moving; the next would have been: %s", maxNudges, loopWindow, verdict.Message)
		} else {
			// Past the cache lifetime the whole context is read again at full
			// price. A change of model empties the cache too. In both cases a
			// cleared context that re-reads the brief costs less.
			c.Cold = idle > s.Settings.CacheTTLOrDefault()
			c.Message, c.Issue = verdict.Message, verdict.Issue
			model := modelOf(queue, verdict.Issue)
			switchModel := model != "" && model != st.model
			if switchModel || (c.Cold && s.Settings.ClearWhenCold()) {
				if err := s.command(w, s.Settings.ClearCommandOrDefault()); err != nil {
					return err
				}
				c.Message = reorient + verdict.Message
			}
			if switchModel {
				if err := s.command(w, s.Settings.ModelCommandFor(model)); err != nil {
					return err
				}
				st.model, c.Model = model, model
			}
			if err := s.Terminal.Send(w.Name, c.Message); err != nil {
				return err
			}
			c.Sent = true
			st.nudges = append(st.nudges, now)
		}
	}
	s.record(st, c)
	return nil
}

// command types a command of the worker's own program, such as its clear or
// model command, and gives it time to take effect.
func (s *Supervisor) command(w config.Worker, line string) error {
	if err := s.Terminal.Send(w.Name, line); err != nil {
		return err
	}
	s.Sleep(clearSettling)
	return nil
}

// modelOf returns the model of the queue item with this number.
func modelOf(queue []QueueItem, number int) string {
	for _, q := range queue {
		if q.Number == number {
			return q.Model
		}
	}
	return ""
}

// queue lists the machine's open issues, oldest first.
func (s *Supervisor) queue(ctx context.Context) ([]QueueItem, error) {
	items := []QueueItem{}
	for _, src := range s.Machine.Issues {
		issues, err := s.Lister.List(ctx, src)
		if err != nil {
			return nil, err
		}
		for _, i := range issues {
			items = append(items, QueueItem{Repo: i.Repo, Number: i.Number, Title: i.Title, Waiting: backlog.Waiting(i, s.Signals),
				Model: s.Settings.Models.For(i.LabelNames())})
		}
	}
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].Repo != items[b].Repo {
			return items[a].Repo < items[b].Repo
		}
		return items[a].Number < items[b].Number
	})
	return items, nil
}

// fingerprint changes when an issue joins or leaves the queue, or when what
// an issue waits on changes (for example the owner answered).
func fingerprint(queue []QueueItem) string {
	var b strings.Builder
	for _, q := range queue {
		fmt.Fprintf(&b, "%s#%d:%s;", q.Repo, q.Number, strings.Join(q.Waiting, ","))
	}
	return b.String()
}

func (s *Supervisor) record(st *workerState, c runlog.Checkin) {
	st.recent = append(st.recent, c)
	if len(st.recent) > recentKept {
		st.recent = st.recent[len(st.recent)-recentKept:]
	}
	s.Logf("worker %s: %s %s: %s", c.Worker, c.Kind, c.Verdict, c.Reason)
	if err := runlog.AppendCheckin(s.StateDir, c); err != nil {
		s.Logf("worker %s: %v", c.Worker, err)
	}
}

// writeBrief writes the worker's brief into its directory and reports
// whether an earlier brief there was different.
func (s *Supervisor) writeBrief(w config.Worker) (changed bool, err error) {
	dir := expandHome(w.Dir)
	path := filepath.Join(dir, BriefFile)
	content := s.briefText(w)
	old, readErr := os.ReadFile(path)
	if readErr == nil && string(old) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("worker %s: create brief directory: %w", w.Name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return false, fmt.Errorf("worker %s: write brief: %w", w.Name, err)
	}
	excludeFromGit(dir)
	return readErr == nil, nil
}

func (s *Supervisor) briefText(w config.Worker) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Brief for worker %s on %s\n\n%s\n", w.Name, s.Machine.Name, s.scope(w))
	if len(s.Machine.Issues) > 0 {
		b.WriteString("\n## Queue\n\nYour queue is the open GitHub issues of:\n\n")
		for _, src := range s.Machine.Issues {
			fmt.Fprintf(&b, "- %s", src.Repo)
			if src.Label != "" {
				fmt.Fprintf(&b, " with label `%s`", src.Label)
			}
			if src.Assignee != "" {
				fmt.Fprintf(&b, " assigned to `%s`", src.Assignee)
			}
			b.WriteString("\n")
		}
		b.WriteString("\nWork one issue at a time. The supervisor names your first issue and each next one; wait for it.\nWhen you finish an issue, or cannot go further on it, report in the issue, say so here, and stop.\n")
	}
	b.WriteString(`
## You work unattended

Nobody is at the keyboard. A supervisor reads this terminal from time to time
and may type a short message. Treat it as guidance inside this brief, not as a
new brief.

- Stay inside this brief. Work that it does not cover is out of scope: note it in the issue and leave it.
- Do not wait for an answer. When a decision belongs to the owner, write the question and your recommendation in an issue comment`)
	if phrase := s.decisionPhrase(); phrase != "" {
		fmt.Fprintf(&b, " after the words `%s`", phrase)
	}
	b.WriteString(`, then take the next item.
- Before you stop for any reason, write your plan and your progress in the issue. Your context may be cleared between items. What is not in the issue, the branch or a pull request is lost.
- When nothing is left that you can act on, say so and stop. Do not invent work.
`)
	return b.String()
}

// scope is what the worker must stay inside: its brief and the owner's
// standing orders. The worker reads it and the reviewer judges against it.
func (s *Supervisor) scope(w config.Worker) string {
	scope := strings.TrimSpace(w.Brief)
	if orders := strings.TrimSpace(s.StandingOrders); orders != "" {
		scope += "\n\n## Standing orders\n\n" + orders
	}
	return scope
}

// decisionPhrase is the phrase that makes shed status show an issue as
// waiting on a decision.
func (s *Supervisor) decisionPhrase() string {
	for _, sig := range s.Signals {
		if sig.Name == "decision" {
			return sig.Ask
		}
	}
	return ""
}

// excludeFromGit keeps the brief out of the worker's commits. It is best
// effort: the directory may not be a git repository.
func excludeFromGit(dir string) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return
	}
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	exclude := filepath.Join(gitDir, "info", "exclude")
	if data, _ := os.ReadFile(exclude); strings.Contains(string(data), ".shed/") {
		return
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		return
	}
	if f, err := os.OpenFile(exclude, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.WriteString("\n.shed/\n")
		f.Close()
	}
}

func expandHome(dir string) string {
	if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return dir
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// within keeps the times inside loopWindow of now.
func within(times []time.Time, now time.Time) []time.Time {
	kept := times[:0]
	for _, t := range times {
		if now.Sub(t) < loopWindow {
			kept = append(kept, t)
		}
	}
	return kept
}
