package backlog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// CostMarker is the line that identifies the cost comment shed posts, so a
// later pass updates that comment.
const CostMarker = "<!-- shed-cost -->"

// PostCost creates the cost comment on the pull request, or replaces the
// one shed posted before. The body must contain CostMarker.
func (GH) PostCost(ctx context.Context, repo string, pr int, body string) error {
	if !strings.Contains(body, CostMarker) {
		return fmt.Errorf("gh comment %s#%d: cost comment has no marker", repo, pr)
	}
	listed, err := ghAPI(ctx, "GET", fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", repo, pr), nil)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return err
	}
	if id, ok := costCommentID(listed); ok {
		_, err = ghAPI(ctx, "PATCH", fmt.Sprintf("repos/%s/issues/comments/%d", repo, id), payload)
		return err
	}
	_, err = ghAPI(ctx, "POST", fmt.Sprintf("repos/%s/issues/%d/comments", repo, pr), payload)
	return err
}

// costCommentID returns the id of the last comment that carries the marker.
func costCommentID(data []byte) (int, bool) {
	var comments []struct {
		ID   int    `json:"id"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(data, &comments); err != nil {
		return 0, false
	}
	var id int
	var found bool
	for _, c := range comments {
		if strings.Contains(c.Body, CostMarker) {
			id, found = c.ID, true
		}
	}
	return id, found
}

func ghAPI(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	args := []string{"api", "--method", method, path}
	cmd := exec.CommandContext(ctx, "gh", args...)
	if body != nil {
		cmd.Stdin = bytes.NewReader(body)
		cmd.Args = append(cmd.Args, "--input", "-")
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh api %s %s: %w: %s", method, path, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
