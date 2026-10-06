// Package status combines what the machines report and what GitHub says into
// one answer: is each machine on track, and what needs the owner.
package status

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/looks"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

const (
	probeTimeout   = 45 * time.Second
	diskFullPct    = 90
	heartbeatStale = 3 * time.Minute
	missedGrace    = 2 * time.Minute
	workerQuiet    = 30 * time.Minute
)

// Issue states.
const (
	Working = "working"   // a tmux window is named for the issue
	Rework  = "rework"    // handed back, but its pull request cannot merge; a worker will be sent back to it
	Queued  = "queued"    // nobody is on it
	Waiting = "waiting"   // it waits on the owner
	Look    = "eye check" // its merged work waits for a worker to look at it on staging
	Draft   = "draft"     // its pull request is a draft on purpose and says what it waits for
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
}

// CollectRepos reports on every repository under `repos` in the fleet file.
func (c Collector) CollectRepos(ctx context.Context, cfg *config.Config) []RepoReport {
	reports := make([]RepoReport, len(cfg.Repos))
	for i, repo := range cfg.Repos {
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		reports[i] = c.collectRepo(ctx, cfg, repo)
		cancel()
	}
	return reports
}

func (c Collector) collectRepo(ctx context.Context, cfg *config.Config, repo config.Repo) RepoReport {
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
	read, err := c.Looks.Read(ctx, repo, sources, open, cfg.Signals)
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
	return Report{Machines: c.collectMachines(ctx, cfg, workerLooks), Repos: repos}
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

	report := AssessWith(m, cfg.Signals, time.Now(), res, issues, workerLooks)
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
		agentUp := assessAgent(m, *res, attend)
		assessTasks(m, *res, agentUp, &report, attend)
		assessWorkers(m, *res, &report, attend)
		assessOneOff(*res, &report)
	}

	queued, working := 0, 0
	for _, issue := range issues {
		st := IssueStatus{Repo: issue.Repo, Number: issue.Number, Title: issue.Title, URL: issue.URL,
			State: Queued, Asks: backlog.Asks(issue, signals), Rework: backlog.Rework(issue, signals),
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
			if slices.ContainsFunc(m.Workers, func(w config.Worker) bool { return w.Name == to }) {
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
			attend("%s#%d waits on you (%s): %s", issue.Repo, issue.Number, askWords(st.Asks, now), issue.Title)
		case st.Rework != "" && res != nil && runlog.ReworkSpent(res.Checkins, issue.Number, res.Now):
			// A worker was sent back to it as often as allowed. It is the
			// owner's now, whoever has it in hand.
			st.State = Rework
			attend("%s#%d: %s after %d tries by a worker: %s", issue.Repo, issue.Number, st.Rework, runlog.ReworkLimit, issue.Title)
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
	// On a machine with supervised workers the supervisor hands out the
	// queue, and says so when it cannot.
	if res != nil && queued > 0 && working == 0 && len(m.Workers) == 0 {
		attend("%d issue(s) queued and no worker is running", queued)
	}
	return report
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
		st := WorkerStatus{Name: w.Name, Issue: runlog.IssueInHand(res.Checkins, w.Name), Task: runlog.TaskInHand(res.Checkins, w.Name), RecyclePending: slices.Contains(res.Recycle, w.Name)}
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
		report.Workers = append(report.Workers, st)
	}
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
