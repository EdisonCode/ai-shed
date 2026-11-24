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
	// Labels are the names of its labels, for the goal it serves.
	Labels []string
}

// flowLimit is how many issues of a window are read for one repository.
const flowLimit = 1000

// IssuesSince lists the repository's issues opened or closed since a time.
// It asks two searches, one for each, rather than one for the issues
// updated since then: on a busy repository a comment updates an issue, and
// the issues updated would pass the search's limit long before the ones
// opened or closed do. It reads no comments.
func (GH) IssuesSince(ctx context.Context, repo string, since time.Time) ([]FlowIssue, error) {
	day := since.UTC().Format("2006-01-02")
	var found [2][]FlowIssue
	for n, filter := range []string{"created:>=" + day, "closed:>=" + day} {
		out, err := gh(ctx, "issue", "list", "--repo", repo, "--state", "all", "--limit", fmt.Sprint(flowLimit),
			"--search", filter, "--json", "number,body,createdAt,closedAt,closedByPullRequestsReferences,labels")
		if err != nil {
			return nil, err
		}
		if found[n], err = parseFlowIssues(out); err != nil {
			return nil, fmt.Errorf("%s: %w", repo, err)
		}
	}
	return joinFlow(found[0], found[1]), nil
}

// joinFlow lists the issues of both searches, each once.
func joinFlow(opened, closed []FlowIssue) []FlowIssue {
	seen := map[int]bool{}
	var all []FlowIssue
	for _, i := range append(opened, closed...) {
		if !seen[i.Number] {
			seen[i.Number] = true
			all = append(all, i)
		}
	}
	return all
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
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
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
		for _, l := range r.Labels {
			i.Labels = append(i.Labels, l.Name)
		}
		issues = append(issues, i)
	}
	return issues, nil
}
