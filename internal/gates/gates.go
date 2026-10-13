// Package gates runs a repository's gates on the machine that built a pull
// request, on the exact commit it pushed, and posts the result as a commit
// status. A worker hears of a failure in seconds or minutes, on a machine
// whose caches are warm, instead of after a hosted runner's queue.
//
// The status is fast feedback, not proof: a worker shares the machine and
// its GitHub login. The full suite in a merge queue stays the gate a worker
// cannot fake.
package gates

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

const (
	// Context is the name of the commit status shed posts.
	Context = "shed/gates"
	// Marker identifies the comment shed posts on a pull request whose
	// gates failed, so a later run replaces it.
	Marker = "<!-- shed-gates -->"
	// Dir holds the gate trees and the full output of each run, under the
	// agent's state directory.
	Dir = "gates"
	// retryAfter is how long a commit whose gate tree could not be checked
	// out waits before the next try.
	retryAfter = 10 * time.Minute
	// commentLines is how much of a failed run's output the comment shows.
	commentLines = 60
	outputTail   = 4000
	maxStatus    = 140
)

// Pull is an open pull request and the commit at its head.
type Pull struct {
	Number int
	Head   string
	Draft  bool
}

// GitHub is what the runner reads from and writes to GitHub.
type GitHub interface {
	OpenPulls(ctx context.Context, repo string) ([]Pull, error)
	// SetStatus sets the shed/gates status of a commit: pending, success
	// or failure.
	SetStatus(ctx context.Context, repo, commit, state, description string) error
	// PostGates creates the gates comment on the pull request, or replaces
	// the one shed posted before. The body carries Marker.
	PostGates(ctx context.Context, repo string, pr int, body string) error
}

// Tree is the git work the runner needs in a worker's checkout.
type Tree interface {
	// Has reports whether the checkout in dir has the commit: whether it
	// was built on this machine.
	Has(ctx context.Context, dir, commit string) bool
	// Checkout puts the commit in the gate tree of the checkout in dir,
	// creating the tree when it does not exist. Files git ignores, such as
	// installed dependencies and build caches, are kept between runs.
	Checkout(ctx context.Context, dir, worktree, commit string) error
}

// Runner gates the pull requests built on one machine.
type Runner struct {
	Machine  config.Machine
	Repos    []config.Repo
	GitHub   GitHub
	Tree     Tree
	StateDir string
	// Run runs a script in dir and returns its combined output and exit
	// code, -1 when it timed out or could not start.
	Run  func(ctx context.Context, dir, script string, timeout time.Duration) (string, int)
	Now  func() time.Time
	Busy func(ctx context.Context) string
	Logf func(format string, args ...any)

	// waitingSince is when a run first waited for the machine to have room.
	waitingSince time.Time
	// notBefore holds the commits whose gate tree could not be checked out,
	// with when to try again.
	notBefore map[string]time.Time
}

// candidate is a commit that owes a gate run.
type candidate struct {
	repo   config.Repo
	pull   Pull
	worker config.Worker
}

// Gated reports whether any repository the machine takes issues from has
// gates.
func Gated(m config.Machine, repos []config.Repo) bool {
	for _, src := range m.AllSources() {
		if r, ok := repoOf(repos, src.Repo); ok && r.Gates.Command != "" {
			return true
		}
	}
	return false
}

// Tick runs the gates of at most one commit: the first open pull request
// built on this machine whose head has not been gated.
func (r *Runner) Tick(ctx context.Context) {
	c, ok, err := r.next(ctx)
	if err != nil {
		r.Logf("gates: %v", err)
		return
	}
	if !ok {
		r.waitingSince = time.Time{}
		return
	}
	now := r.Now()
	if r.Busy != nil {
		if reason := r.Busy(ctx); reason != "" {
			if r.waitingSince.IsZero() {
				r.waitingSince = now
			}
			if now.Sub(r.waitingSince) < r.Machine.Capacity.MaxWaitOrDefault() {
				return
			}
			r.Logf("gates: running on %s#%d after waiting %s, although: %s", c.repo.Name, c.pull.Number, now.Sub(r.waitingSince).Round(time.Second), reason)
		}
	}
	r.waitingSince = time.Time{}
	if err := r.gate(ctx, c); err != nil {
		r.Logf("gates: %s#%d: %v", c.repo.Name, c.pull.Number, err)
	}
}

// next finds the commit to gate.
func (r *Runner) next(ctx context.Context) (candidate, bool, error) {
	done, err := runlog.ReadGates(r.StateDir)
	if err != nil {
		return candidate{}, false, err
	}
	gated := map[string]bool{}
	for _, g := range done {
		gated[g.Repo+"@"+g.Commit] = true
	}
	now := r.Now()
	var seen []string
	for _, src := range r.Machine.AllSources() {
		repo, ok := repoOf(r.Repos, src.Repo)
		if !ok || repo.Gates.Command == "" || slices.Contains(seen, repo.Name) {
			continue
		}
		seen = append(seen, repo.Name)
		pulls, err := r.GitHub.OpenPulls(ctx, repo.Name)
		if err != nil {
			return candidate{}, false, err
		}
		for _, p := range pulls {
			key := repo.Name + "@" + p.Head
			if p.Draft || gated[key] || now.Before(r.notBefore[key]) {
				continue
			}
			for _, w := range r.Machine.Workers {
				if w.Ask || !r.Tree.Has(ctx, expandHome(w.Dir), p.Head) {
					continue
				}
				return candidate{repo: repo, pull: p, worker: w}, true, nil
			}
		}
	}
	return candidate{}, false, nil
}

// gate runs the gates on one commit and posts what came of it.
func (r *Runner) gate(ctx context.Context, c candidate) error {
	key := c.repo.Name + "@" + c.pull.Head
	tree := filepath.Join(r.StateDir, Dir, c.worker.Name)
	if err := r.Tree.Checkout(ctx, expandHome(c.worker.Dir), tree, c.pull.Head); err != nil {
		if r.notBefore == nil {
			r.notBefore = map[string]time.Time{}
		}
		r.notBefore[key] = r.Now().Add(retryAfter)
		return fmt.Errorf("check out %s in %s, trying again in %s: %w", short(c.pull.Head), tree, retryAfter, err)
	}
	if err := r.GitHub.SetStatus(ctx, c.repo.Name, c.pull.Head, "pending", "running on "+r.Machine.Name); err != nil {
		return err
	}
	start := r.Now()
	output, exit := r.Run(ctx, tree, r.Machine.Command(c.repo.Gates.Command), c.repo.Gates.TimeoutOrDefault())
	if ctx.Err() != nil {
		// The agent is stopping: a reload of the fleet file, an update. The
		// work did not fail; the commit is gated again at the next start.
		return fmt.Errorf("stopped by the agent before it finished on %s; it runs again", short(c.pull.Head))
	}
	g := runlog.Gate{Repo: c.repo.Name, PR: c.pull.Number, Commit: c.pull.Head, Worker: c.worker.Name,
		Start: start, End: r.Now(), ExitCode: exit, Output: tail(output, outputTail)}
	logPath := filepath.Join(r.StateDir, Dir, c.pull.Head+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		r.Logf("gates: keep the output of %s: %v", short(c.pull.Head), err)
	} else if err := os.WriteFile(logPath, []byte(output), 0o644); err != nil {
		r.Logf("gates: keep the output of %s: %v", short(c.pull.Head), err)
	}
	if err := runlog.AppendGate(r.StateDir, g); err != nil {
		return err
	}
	took := spell(g.End.Sub(g.Start))
	if g.Passed() {
		if err := r.GitHub.SetStatus(ctx, c.repo.Name, c.pull.Head, "success", fmt.Sprintf("passed in %s on %s", took, r.Machine.Name)); err != nil {
			return err
		}
		// A comment is left only where it replaces a failure.
		if r.failedBefore(c) {
			body := fmt.Sprintf("%s\n**shed gates** passed on %s (`%s`, %s on %s).\n", Marker, short(c.pull.Head), c.repo.Gates.Command, took, r.Machine.Name)
			return r.GitHub.PostGates(ctx, c.repo.Name, c.pull.Number, body)
		}
		return nil
	}
	description := fmt.Sprintf("failed in %s on %s: %s", took, r.Machine.Name, lastLine(output))
	if err := r.GitHub.SetStatus(ctx, c.repo.Name, c.pull.Head, "failure", clip(description, maxStatus)); err != nil {
		return err
	}
	body := fmt.Sprintf("%s\n**shed gates** failed on %s (`%s`, exit %d after %s on %s). The last %d lines:\n\n```\n%s\n```\n\nThe whole output is on %s in `~/%s/%s/%s.log`.\n",
		Marker, short(c.pull.Head), c.repo.Gates.Command, exit, took, r.Machine.Name, commentLines,
		lastLines(output, commentLines), r.Machine.Name, runlog.StateDir, Dir, c.pull.Head)
	return r.GitHub.PostGates(ctx, c.repo.Name, c.pull.Number, body)
}

// failedBefore reports whether an earlier run on the pull request failed.
func (r *Runner) failedBefore(c candidate) bool {
	done, _ := runlog.ReadGates(r.StateDir)
	for _, g := range done {
		if g.Repo == c.repo.Name && g.PR == c.pull.Number && g.Commit != c.pull.Head && !g.Passed() {
			return true
		}
	}
	return false
}

func repoOf(repos []config.Repo, name string) (config.Repo, bool) {
	for _, r := range repos {
		if r.Name == name {
			return r, true
		}
	}
	return config.Repo{}, false
}

func short(commit string) string { return commit[:min(len(commit), 7)] }

func lastLine(output string) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func lastLines(output string, n int) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

func tail(s string, n int) string { return s[max(0, len(s)-n):] }

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// spell writes a duration in whole minutes, or seconds under a minute.
func spell(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

func expandHome(dir string) string {
	if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return dir
}
