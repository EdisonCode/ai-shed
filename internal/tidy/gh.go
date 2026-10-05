package tidy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// GH asks GitHub with the GitHub CLI.
type GH struct{}

func (GH) MergedPR(ctx context.Context, repo, commit string) (int, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", "repos/"+repo+"/commits/"+commit+"/pulls")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// GitHub does not know a commit that was never pushed. That is an
		// answer, not a failure: nothing was merged from it.
		if strings.Contains(stderr.String(), "HTTP 422") || strings.Contains(stderr.String(), "HTTP 404") {
			return 0, nil
		}
		return 0, fmt.Errorf("gh api %s commit %s pulls: %w: %s", repo, commit, err, strings.TrimSpace(stderr.String()))
	}
	return mergedPR(out, commit)
}

// mergedPR picks the merged pull request whose head is the commit. A pull
// request that merely contains the commit does not count: its branch went on
// after it.
func mergedPR(data []byte, commit string) (int, error) {
	var prs []struct {
		Number   int     `json:"number"`
		MergedAt *string `json:"merged_at"`
		Head     struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(data, &prs); err != nil {
		return 0, fmt.Errorf("gh api: parse pull requests of commit %s: %w", commit, err)
	}
	for _, pr := range prs {
		if pr.MergedAt != nil && pr.Head.SHA == commit {
			return pr.Number, nil
		}
	}
	return 0, nil
}
