package backlog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
)

// fakeGH answers gh api reads from bodies by path, with an ETag each, and
// answers 304 to a read that sends the ETag it gave.
type fakeGH struct {
	bodies map[string]string
	calls  []string
}

func (f *fakeGH) get(_ context.Context, args ...string) ([]byte, error) {
	path := args[len(args)-1]
	etag := fmt.Sprintf("\"%x\"", len(f.bodies[path]))
	sent := ""
	for n, a := range args {
		if a == "-H" {
			sent = strings.TrimPrefix(args[n+1], "If-None-Match: ")
		}
	}
	f.calls = append(f.calls, path)
	if sent == etag {
		return []byte("HTTP/2.0 304 Not Modified\r\n\r\ngh: HTTP 304"), errors.New("gh api: HTTP 304")
	}
	body, ok := f.bodies[path]
	if !ok {
		return nil, fmt.Errorf("no such path %s", path)
	}
	return []byte("HTTP/2.0 200 OK\r\nEtag: " + etag + "\r\n\r\n" + body), nil
}

func withFakeGH(t *testing.T, bodies map[string]string) *fakeGH {
	t.Helper()
	f := &fakeGH{bodies: bodies}
	oldGet, oldDir := ghGet, condDir
	dir := t.TempDir()
	ghGet, condDir = f.get, func() string { return dir }
	commentCache.Lock()
	commentCache.kept = map[string]keptComments{}
	commentCache.Unlock()
	t.Cleanup(func() { ghGet, condDir = oldGet, oldDir })
	return f
}

const issuesPath = "repos/org/app/issues?state=open&per_page=100&labels=machine%3Abox"

func TestAReadThatDidNotChangeReusesWhatWasKept(t *testing.T) {
	f := withFakeGH(t, map[string]string{"repos/org/app/pulls": `[1]`})
	first, changed, err := conditional(context.Background(), "repos/org/app/pulls")
	if err != nil || !changed || string(first) != "[1]" {
		t.Fatalf("first read = %q, %v, %v", first, changed, err)
	}
	again, changed, err := conditional(context.Background(), "repos/org/app/pulls")
	if err != nil || changed || string(again) != "[1]" || len(f.calls) != 2 {
		t.Fatalf("second read = %q, changed %v, %v after %d calls", again, changed, err, len(f.calls))
	}
}

func TestALabelledQueueReadsTheCommentsOfAnIssueOnlyWhenItChanged(t *testing.T) {
	f := withFakeGH(t, map[string]string{
		issuesPath: `[{"number":12,"title":"Fix it","body":"b","html_url":"u","updated_at":"2026-11-26T09:00:00Z","comments":1,"labels":[{"name":"machine:box"}]},
			{"number":13,"title":"A pull request","pull_request":{}}]`,
		"repos/org/app/issues/12/comments?per_page=100": `[{"body":"## Hand-back","created_at":"2026-11-26T09:00:00Z","user":{"login":"me"}}]`,
	})
	src := config.IssueSource{Repo: "org/app", Label: "machine:box"}
	issues, err := restIssues(context.Background(), src)
	if err != nil || len(issues) != 1 || issues[0].Number != 12 || len(issues[0].Comments) != 1 || issues[0].Labels[0].Name != "machine:box" {
		t.Fatalf("issues = %+v, %v", issues, err)
	}
	f.calls = nil
	if _, err := restIssues(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] != issuesPath {
		t.Fatalf("calls = %q, want only the list: #12 did not change", f.calls)
	}
}

func TestAListWithManyChangedIssuesIsReadInOneQuery(t *testing.T) {
	var list []string
	for n := 1; n <= maxCommentReads+1; n++ {
		list = append(list, fmt.Sprintf(`{"number":%d,"title":"t","updated_at":"2026-11-26T09:00:00Z","comments":2}`, n))
	}
	withFakeGH(t, map[string]string{issuesPath: "[" + strings.Join(list, ",") + "]"})
	if _, err := restIssues(context.Background(), config.IssueSource{Repo: "org/app", Label: "machine:box"}); !errors.Is(err, errColdComments) {
		t.Fatalf("err = %v, want the list read in one query", err)
	}
}

func TestRecentlyMergedPullRequestsAreTheClosedOnesWithAMergeTime(t *testing.T) {
	withFakeGH(t, map[string]string{
		"repos/org/app/pulls?state=closed&sort=updated&direction=desc&per_page=100&page=1": `[{"number":41,"merged_at":"2026-11-26T09:00:00Z"},{"number":42,"merged_at":null}]`,
	})
	merged, err := restMerged(context.Background(), "org/app")
	if err != nil || !merged[41] || merged[42] {
		t.Fatalf("merged = %v, %v", merged, err)
	}
}
