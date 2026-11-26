package backlog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

// A conditional read sends the ETag of the last response it kept. GitHub
// answers 304 when nothing changed, and a 304 costs none of the account's
// quota: an idle fleet that asks every check spends close to nothing.

// ghGet runs gh api for a read; a test replaces it.
var ghGet = func(ctx context.Context, args ...string) ([]byte, error) {
	return gh(ctx, append([]string{"api"}, args...)...)
}

// errNotModified is gh's answer to a conditional read that found no change.
var errNotModified = errors.New("not modified")

// errPaged is a list longer than one page: the caller reads it the usual way.
var errPaged = errors.New("more than one page")

// errColdComments is a list with more changed issues than are worth a
// read each: the caller reads it in one query, and keepComments keeps them.
var errColdComments = errors.New("too many issues changed to read their comments one by one")

// maxCommentReads is how many issues' comments a list reads one by one.
const maxCommentReads = 8

// commentCache keeps each issue's comments as of the time the issue last
// changed: an issue that did not change is not read again.
var commentCache = struct {
	sync.Mutex
	kept map[string]keptComments
}{kept: map[string]keptComments{}}

type keptComments struct {
	updated  time.Time
	comments []Comment
}

// keepComments keeps the comments of issues read some other way.
func keepComments(issues []Issue) {
	commentCache.Lock()
	defer commentCache.Unlock()
	for _, i := range issues {
		commentCache.kept[issueKey(i.Repo, i.Number)] = keptComments{updated: i.UpdatedAt, comments: i.Comments}
	}
}

// keptCommentsOf returns an issue's comments if they were read since it
// last changed.
func keptCommentsOf(repo string, number int, updated time.Time) ([]Comment, bool) {
	commentCache.Lock()
	defer commentCache.Unlock()
	k, ok := commentCache.kept[issueKey(repo, number)]
	return k.comments, ok && k.updated.Equal(updated)
}

func issueKey(repo string, number int) string { return repo + "#" + strconv.Itoa(number) }

// condDir is where conditional reads keep their responses.
var condDir = sync.OnceValue(func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "shed", "gh")
})

type kept struct {
	ETag string          `json:"etag"`
	Body json.RawMessage `json:"body"`
}

// conditional reads a REST path, and reports whether it changed since the
// response kept for it. A path whose list goes on to a next page is
// errPaged, unless it names its page.
func conditional(ctx context.Context, path string) (body []byte, changed bool, err error) {
	sum := sha256.Sum256([]byte(path))
	file := ""
	var last kept
	if dir := condDir(); dir != "" {
		file = filepath.Join(dir, hex.EncodeToString(sum[:16])+".json")
		if data, err := os.ReadFile(file); err == nil {
			json.Unmarshal(data, &last)
		}
	}
	args := []string{"-i", path}
	if last.ETag != "" && len(last.Body) > 0 {
		args = []string{"-i", "-H", "If-None-Match: " + last.ETag, path}
	}
	out, err := ghGet(ctx, args...)
	if status(out) == 304 || (err != nil && strings.Contains(err.Error(), "HTTP 304")) {
		return last.Body, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	headers, data, _ := bytes.Cut(out, []byte("\r\n\r\n"))
	if len(data) == 0 {
		headers, data, _ = bytes.Cut(out, []byte("\n\n"))
	}
	if !strings.Contains(path, "&page=") && strings.Contains(strings.ToLower(header(headers, "link")), `rel="next"`) {
		return nil, false, errPaged
	}
	if file != "" {
		if etag := header(headers, "etag"); etag != "" {
			if saved, err := json.Marshal(kept{ETag: etag, Body: data}); err == nil {
				os.MkdirAll(filepath.Dir(file), 0o755)
				os.WriteFile(file, saved, 0o644)
			}
		}
	}
	return data, true, nil
}

// status reads the status code of an HTTP response's first line; 0 when
// there is none.
func status(out []byte) int {
	line, _, _ := bytes.Cut(out, []byte("\n"))
	fields := strings.Fields(string(line))
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return 0
	}
	code, _ := strconv.Atoi(fields[1])
	return code
}

// header reads one header of a response, in any case.
func header(headers []byte, name string) string {
	for _, line := range strings.Split(string(headers), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// restIssues reads a labelled source's open issues with conditional reads:
// the list, and the comments of each issue that changed since they were
// last read. errPaged when the list or an issue's comments is longer than
// a page: the caller reads it the usual way.
func restIssues(ctx context.Context, src config.IssueSource) ([]Issue, error) {
	path := fmt.Sprintf("repos/%s/issues?state=open&per_page=100&labels=%s", src.Repo, url.QueryEscape(src.Label))
	data, _, err := conditional(ctx, path)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Number    int       `json:"number"`
		Title     string    `json:"title"`
		Body      string    `json:"body"`
		URL       string    `json:"html_url"`
		UpdatedAt time.Time `json:"updated_at"`
		Comments  int       `json:"comments"`
		Labels    []Label   `json:"labels"`
		Pull      *struct{} `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("gh api %s: parse output: %w", path, err)
	}
	issues := []Issue{}
	var stale []int
	for _, r := range raw {
		if r.Pull != nil {
			continue
		}
		i := Issue{Repo: src.Repo, Number: r.Number, Title: r.Title, Body: r.Body, URL: r.URL, UpdatedAt: r.UpdatedAt, Labels: r.Labels}
		if r.Comments > 0 {
			var ok bool
			if i.Comments, ok = keptCommentsOf(src.Repo, r.Number, r.UpdatedAt); !ok {
				stale = append(stale, len(issues))
			}
		}
		issues = append(issues, i)
	}
	if len(stale) > maxCommentReads {
		return nil, errColdComments
	}
	for _, n := range stale {
		if issues[n].Comments, err = restComments(ctx, src.Repo, issues[n].Number); err != nil {
			return nil, err
		}
	}
	keepComments(issues)
	return issues, nil
}

// restComments reads an issue's comments, oldest first, with a conditional
// read: an issue that did not change costs nothing.
func restComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	path := fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", repo, number)
	data, _, err := conditional(ctx, path)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("gh api %s: parse output: %w", path, err)
	}
	comments := make([]Comment, len(raw))
	for n, c := range raw {
		comments[n] = Comment{Body: c.Body, CreatedAt: c.CreatedAt, Author: Author{Login: c.User.Login}}
	}
	return comments, nil
}

// restMerged reads the numbers of the repository's recently merged pull
// requests, two pages of the most recently closed, with conditional reads.
func restMerged(ctx context.Context, repo string) (map[int]bool, error) {
	merged := map[int]bool{}
	for page := 1; page <= 2; page++ {
		path := fmt.Sprintf("repos/%s/pulls?state=closed&sort=updated&direction=desc&per_page=100&page=%d", repo, page)
		data, _, err := conditional(ctx, path)
		if err != nil {
			return nil, err
		}
		var raw []struct {
			Number   int     `json:"number"`
			MergedAt *string `json:"merged_at"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("gh api %s: parse output: %w", path, err)
		}
		for _, p := range raw {
			if p.MergedAt != nil {
				merged[p.Number] = true
			}
		}
		if len(raw) < 100 {
			break
		}
	}
	return merged, nil
}
