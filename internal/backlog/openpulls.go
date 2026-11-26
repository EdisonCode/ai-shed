package backlog

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The CI state of an open pull request, from its worst check.
const (
	CIFailed  = "failed"  // a check failed
	CIPending = "pending" // a check still runs, and none failed
	CIPassed  = "passed"  // every check finished, and none failed
	CISkipped = "skipped" // every check was skipped or cancelled: nothing ran
	CINone    = "none"    // no check reported
)

// OpenPull is an open pull request, as `shed status` lists one that no
// queue covers or a draft that has gone quiet.
type OpenPull struct {
	Repo      string
	Number    int
	Title     string
	Body      string
	CreatedAt time.Time
	Draft     bool
	// Batch is set on shed's own merge train batch: shed opened it, and
	// shed closes it.
	Batch bool
	// Closes are the issues the pull request closes when it merges.
	Closes []Dep
	CI     string
	// LastCommit is when the pull request last changed: a push, a comment, a
	// review or an edit. Asking GitHub for the commits themselves costs a
	// node per commit author, which puts a listing of many pull requests
	// over GitHub's limit on a query's size.
	LastCommit time.Time
}

// openPullsLimit is how many open pull requests of a repository are read.
const openPullsLimit = 200

// openPullsFields are what each open pull request is read with. Each costs
// GitHub at most a few nodes per pull request: no field that expands every
// commit (such as commits, whose authors multiply the count) belongs here.
const openPullsFields = "number,title,body,headRefName,createdAt,updatedAt,isDraft,closingIssuesReferences,statusCheckRollup"

// OpenPulls lists the repository's open pull requests.
func (GH) OpenPulls(ctx context.Context, repo string) ([]OpenPull, error) {
	out, err := gh(ctx, "pr", "list", "--repo", repo, "--state", "open", "--limit", fmt.Sprint(openPullsLimit),
		"--json", openPullsFields)
	if err != nil {
		return nil, err
	}
	return parseOpenPulls(out, repo)
}

func parseOpenPulls(data []byte, repo string) ([]OpenPull, error) {
	var prs []struct {
		Number  int       `json:"number"`
		Title   string    `json:"title"`
		Body    string    `json:"body"`
		Branch  string    `json:"headRefName"`
		Created time.Time `json:"createdAt"`
		Updated time.Time `json:"updatedAt"`
		Draft   bool      `json:"isDraft"`
		Closes  []struct {
			Number     int `json:"number"`
			Repository struct {
				Name  string `json:"name"`
				Owner struct {
					Login string `json:"login"`
				} `json:"owner"`
			} `json:"repository"`
		} `json:"closingIssuesReferences"`
		Checks []check `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(data, &prs); err != nil {
		return nil, fmt.Errorf("gh pr list %s: parse output: %w", repo, err)
	}
	pulls := make([]OpenPull, 0, len(prs))
	for _, pr := range prs {
		p := OpenPull{Repo: repo, Number: pr.Number, Title: pr.Title, Body: pr.Body, CreatedAt: pr.Created, Draft: pr.Draft,
			Batch: strings.HasPrefix(pr.Branch, BatchBranchPrefix) && strings.HasPrefix(strings.TrimSpace(pr.Body), BatchPhrase),
			CI:    ciState(pr.Checks), LastCommit: pr.Updated}
		for _, c := range pr.Closes {
			p.Closes = append(p.Closes, Dep{Repo: c.Repository.Owner.Login + "/" + c.Repository.Name, Number: c.Number})
		}
		pulls = append(pulls, p)
	}
	return pulls, nil
}

// ciState is the state of the worst of the checks: a failure outweighs a
// check that still runs.
func ciState(checks []check) string {
	switch {
	case len(checks) == 0:
		return CINone
	case prProblem("", checks) != "":
		return CIFailed
	case pending(checks):
		return CIPending
	case noCI(checks):
		return CISkipped
	}
	return CIPassed
}

// HoldReason is the line of a pull request's body that says what it waits
// for, the words a hand-back uses for a draft held on purpose. "" when no
// line says.
func HoldReason(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "waits for") || strings.Contains(lower, "wait for") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
