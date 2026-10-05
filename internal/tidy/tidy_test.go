package tidy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
)

// fakeGitHub knows which commits are the head of a merged pull request.
type fakeGitHub struct {
	merged map[string]int
	asked  []string
}

func (f *fakeGitHub) MergedPR(_ context.Context, _ string, commit string) (int, error) {
	f.asked = append(f.asked, commit)
	return f.merged[commit], nil
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// sh runs a script the way it runs on a machine.
func sh(t *testing.T, script string, args ...string) string {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"-s", "--"}, args...)...)
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	return string(out)
}

// machine is a clone with a worker's directory and worktrees in every state
// tidy has to tell apart. It returns the root and the head of each worktree.
func machine(t *testing.T) (root string, heads map[string]string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root, _ = filepath.EvalSymlinks(t.TempDir())
	origin := filepath.Join(root, "origin.git")
	git(t, root, "init", "-q", "--bare", "-b", "main", origin)
	clone := filepath.Join(root, "app")
	git(t, root, "clone", "-q", origin, clone)
	os.WriteFile(filepath.Join(clone, "README"), []byte("app\n"), 0o644)
	git(t, clone, "add", ".")
	git(t, clone, "commit", "-q", "-m", "first")
	git(t, clone, "push", "-q", "origin", "main")
	git(t, clone, "remote", "set-head", "origin", "main")
	git(t, clone, "remote", "set-url", "origin", "https://github.com/org/app.git")

	heads = map[string]string{}
	add := func(name, branch string, commit bool) string {
		path := filepath.Join(root, name)
		if branch == "" {
			git(t, clone, "worktree", "add", "-q", "--detach", path)
		} else {
			git(t, clone, "worktree", "add", "-q", "-b", branch, path)
		}
		if commit {
			os.WriteFile(filepath.Join(path, name), []byte(name+"\n"), 0o644)
			git(t, path, "add", ".")
			git(t, path, "commit", "-q", "-m", name)
		}
		heads[name] = git(t, path, "rev-parse", "HEAD")
		return path
	}
	add("worker", "task/9-in-hand", true)
	add("wt-merged", "task/1-merged", true)
	dirty := add("wt-dirty", "task/2-dirty", true)
	os.WriteFile(filepath.Join(dirty, "notes.txt"), []byte("not committed\n"), 0o644)
	add("wt-open", "task/3-open", true)
	add("wt-fresh", "task/4-fresh", false) // just made: no commit of its own yet
	add("gates", "", false)
	// Branches with no worktree: one merged, one not.
	git(t, clone, "branch", "task/5-merged-branch", heads["wt-merged"])
	git(t, clone, "branch", "task/6-open-branch", heads["wt-open"])
	return root, heads
}

func planFor(t *testing.T, root string, gh Merged) ([]Item, map[string]string) {
	t.Helper()
	m := config.Machine{Workers: []config.Worker{{Name: "app", Dir: filepath.Join(root, "worker")}}}
	if got, want := CollectCommand(m), "sh -s -- '"+filepath.Join(root, "worker")+"'"; got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
	repos, err := Parse(sh(t, CollectScript(), filepath.Join(root, "worker")))
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Name != "org/app" || repos[0].Default != "origin/main" {
		t.Fatalf("repos = %+v", repos)
	}
	items, err := Plan(context.Background(), repos, gh)
	if err != nil {
		t.Fatal(err)
	}
	decided := map[string]string{}
	for _, it := range items {
		name := it.Branch
		if it.Path != "" {
			name = filepath.Base(it.Path)
		}
		verdict := "keep: "
		if it.Remove {
			verdict = "remove: "
		}
		decided[name] = verdict + it.Why
	}
	return items, decided
}

func TestOnlyCleanWorktreesAndBranchesOfMergedPullRequestsGo(t *testing.T) {
	root, heads := machine(t)
	gh := &fakeGitHub{merged: map[string]int{heads["wt-merged"]: 41, heads["wt-dirty"]: 42, heads["worker"]: 49}}
	_, decided := planFor(t, root, gh)

	want := map[string]string{
		"app":                  "keep: the clone itself",
		"worker":               "keep: a worker's directory",
		"wt-merged":            "remove: merged in #41",
		"wt-dirty":             "keep: 1 uncommitted or untracked file(s)",
		"wt-open":              "keep: no merged pull request ends at its commit",
		"wt-fresh":             "keep: no merged pull request ends at its commit",
		"gates":                "keep: detached: not on a branch",
		"main":                 "keep: the default branch",
		"task/1-merged":        "remove: merged in #41",
		"task/2-dirty":         "keep: checked out in a worktree that stays",
		"task/3-open":          "keep: checked out in a worktree that stays",
		"task/4-fresh":         "keep: checked out in a worktree that stays",
		"task/5-merged-branch": "remove: merged in #41",
		"task/6-open-branch":   "keep: no merged pull request ends at its commit",
		"task/9-in-hand":       "keep: checked out in a worktree that stays",
	}
	for name, verdict := range want {
		if decided[name] != verdict {
			t.Errorf("%s: %q, want %q", name, decided[name], verdict)
		}
	}
	if len(decided) != len(want) {
		t.Errorf("decided about %d items, want %d: %v", len(decided), len(want), decided)
	}
	if slices.Contains(gh.asked, heads["wt-dirty"]) || slices.Contains(gh.asked, heads["worker"]) {
		t.Errorf("GitHub was asked about a worktree that stays whatever it says")
	}
}

func TestApplyRemovesWhatWasPlannedAndNothingElse(t *testing.T) {
	root, heads := machine(t)
	items, _ := planFor(t, root, &fakeGitHub{merged: map[string]int{heads["wt-merged"]: 41}})

	res, err := ParseApplied(sh(t, ApplyScript(items)))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.RemovedWorktrees, []string{filepath.Join(root, "wt-merged")}) || len(res.Kept) != 0 {
		t.Fatalf("result = %+v", res)
	}
	slices.Sort(res.DeletedBranches)
	if !slices.Equal(res.DeletedBranches, []string{"task/1-merged", "task/5-merged-branch"}) {
		t.Fatalf("deleted = %v", res.DeletedBranches)
	}
	left := git(t, filepath.Join(root, "app"), "worktree", "list")
	for _, name := range []string{"worker", "wt-dirty", "wt-open", "wt-fresh", "gates"} {
		if !strings.Contains(left, filepath.Join(root, name)) {
			t.Errorf("worktree %s is gone:\n%s", name, left)
		}
	}
	branches := git(t, filepath.Join(root, "app"), "branch", "--format=%(refname:short)")
	if strings.Contains(branches, "task/1-merged") || !strings.Contains(branches, "task/6-open-branch") || !strings.Contains(branches, "task/9-in-hand") {
		t.Errorf("branches left:\n%s", branches)
	}
}

func TestApplyLeavesWhatChangedSinceItWasListed(t *testing.T) {
	root, heads := machine(t)
	items, _ := planFor(t, root, &fakeGitHub{merged: map[string]int{heads["wt-merged"]: 41}})

	// Between the list and the removal, someone works in the worktree and
	// moves the other merged branch on.
	merged := filepath.Join(root, "wt-merged")
	os.WriteFile(filepath.Join(merged, "more.txt"), []byte("new work\n"), 0o644)
	git(t, filepath.Join(root, "app"), "branch", "-f", "task/5-merged-branch", heads["wt-open"])

	res, err := ParseApplied(sh(t, ApplyScript(items)))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RemovedWorktrees) != 0 || len(res.DeletedBranches) != 0 || len(res.Kept) != 3 {
		t.Fatalf("result = %+v, want nothing removed and three items reported as kept", res)
	}
	if _, err := os.Stat(filepath.Join(merged, "more.txt")); err != nil {
		t.Fatalf("the new work is gone: %v", err)
	}
	if got := git(t, filepath.Join(root, "app"), "rev-parse", "task/5-merged-branch"); got != heads["wt-open"] {
		t.Fatalf("the branch that moved on was touched: %s", got)
	}
}

func TestListThatWasCutShortIsNotPlannedFrom(t *testing.T) {
	if _, err := Parse("@@repo origin/main|https://github.com/org/app.git|/x/.git\n@@branch abc|task/1\n"); err == nil {
		t.Fatal("a list with no end marker was accepted")
	}
}

func TestMergedPullRequestMustEndAtTheCommit(t *testing.T) {
	data := `[{"number":41,"merged_at":"2026-10-04T10:00:00Z","head":{"sha":"aaa"}},
	          {"number":42,"merged_at":null,"head":{"sha":"bbb"}},
	          {"number":43,"merged_at":"2026-10-04T10:00:00Z","head":{"sha":"later"}}]`
	for commit, want := range map[string]int{"aaa": 41, "bbb": 0, "ccc": 0} {
		if got, err := mergedPR([]byte(data), commit); err != nil || got != want {
			t.Errorf("commit %s: pull request %d, %v; want %d", commit, got, err, want)
		}
	}
}
