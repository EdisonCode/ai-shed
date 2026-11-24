package status

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/looks"
)

// FlowLister lists a repository's issues opened or closed since a time.
type FlowLister interface {
	IssuesSince(ctx context.Context, repo string, since time.Time) ([]looks.FlowIssue, error)
}

// RepoFlow is whether one repository's backlog shrank in a window, and what
// it cost to close what closed. No count is per worker: that would reward
// closing work that is not finished.
type RepoFlow struct {
	// Error says why the issues opened and closed could not be read; the
	// counts of them are then zero.
	Error  string `json:"error,omitempty"`
	Opened int    `json:"opened"`
	Closed int    `json:"closed"`
	// Net is opened minus closed: above zero, the backlog grew.
	Net int `json:"net"`
	// OpenedByWorkers counts the opened issues that carry the marker a
	// worker writes in an issue it found while building another.
	OpenedByWorkers int `json:"opened_by_workers"`
	// PullsPerClosed is the pull requests that close the closed issues, per
	// closed issue; MostPulls is the closed issue with the most.
	PullsPerClosed float64     `json:"pulls_per_closed_issue"`
	MostPulls      *IssueCount `json:"most_pulls,omitempty"`
	// ReworkSends is how often a worker was sent back to a pull request in
	// the window, as far back as each machine's check-in log reaches.
	ReworkSends     int         `json:"rework_sends"`
	MergedPulls     int         `json:"merged_pulls"`
	ReworkPerMerged float64     `json:"rework_per_merged_pull"`
	MostRework      *IssueCount `json:"most_rework,omitempty"`
	// Looks is how many eye checks were done on the issues closed in the
	// window, and FirstPass how many of those passed on the issue's first
	// look.
	Looks     int `json:"looks,omitempty"`
	FirstPass int `json:"first_pass_looks,omitempty"`
	// OldestWait is when the oldest item that waits on the owner was asked,
	// and OldestIssue its issue.
	OldestWait  time.Time `json:"oldest_wait,omitzero"`
	OldestIssue int       `json:"oldest_wait_issue,omitempty"`
	// Chain is the longest chain of open dependencies among the open queued
	// issues, the first dependency first. Empty when no queued issue
	// depends on another open one.
	Chain []int `json:"chain,omitempty"`
	// chainWords names the chain's links, with the repository of a link in
	// another repository.
	chainWords []string
}

// IssueCount is the issue that is worst at a count, and its count.
type IssueCount struct {
	Number int `json:"number"`
	Count  int `json:"count"`
}

// collectFlow counts, for each repository a machine takes issues from, what
// opened and closed in the window and what closing it took.
func (c Collector) collectFlow(ctx context.Context, cfg *config.Config, d *Digest) {
	if c.Flow == nil {
		return
	}
	d.Flow = map[string]RepoFlow{}
	for _, repo := range fleetRepos(cfg) {
		f := RepoFlow{}
		issues, err := c.Flow.IssuesSince(ctx, repo, d.Since)
		if err != nil {
			d.Problems = append(d.Problems, err.Error())
			f.Error = err.Error()
		}
		countOpenedClosed(&f, issues, d.Since)
		if d.flowIssues == nil {
			d.flowIssues = map[string][]looks.FlowIssue{}
		}
		d.flowIssues[repo] = issues
		countRework(&f, d, repo)
		c.countLooks(ctx, cfg, &f, d, repo)
		oldestWait(&f, d, repo)
		longestChain(&f, d, repo)
		d.Flow[repo] = f
	}
}

func countOpenedClosed(f *RepoFlow, issues []looks.FlowIssue, since time.Time) {
	pulls := 0
	for _, i := range issues {
		if i.CreatedAt.After(since) {
			f.Opened++
			if backlog.FoundWhileBuilding(i.Body) {
				f.OpenedByWorkers++
			}
		}
		if i.ClosedAt.After(since) {
			f.Closed++
			pulls += len(i.Pulls)
			if len(i.Pulls) > 0 && (f.MostPulls == nil || len(i.Pulls) > f.MostPulls.Count) {
				f.MostPulls = &IssueCount{Number: i.Number, Count: len(i.Pulls)}
			}
		}
	}
	f.Net = f.Opened - f.Closed
	if f.Closed > 0 {
		f.PullsPerClosed = float64(pulls) / float64(f.Closed)
	}
}

// countRework counts the times a worker was sent back to a pull request of
// the repository in the window, from each machine's check-in log.
func countRework(f *RepoFlow, d *Digest, repo string) {
	for _, rm := range d.Merged {
		if rm.Repo == repo {
			f.MergedPulls = len(rm.Pulls)
		}
	}
	byIssue := map[int]int{}
	for _, m := range d.Report.Machines {
		if m.Probe == nil {
			continue
		}
		for _, ci := range m.Probe.Checkins {
			if ci.Sent && ci.Rework != "" && ci.Repo == repo && ci.Time.After(d.Since) {
				f.ReworkSends++
				byIssue[ci.Issue]++
			}
		}
	}
	f.MostRework = worst(byIssue)
	if f.MergedPulls > 0 {
		f.ReworkPerMerged = float64(f.ReworkSends) / float64(f.MergedPulls)
	}
}

// worst is the issue with the highest count, the lowest number of those
// tied; nil when there are none.
func worst(counts map[int]int) *IssueCount {
	var w *IssueCount
	for n, count := range counts {
		if w == nil || count > w.Count || (count == w.Count && n < w.Number) {
			w = &IssueCount{Number: n, Count: count}
		}
	}
	return w
}

// countLooks counts the eye checks done on the repository's issues closed
// in the window, and how many passed on the first look.
func (c Collector) countLooks(ctx context.Context, cfg *config.Config, f *RepoFlow, d *Digest, repo string) {
	if c.Closed == nil {
		return
	}
	seen := map[int]bool{}
	for _, m := range cfg.Machines {
		for _, src := range m.AllSources() {
			if src.Repo != repo {
				continue
			}
			// What cannot be read is said by the other parts that read it.
			issues, err := c.Closed.ClosedSince(ctx, src, d.Since)
			if err != nil {
				continue
			}
			for _, i := range issues {
				if seen[i.Number] {
					continue
				}
				seen[i.Number] = true
				done, firstPass := backlog.LooksDone(i, cfg.Signals)
				f.Looks += done
				if firstPass {
					f.FirstPass++
				}
			}
		}
	}
}

// oldestWait finds the repository's oldest item that waits on the owner.
func oldestWait(f *RepoFlow, d *Digest, repo string) {
	for _, m := range d.Report.Machines {
		for _, i := range m.Issues {
			if i.Repo != repo {
				continue
			}
			for _, a := range i.Asks {
				if !a.Since.IsZero() && (f.OldestWait.IsZero() || a.Since.Before(f.OldestWait)) {
					f.OldestWait, f.OldestIssue = a.Since, i.Number
				}
			}
		}
	}
}

// longestChain finds the longest chain of open dependencies that ends at
// one of the repository's open queued issues. A dependency that is in no
// queue ends the chain: what it depends on is not read.
func longestChain(f *RepoFlow, d *Digest, repo string) {
	deps := map[backlog.Dep][]backlog.Dep{}
	for _, m := range d.Report.Machines {
		for _, i := range m.Issues {
			node := backlog.Dep{Repo: i.Repo, Number: i.Number}
			if _, seen := deps[node]; seen {
				continue
			}
			deps[node] = []backlog.Dep{}
			for _, dep := range i.deps {
				deps[node] = append(deps[node], backlog.Dep{Repo: dep.Repo, Number: dep.Number})
			}
		}
	}
	var nodes []backlog.Dep
	for n := range deps {
		if n.Repo == repo {
			nodes = append(nodes, n)
		}
	}
	slices.SortFunc(nodes, func(a, b backlog.Dep) int { return cmp.Compare(a.Number, b.Number) })
	memo := map[backlog.Dep][]backlog.Dep{}
	var best []backlog.Dep
	for _, n := range nodes {
		if chain := chainTo(n, deps, memo, map[backlog.Dep]bool{}); len(chain) > len(best) {
			best = chain
		}
	}
	if len(best) < 2 {
		return
	}
	for _, link := range best {
		f.Chain = append(f.Chain, link.Number)
		f.chainWords = append(f.chainWords, backlog.DepWords([]backlog.Dep{link}, repo))
	}
}

// chainTo is the longest chain of dependencies that ends at n, the first
// dependency first. A cycle is followed once round.
func chainTo(n backlog.Dep, deps, memo map[backlog.Dep][]backlog.Dep, visiting map[backlog.Dep]bool) []backlog.Dep {
	if chain, ok := memo[n]; ok {
		return chain
	}
	if visiting[n] {
		return nil
	}
	visiting[n] = true
	var longest []backlog.Dep
	for _, dep := range deps[n] {
		if chain := chainTo(dep, deps, memo, visiting); len(chain) > len(longest) {
			longest = chain
		}
	}
	visiting[n] = false
	chain := append(slices.Clone(longest), n)
	memo[n] = chain
	return chain
}

// renderFlow writes each repository's flow lines.
func renderFlow(w io.Writer, d Digest) {
	for _, repo := range slices.Sorted(maps.Keys(d.Flow)) {
		f := d.Flow[repo]
		fmt.Fprintf(w, "\nFlow: %s\n", repo)
		line := func(label, format string, args ...any) {
			fmt.Fprintf(w, "  %-15s%s\n", label, fmt.Sprintf(format, args...))
		}
		issues := fmt.Sprintf("opened %d, closed %d, net %+d", f.Opened, f.Closed, f.Net)
		if f.Opened > 0 {
			issues += fmt.Sprintf("; %d opened by workers (%.0f%%)", f.OpenedByWorkers, 100*float64(f.OpenedByWorkers)/float64(f.Opened))
		}
		if f.Error != "" {
			issues = "cannot be read (see Also needs you)"
		}
		line("issues", "%s", issues)
		if f.Closed > 0 {
			line("pull requests", "%.1f per closed issue%s", f.PullsPerClosed, mostWords(f.MostPulls))
		}
		switch {
		case f.MergedPulls > 0:
			line("rework", "%.1f sends per merged pull request%s", f.ReworkPerMerged, mostWords(f.MostRework))
		case f.ReworkSends > 0:
			line("rework", "%d sends, no pull request merged%s", f.ReworkSends, mostWords(f.MostRework))
		}
		if f.Looks > 0 {
			line("eye checks", "%d of %d passed on the first look (%.0f%%)", f.FirstPass, f.Looks, 100*float64(f.FirstPass)/float64(f.Looks))
		}
		if !f.OldestWait.IsZero() {
			line("oldest wait", "%s  %s#%d", Short(d.Now.Sub(f.OldestWait)), repo, f.OldestIssue)
		}
		if len(f.chainWords) > 0 {
			line("longest chain", "%s (%d)", strings.Join(f.chainWords, " → "), len(f.chainWords))
		}
	}
}

func mostWords(c *IssueCount) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf(", most #%d (%d)", c.Number, c.Count)
}

// onceClosed reads each source's closed issues once in a digest: several
// parts of it read the same ones.
type onceClosed struct {
	lister ClosedLister
	read   map[config.IssueSourceKey]closedRead
}

type closedRead struct {
	since  time.Time
	issues []backlog.Issue
	err    error
}

func (o *onceClosed) ClosedSince(ctx context.Context, src config.IssueSource, since time.Time) ([]backlog.Issue, error) {
	if r, ok := o.read[src.Key()]; ok && r.since.Equal(since) {
		return r.issues, r.err
	}
	issues, err := o.lister.ClosedSince(ctx, src, since)
	o.read[src.Key()] = closedRead{since: since, issues: issues, err: err}
	return issues, err
}
