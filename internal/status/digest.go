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
	d := Digest{Since: since, Now: now, Report: c.CollectAll(ctx, cfg), Merged: []RepoMerged{}}
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
	var repos []string
	for _, m := range cfg.Machines {
		for _, src := range m.AllSources() {
			if !slices.Contains(repos, src.Repo) {
				repos = append(repos, src.Repo)
			}
		}
	}
	for _, repo := range repos {
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
	c.collectStages(ctx, &d)
	c.collectEscapes(ctx, cfg, &d)
	c.collectLessons(ctx, cfg, &d)
	c.collectDefaults(ctx, cfg, &d)
	c.collectReleases(ctx, cfg, &d)
	return d
}

// waitingItem is one thing an issue asks of the owner.
type waitingItem struct {
	issue  IssueStatus
	what   string
	since  time.Time
	effect string
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
				fmt.Fprintf(tw, "    #%d\tPR #%d\t%s\t%s ago\t%s\n", l.Number, l.PR, lookWords[l.State], Short(d.Now.Sub(l.Since)), l.Title)
			}
		}
		tw.Flush()
	}

	// What waits on the owner, the oldest first. An issue in the queue of
	// two machines waits once.
	var waiting []waitingItem
	seen := map[string]bool{}
	for _, m := range d.Report.Machines {
		for _, i := range m.Issues {
			for _, a := range i.Asks {
				key := issueKey(i.Repo, i.Number) + " " + a.Name
				if seen[key] {
					continue
				}
				seen[key] = true
				waiting = append(waiting, waitingItem{issue: i, what: a.What(), since: a.Since, effect: a.Effect})
			}
		}
	}
	// In the order to answer them: what returns to a worker, what is to be
	// read, the merges, then decisions the worker still has time on.
	slices.SortStableFunc(waiting, func(a, b waitingItem) int {
		if d := effectOrder[a.effect] - effectOrder[b.effect]; d != 0 {
			return d
		}
		return a.since.Compare(b.since)
	})
	renderLessons(w, d)

	fmt.Fprintf(w, "\nWaiting on you: %d\n", len(waiting))
	groups := []struct{ effect, title string }{
		{backlog.EffectReturns, "returns to a worker when you answer"},
		{backlog.EffectRead, "to read"},
		{backlog.EffectMerge, "to merge: verified at low risk, checks green"},
		{backlog.EffectGrace, "in its grace period: the worker goes on with the rest"},
	}
	for _, g := range groups {
		var items []waitingItem
		for _, item := range waiting {
			if item.effect == g.effect || (g.effect == backlog.EffectRead && item.effect == "") {
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
			if !item.since.IsZero() {
				age = Short(d.Now.Sub(item.since))
			}
			fmt.Fprintf(tw, "    %s\t%s\t%s#%d\t%s\n", age, item.what, item.issue.Repo, item.issue.Number, item.issue.Title)
		}
		tw.Flush()
	}

	// What the workers chose by themselves. It asks nothing of the owner,
	// who may overrule a choice with a ruling on the issue.
	var decided []string
	seen = map[string]bool{}
	for _, m := range d.Report.Machines {
		for _, i := range m.Issues {
			if key := issueKey(i.Repo, i.Number); !seen[key] {
				seen[key] = true
				for _, choice := range i.Decided {
					decided = append(decided, fmt.Sprintf("%s  %s", key, choice))
				}
			}
		}
	}
	if len(decided) > 0 {
		fmt.Fprintf(w, "\nDecided without you: %d\n", len(decided))
		for _, line := range decided {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}

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
