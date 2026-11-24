package backlog

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// GoalLabel returns the issue's goal: its label that starts with the
// prefix, the first in order when it has several. "" when it has none, or
// when there is no prefix.
func GoalLabel(labels []string, prefix string) string {
	if prefix == "" {
		return ""
	}
	var goals []string
	for _, l := range labels {
		if strings.HasPrefix(l, prefix) {
			goals = append(goals, l)
		}
	}
	if len(goals) == 0 {
		return ""
	}
	return slices.Min(goals)
}

// goalLimit is how many issues of one goal label are counted.
const goalLimit = 1000

// GoalIssue is one issue of a goal: whether it is closed, and when it was
// opened and closed.
type GoalIssue struct {
	Closed    bool      `json:"closed"`
	CreatedAt time.Time `json:"created_at"`
	ClosedAt  time.Time `json:"closed_at,omitempty"`
}

// paceWindow is how far back a goal's pace is read.
const paceWindow = 7 * 24 * time.Hour

// Pace is how fast a goal moves: its issues opened and closed in the last
// seven days, and when it will be done at that rate.
type Pace struct {
	Open       int `json:"open"`
	OpenedWeek int `json:"opened_week"`
	ClosedWeek int `json:"closed_week"`
	// PerDay is the issues closed, less those opened, per day over the
	// week: how fast the open count falls.
	PerDay float64 `json:"per_day"`
	// ETA is when the open count reaches none at PerDay. Zero when the goal
	// is done, or does not converge: issues open as fast as they close.
	ETA time.Time `json:"eta,omitzero"`
}

// GoalPace reads a goal's pace from its issues.
func GoalPace(issues []GoalIssue, now time.Time) Pace {
	var p Pace
	since := now.Add(-paceWindow)
	for _, i := range issues {
		if !i.Closed {
			p.Open++
		}
		if i.CreatedAt.After(since) {
			p.OpenedWeek++
		}
		if i.Closed && i.ClosedAt.After(since) {
			p.ClosedWeek++
		}
	}
	p.PerDay = float64(p.ClosedWeek-p.OpenedWeek) / (paceWindow.Hours() / 24)
	if p.Open > 0 && p.PerDay > 0 {
		p.ETA = now.Add(time.Duration(float64(p.Open) / p.PerDay * float64(24*time.Hour)))
	}
	return p
}

// Words says the pace in a line: when the goal will be done, or why that
// cannot be said.
func (p Pace) Words() string {
	switch {
	case p.Open == 0:
		return "done"
	case p.ETA.IsZero():
		return fmt.Sprintf("not converging: %d opened, %d closed in 7 days", p.OpenedWeek, p.ClosedWeek)
	}
	return fmt.Sprintf("done about %s at %.1f a day", p.ETA.Format("Jan 2"), p.PerDay)
}

// GoalIssues lists the repository's issues with the label.
func (GH) GoalIssues(ctx context.Context, repo, label string) ([]GoalIssue, error) {
	out, err := gh(ctx, "issue", "list", "--repo", repo, "--label", label, "--state", "all", "--limit", fmt.Sprint(goalLimit), "--json", "state,createdAt,closedAt")
	if err != nil {
		return nil, err
	}
	issues, err := parseGoalIssues(out)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", repo, label, err)
	}
	return issues, nil
}

func parseGoalIssues(data []byte) ([]GoalIssue, error) {
	var raw []struct {
		State     string    `json:"state"`
		CreatedAt time.Time `json:"createdAt"`
		ClosedAt  time.Time `json:"closedAt"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("gh issue list: parse output: %w", err)
	}
	out := make([]GoalIssue, len(raw))
	for n, i := range raw {
		out[n] = GoalIssue{Closed: i.State == "CLOSED", CreatedAt: i.CreatedAt, ClosedAt: i.ClosedAt}
	}
	return out, nil
}

// GoalCounts counts the repository's issues with the label: how many are
// closed, of how many in all.
func (g GH) GoalCounts(ctx context.Context, repo, label string) (closed, total int, err error) {
	issues, err := g.GoalIssues(ctx, repo, label)
	if err != nil {
		return 0, 0, err
	}
	for _, i := range issues {
		if i.Closed {
			closed++
		}
	}
	return closed, len(issues), nil
}
