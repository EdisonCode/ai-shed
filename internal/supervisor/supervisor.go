// Package supervisor keeps a machine's workers going while the owner is
// away. It starts each worker's session, checks in when a worker goes quiet
// or has worked a while, and types a short message when that gets work
// moving again.
package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

	// A limited worker is looked at again a little after its limit resets.
	// When the screen names no reset time, or the reviewer itself is
	// limited, it is looked at again after limitRecheck.
	limitRecheck = 15 * time.Minute
	limitSlack   = time.Minute
	limitLongest = 6 * time.Hour

	terminalLines = 80
	recentKept    = 3

	// BriefFile is the worker's brief, inside its directory so reading it
	// needs no permission.
	BriefFile = ".shed/BRIEF.md"

	startPrompt      = "You are an unattended worker. Read " + BriefFile + " in full and carry it out."
	briefChanged     = "Your brief changed. Read " + BriefFile + " again and follow it."
	briefChangedThen = "Your brief changed. Read " + BriefFile + " again. Then: "
	reorient         = "Your context was cleared. Read " + BriefFile + " first, then check git status, git log and the issue comments to see where the work stands. Then: "
	modelTold        = " The model for this issue is %s."
	clearSettling    = 2 * time.Second
)

// Check-in kinds: why the supervisor looked.
const (
	KindStart    = "start"    // the worker had no window
	KindRestart  = "restart"  // its session had exited to the shell
	KindIdle     = "idle"     // it went quiet
	KindScope    = "scope"    // it has worked a while; is it still in its brief?
	KindQueue    = "queue"    // it was resting and its queue changed
	KindBrief    = "brief"    // its brief was edited
	KindLimit    = "limit"    // its usage limit was due to reset
	KindRecycle  = "recycle"  // the owner asked for a fresh session
	KindCapacity = "capacity" // a held hand-over was delivered
	KindError    = "error"
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
	// Busy says why the machine has no room for new work, or "" when it
	// has. Nil means it always has room.
	Busy func(ctx context.Context) string
	// Notify tells the owner that a worker has started to need them. It may
	// be nil.
	Notify func(worker, message string)
	// Sleep waits for the worker's terminal to take a command.
	Sleep func(time.Duration)
	Logf  func(format string, args ...any)

	workers map[string]*workerState
}

// workerState is what the supervisor remembers between ticks. A new agent
// rebuilds it from the check-in log, so a restart or a reloaded fleet file
// costs a working worker nothing: no extra review and no repeated message.
type workerState struct {
	briefChecked bool
	// held is a hand-over that waits for the machine to have room.
	held *heldHandOver
	// limitedUntil is set while the worker is at a usage limit. Nothing is
	// reviewed or typed before then.
	limitedUntil time.Time
	// briefStale is set when the brief changed while the worker had an issue
	// in hand. It is told with the next message, not in the middle of work.
	briefStale bool
	lastReview time.Time
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
	model string
	// issue is the issue the worker was last handed; 0 when it has had none
	// since its session started.
	issue  int
	recent []runlog.Checkin
}

// lastVerdict is the verdict of the worker's latest check-in.
func (st *workerState) lastVerdict() string {
	if len(st.recent) == 0 {
		return ""
	}
	return st.recent[len(st.recent)-1].Verdict
}

// heldHandOver is a hand-over the reviewer chose that was not delivered
// because the machine was busy.
type heldHandOver struct {
	verdict Verdict
	since   time.Time
	// activity is the worker's last output when the hand-over was chosen. If
	// the screen has changed since, the choice is out of date.
	activity time.Time
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
			st = s.recover(w.Name)
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
		// The agent is stopping or reloading. A check it cut short is not a
		// failure of the worker.
		if ctx.Err() != nil {
			return
		}
		c := runlog.Checkin{Time: now, Worker: w.Name, Kind: KindError, Verdict: runlog.VerdictError, Reason: err.Error()}
		st.notBefore = now.Add(retryAfter)
		// A reviewer at its usage limit is not a fault and needs nobody: the
		// worker is simply not looked at until the limit may have reset.
		if isLimit(err) {
			st.notBefore = now.Add(limitRecheck)
			c.Kind, c.Verdict, c.Until = KindLimit, runlog.VerdictLimited, st.notBefore
			c.Reason = "the reviewer is at its usage limit: " + err.Error()
		}
		// The same failure is recorded once, not every retry.
		if err.Error() != st.lastError {
			st.lastError = err.Error()
			s.record(st, c)
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
		switch {
		case !changed:
		case st.issue != 0:
			st.briefStale = true
			s.record(st, runlog.Checkin{Time: now, Worker: w.Name, Kind: KindBrief, Verdict: runlog.VerdictOnTrack,
				Reason: fmt.Sprintf("the brief was edited; it is on #%d and will be told with its next message", st.issue)})
		default:
			if err := s.Terminal.Send(w.Name, briefChanged); err != nil {
				return err
			}
			s.record(st, runlog.Checkin{Time: now, Worker: w.Name, Kind: KindBrief, Verdict: runlog.VerdictNudge, Reason: "the brief was edited", Message: briefChanged, Sent: true})
			return nil
		}
	}

	// A fresh session was asked for. It waits for the issue in hand unless
	// the owner said now. A worker that was last found done has handed that
	// issue back: nothing is in progress.
	if pending, immediately := s.recycleRequested(w.Name); pending && (immediately || st.issue == 0 || st.lastVerdict() == runlog.VerdictDone) {
		return s.recycle(w, st, now)
	}

	if st.held != nil {
		if !obs.LastActivity.Equal(st.held.activity) {
			// The worker did something. What was chosen for it is out of date.
			st.held = nil
		} else {
			reason, waited := s.busy(ctx), now.Sub(st.held.since)
			switch {
			case reason == "":
				return s.release(ctx, w, st, obs, now, fmt.Sprintf("the machine has room again after %s", waited.Round(time.Second)))
			case waited >= s.Machine.Capacity.MaxWaitOrDefault():
				// Resist, not refuse: a machine that never goes quiet must
				// not starve its workers.
				return s.release(ctx, w, st, obs, now, fmt.Sprintf("handed over after waiting %s, although: %s", waited.Round(time.Second), reason))
			}
			return nil
		}
	}

	idle := now.Sub(obs.LastActivity)
	switch {
	case now.Before(st.limitedUntil):
		return nil
	case !st.limitedUntil.IsZero():
		// The limit was due to reset. The screen has not changed, so only
		// this makes the supervisor look again.
		return s.review(ctx, w, st, obs, now, KindLimit, nil)
	case idle < settle:
		if now.Sub(st.lastReview) < s.Settings.ScopeEveryOrDefault() {
			return nil
		}
		return s.review(ctx, w, st, obs, now, KindScope, nil)
	case !obs.LastActivity.Equal(st.reviewedActivity):
		return s.review(ctx, w, st, obs, now, KindIdle, nil)
	case now.Sub(st.lastQueueCheck) >= queueRecheck:
		// The worker is resting after a review. Only new work wakes it.
		queue, err := s.queue(ctx, w)
		if err != nil {
			return err
		}
		st.lastQueueCheck = now
		seen := fingerprint(queue)
		if seen == st.queueSeen {
			return nil
		}
		// What changed may have left nothing to hand over: the last workable
		// item was closed, or went to another worker. That needs no review.
		if !slices.ContainsFunc(queue, QueueItem.Workable) {
			st.queueSeen = seen
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
	st.lastReview, st.model, st.issue = now, "", 0
	s.record(st, runlog.Checkin{Time: now, Worker: w.Name, Kind: kind, Verdict: runlog.VerdictStarted, Message: command, Sent: true})
	return nil
}

// review asks the reviewer about one worker and acts on the verdict.
func (s *Supervisor) review(ctx context.Context, w config.Worker, st *workerState, obs Observation, now time.Time, kind string, queue []QueueItem) error {
	if queue == nil {
		var err error
		if queue, err = s.queue(ctx, w); err != nil {
			return err
		}
	}
	terminal, err := s.Terminal.Capture(w.Name, terminalLines)
	if err != nil {
		return err
	}
	idle := now.Sub(obs.LastActivity)
	in := ReviewInput{Worker: w.Name, Brief: s.scope(w), Queue: queue, Recent: st.recent, Terminal: terminal, Now: now, LimitedUntil: st.limitedUntil, InHand: st.issue}
	if idle >= settle {
		in.Idle = idle
	}
	verdict, err := s.Reviewer.Review(ctx, Prompt(in))
	if err != nil {
		return err
	}
	// The worker has reached a point where nothing is in progress: it is done,
	// or it is about to be given another issue. A fresh session that was
	// asked for starts here; the hand-over happens in the new session.
	if pending, _ := s.recycleRequested(w.Name); pending {
		handOver := verdict.Verdict == runlog.VerdictNudge && verdict.Issue != 0 && verdict.Issue != st.issue
		if handOver || verdict.Verdict == runlog.VerdictDone {
			return s.recycle(w, st, now)
		}
	}
	item, inQueue := find(queue, verdict.Issue)
	if verdict.Issue != 0 && !inQueue {
		// Another worker may have it in hand, or the reviewer made it up.
		return fmt.Errorf("the reviewer handed over #%d, which is not in this worker's queue", verdict.Issue)
	}
	st.lastReview, st.reviewedActivity = now, obs.LastActivity
	st.lastQueueCheck, st.queueSeen = now, fingerprint(queue)

	c := runlog.Checkin{Time: now, Worker: w.Name, Kind: kind, Verdict: verdict.Verdict, Reason: verdict.Reason,
		Activity: obs.LastActivity, Queue: st.queueSeen}
	st.limitedUntil = time.Time{}
	if verdict.Verdict == runlog.VerdictLimited {
		wait := limitRecheck
		if verdict.ResumeInMinutes > 0 {
			wait = min(time.Duration(verdict.ResumeInMinutes)*time.Minute+limitSlack, limitLongest)
		}
		st.limitedUntil = now.Add(wait)
		c.Until = st.limitedUntil
	}
	if verdict.Verdict == runlog.VerdictNudge {
		// A different issue is a hand-over: new work for the machine. It is
		// held while the machine is busy with something else.
		handOver := verdict.Issue != 0 && verdict.Issue != st.issue
		if reason := s.busy(ctx); handOver && reason != "" {
			st.held = &heldHandOver{verdict: verdict, since: now, activity: obs.LastActivity}
			c.Verdict, c.Issue = runlog.VerdictHeld, verdict.Issue
			c.Reason = fmt.Sprintf("#%d is held for this worker: %s", verdict.Issue, reason)
		} else if err := s.deliver(w, st, now, idle, kind, verdict, item, &c); err != nil {
			return err
		}
	}
	s.record(st, c)
	return nil
}

// deliver types a nudge into the worker, unless nudges have stopped helping.
// It fills in the check-in with what was sent.
func (s *Supervisor) deliver(w config.Worker, st *workerState, now time.Time, idle time.Duration, kind string, verdict Verdict, item QueueItem, c *runlog.Checkin) error {
	st.nudges = within(st.nudges, now)
	if len(st.nudges) >= maxNudges {
		c.Verdict = runlog.VerdictStuck
		c.Reason = fmt.Sprintf("%d nudges in %s did not get it moving; the next would have been: %s", maxNudges, loopWindow, verdict.Message)
		return nil
	}
	// A context is worth keeping only while it is warm and still about
	// the work in hand. Past the cache lifetime it is read again at
	// full price; a change of model empties the cache too; and a
	// worker set to start each issue fresh should not carry the last
	// issue into the next. In each case a cleared context that
	// re-reads the brief costs less.
	c.Cold = idle > s.Settings.CacheTTLOrDefault()
	c.Message, c.Issue = verdict.Message, verdict.Issue
	// A nudge may name the issue the worker is already on. Only a
	// different issue is a hand-over.
	newIssue := verdict.Issue != 0 && verdict.Issue != st.issue
	model := item.Model
	c.Rework = item.Rework
	tellOnly := s.Settings.Models.TellOnly()
	switchModel := newIssue && !tellOnly && model != "" && model != st.model
	// The first issue of a session lands on a context that is
	// already empty.
	fresh := newIssue && w.FreshPerIssue && st.issue != 0
	// A worker stopped by a usage limit was cut off in the middle of
	// its work and wrote nothing down. Its context is all there is of
	// that work, so it is kept, at the price of reading it again.
	coldClear := c.Cold && s.Settings.ClearWhenCold() && kind != KindLimit
	cleared := switchModel || fresh || coldClear
	switch {
	case cleared:
		if err := s.command(w, s.Settings.ClearCommandOrDefault()); err != nil {
			return err
		}
		// This message already sends the worker back to its brief.
		c.Message = reorient + verdict.Message
	case st.briefStale:
		c.Message = briefChangedThen + verdict.Message
	}
	st.briefStale = false
	if switchModel {
		if err := s.command(w, s.Settings.ModelCommandFor(model)); err != nil {
			return err
		}
		st.model, c.Model = model, model
	}
	if newIssue {
		st.issue = verdict.Issue
	}
	if tellOnly && model != "" {
		c.Model = model
		c.Message += fmt.Sprintf(modelTold, model)
	}
	if err := s.Terminal.Send(w.Name, c.Message); err != nil {
		return err
	}
	c.Sent = true
	st.nudges = append(st.nudges, now)
	return nil
}

// busy says why the machine has no room for new work, or "" when it has.
func (s *Supervisor) busy(ctx context.Context) string {
	if s.Busy == nil {
		return ""
	}
	return s.Busy(ctx)
}

// release hands over an issue that was held while the machine was busy. The
// reviewer already chose it; it is not asked again.
func (s *Supervisor) release(ctx context.Context, w config.Worker, st *workerState, obs Observation, now time.Time, reason string) error {
	held := st.held
	st.held = nil
	queue, err := s.queue(ctx, w)
	if err != nil {
		return err
	}
	item, inQueue := find(queue, held.verdict.Issue)
	if !inQueue {
		// It was closed, or taken, while it was held. The next tick reviews
		// the worker afresh.
		st.reviewedActivity = time.Time{}
		return nil
	}
	c := runlog.Checkin{Time: now, Worker: w.Name, Kind: KindCapacity, Verdict: runlog.VerdictNudge, Reason: reason,
		Activity: obs.LastActivity, Queue: fingerprint(queue)}
	if err := s.deliver(w, st, now, now.Sub(obs.LastActivity), KindCapacity, held.verdict, item, &c); err != nil {
		return err
	}
	st.lastReview, st.reviewedActivity = now, obs.LastActivity
	st.lastQueueCheck, st.queueSeen = now, c.Queue
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

// recycleRequested reports whether the owner asked for a fresh session for
// the worker, and whether they asked for it at once.
func (s *Supervisor) recycleRequested(worker string) (pending, immediately bool) {
	data, err := os.ReadFile(filepath.Join(s.StateDir, runlog.RecycleDir, worker))
	if err != nil {
		return false, false
	}
	return true, strings.TrimSpace(string(data)) == runlog.RecycleNow
}

// recycle ends the worker's session. The next tick finds no window and
// starts a new one, which reads the brief and waits for its first issue.
func (s *Supervisor) recycle(w config.Worker, st *workerState, now time.Time) error {
	if err := s.Terminal.Close(w.Name); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.StateDir, runlog.RecycleDir, w.Name)); err != nil {
		return fmt.Errorf("worker %s: clear the recycle request: %w", w.Name, err)
	}
	st.issue, st.model, st.limitedUntil = 0, "", time.Time{}
	s.record(st, runlog.Checkin{Time: now, Worker: w.Name, Kind: KindRecycle, Verdict: runlog.VerdictRecycled, Reason: "its session was ended so that a fresh one starts"})
	return nil
}

// isLimit reports whether a reviewer failed because it reached a usage limit.
func isLimit(err error) bool {
	text := strings.ToLower(err.Error())
	for _, phrase := range []string{"usage limit", "rate limit", "limit reached", "hit your limit"} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// recover rebuilds a worker's state from the check-in log.
func (s *Supervisor) recover(worker string) *workerState {
	st := &workerState{}
	checkins, err := runlog.ReadCheckins(s.StateDir)
	if err != nil {
		s.Logf("worker %s: %v", worker, err)
		return st
	}
	for _, c := range checkins {
		if c.Worker != worker {
			continue
		}
		st.recent = append(st.recent, c)
		switch {
		case c.Verdict == runlog.VerdictStarted:
			// A new session: nothing in hand, the model its command gave it.
			st.issue, st.model, st.briefStale = 0, "", false
			st.starts = append(st.starts, c.Time)
			st.lastReview = c.Time
		case c.Verdict == runlog.VerdictRecycled:
			st.issue, st.model, st.limitedUntil = 0, "", time.Time{}
		case c.Kind == KindBrief:
			st.briefStale = !c.Sent
		case c.Verdict == runlog.VerdictLimited && c.Activity.IsZero():
			// The reviewer was limited; the worker itself was not reviewed.
		case c.Kind == KindIdle || c.Kind == KindScope || c.Kind == KindQueue || c.Kind == KindLimit || c.Kind == KindCapacity:
			st.lastReview, st.lastQueueCheck = c.Time, c.Time
			st.reviewedActivity, st.queueSeen = c.Activity, c.Queue
			st.limitedUntil = time.Time{}
			if c.Verdict == runlog.VerdictLimited {
				st.limitedUntil = c.Until
			}
			if c.Verdict == runlog.VerdictHeld {
				// What was held is not in the log in full. The new agent
				// reviews the worker once more and holds or hands over.
				st.reviewedActivity = time.Time{}
			}
			if !c.Sent {
				continue
			}
			st.nudges = append(st.nudges, c.Time)
			st.briefStale = false
			if c.Issue != 0 {
				st.issue = c.Issue
			}
			if c.Model != "" {
				st.model = c.Model
			}
		}
	}
	if len(st.recent) > recentKept {
		st.recent = st.recent[len(st.recent)-recentKept:]
	}
	return st
}

// queue lists the worker's open issues in the order to work them: rework
// first, since finishing started work beats starting more, then by the
// owner's priority, then oldest first. An issue that another worker has in
// hand is not in it.
func (s *Supervisor) queue(ctx context.Context, w config.Worker) ([]QueueItem, error) {
	checkins, err := runlog.ReadCheckins(s.StateDir)
	if err != nil {
		return nil, err
	}
	taken := map[int]bool{}
	for _, other := range s.Machine.Workers {
		if other.Name != w.Name {
			taken[runlog.IssueInHand(checkins, other.Name)] = true
		}
	}
	items := []QueueItem{}
	for _, src := range s.Machine.SourcesFor(w) {
		issues, err := s.Lister.List(ctx, src)
		if err != nil {
			return nil, err
		}
		for _, i := range issues {
			if taken[i.Number] {
				continue
			}
			item := QueueItem{Repo: i.Repo, Number: i.Number, Title: i.Title, Waiting: backlog.Waiting(i, s.Signals),
				Model: s.Settings.Models.For(i.LabelNames()), rank: src.Rank(i.LabelNames())}
			if reason := backlog.Rework(i, s.Signals); reason != "" {
				if runlog.ReworkSpent(checkins, i.Number, s.Now()) {
					item.Waiting = append(item.Waiting, fmt.Sprintf("%s after %d tries", reason, runlog.ReworkLimit))
				} else {
					item.Rework = reason
				}
			}
			items = append(items, item)
		}
	}
	sort.SliceStable(items, func(a, b int) bool {
		x, y := items[a], items[b]
		switch {
		case (x.Rework != "") != (y.Rework != ""):
			return x.Rework != ""
		case x.rank != y.rank:
			return x.rank < y.rank
		case x.Repo != y.Repo:
			return x.Repo < y.Repo
		}
		return x.Number < y.Number
	})
	return items, nil
}

// find returns the queue item with this number.
func find(queue []QueueItem, number int) (QueueItem, bool) {
	for _, q := range queue {
		if q.Number == number {
			return q, true
		}
	}
	return QueueItem{}, false
}

// fingerprint is a digest of what the queue holds for a worker to do: its
// workable items. It changes when one joins or leaves, when the owner's
// answer frees an issue, or when a handed-back pull request needs a worker
// again. An issue that only waits on the owner is not in it: a label, a
// comment or a new hand-back that gives a worker nothing to do must not cost
// every resting worker a review.
func fingerprint(queue []QueueItem) string {
	h := sha256.New()
	for _, q := range queue {
		if q.Workable() {
			fmt.Fprintf(h, "%s#%d:%s;", q.Repo, q.Number, q.Rework)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// needsOwner maps the verdicts that need the owner to how a notification
// says them.
var needsOwner = map[string]string{
	runlog.VerdictNeedsOwner: "needs you",
	runlog.VerdictStuck:      "is stuck",
	runlog.VerdictError:      "could not be checked",
}

func (s *Supervisor) record(st *workerState, c runlog.Checkin) {
	// The owner is told when a worker starts to need them, not every time
	// the same state is seen again.
	previous := ""
	if len(st.recent) > 0 {
		previous = st.recent[len(st.recent)-1].Verdict
	}
	if phrase, ok := needsOwner[c.Verdict]; ok && c.Verdict != previous && s.Notify != nil {
		s.Notify(c.Worker, fmt.Sprintf("%s: worker %s %s: %s", s.Machine.Name, c.Worker, phrase, c.Reason))
	}
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
	if sources := s.Machine.SourcesFor(w); len(sources) > 0 {
		b.WriteString("\n## Queue\n\nYour queue is the open GitHub issues of:\n\n")
		for _, src := range sources {
			fmt.Fprintf(&b, "- %s", src.Repo)
			if src.Label != "" {
				fmt.Fprintf(&b, " with label `%s`", src.Label)
			}
			if src.Assignee != "" {
				fmt.Fprintf(&b, " assigned to `%s`", src.Assignee)
			}
			b.WriteString("\n")
		}
		b.WriteString("\nWork one issue at a time. The supervisor names your first issue and each next one; wait for it.\nStart each issue on a new branch from the current default branch.\nWhen you finish an issue, or cannot go further on it, report in the issue, say so here, and stop.\n")
	}
	b.WriteString(`
## You work unattended

Nobody is at the keyboard. A supervisor reads this terminal from time to time
and may type a short message. Treat it as guidance inside this brief, not as a
new brief.

- Stay inside this brief. Work that it does not cover is out of scope: note it in the issue and leave it.
- Do not wait for an answer. When a decision belongs to the owner, write the question and your recommendation in an issue comment`)
	if phrase, handBack := s.decisionPhrase(); phrase != "" {
		fmt.Fprintf(&b, " after the words `%s`", phrase)
		if handBack != "" {
			fmt.Fprintf(&b, ", in a comment headed `%s`", handBack)
		}
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
// waiting on a decision, and the phrase that must head the comment for it to
// count.
func (s *Supervisor) decisionPhrase() (ask, handBack string) {
	for _, sig := range s.Signals {
		if sig.Name == "decision" {
			return sig.Ask, sig.HandBack
		}
	}
	return "", ""
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
