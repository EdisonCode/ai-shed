package status

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/looks"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// MergedLister lists a repository's pull requests merged after a time.
type MergedLister interface {
	MergedSince(ctx context.Context, repo string, since time.Time) (map[int]looks.Pull, error)
}

// Digest is what happened in a window and what it left for the owner: what
// merged, how the eye checks went, and what waits on them now.
type Digest struct {
	Since time.Time `json:"since"`
	Now   time.Time `json:"now"`
	// Merged lists each repository's pull requests merged in the window.
	Merged []RepoMerged `json:"merged"`
	// Report is the state now: machines, and each repository's release state.
	Report Report `json:"report"`
	// IdleHours is how long the workers were idle in the window, in worker-
	// hours by cause, as far back as each machine's check-in log reaches.
	IdleHours map[string]float64 `json:"idle_hours,omitempty"`
	// LimitHours is, by machine, how many worker-hours were idle at a usage
	// limit in the window.
	LimitHours map[string]float64 `json:"limit_hours,omitempty"`
	// MachineIdle is, by machine, how many worker-hours were idle in the
	// window, by cause.
	MachineIdle map[string]map[string]float64 `json:"machine_idle_hours,omitempty"`
	// Peaks is, by machine, the highest load and swap of the hours in the
	// window that its agent recorded.
	Peaks map[string]Peak `json:"peaks,omitempty"`
	// Imbalance names each machine whose workers idled with no work while
	// another machine on the same repository held work for room.
	Imbalance []string `json:"imbalance,omitempty"`
	// Usage is what each machine's workers spent in the window, when the
	// collector reads usage.
	Usage []MachineUsage `json:"usage,omitempty"`
	// Stages is where the time of each pull request merged in the window
	// went, when the collector reads pull request times.
	Stages []PullStages `json:"stages,omitempty"`
	// Verification is how each risk tier's verifications went, over the
	// issues closed in the window.
	Verification []TierOutcome `json:"verification,omitempty"`
	// Lessons are what workers wrote in their hand-backs for the next
	// worker to know, that the owner has not settled.
	Lessons []PendingLesson `json:"lessons,omitempty"`
	// Defaults is how the safe defaults of each area went, over the issues
	// closed in the window.
	Defaults []DefaultGroup `json:"defaults,omitempty"`
	// Releases counts, by machine/worker, the releases to the fleet on the
	// issues closed in the window.
	Releases map[string]int `json:"releases,omitempty"`
	// Flow is, by repository, whether its backlog shrank in the window.
	Flow map[string]RepoFlow `json:"flow,omitempty"`
	// Goals is each goal label's progress, for goal_labels.
	Goals []GoalProgress `json:"goals,omitempty"`
	// Waiting is what the report's issues ask of the owner, in the order to
	// answer it: the same list as "Waiting on you".
	Waiting []WaitingAsk `json:"waiting,omitempty"`
	// RepoAttention is what the repositories' release state needs from the
	// owner, one line each: the same lines shed observe shows.
	RepoAttention []string `json:"repo_attention,omitempty"`
	// flowIssues are, by repository, the issues opened or closed in the
	// window, as the flow read them.
	flowIssues map[string][]looks.FlowIssue
	// Problems lists what could not be read.
	Problems []string `json:"problems,omitempty"`
}

// RepoMerged is one repository's pull requests merged in the window, oldest
// first.
type RepoMerged struct {
	Repo  string       `json:"repo"`
	Pulls []looks.Pull `json:"pulls"`
}

// Digest collects the report and the pull requests merged since a time, for
// every repository a machine takes issues from.
func (c Collector) Digest(ctx context.Context, cfg *config.Config, merged MergedLister, since, now time.Time) Digest {
	return c.DigestFrom(ctx, cfg, merged, c.CollectAll(ctx, cfg), since, now)
}

// DigestFrom is Digest on a report already collected: a page that reads the
// report often reads the rest of the digest less often, without a second
// read of the fleet.
func (c Collector) DigestFrom(ctx context.Context, cfg *config.Config, merged MergedLister, report Report, since, now time.Time) Digest {
	if c.Closed != nil {
		// Several parts of the digest read the same closed issues.
		c.Closed = &onceClosed{lister: c.Closed, read: map[config.IssueSourceKey]closedRead{}}
	}
	d := Digest{Since: since, Now: now, Report: report, Merged: []RepoMerged{},
		Waiting: WaitingOn(report), RepoAttention: RepoAttention(report)}
	for _, m := range d.Report.Machines {
		if m.Probe == nil {
			continue
		}
		for cause, idle := range runlog.IdleByCause(m.Probe.Checkins, since, m.Probe.Now) {
			if d.IdleHours == nil {
				d.IdleHours = map[string]float64{}
			}
			d.IdleHours[cause] += idle.Hours()
		}
		if p, ok := peak(m.Probe.LoadPeaks, since); ok {
			if d.Peaks == nil {
				d.Peaks = map[string]Peak{}
			}
			d.Peaks[m.Name] = p
		}
		byCause := runlog.IdleByCause(m.Probe.Checkins, since, m.Probe.Now)
		if idle := byCause[runlog.IdleLimit]; idle > 0 {
			if d.LimitHours == nil {
				d.LimitHours = map[string]float64{}
			}
			d.LimitHours[m.Name] += idle.Hours()
		}
		for cause, idle := range byCause {
			if d.MachineIdle == nil {
				d.MachineIdle = map[string]map[string]float64{}
			}
			if d.MachineIdle[m.Name] == nil {
				d.MachineIdle[m.Name] = map[string]float64{}
			}
			d.MachineIdle[m.Name][cause] += idle.Hours()
		}
	}
	d.Imbalance = imbalance(cfg, d.MachineIdle, d.Peaks)
	c.collectUsage(ctx, cfg, &d)
	for _, repo := range fleetRepos(cfg) {
		pulls, err := merged.MergedSince(ctx, repo, since)
		if err != nil {
			d.Problems = append(d.Problems, err.Error())
			continue
		}
		rm := RepoMerged{Repo: repo, Pulls: []looks.Pull{}}
		for _, p := range pulls {
			rm.Pulls = append(rm.Pulls, p)
		}
		slices.SortFunc(rm.Pulls, func(a, b looks.Pull) int { return a.MergedAt.Compare(b.MergedAt) })
		d.Merged = append(d.Merged, rm)
	}
	c.collectFlow(ctx, cfg, &d)
	c.collectGoals(ctx, cfg, &d)
	c.collectStages(ctx, &d)
	c.collectEscapes(ctx, cfg, &d)
	c.collectLessons(ctx, cfg, &d)
	c.collectDefaults(ctx, cfg, &d)
	c.collectReleases(ctx, cfg, &d)
	return d
}

// WaitingAsk is one thing an issue asks of the owner.
type WaitingAsk struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url,omitempty"`
	// What names the ask as the digest does, such as "decision" or
	// "review #52".
	What string      `json:"what"`
	Ask  backlog.Ask `json:"ask"`
	// ReturnsTo is the worker an answer hands the issue back to, and
	// ReturnsIdle says that worker has no work now.
	ReturnsTo   string `json:"returns_to,omitempty"`
	ReturnsIdle bool   `json:"returns_idle,omitempty"`
}

// WaitingOn lists what the report's issues ask of the owner, in the order
// to answer it: what returns to a worker (an idle one first), what is to be
// read, the merges, then decisions the worker still has time on; the oldest
// first inside each. An issue in the queue of two machines waits once.
func WaitingOn(r Report) []WaitingAsk {
	var waiting []WaitingAsk
	seen := map[string]bool{}
	for _, m := range r.Machines {
		for _, i := range m.Issues {
			for _, a := range i.Asks {
				key := issueKey(i.Repo, i.Number) + " " + a.Name
				if seen[key] {
					continue
				}
				seen[key] = true
				waiting = append(waiting, WaitingAsk{Repo: i.Repo, Number: i.Number, Title: i.Title, URL: i.URL,
					What: a.What(), Ask: a, ReturnsTo: i.ReturnsTo, ReturnsIdle: i.ReturnsIdle})
			}
		}
	}
	slices.SortStableFunc(waiting, func(a, b WaitingAsk) int {
		if d := effectOrder[a.Ask.Effect] - effectOrder[b.Ask.Effect]; d != 0 {
			return d
		}
		// A return to an idle worker frees it now.
		if idle := a.idleReturn(); idle != b.idleReturn() {
			if idle {
				return -1
			}
			return 1
		}
		return a.Ask.Since.Compare(b.Ask.Since)
	})
	return waiting
}

func (w WaitingAsk) idleReturn() bool {
	return w.Ask.Effect == backlog.EffectReturns && w.ReturnsIdle
}

// RenderDigest writes the digest for a terminal or a message.
func RenderDigest(w io.Writer, d Digest) {
	fmt.Fprintf(w, "shed digest: the last %s, since %s\n", Short(d.Now.Sub(d.Since)), d.Since.Local().Format("Mon 15:04"))

	total := 0
	for _, rm := range d.Merged {
		total += len(rm.Pulls)
	}
	fmt.Fprintf(w, "\nMerged: %d pull request(s)\n", total)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, rm := range d.Merged {
		for _, p := range rm.Pulls {
			fmt.Fprintf(tw, "  %s#%d\t%s ago\t%s\n", rm.Repo, p.Number, Short(d.Now.Sub(p.MergedAt)), p.Title)
		}
	}
	tw.Flush()

	renderFlow(w, d)
	renderGoals(w, d.Goals)
	renderStages(w, d.Stages)
	renderEscapes(w, d.Verification)
	renderDefaults(w, d.Defaults)
	renderReleases(w, d.Releases)

	// What the idle time was lost to: each cause has its own remedy.
	var idle []string
	for _, cause := range runlog.IdleCauses {
		if hours := d.IdleHours[cause]; hours >= 0.05 {
			idle = append(idle, fmt.Sprintf("%s %.1fh", cause, hours))
		}
	}
	if len(idle) > 0 {
		fmt.Fprintf(w, "\nIdle worker-hours: %s\n", strings.Join(idle, ", "))
	}
	renderMachineIdle(w, d)

	renderUsage(w, d)

	for _, r := range d.Report.Repos {
		fmt.Fprintf(w, "\nEye checks: %s\n", r.Repo)
		if r.Error != "" {
			fmt.Fprintf(w, "  ! cannot read the release state: %s\n", r.Error)
			continue
		}
		passed := 0
		for _, l := range r.Passed {
			if l.Since.After(d.Since) {
				passed++
			}
		}
		fmt.Fprintf(w, "  %d passed in this window; now %s, %d failed\n", passed, r.notDone(), r.count(looks.Failed))
		if r.Unchecked != "" {
			fmt.Fprintf(w, "  ! cannot tell which merged work staging serves: %s\n", r.Unchecked)
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, l := range r.Passed {
			if l.Since.After(d.Since) {
				fmt.Fprintf(tw, "    #%d\tPR #%d\tpassed\t%s ago\t%s\n", l.Number, l.PR, Short(d.Now.Sub(l.Since)), l.Title)
			}
		}
		for _, l := range r.Looks {
			if l.State == looks.Failed || l.State == looks.Blocked {
				fmt.Fprintf(tw, "    #%d\tPR #%d\t%s\t%s ago\t%s\n", l.Number, l.PR, lookState(l), Short(d.Now.Sub(l.Since)), l.Title)
			}
		}
		tw.Flush()
	}

	// What waits on the owner, in the order to answer it.
	waiting := WaitingOn(d.Report)
	renderLessons(w, d)

	if !d.Report.Pulls.empty() {
		fmt.Fprintln(w)
		renderPulls(w, d.Report.Pulls, d.Now)
	}

	fmt.Fprintf(w, "\nWaiting on you: %d\n", len(waiting))
	groups := []struct{ effect, title string }{
		{backlog.EffectReturns, "returns to a worker when you answer"},
		{backlog.EffectRead, "to read"},
		{backlog.EffectMerge, "to merge: verified at low risk, checks green"},
		{backlog.EffectGrace, "in its grace period: the worker goes on with the rest"},
	}
	for _, g := range groups {
		var items []WaitingAsk
		for _, item := range waiting {
			if item.Ask.Effect == g.effect || (g.effect == backlog.EffectRead && item.Ask.Effect == "") {
				items = append(items, item)
			}
		}
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(w, "  %s\n", g.title)
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, item := range items {
			age := "-"
			if !item.Ask.Since.IsZero() {
				age = Short(d.Now.Sub(item.Ask.Since))
			}
			fmt.Fprintf(tw, "    %s\t%s\t%s#%d\t%s\n", age, item.What, item.Repo, item.Number, item.Title)
		}
		tw.Flush()
	}

	// What the workers chose by themselves. It asks nothing of the owner,
	// who may overrule a choice with a ruling on the issue.
	renderDecided(w, d)

	// Everything else that needs the owner: a machine, a worker, a task.
	var other []string
	for _, m := range d.Report.Machines {
		for _, a := range m.Attention {
			if !strings.Contains(a, " waits on you (") || strings.Contains(a, "have no work") {
				other = append(other, m.Name+": "+a)
			}
		}
	}
	for _, r := range d.Report.Repos {
		other = append(other, trainAttention(r.Repo, r.Train, d.Now)...)
	}
	other = append(other, d.Problems...)
	if len(other) > 0 {
		fmt.Fprintf(w, "\nAlso needs you: %d\n", len(other))
		for _, a := range other {
			fmt.Fprintf(w, "  ! %s\n", a)
		}
	}
}
