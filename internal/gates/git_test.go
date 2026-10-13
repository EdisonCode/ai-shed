package gates

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", file)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

func TestTheGateTreeFollowsTheCommitAndKeepsIgnoredFiles(t *testing.T) {
	repo, tree := t.TempDir(), filepath.Join(t.TempDir(), "gates", "app")
	gitIn(t, repo, "init", "-q")
	commit(t, repo, ".gitignore", "deps/\n")
	first := commit(t, repo, "a.txt", "one")
	ctx := context.Background()

	if !(Git{}).Has(ctx, repo, first) || (Git{}).Has(ctx, repo, strings.Repeat("0", 40)) {
		t.Fatal("Has does not tell a commit of the checkout from one it lacks")
	}
	if err := (Git{}).Checkout(ctx, repo, tree, first); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(tree, "deps"), 0o755)
	os.WriteFile(filepath.Join(tree, "deps", "cache"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(tree, "stray.txt"), []byte("x"), 0o644)

	second := commit(t, repo, "a.txt", "two")
	if err := (Git{}).Checkout(ctx, repo, tree, second); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(tree, "a.txt")); string(got) != "two" {
		t.Fatalf("a.txt = %q, want the second commit's", got)
	}
	if _, err := os.Stat(filepath.Join(tree, "stray.txt")); err == nil {
		t.Fatal("an untracked file survived the checkout")
	}
	if _, err := os.Stat(filepath.Join(tree, "deps", "cache")); err != nil {
		t.Fatal("an ignored cache was removed")
	}
	if head := gitIn(t, repo, "rev-parse", "HEAD"); head != second {
		t.Fatal("the worker's own checkout moved")
	}
}

func TestAGateRunThatOutlastsItsTimeoutIsStopped(t *testing.T) {
	start := time.Now()
	out, exit := Shell(context.Background(), t.TempDir(), "echo started; sleep 30", 300*time.Millisecond)
	if exit != -1 || !strings.Contains(out, "started") || !strings.Contains(out, "stopped after the 300ms timeout") {
		t.Fatalf("exit %d, output %q", exit, out)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the timeout did not stop the script")
	}
	if _, exit := Shell(context.Background(), t.TempDir(), "exit 3", time.Minute); exit != 3 {
		t.Fatalf("exit = %d, want 3", exit)
	}
}
