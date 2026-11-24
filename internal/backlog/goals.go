package backlog

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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

// GoalCounts counts the repository's issues with the label: how many are
// closed, of how many in all.
func (GH) GoalCounts(ctx context.Context, repo, label string) (closed, total int, err error) {
	out, err := gh(ctx, "issue", "list", "--repo", repo, "--label", label, "--state", "all", "--limit", fmt.Sprint(goalLimit), "--json", "state")
	if err != nil {
		return 0, 0, err
	}
	closed, total, err = parseGoalCounts(out)
	if err != nil {
		return 0, 0, fmt.Errorf("%s %s: %w", repo, label, err)
	}
	return closed, total, nil
}

func parseGoalCounts(data []byte) (closed, total int, err error) {
	var issues []struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &issues); err != nil {
		return 0, 0, fmt.Errorf("gh issue list: parse output: %w", err)
	}
	for _, i := range issues {
		if i.State == "CLOSED" {
			closed++
		}
	}
	return closed, len(issues), nil
}
