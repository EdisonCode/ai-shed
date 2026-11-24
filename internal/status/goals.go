package status

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

// GoalCounter counts the issues of a repository with a goal label: how many
// are closed, of how many in all.
type GoalCounter interface {
	GoalIssues(ctx context.Context, repo, label string) ([]backlog.GoalIssue, error)
}

// GoalProgress is how far one goal is from done.
type GoalProgress struct {
	Repo  string `json:"repo"`
	Label string `json:"label"`
	// Closed and Total count the issues with the label, closed of all.
	Closed int `json:"closed"`
	Total  int `json:"total"`
	// Net is the goal's issues opened minus closed in the window, as far as
	// the digest's flow read them.
	Net int `json:"net"`
	// Next says what holds the goal's next slice: the first of its open
	// queued issues that a worker could take, or why none can be taken.
	Next string `json:"next"`
	// Pace is how fast the goal moves over the last week, and when it will
	// be done at that rate.
	Pace backlog.Pace `json:"pace"`
	// Error says why the goal could not be counted.
	Error string `json:"error,omitempty"`
}

// markGoals puts on each issue of the report its goal, and the group its
// decisions are listed under in the digest. Nothing is marked when the
// fleet has no goal labels.
func markGoals(report *MachineReport, issues []backlog.Issue, goalPrefix, overlapPrefix string) {
	if goalPrefix == "" {
		return
	}
	labels := map[string][]string{}
	for _, i := range issues {
		labels[issueKey(i.Repo, i.Number)] = i.LabelNames()
	}
	for n := range report.Issues {
		st := &report.Issues[n]
		names := labels[issueKey(st.Repo, st.Number)]
		st.Goal = backlog.GoalLabel(names, goalPrefix)
		st.group = cmp.Or(st.Goal, backlog.GoalLabel(names, overlapPrefix), st.Repo)
	}
}

// collectGoals reports each goal of the open queued issues: closed of
// total, net in the window, and what holds its next slice.
func (c Collector) collectGoals(ctx context.Context, cfg *config.Config, d *Digest) {
	if c.Goals == nil || cfg.Supervisor.GoalLabels == "" {
		return
	}
	byGoal := map[[2]string][]IssueStatus{}
	seen := map[string]bool{}
	for _, m := range d.Report.Machines {
		for _, i := range m.Issues {
			if key := issueKey(i.Repo, i.Number); i.Goal != "" && !seen[key] {
				seen[key] = true
				goal := [2]string{i.Repo, i.Goal}
				byGoal[goal] = append(byGoal[goal], i)
			}
		}
	}
	goals := slices.SortedFunc(maps.Keys(byGoal), func(a, b [2]string) int { return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1])) })
	for _, g := range goals {
		p := GoalProgress{Repo: g[0], Label: g[1], Next: nextSlice(byGoal[g])}
		issues, err := c.Goals.GoalIssues(ctx, g[0], g[1])
		if err != nil {
			p.Error = err.Error()
			d.Problems = append(d.Problems, err.Error())
		}
		p.Pace = backlog.GoalPace(issues, d.Now)
		p.Total, p.Closed = len(issues), len(issues)-p.Pace.Open
		for _, i := range d.flowIssues[g[0]] {
			if !slices.Contains(i.Labels, g[1]) {
				continue
			}
			if i.CreatedAt.After(d.Since) {
				p.Net++
			}
			if i.ClosedAt.After(d.Since) {
				p.Net--
			}
		}
		d.Goals = append(d.Goals, p)
	}
}

// nextSlice says what holds a goal's next slice, from its open queued
// issues: the first a worker could take, or else why the first that nobody
// has in hand waits.
func nextSlice(issues []IssueStatus) string {
	slices.SortFunc(issues, func(a, b IssueStatus) int { return cmp.Compare(a.Number, b.Number) })
	var first *IssueStatus
	for n, i := range issues {
		if i.State == Queued {
			return fmt.Sprintf("next #%d is workable", i.Number)
		}
		if i.State != Working && first == nil {
			first = &issues[n]
		}
	}
	if first == nil {
		return "every open issue is in hand"
	}
	why := first.State
	switch {
	case first.Held != "":
		why += ": " + first.Held
	case len(first.Waiting) > 0:
		why += " on you: " + strings.Join(first.Waiting, ", ")
	case first.Rework != "":
		why += ": " + first.Rework
	}
	return fmt.Sprintf("next #%d is %s", first.Number, why)
}

// renderGoals writes each goal's progress.
func renderGoals(w io.Writer, goals []GoalProgress) {
	if len(goals) == 0 {
		return
	}
	fmt.Fprintf(w, "\nGoals: %d\n", len(goals))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, g := range goals {
		count, pace := fmt.Sprintf("%d of %d closed", g.Closed, g.Total), g.Pace.Words()
		if g.Error != "" {
			count, pace = "cannot be counted", ""
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\tnet %+d\t%s\t%s\n", g.Repo, g.Label, count, g.Net, pace, g.Next)
	}
	tw.Flush()
}

// renderDecided writes the choices the workers made by themselves. They ask
// nothing of the owner, who may overrule one with a ruling on the issue.
// With goal labels they are grouped by goal, then by overlap label, then
// by repository.
func renderDecided(w io.Writer, d Digest) {
	type choice struct{ group, line string }
	var decided []choice
	seen := map[string]bool{}
	for _, m := range d.Report.Machines {
		for _, i := range m.Issues {
			if key := issueKey(i.Repo, i.Number); !seen[key] {
				seen[key] = true
				for _, c := range i.Decided {
					decided = append(decided, choice{group: i.group, line: fmt.Sprintf("%s  %s", key, c)})
				}
			}
		}
	}
	if len(decided) == 0 {
		return
	}
	fmt.Fprintf(w, "\nDecided without you: %d\n", len(decided))
	var groups []string
	for _, c := range decided {
		if !slices.Contains(groups, c.group) {
			groups = append(groups, c.group)
		}
	}
	// A goal first, then an area, then a repository.
	slices.SortStableFunc(groups, func(a, b string) int { return cmp.Compare(groupOrder(d, a), groupOrder(d, b)) })
	for _, g := range groups {
		indent := "  "
		if g != "" {
			fmt.Fprintf(w, "  %s\n", g)
			indent = "    "
		}
		for _, c := range decided {
			if c.group == g {
				fmt.Fprintf(w, "%s%s\n", indent, c.line)
			}
		}
	}
}

// groupOrder places a group of decisions: a goal, an overlap label, then a
// repository.
func groupOrder(d Digest, group string) int {
	for _, m := range d.Report.Machines {
		for _, i := range m.Issues {
			switch {
			case i.group != group:
			case i.Goal == group:
				return 0
			case i.Repo == group:
				return 2
			default:
				return 1
			}
		}
	}
	return 2
}
