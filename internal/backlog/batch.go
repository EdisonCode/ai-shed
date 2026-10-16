package backlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A batch train tests several verified pull requests together, on a draft
// pull request of its own: the base branch with each of them merged in, in
// order. One CI run then tests the combination, where a train of one pull
// request at a time runs CI once for each.

// BatchBranchPrefix starts the name of each branch a batch train builds.
// The machine's name follows, so each machine knows its own.
const BatchBranchPrefix = "shed-train/"

// BatchPhrase starts the description of a batch's draft pull request.
const BatchPhrase = "Merge train batch by shed:"

// Car is a pull request in a batch, at the head commit that was batched.
type Car struct {
	Issue int    `json:"issue"`
	PR    int    `json:"pr"`
	SHA   string `json:"sha"`
}

// Batch is the draft pull request of a batch train.
type Batch struct {
	PR      int       `json:"pr"`
	Branch  string    `json:"branch"`
	Created time.Time `json:"created"`
	Cars    []Car     `json:"cars"`
}

// BatchBranch is the name of a new batch branch of the machine.
func BatchBranch(machine string, at time.Time) string {
	return fmt.Sprintf("%s%s/%d", BatchBranchPrefix, machine, at.Unix())
}

// BatchOf reports whether the branch is a batch branch of the machine.
func BatchOf(branch, machine string) bool {
	return strings.HasPrefix(branch, BatchBranchPrefix+machine+"/")
}

// BatchBody is the description of a batch's draft pull request: what it is
// for, and each pull request in it at the head that was batched. shed reads
// the batch from it again after a restart, and shed status shows it.
func BatchBody(cars []Car) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s the base branch with each pull request below merged in, in order, so that one CI run tests them together. Do not merge this pull request: shed merges each pull request below when these checks pass, and closes this one.\n\n", BatchPhrase)
	for _, c := range cars {
		fmt.Fprintf(&b, "- #%d at %s (issue #%d)\n", c.PR, c.SHA, c.Issue)
	}
	return b.String()
}

var carRE = regexp.MustCompile(`(?m)^- #(\d+) at ([0-9a-f]+) \(issue #(\d+)\)\s*$`)

// ParseBatchBody reads the pull requests of a batch from its description.
func ParseBatchBody(body string) []Car {
	var cars []Car
	for _, m := range carRE.FindAllStringSubmatch(body, -1) {
		pr, _ := strconv.Atoi(m[1])
		issue, _ := strconv.Atoi(m[3])
		cars = append(cars, Car{Issue: issue, PR: pr, SHA: m[2]})
	}
	return cars
}

// MergeConflict is a pull request that did not merge cleanly into a batch,
// on top of the base and the pull requests before it.
type MergeConflict struct {
	PR int
}

func (e *MergeConflict) Error() string {
	return fmt.Sprintf("pull request #%d does not merge cleanly into the batch", e.PR)
}

// RequiresUpToDate reports whether the base branch requires a pull request
// to be up to date with it before it merges, by a branch protection rule or
// a ruleset. A batch train merges each pull request as it stands, so it
// cannot run on such a base.
func (GH) RequiresUpToDate(ctx context.Context, repo, base string) (bool, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return false, fmt.Errorf("repository %q is not owner/name", repo)
	}
	query := `query($owner: String!, $name: String!, $ref: String!) { repository(owner: $owner, name: $name) { ref(qualifiedName: $ref) { branchProtectionRule { requiresStrictStatusChecks } } } }`
	out, err := gh(ctx, "api", "graphql", "-f", "query="+query, "-f", "owner="+owner, "-f", "name="+name, "-f", "ref=refs/heads/"+base,
		"--jq", ".data.repository.ref.branchProtectionRule.requiresStrictStatusChecks // false")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(out)) == "true" {
		return true, nil
	}
	out, err = gh(ctx, "api", fmt.Sprintf("repos/%s/rules/branches/%s", repo, url.PathEscape(base)))
	if err != nil {
		return false, err
	}
	return strictRules(out)
}

// strictRules reports whether any of a branch's rules requires its status
// checks to pass on a branch that is up to date.
func strictRules(data []byte) (bool, error) {
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			Strict bool `json:"strict_required_status_checks_policy"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(data, &rules); err != nil {
		return false, fmt.Errorf("parse the branch rules: %w", err)
	}
	for _, r := range rules {
		if r.Type == "required_status_checks" && r.Parameters.Strict {
			return true, nil
		}
	}
	return false, nil
}

// BuildBatch makes the batch branch from the base, merges each pull request
// into it at its batched head, in order, and opens the branch as a draft
// pull request, so that the repository's own CI runs on it. A pull request
// that does not merge cleanly comes back as a *MergeConflict, and the branch
// is deleted.
func (GH) BuildBatch(ctx context.Context, repo, base, branch string, cars []Car) (int, error) {
	out, err := gh(ctx, "api", fmt.Sprintf("repos/%s/git/ref/heads/%s", repo, base), "--jq", ".object.sha")
	if err != nil {
		return 0, err
	}
	if _, err := gh(ctx, "api", "-X", "POST", fmt.Sprintf("repos/%s/git/refs", repo), "-f", "ref=refs/heads/"+branch, "-f", "sha="+strings.TrimSpace(string(out))); err != nil {
		return 0, err
	}
	for _, c := range cars {
		_, err := gh(ctx, "api", "-X", "POST", fmt.Sprintf("repos/%s/merges", repo), "-f", "base="+branch, "-f", "head="+c.SHA,
			"-f", fmt.Sprintf("commit_message=Merge pull request #%d into the batch", c.PR))
		if err == nil {
			continue
		}
		conflict := strings.Contains(err.Error(), "HTTP 409")
		if _, cleanup := gh(ctx, "api", "-X", "DELETE", fmt.Sprintf("repos/%s/git/refs/heads/%s", repo, branch)); cleanup != nil {
			err = errors.Join(err, cleanup)
		}
		if conflict {
			return 0, &MergeConflict{PR: c.PR}
		}
		return 0, err
	}
	out, err = gh(ctx, "api", "-X", "POST", fmt.Sprintf("repos/%s/pulls", repo), "-f", "title="+batchTitle(cars), "-f", "head="+branch, "-f", "base="+base,
		"-f", "body="+BatchBody(cars), "-F", "draft=true", "--jq", ".number")
	if err != nil {
		return 0, err
	}
	pr, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("open the batch pull request of %s: number %q: %w", branch, out, err)
	}
	return pr, nil
}

func batchTitle(cars []Car) string {
	var prs []string
	for _, c := range cars {
		prs = append(prs, "#"+strconv.Itoa(c.PR))
	}
	return "Merge train batch: " + strings.Join(prs, ", ")
}

// Batches lists the open draft pull requests of the repository's batch
// trains, on every machine.
func (GH) Batches(ctx context.Context, repo string) ([]Batch, error) {
	out, err := gh(ctx, "pr", "list", "--repo", repo, "--state", "open", "--limit", "100", "--search", `"`+BatchPhrase+`" in:body`,
		"--json", "number,headRefName,body,createdAt")
	if err != nil {
		return nil, err
	}
	return parseBatches(out)
}

func parseBatches(data []byte) ([]Batch, error) {
	var prs []struct {
		Number  int       `json:"number"`
		Branch  string    `json:"headRefName"`
		Body    string    `json:"body"`
		Created time.Time `json:"createdAt"`
	}
	if err := json.Unmarshal(data, &prs); err != nil {
		return nil, fmt.Errorf("parse the batch pull requests: %w", err)
	}
	var batches []Batch
	for _, p := range prs {
		if !strings.HasPrefix(p.Branch, BatchBranchPrefix) || !strings.HasPrefix(strings.TrimSpace(p.Body), BatchPhrase) {
			continue
		}
		batches = append(batches, Batch{PR: p.Number, Branch: p.Branch, Created: p.Created, Cars: ParseBatchBody(p.Body)})
	}
	return batches, nil
}

// CloseBatch closes a batch's draft pull request and deletes its branch.
func (GH) CloseBatch(ctx context.Context, repo string, pr int) error {
	_, err := gh(ctx, "pr", "close", strconv.Itoa(pr), "--repo", repo, "--delete-branch")
	return err
}
