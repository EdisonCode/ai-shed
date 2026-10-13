package sources

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

const review = "Every pull request is reviewed by a second reader before it merges, and the review checks the tests as well as the code that they cover."

// layout is a home with a global instructions file, a repository with its
// own, imports and a memory index, and a worker's brief.
func layout(t *testing.T) (home, dir string) {
	home = t.TempDir()
	dir = filepath.Join(home, "Projects", "my-app")
	write(t, filepath.Join(home, ".claude", "CLAUDE.md"), "# Global\n\n"+review+"\n\nWrite a staged plan file for every change.\n")
	write(t, filepath.Join(dir, "CLAUDE.md"), "# my-app\n\n@docs/testing.md\n\nSee `internal/export/csv.go` and `internal/gone.go`.\n\nIssue #12 is still open.\n")
	write(t, filepath.Join(dir, "docs", "testing.md"), "# Testing\n\n"+strings.Replace(review, "second reader", "second, independent reader", 1)+"\n")
	write(t, filepath.Join(dir, "internal", "export", "csv.go"), "package export\n")
	write(t, filepath.Join(dir, ".shed", "BRIEF.md"), "# Brief\n\nPlans go in one ticket, no stage scaffolding.\n")
	write(t, filepath.Join(home, ".claude", "projects", MemorySlug(dir), "memory", "MEMORY.md"), "- [Testing](testing.md) — run make test\n")
	return home, dir
}

func TestTheSourcesAreWhatTheToolLoadsAtSessionStart(t *testing.T) {
	home, dir := layout(t)
	found := Collect(dir, home)
	var names []string
	for _, s := range found {
		names = append(names, s.Kind+" "+strings.TrimPrefix(s.Path, home))
	}
	want := []string{
		"global /.claude/CLAUDE.md",
		"repository /Projects/my-app/CLAUDE.md",
		"import /Projects/my-app/docs/testing.md",
		"memory /.claude/projects/" + MemorySlug(dir) + "/memory/MEMORY.md",
		"brief /Projects/my-app/.shed/BRIEF.md",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sources:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}
	if found[0].Tokens == 0 {
		t.Fatal("a source has no token count")
	}
}

func TestAnAuditFindsDuplicatesAcrossSourcesAndStaleReferences(t *testing.T) {
	home, dir := layout(t)
	states := func(_ context.Context, n int) (bool, error) { return n != 12, nil } // #12 is closed
	a := Run(context.Background(), dir, home, states)
	if a.Total == 0 || len(a.Sources) != 5 {
		t.Fatalf("audit = %+v", a)
	}
	if len(a.Duplicates) != 1 || !strings.HasSuffix(a.Duplicates[0].A.Path, ".claude/CLAUDE.md") || !strings.HasSuffix(a.Duplicates[0].B.Path, "docs/testing.md") {
		t.Fatalf("duplicates = %+v, want the review rule in the global file and the testing doc", a.Duplicates)
	}

	var stale []string
	for _, s := range a.Stale {
		stale = append(stale, s.Ref+": "+s.Why)
	}
	want := []string{"internal/gone.go: no such path in the checkout", "#12: closed, and the text says it is open"}
	if strings.Join(stale, "\n") != strings.Join(want, "\n") {
		t.Fatalf("stale:\n%s\nwant:\n%s", strings.Join(stale, "\n"), strings.Join(want, "\n"))
	}
}

func TestContradictionsAreReadFromTheReviewersAnswer(t *testing.T) {
	answer := "Here they are:\n[{\"a\": \"Write a staged plan file for every change.\", \"b\": \"Plans go in one ticket, no stage scaffolding.\", \"why\": \"one asks for a plan file, the other forbids one\"}]"
	got, err := parseContradictions(answer)
	if err != nil || len(got) != 1 || got[0].Why != "one asks for a plan file, the other forbids one" {
		t.Fatalf("contradictions = %+v, err %v", got, err)
	}
	if got, err := parseContradictions("[]"); err != nil || len(got) != 0 {
		t.Fatalf("none = %+v, err %v", got, err)
	}
}

func TestMemorySlugIsTheDirectoryWithItsSeparatorsReplaced(t *testing.T) {
	if got := MemorySlug("/Users/me/Documents/GitHub/my.app"); got != "-Users-me-Documents-GitHub-my-app" {
		t.Fatalf("slug = %q", got)
	}
}

func TestTheSameRuleInOtherWordsIsSaidTwice(t *testing.T) {
	a := Source{Path: "a", Text: "# Mine\n\n- **Incremental progress.** Small changes that compile and pass tests.\n- Commit often.\n"}
	b := Source{Path: "b", Text: "## Core\n\n- **Incremental progress over big bangs** - Small changes that compile and pass tests\n- Ship on Fridays when the moon is full.\n"}
	got := duplicates([]Source{a, b})
	if len(got) != 1 || got[0].A.Line != 3 || got[0].B.Line != 3 {
		t.Fatalf("duplicates = %+v, want the incremental-progress rule once", got)
	}
}
