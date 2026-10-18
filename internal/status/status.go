// Package status combines what the machines report and what GitHub says into
// one answer: is each machine on track, and what needs the owner.
package status

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/looks"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

const (
	probeTimeout = 45 * time.Second
	diskFullPct  = 90
	// highLoadAfter is how long the load must stay at or above the cores to
	// be flagged: a build's burst is not a full machine.
	highLoadAfter  = 10 * time.Minute
	heartbeatStale = 3 * time.Minute
	missedGrace    = 2 * time.Minute
	workerQuiet    = 30 * time.Minute
)

// Issue states.
const (
	Working   = "working"   // a tmux window is named for the issue
	Rework    = "rework"    // handed back, but its pull request cannot merge; a worker will be sent back to it
	Queued    = "queued"    // nobody is on it
	Waiting   = "waiting"   // it waits on the owner
	Look      = "eye check" // its merged work waits for a worker to look at it on staging
	Draft     = "draft"     // its pull request is a draft on purpose and says what it waits for
	Merged    = "merged"    // its pull request merged and its hand-back lists nothing still to do; it is not handed out
	Held      = "held"      // it waits for an issue it depends on, or for work in flight on the same code
	Verifying = "verifying" // handed back; its pull request waits for its checks and a verifier before the owner reviews it
)

// Task states.
const (
	TaskNever   = "never"
	TaskRunning = "running"
	TaskOK      = "ok"
	TaskFailed  = "failed"
)

// Report is the whole answer, as `shed status -json` prints it.
type Report struct {
	Machines []MachineReport `json:"machines"`
	// Repos is the release state of each repository under `repos` in the
	// fleet file; empty when it has none.
	Repos []RepoReport `json:"repos"`
	// Released are the issues a spent machine gave to the fleet that no
	// machine has taken yet.
	Released []ReleasedIssue `json:"released,omitempty"`
	// Balance names each machine whose workers are idle while another has
	// work nobody has started and no room for it.
	Balance []string `json:"balance,omitempty"`
}

// ReleasedIssue is an issue on an overflow label, waiting for a machine with
// room.
type ReleasedIssue struct {
	Repo   string    `json:"repo"`
	Number int       `json:"number"`
	Title  string    `json:"title"`
	By     string    `json:"by"`
	Since  time.Time `json:"since"`
	Label  string    `json:"label"`
}

type MachineReport struct {
	Name string `json:"name"`
	Host string `json:"host"`
	// Error is set when the machine could not be probed; Probe is then nil.
	Error  string        `json:"error,omitempty"`
	Probe  *probe.Result `json:"probe,omitempty"`
	Issues []IssueStatus `json:"issues"`
	Tasks  []TaskStatus  `json:"tasks"`
	// Workers are the supervised workers from the fleet file.
	Workers []WorkerStatus `json:"workers"`
	// OneOff lists the one-off tasks pushed to the machine: the ones that
	// wait, the ones in hand, and the ones the check-in log still knows of.
	OneOff []TaskState `json:"one_off,omitempty"`
	// Attention lists what needs the owner. Empty means the machine is on track.
	Attention []string `json:"attention"`
	// Unsupervised names the agent sessions on a machine with supervised
	// workers that are not one of them, as "session:window". They share
	// the machine's account and CPU with the workers.
	Unsupervised []string `json:"unsupervised,omitempty"`
	// sharedWith names, for the terminal, the machines that share this
	// one's account.
	sharedWith string
	// Now is when the report was made, by the watcher's clock. The age of
	// what waits on the owner is counted from it.
	Now time.Time `json:"now"`
}

type IssueStatus struct {
	Repo    string   `json:"repo"`
	Number  int      `json:"number"`
	Title   string   `json:"title"`
	URL     string   `json:"url"`
	State   string   `json:"state"`
	Waiting []string `json:"waiting,omitempty"`
	// Asks are the open signals behind Waiting, with when each was asked.
	Asks []backlog.Ask `json:"asks,omitempty"`
	// Drafts are the pull requests that are drafts on purpose: each says
	// what it waits for. They ask nothing of the owner yet.
	Drafts []backlog.Ask `json:"drafts,omitempty"`
	Worker string        `json:"worker,omitempty"`
	// Rework says why the issue's pull request cannot merge as it stands.
	Rework string `json:"rework,omitempty"`
	// Decided lists the choices a worker made by itself and recorded in its
	// last hand-back. They are for the owner to read; they hold nothing.
	Decided []string `json:"decided,omitempty"`
	// Bumped is set when the owner put the issue first in line with
	// `shed bump`, and BumpedTo names the worker it was moved to, if any.
	Bumped   bool   `json:"bumped,omitempty"`
	BumpedTo string `json:"bumped_to,omitempty"`
	// Held says why a queued issue is not handed out: an issue it depends on
	// is open, or one in a worker's hands touches the same code.
	Held string `json:"held,omitempty"`
	// ReturnsTo is the worker an answer hands the issue back to, when it
	// returns to one, and ReturnsIdle says that worker has no work now.
	ReturnsTo   string `json:"returns_to,omitempty"`
	ReturnsIdle bool   `json:"returns_idle,omitempty"`
}

// One-off task states.
const (
	OneOffQueued   = "queued"   // it waits for a worker to have nothing in progress
	OneOffInHand   = "in hand"  // a worker was handed it
	OneOffFinished = "finished" // the worker it was handed to moved on
)

// TaskState is where a one-off task stands.
type TaskState struct {
	ID    string `json:"id"`
	State string `json:"state"`
	// Worker is the worker it waits for or was handed to; empty while it
	// waits for any worker.
	Worker string `json:"worker,omitempty"`
}

type TaskStatus struct {
	Name     string         `json:"name"`
	Schedule string         `json:"schedule"`
	State    string         `json:"state"`
	Missed   bool           `json:"missed,omitempty"`
	Last     *runlog.Record `json:"last,omitempty"`
}

type WorkerStatus struct {
	Name string `json:"name"`
	// Tool is the program the worker runs, from its command in the fleet file.
	Tool string `json:"tool"`
	// Issue is the issue the supervisor last handed it; 0 for none.
	Issue int `json:"issue,omitempty"`
	// Task is the one-off task it has in hand; empty for none.
	Task string `json:"task,omitempty"`
	// Ask is set for a worker that answers the owner's questions and is
	// handed no work.
	Ask bool `json:"ask,omitempty"`
	// State is what the worker's own tool reports through its hooks:
	// working, idle or waiting for a person. Empty when the tool has no
	// hooks. StateSince is when it began; zero when that is not known.
	State      string    `json:"state,omitempty"`
	StateSince time.Time `json:"state_since,omitzero"`
	// RecyclePending is set when the owner asked for a fresh session and the
	// supervisor waits for the issue in hand to finish.
	RecyclePending bool `json:"recycle_pending,omitempty"`
	// Last is the supervisor's latest check-in; nil before the first one.
	Last *runlog.Checkin `json:"last,omitempty"`
}

type Collector struct {
	Runner probe.Runner
	Lister backlog.Lister
	// Looks reads the eye checks that merged work still owes. It is needed
	// only for CollectRepos.
	Looks LookReader
	// Usage reads what each machine's workers spent. It is needed only for
	// a digest, and nil leaves usage out.
	Usage UsageReader
	// Closed lists the issues closed in a digest's window, for the escape
	// rate of each verification tier. Nil leaves it out.
	Closed ClosedLister
	// Times reads when each step of a merged pull request happened. It is
	// needed only for a digest, and nil leaves the stages out.
	Times PullTimer
	// Batches reads the open batches of a repository with merge_train:
	// batch. Nil leaves them out.
	Batches BatchReader
}

// LookReader reads the eye checks a repository's merged work still owes.
type LookReader interface {
	Read(ctx context.Context, repo config.Repo, sources []config.IssueSource, open []backlog.Issue, signals []config.Signal) (looks.Report, error)
}

// RepoReport is the release state of one repository: what is merged since
// production and what of it nobody has looked at on staging.
type RepoReport struct {
	looks.Report
	// Error is set when the state could not be read.
	Error string `json:"error,omitempty"`
	// Lookers counts the workers that do eye checks for the repository.
	// With none, a look that is due is the owner's.
	Lookers int `json:"lookers"`
	// Now is when the report was made, by the watcher's clock.
	Now time.Time `json:"now"`
	// Train is where the merge train stands; nil where the repository runs
	// none.
	Train *Train `json:"train,omitempty"`
}

// CollectRepos reports on every repository under `repos` in the fleet file.
func (c Collector) CollectRepos(ctx context.Context, cfg *config.Config) []RepoReport {
	var reports []RepoReport
	for _, repo := range cfg.Repos {
		if repo.Deployed() || repo.Trains() {
			reports = append(reports, c.collectRepo(ctx, cfg, repo))
		}
	}
	return reports
}

// collectRepo lists the repository's issues and then reads its release
// state, each on its own deadline: a long backlog to list does not leave
// the release state without time.
func (c Collector) collectRepo(parent context.Context, cfg *config.Config, repo config.Repo) RepoReport {
	ctx, cancel := context.WithTimeout(parent, probeTimeout)
	defer cancel()
	report := RepoReport{Report: looks.Report{Repo: repo.Name}, Now: time.Now()}
	var sources []config.IssueSource
	var open []backlog.Issue
	seen := map[config.IssueSourceKey]bool{}
	for _, m := range cfg.Machines {
		for _, w := range m.Workers {
			if w.EyeChecks && slices.ContainsFunc(m.SourcesFor(w), func(src config.IssueSource) bool { return src.Repo == repo.Name }) {
				report.Lookers++
			}
		}
		for _, src := range m.AllSources() {
			if src.Repo != repo.Name || seen[src.Key()] {
				continue
			}
			seen[src.Key()] = true
			sources = append(sources, src)
			issues, err := c.Lister.List(ctx, src)
			if err != nil {
				report.Error = err.Error()
				return report
			}
			open = append(open, issues...)
		}
	}
	if repo.Trains() {
		report.Train = readTrain(open, cfg.Signals, report.Now, report.Lookers > 0, repo.Verify.MergeHigh)
		if repo.Batches() && c.Batches != nil {
			if err := readBatches(ctx, c.Batches, repo.Name, report.Train); err != nil {
				report.Error = err.Error()
				return report
			}
		}
	}
	if !repo.Deployed() {
		return report
	}
	lookCtx, cancelLooks := context.WithTimeout(parent, probeTimeout)
	defer cancelLooks()
	read, err := c.Looks.Read(lookCtx, repo, sources, open, cfg.Signals)
	if err != nil {
		report.Error = err.Error()
		return report
	}
	report.Report = read
	return report
}

// Attention lists what a repository's release state needs from the owner.
func (r RepoReport) Attention() []string {
	if r.Error != "" {
		return []string{"cannot read the release state: " + r.Error}
	}
	var attention []string
	if r.Unchecked != "" {
		attention = append(attention, "cannot tell which merged work staging serves, so none is counted as on staging: "+r.Unchecked)
	}
	if due := r.count(looks.Due); due > 0 && r.Lookers == 0 {
		attention = append(attention, fmt.Sprintf("%d eye check(s) are due on staging and no worker does eye checks: they are yours", due))
	}
	// A look a worker could not do is the owner's to clear. No worker waits
	// on it, so nothing else says so.
	for _, l := range r.Looks {
		if l.State != looks.Blocked {
			continue
		}
		why := l.Note
		if why == "" {
			why = "the worker's comment on the issue says why"
		}
		attention = append(attention, fmt.Sprintf("the eye check of %s#%d (pull request #%d) is blocked: %s", l.Repo, l.Number, l.PR, why))
	}
	return attention
}

func (r RepoReport) count(state string) int {
	n := 0
	for _, l := range r.Looks {
		if l.State == state {
			n++
		}
	}
	return n
}

// Collect reports on every machine, in config order. Machines are asked in
// parallel; one slow or dead machine does not hold up the others.
func (c Collector) Collect(ctx context.Context, cfg *config.Config) []MachineReport {
	return c.collectMachines(ctx, cfg, nil)
}

// CollectAll reports on the repositories and then on the machines. The
// release state comes first because it says which eye checks a worker will
// do: those are not the owner's backlog.
func (c Collector) CollectAll(ctx context.Context, cfg *config.Config) Report {
	repos := c.CollectRepos(ctx, cfg)
	workerLooks := map[string]bool{}
	for _, r := range repos {
		for _, l := range r.Looks {
			// A look that is due is a worker's. One that waits for a staging
			// deploy asks for no eyes yet.
			if r.Lookers > 0 && l.State != looks.Failed && l.State != looks.Blocked {
				workerLooks[issueKey(l.Repo, l.Number)] = true
			}
		}
	}
	machines := c.collectMachines(ctx, cfg, workerLooks)
	return Report{Machines: machines, Repos: repos, Released: c.collectReleased(ctx, cfg), Balance: Balance(cfg, machines)}
}

func issueKey(repo string, number int) string {
	return fmt.Sprintf("%s#%d", repo, number)
}

func (c Collector) collectMachines(ctx context.Context, cfg *config.Config, workerLooks map[string]bool) []MachineReport {
	reports := make([]MachineReport, len(cfg.Machines))
	var wg sync.WaitGroup
	for i, m := range cfg.Machines {
		wg.Go(func() { reports[i] = c.collectOne(ctx, cfg, m, workerLooks) })
	}
	wg.Wait()
	return reports
}

func (c Collector) collectOne(ctx context.Context, cfg *config.Config, m config.Machine, workerLooks map[string]bool) MachineReport {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var res *probe.Result
	var probeErr error
	if r, err := probe.Run(ctx, c.Runner, m, cfg.ChecksFor(m)); err != nil {
		probeErr = err
	} else {
		res = &r
	}

	var issues []backlog.Issue
	var issueErrs []error
	for _, src := range m.AllSources() {
		found, err := c.Lister.List(ctx, src)
		if err != nil {
			issueErrs = append(issueErrs, err)
			continue
		}
		issues = append(issues, found...)
	}
	for n := range issues {
		if r, ok := cfg.Repo(issues[n].Repo); ok && r.Verify != nil {
			issues[n].Verify, issues[n].MergeHigh = true, r.Verify.MergeHigh
		}
	}

	report := AssessWith(m, cfg.Signals, time.Now(), res, issues, workerLooks)
	MarkHeld(&report, issues, cfg.Supervisor.OverlapLabels, cfg.Repos, cfg.Signals)
	if res != nil {
		AssessContext(&report, m, *res, cfg.Supervisor.ContextBudget)
	}
	if probeErr != nil {
		report.Error = probeErr.Error()
		report.Attention = append([]string{"unreachable: " + probeErr.Error()}, report.Attention...)
	}
	for _, err := range issueErrs {
		report.Attention = append(report.Attention, "cannot list issues: "+err.Error())
	}
	return report
}

// Assess applies the attention rules to one machine. res is nil when the
// machine could not be probed; the issue rules still apply. now is the
// watcher's clock.
func Assess(m config.Machine, signals []config.Signal, now time.Time, res *probe.Result, issues []backlog.Issue) MachineReport {
	return AssessWith(m, signals, now, res, issues, nil)
}

// AssessWith is Assess that knows which issues' eye checks a worker will do
// on staging (by "owner/name#number"). Such a look is not asked of the owner.
func AssessWith(m config.Machine, signals []config.Signal, now time.Time, res *probe.Result, issues []backlog.Issue, workerLooks map[string]bool) MachineReport {
	report := MachineReport{Name: m.Name, Host: m.Host, Probe: res, Attention: []string{}, Now: now}
	attend := func(format string, args ...any) {
		report.Attention = append(report.Attention, fmt.Sprintf(format, args...))
	}

	var windows []probe.Window
	if res != nil {
		windows = res.Windows
		for _, c := range res.Checks {
			if !c.OK {
				attend("check failed: %s%s", c.Name, parenthesized(c.Detail))
			}
		}
		if res.DiskUsedPct >= diskFullPct {
			attend("disk is %d%% full", res.DiskUsedPct)
		}
		if d, ok := highLoad(*res); ok {
			attend("load has been at or above the %d cores for %s: tests time out and builds slow", res.Cores, Short(d))
		}
		agentUp := assessAgent(m, *res, attend)
		assessTasks(m, *res, agentUp, &report, attend)
		assessWorkers(m, *res, &report, attend)
		for _, d := range LastDeploys(res.Deploys) {
			if d.Result == runlog.DeployFailed || d.Result == runlog.DeployNotServed {
				attend("the staging deploy of %s %s %s ago; it is tried again once looks have waited long enough again", d.Repo, deployWords(d), Short(res.Now.Sub(d.End)))
			}
		}
		assessOneOff(*res, &report)
		report.Unsupervised = Unsupervised(m, res.Windows)
	}

	queued, working := 0, 0
	// What waits on the owner is told after the loop, in the order to
	// answer it.
	var waiting []IssueStatus
	for _, issue := range issues {
		st := IssueStatus{Repo: issue.Repo, Number: issue.Number, Title: issue.Title, URL: issue.URL,
			State: Queued, Asks: backlog.Effects(issue, signals, now), Rework: backlog.Rework(issue, signals),
			Decided: backlog.Decided(issue, signals)}
		workerLook := workerLooks[issueKey(issue.Repo, issue.Number)]
		if workerLook {
			st.Asks = slices.DeleteFunc(st.Asks, func(a backlog.Ask) bool { return a.Name == config.EyesSignal })
		}
		// A draft that says what it waits for is not the owner's to review.
		for _, a := range st.Asks {
			if a.WaitsFor != "" {
				st.Drafts = append(st.Drafts, a)
			}
		}
		st.Asks = slices.DeleteFunc(st.Asks, func(a backlog.Ask) bool { return a.WaitsFor != "" })
		for _, a := range st.Asks {
			st.Waiting = append(st.Waiting, a.Name)
		}
		if res != nil {
			// A bump to a worker the machine does not have moves nothing.
			to, bumped := res.Bumps[issue.Number]
			st.Bumped = bumped
			if m.TakesWork(to) == nil {
				st.BumpedTo = to
			}
		}
		// A supervised worker has the issue its supervisor handed it. Any
		// other worker is matched by the name of its tmux window.
		supervised := supervisedOn(report.Workers, issue.Number)
		window, hasWindow := workerFor(issue.Number, windows)
		switch {
		case supervised != "":
			st.Worker, st.State = supervised, Working
		case hasWindow:
			st.Worker, st.State = window.Name, Working
		}
		switch {
		case len(st.Waiting) > 0:
			st.State = Waiting
			if res != nil && effectOf(st.Asks) == backlog.EffectReturns {
				st.ReturnsTo = runlog.Builder(res.Checkins, issue.Number)
				st.ReturnsIdle = workerIdle(report.Workers, st.ReturnsTo)
			}
			waiting = append(waiting, st)
		case st.Rework != "" && res != nil && runlog.ReworkSpent(res.Checkins, issue.Number, res.Now):
			// A worker was sent back to it as often as allowed. It is the
			// owner's now, whoever has it in hand.
			st.State = Rework
			attend("%s#%d: %s after %d tries by a worker: %s", issue.Repo, issue.Number, st.Rework, runlog.ReworkLimit, issue.Title)
		case verifyState(issue, signals) != "":
			// Neither the owner's nor a builder's yet.
			st.State, st.Worker, st.Held = Verifying, supervised, verifyState(issue, signals)
		case supervised != "":
			// Its supervisor watches it; a quiet spell is the supervisor's to judge.
			working++
		case st.Rework != "":
			st.State = Rework
			// A supervisor sends a worker back to it. Without one it is the owner's.
			if len(m.Workers) == 0 {
				attend("%s#%d: %s: %s", issue.Repo, issue.Number, st.Rework, issue.Title)
			}
		case workerLook:
			// Nobody builds it and nobody is asked: it waits for its look.
			st.State = Look
		case len(st.Drafts) > 0:
			// Nobody builds it and nobody is asked: it waits for what its
			// hand-back names.
			st.State = Draft
		case backlog.Finished(issue, signals) != 0 && !hasWindow:
			// Open, but done: the merge did not close it. Nobody builds it.
			st.State = Merged
		case hasWindow:
			working++
			if quiet := res.Now.Sub(window.LastActivity); quiet > workerQuiet {
				attend("worker %s on #%d has been quiet for %s: finished or stuck?", window.Name, issue.Number, Short(quiet))
			}
		default:
			queued++
		}
		report.Issues = append(report.Issues, st)
	}
	attendWaiting(waiting, now, attend)
	if line := noWork(report, now); line != "" {
		// It goes above the rest: it is why the other lines matter.
		report.Attention = append([]string{line}, report.Attention...)
	}
	// On a machine with supervised workers the supervisor hands out the
	// queue, and says so when it cannot.
	if res != nil && queued > 0 && working == 0 && len(m.Workers) == 0 {
		attend("%d issue(s) queued and no worker is running", queued)
	}
	return report
}

// noWork says how many of the machine's workers have nothing to do because
// of what waits on the owner, as in "5 of 8 workers have no work: every
// queued issue waits on you (oldest 27h)". "" when no worker is idle for
// that reason: an empty queue is not the owner's backlog.
func noWork(r MachineReport, now time.Time) string {
	idle, builders := idleBuilders(r)
	waiting, oldest := 0, time.Duration(0)
	first, firstAt := 0, time.Time{}
	for _, i := range r.Issues {
		if len(i.Asks) > 0 {
			waiting++
		}
		if t := oldestAsk(i.Asks); effectOf(i.Asks) == backlog.EffectReturns && (firstAt.IsZero() || t.Before(firstAt)) {
			first, firstAt = i.Number, t
		}
		for _, a := range i.Asks {
			if !a.Since.IsZero() {
				oldest = max(oldest, now.Sub(a.Since))
			}
		}
	}
	if idle == 0 || waiting == 0 {
		return ""
	}
	why := fmt.Sprintf("%d of the %d queued issues wait on you", waiting, len(r.Issues))
	if waiting == len(r.Issues) {
		why = "every queued issue waits on you"
	}
	if oldest > 0 {
		why += fmt.Sprintf(" (oldest %s)", Short(oldest))
	}
	if first != 0 {
		why += fmt.Sprintf("; #%d goes back to a worker when you answer it", first)
	}
	return fmt.Sprintf("%d of %d workers have no work: %s", idle, builders, why)
}

// idleBuilders counts the machine's workers that build, and how many of
// them have no work: the last check-in found them done, with no task.
func idleBuilders(r MachineReport) (idle, builders int) {
	for _, w := range r.Workers {
		if w.Ask {
			continue
		}
		builders++
		if w.Last != nil && w.Last.Verdict == runlog.VerdictDone && w.Task == "" {
			idle++
		}
	}
	return idle, builders
}

// askWords says what an issue asks of the owner and for how long, as in
// "eyes 3h, review #41 3h".
func askWords(asks []backlog.Ask, now time.Time) string {
	words := make([]string, len(asks))
	for i, a := range asks {
		words[i] = a.What()
		if !a.Since.IsZero() {
			words[i] += " " + Short(now.Sub(a.Since))
		}
	}
	return strings.Join(words, ", ")
}

// assessOneOff says where each one-off task on the machine stands. A task
// that was handed over long ago is left out once the check-in log no longer
// says who had it.
func assessOneOff(res probe.Result, report *MachineReport) {
	for _, t := range res.Tasks {
		switch t.Where {
		case runlog.TaskTaken:
			worker := runlog.TaskWorker(res.Checkins, t.ID)
			if worker == "" {
				continue
			}
			state := OneOffFinished
			if runlog.TaskInHand(res.Checkins, worker) == t.ID {
				state = OneOffInHand
			}
			report.OneOff = append(report.OneOff, TaskState{ID: t.ID, State: state, Worker: worker})
		case runlog.TaskAny:
			report.OneOff = append(report.OneOff, TaskState{ID: t.ID, State: OneOffQueued})
		default:
			report.OneOff = append(report.OneOff, TaskState{ID: t.ID, State: OneOffQueued, Worker: t.Where})
		}
	}
}

// supervisedOn returns the supervised worker that has this issue in hand.
func supervisedOn(workers []WorkerStatus, issue int) string {
	for _, w := range workers {
		if w.Issue == issue {
			return w.Name
		}
	}
	return ""
}

// assessAgent reports whether the agent is alive. A machine with no tasks
// and no workers needs no agent.
func assessAgent(m config.Machine, res probe.Result, attend func(string, ...any)) bool {
	if len(m.Tasks)+len(m.Workers) == 0 {
		return true
	}
	idle := fmt.Sprintf("%d task(s) will not run and %d worker(s) are not supervised", len(m.Tasks), len(m.Workers))
	switch age := res.Now.Sub(res.Heartbeat); {
	case res.Heartbeat.IsZero():
		attend("agent is not running: %s", idle)
		return false
	case age > heartbeatStale:
		attend("agent stopped %s ago: %s", Short(age), idle)
		return false
	}
	return true
}

// assessWorkers reports the supervisor's latest word on each worker.
func assessWorkers(m config.Machine, res probe.Result, report *MachineReport, attend func(string, ...any)) {
	latest := runlog.LatestCheckins(res.Checkins)
	for _, w := range m.Workers {
		st := WorkerStatus{Name: w.Name, Issue: runlog.IssueInHand(res.Checkins, w.Name), Task: runlog.TaskInHand(res.Checkins, w.Name), RecyclePending: slices.Contains(res.Recycle, w.Name), Ask: w.Ask}
		if fields := strings.Fields(w.CommandOrDefault()); len(fields) > 0 {
			st.Tool = fields[0]
		}
		if mark, ok := res.Marks[w.Name]; ok {
			lastActivity := mark.Time
			for _, win := range res.Windows {
				if win.Session == workerSession && win.Name == w.Name {
					lastActivity = win.LastActivity
				}
			}
			st.State = mark.StateAt(lastActivity)
			// A prompt that was answered: the tool did not say when.
			if st.State == mark.State {
				st.StateSince = mark.Time
			}
		}
		if c, ok := latest[w.Name]; ok {
			st.Last = &c
			switch c.Verdict {
			case runlog.VerdictNeedsOwner:
				attend("worker %s needs you: %s", w.Name, c.Reason)
			case runlog.VerdictStuck:
				attend("worker %s is stuck: %s", w.Name, c.Reason)
			case runlog.VerdictError:
				attend("worker %s could not be checked: %s", w.Name, c.Reason)
			}
		}
		for _, p := range res.DirProblems {
			if p.Worker == w.Name {
				p.Path = w.Dir
				attend("worker %s: %s", w.Name, p.Describe(res.DirHost))
			}
		}
		for _, p := range res.AgentDirs.Problems {
			if p.Worker == w.Name {
				p.Path = w.Dir
				attend("worker %s: %s", w.Name, p.Describe(res.AgentDirs.Host))
			}
		}
		report.Workers = append(report.Workers, st)
	}
	if until, ok := allWeekly(report.Workers, res.Now); ok {
		attend("every worker is at its weekly limit: switch the machine's account or wait until %s", until)
	}
}

// versionRE matches a process title that is a version number, as Claude
// Code sets its own.
var versionRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// Unsupervised lists the windows on a machine with supervised workers that
// run an agent tool and are not a worker's own: a manual session, or one
// left from another workflow. An agent tool is one the fleet's workers run,
// or a process titled with a version number.
func Unsupervised(m config.Machine, windows []probe.Window) []string {
	if len(m.Workers) == 0 {
		return nil
	}
	tools, workers := map[string]bool{"claude": true}, map[string]bool{}
	for _, w := range m.Workers {
		workers[w.Name] = true
		if fields := strings.Fields(w.CommandOrDefault()); len(fields) > 0 {
			tools[filepath.Base(fields[0])] = true
		}
	}
	var names []string
	for _, win := range windows {
		if !tools[win.Command] && !versionRE.MatchString(win.Command) {
			continue
		}
		if win.Session == workerSession && (workers[win.Name] || strings.HasPrefix(win.Name, "preflight-") || strings.HasPrefix(win.Name, "login-")) {
			continue
		}
		names = append(names, win.Session+":"+win.Name)
	}
	return names
}

// allWeekly reports whether every worker is held at its weekly limit, and
// when the first of them resets. Waiting can idle the machine for days.
func allWeekly(workers []WorkerStatus, now time.Time) (string, bool) {
	var first time.Time
	builders := 0
	for _, w := range workers {
		// A worker that only answers questions spends little and is not
		// what the machine is for.
		if w.Ask {
			continue
		}
		builders++
		if w.Last == nil || w.Last.Verdict != runlog.VerdictLimited || w.Last.Limit != runlog.LimitWeekly {
			return "", false
		}
		if r := w.Last.Resets; r.After(now) && (first.IsZero() || r.Before(first)) {
			first = r
		}
	}
	if builders == 0 {
		return "", false
	}
	if first.IsZero() {
		return "it resets", true
	}
	return first.In(now.Location()).Format("Mon 15:04"), true
}

func assessTasks(m config.Machine, res probe.Result, agentUp bool, report *MachineReport, attend func(string, ...any)) {
	latest := runlog.Latest(res.Runs)
	for _, t := range m.Tasks {
		st := TaskStatus{Name: t.Name, Schedule: t.Schedule, State: TaskNever}
		if rec, ok := latest[t.Name]; ok {
			st.Last = &rec
			switch {
			case rec.Running():
				st.State = TaskRunning
			case rec.ExitCode != 0:
				st.State = TaskFailed
				attend("task %s failed (exit %d) %s ago%s", t.Name, rec.ExitCode, Short(res.Now.Sub(rec.End)), parenthesized(lastLine(rec.Output)))
			default:
				st.State = TaskOK
			}
			// With the agent down every task is late; the agent line says so once.
			st.Missed = !rec.Next.IsZero() && res.Now.After(rec.Next.Add(missedGrace))
			if st.Missed && agentUp {
				attend("task %s missed its %s run", t.Name, rec.Next.Local().Format("Mon 15:04"))
			}
		}
		report.Tasks = append(report.Tasks, st)
	}
}

// workerFor finds the tmux window named for an issue: the name holds the
// issue number as a whole number, as in "app-123" or "fix-123-login".
func workerFor(number int, windows []probe.Window) (probe.Window, bool) {
	re := regexp.MustCompile(`(^|[^0-9])` + strconv.Itoa(number) + `([^0-9]|$)`)
	for _, w := range windows {
		if re.MatchString(w.Name) {
			return w, true
		}
	}
	return probe.Window{}, false
}

// highLoad reports how long the machine's load has stayed at or above its
// cores, by the agent's readings, when that is highLoadAfter or longer.
func highLoad(res probe.Result) (time.Duration, bool) {
	if res.HighLoadSince.IsZero() {
		return 0, false
	}
	d := res.Now.Sub(res.HighLoadSince).Truncate(time.Minute)
	return d, d >= highLoadAfter
}

func parenthesized(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// Short formats a duration for a status line: 45s, 12m, 3h, 5d.
func Short(d time.Duration) string {
	// Two clocks a second apart must not print a negative age.
	d = max(d, 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// NeedsOwner reports whether any machine or repository has an attention item.
func NeedsOwner(reports []MachineReport, repos []RepoReport) bool {
	for _, r := range reports {
		if len(r.Attention) > 0 {
			return true
		}
	}
	for _, r := range repos {
		if len(r.Attention()) > 0 {
			return true
		}
	}
	return false
}

// LastDeploys returns the last deploy record of each repository, in the
// order the repositories were first deployed.
func LastDeploys(records []runlog.Deploy) []runlog.Deploy {
	var order []string
	last := map[string]runlog.Deploy{}
	for _, d := range records {
		if _, ok := last[d.Repo]; !ok {
			order = append(order, d.Repo)
		}
		last[d.Repo] = d
	}
	out := make([]runlog.Deploy, len(order))
	for i, repo := range order {
		out[i] = last[repo]
	}
	return out
}

// deployWords says what became of a staging deploy.
func deployWords(d runlog.Deploy) string {
	switch d.Result {
	case runlog.DeployStarted:
		return fmt.Sprintf("started, waiting for staging to serve a commit after %s", d.Before[:min(len(d.Before), 7)])
	case runlog.DeployServed:
		return fmt.Sprintf("served %s", d.After[:min(len(d.After), 7)])
	case runlog.DeployFailed:
		return fmt.Sprintf("failed (exit %d)", d.ExitCode)
	default:
		return "was not served within the hour"
	}
}

// MarkHeld marks the queued issues that the supervisor holds back, with
// why: an issue they depend on that is still open, a repository at its limit
// of open pull requests, or an issue a worker has in hand that touches the
// same code. It reads what the supervisor reads.
func MarkHeld(report *MachineReport, issues []backlog.Issue, overlapLabels string, repos []config.Repo, signals []config.Signal) {
	limits := map[string]int{}
	for _, r := range repos {
		limits[r.Name] = r.MaxOpenPRs
	}
	atLimit, limitWhy := 0, ""
	byKey := map[string]backlog.Issue{}
	for _, i := range issues {
		byKey[issueKey(i.Repo, i.Number)] = i
	}
	for n := range report.Issues {
		st := &report.Issues[n]
		issue, ok := byKey[issueKey(st.Repo, st.Number)]
		if st.State != Queued || !ok {
			continue
		}
		if hold := backlog.DepHold(issue); hold != "" {
			st.State, st.Held = Held, hold
			continue
		}
		// A bumped issue and one whose question the owner answered go
		// ahead, as the supervisor hands them out.
		if hold := backlog.PRLimitHold(issue, limits[issue.Repo]); hold != "" && !st.Bumped && !backlog.Ruled(issue, signals) {
			st.State, st.Held = Held, hold
			atLimit++
			limitWhy = cmp.Or(limitWhy, hold)
			continue
		}
		for _, other := range report.Issues {
			if other.State != Working || other.Repo != st.Repo {
				continue
			}
			if why := backlog.Overlap(issue, byKey[issueKey(other.Repo, other.Number)], overlapLabels); why != "" {
				st.State, st.Held = Held, fmt.Sprintf("overlaps #%d in hand: %s", other.Number, why)
				break
			}
		}
	}
	// A worker idle beside a queue the limit holds is not stuck: the open
	// pull requests are to be finished first.
	if idle, builders := idleBuilders(*report); atLimit > 0 && idle > 0 {
		report.Attention = append(report.Attention, fmt.Sprintf("%d of %d workers have no work: %d queued issue(s) are held: %s", idle, builders, atLimit, limitWhy))
	}
}

// verifyState says what a handed-back pull request waits for before the
// owner reviews it, or "" when it waits for nothing of the kind.
func verifyState(issue backlog.Issue, signals []config.Signal) string {
	switch v := backlog.Verification(issue, signals); v.State {
	case backlog.VerifyPending:
		return fmt.Sprintf("pull request #%d waits for its checks", v.PR)
	case backlog.VerifyDue:
		return fmt.Sprintf("pull request #%d waits for a verifier", v.PR)
	}
	return ""
}

// AssessContext says which workers load more before their first item than
// the budget allows.
func AssessContext(report *MachineReport, m config.Machine, res probe.Result, budget int) {
	if budget <= 0 {
		return
	}
	for _, w := range m.Workers {
		if n := res.Context[w.Name]; n > budget {
			report.Attention = append(report.Attention, fmt.Sprintf("worker %s loads about %s tokens before its first item, over the context_budget of %s: shed context %s %s", w.Name, count(n), count(budget), m.Name, w.Name))
		}
	}
}

// effectOrder is the order the owner answers asks in: what frees a worker
// first, then what is to be read, then merges, then decisions the worker
// still has time on.
var effectOrder = map[string]int{backlog.EffectReturns: 0, backlog.EffectRead: 1, "": 1, backlog.EffectMerge: 2, backlog.EffectGrace: 3}

// effectOf is the effect of an issue's most urgent ask.
func effectOf(asks []backlog.Ask) string {
	best := ""
	for n, a := range asks {
		if n == 0 || effectOrder[a.Effect] < effectOrder[best] {
			best = a.Effect
		}
	}
	return best
}

// oldestAsk is when the issue's oldest ask was made.
func oldestAsk(asks []backlog.Ask) time.Time {
	var oldest time.Time
	for _, a := range asks {
		if !a.Since.IsZero() && (oldest.IsZero() || a.Since.Before(oldest)) {
			oldest = a.Since
		}
	}
	return oldest
}

// workerIdle reports whether the worker has no work now.
func workerIdle(workers []WorkerStatus, name string) bool {
	for _, w := range workers {
		if w.Name == name {
			return w.Last != nil && w.Last.Verdict == runlog.VerdictDone && w.Task == ""
		}
	}
	return false
}

// attendWaiting tells what waits on the owner in the order to answer it:
// returns first, oldest first inside each group. Merges that a verifier
// passed at low risk are one line: a run of them must not bury a return.
func attendWaiting(waiting []IssueStatus, now time.Time, attend func(string, ...any)) {
	slices.SortStableFunc(waiting, func(a, b IssueStatus) int {
		if d := effectOrder[effectOf(a.Asks)] - effectOrder[effectOf(b.Asks)]; d != 0 {
			return d
		}
		return oldestAsk(a.Asks).Compare(oldestAsk(b.Asks))
	})
	var merges []string
	var oldestMerge time.Time
	for _, st := range waiting {
		effect := effectOf(st.Asks)
		if effect == backlog.EffectMerge {
			merges = append(merges, fmt.Sprintf("#%d", st.Number))
			if t := oldestAsk(st.Asks); oldestMerge.IsZero() || t.Before(oldestMerge) {
				oldestMerge = t
			}
			continue
		}
		what := askWords(st.Asks, now)
		switch {
		case effect == backlog.EffectReturns && st.ReturnsTo != "" && st.ReturnsIdle:
			what += "; returns to " + st.ReturnsTo + ", which is idle"
		case effect == backlog.EffectReturns && st.ReturnsTo != "":
			what += "; returns to " + st.ReturnsTo
		case effect == backlog.EffectReturns:
			what += "; returns to a worker"
		case effect == backlog.EffectGrace:
			what += "; the worker goes on with the rest until it parks"
		}
		attend("%s#%d waits on you (%s): %s", st.Repo, st.Number, what, st.Title)
	}
	if len(merges) > 0 {
		age := ""
		if !oldestMerge.IsZero() {
			age = ", oldest " + Short(now.Sub(oldestMerge))
		}
		attend("%d pull request(s) verified at low risk with green checks wait for your merge%s: %s", len(merges), age, strings.Join(merges, " "))
	}
}

// collectReleased lists the issues on each account's overflow label: work a
// spent machine gave up that no machine has taken yet.
func (c Collector) collectReleased(ctx context.Context, cfg *config.Config) []ReleasedIssue {
	var out []ReleasedIssue
	for _, a := range cfg.Accounts {
		if a.Overflow == "" || c.Lister == nil {
			continue
		}
		var repos []string
		for _, name := range a.Machines {
			if m, ok := cfg.Machine(name); ok {
				for _, src := range m.AllSources() {
					if !slices.Contains(repos, src.Repo) {
						repos = append(repos, src.Repo)
					}
				}
			}
		}
		for _, repo := range repos {
			issues, err := c.Lister.List(ctx, config.IssueSource{Repo: repo, Label: a.Overflow})
			if err != nil {
				continue
			}
			for _, i := range issues {
				r := ReleasedIssue{Repo: i.Repo, Number: i.Number, Title: i.Title, By: backlog.ReleasedBy(i), Label: a.Overflow}
				for _, cm := range i.Comments {
					if strings.HasPrefix(strings.TrimSpace(cm.Body), backlog.ReleasedPhrase) {
						r.Since = cm.CreatedAt
					}
				}
				out = append(out, r)
			}
		}
	}
	return out
}

// RenderReleased lists the issues given to the fleet that wait for a
// machine with room.
func RenderReleased(w io.Writer, released []ReleasedIssue, now time.Time) {
	if len(released) == 0 {
		return
	}
	fmt.Fprintf(w, "Released to the fleet, waiting for a machine with room: %d\n", len(released))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range released {
		age := "-"
		if !r.Since.IsZero() {
			age = Short(now.Sub(r.Since))
		}
		by := r.By
		if by == "" {
			by = "-"
		}
		fmt.Fprintf(tw, "  %s\t%s#%d\tfrom %s\t%s\n", age, r.Repo, r.Number, by, r.Title)
	}
	tw.Flush()
	fmt.Fprintln(w)
}
