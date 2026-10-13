package gates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
)

// GH reads and writes GitHub with the GitHub CLI.
type GH struct{}

func (GH) OpenPulls(ctx context.Context, repo string) ([]Pull, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--repo", repo, "--state", "open", "--limit", "200", "--json", "number,headRefOid,isDraft")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list %s: %w: %s", repo, err, strings.TrimSpace(stderr.String()))
	}
	return parsePulls(out)
}

func parsePulls(data []byte) ([]Pull, error) {
	var prs []struct {
		Number int    `json:"number"`
		Head   string `json:"headRefOid"`
		Draft  bool   `json:"isDraft"`
	}
	if err := json.Unmarshal(data, &prs); err != nil {
		return nil, fmt.Errorf("gh pr list: parse output: %w", err)
	}
	pulls := make([]Pull, len(prs))
	for i, p := range prs {
		pulls[i] = Pull{Number: p.Number, Head: p.Head, Draft: p.Draft}
	}
	return pulls, nil
}

func (GH) SetStatus(ctx context.Context, repo, commit, state, description string) error {
	return backlog.GH{}.SetStatus(ctx, repo, commit, Context, state, description)
}

func (GH) PostGates(ctx context.Context, repo string, pr int, body string) error {
	return backlog.GH{}.PostMarked(ctx, repo, pr, Marker, body)
}

// Git is the real Tree.
type Git struct{}

func (Git) Has(ctx context.Context, dir, commit string) bool {
	return exec.CommandContext(ctx, "git", "-C", dir, "cat-file", "-e", commit+"^{commit}").Run() == nil
}

func (Git) Checkout(ctx context.Context, dir, worktree, commit string) error {
	if _, err := os.Stat(worktree); err != nil {
		return git(ctx, "-C", dir, "worktree", "add", "--detach", worktree, commit)
	}
	if err := git(ctx, "-C", worktree, "checkout", "--force", "--detach", commit); err != nil {
		return err
	}
	// Ignored files stay: installed dependencies and build caches make the
	// next run fast.
	return git(ctx, "-C", worktree, "clean", "-fd")
}

func git(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args[2:], " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Shell runs a script in dir in its own process group, so a timeout stops
// what it started too.
func Shell(ctx context.Context, dir, script string, timeout time.Duration) (string, int) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return string(out) + fmt.Sprintf("\nshed: stopped after the %s timeout", timeout), -1
	case ctx.Err() != nil:
		return string(out) + "\nshed: stopped because the agent shut down", -1
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode()
	default:
		return string(out) + "\nshed: " + err.Error(), -1
	}
}
