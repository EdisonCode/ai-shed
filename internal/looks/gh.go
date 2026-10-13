package looks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

// GH asks GitHub with the GitHub CLI, so shed needs no token of its own.
type GH struct{}

func (GH) ClosedSince(ctx context.Context, src config.IssueSource, since time.Time) ([]backlog.Issue, error) {
	return backlog.GH{}.Closed(ctx, src, since)
}

func (GH) MergedSince(ctx context.Context, repo string, since time.Time) (map[int]Pull, error) {
	out, err := gh(ctx, "pr", "list", "--repo", repo, "--state", "merged", "--limit", "1000",
		"--search", "merged:>="+since.UTC().Format("2006-01-02"), "--json", "number,title,mergedAt,mergeCommit")
	if err != nil {
		return nil, err
	}
	return parseMerged(out, since)
}

// parseMerged keeps the pull requests merged after since: the search is by
// day, the window is to the second.
func parseMerged(data []byte, since time.Time) (map[int]Pull, error) {
	var prs []struct {
		Number      int       `json:"number"`
		Title       string    `json:"title"`
		MergedAt    time.Time `json:"mergedAt"`
		MergeCommit struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := json.Unmarshal(data, &prs); err != nil {
		return nil, fmt.Errorf("gh pr list: parse output: %w", err)
	}
	merged := map[int]Pull{}
	for _, pr := range prs {
		if pr.MergedAt.After(since) && pr.MergeCommit.OID != "" {
			merged[pr.Number] = Pull{Number: pr.Number, Title: pr.Title, MergedAt: pr.MergedAt, Commit: pr.MergeCommit.OID}
		}
	}
	return merged, nil
}

func (GH) CommitTime(ctx context.Context, repo, commit string) (time.Time, error) {
	out, err := gh(ctx, "api", "repos/"+repo+"/commits/"+commit, "--jq", ".commit.committer.date")
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: commit %s has no readable date: %w", repo, commit, err)
	}
	return t, nil
}

func (GH) Contains(ctx context.Context, repo, head, commit string) (bool, error) {
	// The comparison says where head stands relative to commit. The first
	// page carries every changed file with its patch, which runs to
	// megabytes on a range far behind; the second page of one commit has no
	// files and the same status.
	out, err := gh(ctx, "api", "repos/"+repo+"/compare/"+commit+"..."+head+"?per_page=1&page=2", "--jq", ".status")
	if err != nil {
		return false, err
	}
	status := strings.TrimSpace(string(out))
	return status == "ahead" || status == "identical", nil
}

func gh(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	out, err := cmd.Output()
	if err != nil {
		return nil, ghError(ctx, args, err, stderr.String(), time.Since(start))
	}
	return out, nil
}

// ghError says why a gh call failed. A call shed killed at its own deadline
// says so, so it is not read as a fault of gh or of its login.
func ghError(ctx context.Context, args []string, err error, stderr string, took time.Duration) error {
	call := "gh " + strings.Join(args[:min(len(args), 4)], " ")
	if ctx.Err() != nil {
		return fmt.Errorf("%s: timed out at shed's deadline after %s", call, took.Round(100*time.Millisecond))
	}
	return fmt.Errorf("%s: %w: %s", call, err, strings.TrimSpace(stderr))
}
