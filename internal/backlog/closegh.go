package backlog

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ClosedPhrase starts the comment shed closes a finished issue with.
const ClosedPhrase = "Closed by shed:"

// RegressionPhrase names merged work that broke something, as in
// "Regression of #12". Shed does not close an issue a regression names.
const RegressionPhrase = "Regression of"

// Merge is what GitHub says of a merged pull request: when it merged, its
// merge commit, and the state of the checks on that commit (one of the CI
// states, CINone when none reported).
type Merge struct {
	At     time.Time
	Commit string
	Checks string
}

// MergeOf reads the merge of the repository's pull request.
func (GH) MergeOf(ctx context.Context, repo string, pr int) (Merge, error) {
	owner, name, _ := strings.Cut(repo, "/")
	query := fmt.Sprintf(`query { repository(owner: %q, name: %q) { pullRequest(number: %d) { mergedAt mergeCommit { oid statusCheckRollup { state } } } } }`, owner, name, pr)
	out, err := gh(ctx, "api", "graphql", "-f", "query="+query)
	if err != nil {
		return Merge{}, err
	}
	return parseMerge(out, repo, pr)
}

func parseMerge(data []byte, repo string, pr int) (Merge, error) {
	var answer struct {
		Data struct {
			Repository struct {
				PullRequest *struct {
					MergedAt    *time.Time `json:"mergedAt"`
					MergeCommit *struct {
						Oid    string `json:"oid"`
						Rollup *struct {
							State string `json:"state"`
						} `json:"statusCheckRollup"`
					} `json:"mergeCommit"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return Merge{}, fmt.Errorf("%s#%d merge: parse output: %w", repo, pr, err)
	}
	p := answer.Data.Repository.PullRequest
	if p == nil || p.MergedAt == nil || p.MergeCommit == nil {
		return Merge{}, fmt.Errorf("%s#%d has not merged", repo, pr)
	}
	m := Merge{At: *p.MergedAt, Commit: p.MergeCommit.Oid, Checks: CINone}
	if r := p.MergeCommit.Rollup; r != nil {
		switch r.State {
		case "SUCCESS":
			m.Checks = CIPassed
		case "FAILURE", "ERROR":
			m.Checks = CIFailed
		default:
			m.Checks = CIPending
		}
	}
	return m, nil
}

// Regressions lists the issues of the repository whose text or comments
// name a regression of the issue.
func (GH) Regressions(ctx context.Context, repo string, number int) ([]int, error) {
	q := fmt.Sprintf(`repo:%s "%s #%d"`, repo, RegressionPhrase, number)
	out, err := gh(ctx, "api", "-X", "GET", "search/issues", "-f", "q="+q, "--jq", ".items[].number")
	if err != nil {
		return nil, err
	}
	var numbers []int
	for _, line := range strings.Fields(string(out)) {
		n, err := strconv.Atoi(line)
		if err != nil {
			return nil, fmt.Errorf("search %s regressions of #%d: %q is not a number", repo, number, line)
		}
		numbers = append(numbers, n)
	}
	return numbers, nil
}

// Close closes the issue with a comment.
func (GH) Close(ctx context.Context, repo string, number int, comment string) error {
	_, err := gh(ctx, "issue", "close", strconv.Itoa(number), "--repo", repo, "--comment", comment)
	return err
}
