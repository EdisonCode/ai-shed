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
	Working = "working" // a tmux window is named for the issue
	Rework  = "rework"  // handed back, but its pull request cannot merge; a worker will be sent back to it
	Queued  = "queued"  // nobody is on it
	Waiting = "waiting" // it waits on the owner
)

// Task states.
const (
	TaskNever   = "never"
	TaskRunning = "running"
	TaskOK      = "ok"
	TaskFailed  = "failed"
)

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
	// Attention lists what needs the owner. Empty means the machine is on track.
	Attention []string `json:"attention"`
}

type IssueStatus struct {
	Repo    string   `json:"repo"`
	Number  int      `json:"number"`
	Title   string   `json:"title"`
	URL     string   `json:"url"`
	State   string   `json:"state"`
	Waiting []string `json:"waiting,omitempty"`
	Worker  string   `json:"worker,omitempty"`
	// Rework says why the issue's pull request cannot merge as it stands.
	Rework string `json:"rework,omitempty"`
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
	// RecyclePending is set when the owner asked for a fresh session and the
	// supervisor waits for the issue in hand to finish.
	RecyclePending bool `json:"recycle_pending,omitempty"`
	// Last is the supervisor's latest check-in; nil before the first one.
	Last *runlog.Checkin `json:"last,omitempty"`
}

type Collector struct {
	Runner probe.Runner
	Lister backlog.Lister
}

// Collect reports on every machine, in config order. Machines are asked in
// parallel; one slow or dead machine does not hold up the others.
func (c Collector) Collect(ctx context.Context, cfg *config.Config) []MachineReport {
	reports := make([]MachineReport, len(cfg.Machines))
	var wg sync.WaitGroup
	for i, m := range cfg.Machines {
		wg.Go(func() { reports[i] = c.collectOne(ctx, cfg, m) })
	}
	wg.Wait()
	return reports
}

func (c Collector) collectOne(ctx context.Context, cfg *config.Config, m config.Machine) MachineReport {
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

	report := Assess(m, cfg.Signals, res, issues)
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
// machine could not be probed; the issue rules still apply.
func Assess(m config.Machine, signals []config.Signal, res *probe.Result, issues []backlog.Issue) MachineReport {
	report := MachineReport{Name: m.Name, Host: m.Host, Probe: res, Attention: []string{}}
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
	}

	queued, working := 0, 0
	for _, issue := range issues {
		st := IssueStatus{Repo: issue.Repo, Number: issue.Number, Title: issue.Title, URL: issue.URL,
			State: Queued, Waiting: backlog.Waiting(issue, signals), Rework: backlog.Rework(issue, signals)}
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
			attend("%s#%d waits on you (%s): %s", issue.Repo, issue.Number, strings.Join(st.Waiting, ", "), issue.Title)
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
		st := WorkerStatus{Name: w.Name, Issue: runlog.IssueInHand(res.Checkins, w.Name), RecyclePending: slices.Contains(res.Recycle, w.Name)}
		if fields := strings.Fields(w.CommandOrDefault()); len(fields) > 0 {
			st.Tool = fields[0]
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

// NeedsOwner reports whether any machine has an attention item.
func NeedsOwner(reports []MachineReport) bool {
	for _, r := range reports {
		if len(r.Attention) > 0 {
			return true
		}
	}
	return false
}
