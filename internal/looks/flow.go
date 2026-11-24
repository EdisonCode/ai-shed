package looks

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// FlowIssue is an issue as the digest's flow counts it: when it opened and
// closed, and the pull requests that close it.
type FlowIssue struct {
	Number    int
	Body      string
	CreatedAt time.Time
	// ClosedAt is zero while the issue is open.
	ClosedAt time.Time
	// Pulls are the open or merged pull requests that close the issue.
	Pulls []int
}

// flowLimit is how many issues of a window are read for one repository.
const flowLimit = 1000

// IssuesSince lists the repository's issues updated since a time: every
// issue opened or closed since then is among them. It reads no comments.
func (GH) IssuesSince(ctx context.Context, repo string, since time.Time) ([]FlowIssue, error) {
	out, err := gh(ctx, "issue", "list", "--repo", repo, "--state", "all", "--limit", fmt.Sprint(flowLimit),
		"--search", "updated:>="+since.UTC().Format("2006-01-02"), "--json", "number,body,createdAt,closedAt,closedByPullRequestsReferences")
	if err != nil {
		return nil, err
	}
	issues, err := parseFlowIssues(out)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", repo, err)
	}
	return issues, nil
}

func parseFlowIssues(data []byte) ([]FlowIssue, error) {
	var raw []struct {
		Number    int        `json:"number"`
		Body      string     `json:"body"`
		CreatedAt time.Time  `json:"createdAt"`
		ClosedAt  *time.Time `json:"closedAt"`
		ClosedBy  []struct {
			Number int `json:"number"`
		} `json:"closedByPullRequestsReferences"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("gh issue list: parse output: %w", err)
	}
	issues := make([]FlowIssue, 0, len(raw))
	for _, r := range raw {
		i := FlowIssue{Number: r.Number, Body: r.Body, CreatedAt: r.CreatedAt}
		if r.ClosedAt != nil {
			i.ClosedAt = *r.ClosedAt
		}
		for _, pr := range r.ClosedBy {
			i.Pulls = append(i.Pulls, pr.Number)
		}
		issues = append(issues, i)
	}
	return issues, nil
}
