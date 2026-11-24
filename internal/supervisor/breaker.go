package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// BaseReader reads the checks on a repository's default branch.
type BaseReader interface {
	BaseChecks(ctx context.Context, repo, base string) (backlog.BaseState, error)
}

// breakerEvery is how long the state of a default branch is kept before it
// is read again.
const breakerEvery = 5 * time.Minute

// refreshBreaker reads again, at most every breakerEvery, the checks on the
// default branch of each repository with circuit_breaker. A failure every
// worker shares has no owner: each new pull request inherits it and goes
// back as rework. While the branch is red its new work is held, and a
// worker with health is given one task to fix it.
func (s *Supervisor) refreshBreaker(ctx context.Context) {
	if s.Base == nil {
		return
	}
	now := s.Now()
	for _, repo := range s.Repos {
		if !repo.CircuitBreaker || now.Sub(s.breakerRead[repo.Name]) < breakerEvery {
			continue
		}
		if s.breakerRead == nil {
			s.breakerRead, s.broken = map[string]time.Time{}, map[string]backlog.BaseState{}
		}
		s.breakerRead[repo.Name] = now
		state, err := s.Base.BaseChecks(ctx, repo.Name, "")
		if err != nil {
			s.Logf("read the default branch of %s: %v", repo.Name, err)
			continue
		}
		if !state.Red() {
			if _, was := s.broken[repo.Name]; was {
				s.Logf("the default branch of %s is green again at %s: new work goes ahead", repo.Name, short(state.SHA))
			}
			delete(s.broken, repo.Name)
			continue
		}
		s.broken[repo.Name] = state
		s.queueHealth(repo.Name, state)
	}
}

// breakerHold says why a new issue waits: its repository's default branch
// is red. "" when it is not.
func (s *Supervisor) breakerHold(issue backlog.Issue) string {
	state, ok := s.broken[issue.Repo]
	if !ok {
		return ""
	}
	return fmt.Sprintf("the default branch %s of %s is red at %s (%s): new work waits until it is green", state.Branch, issue.Repo, short(state.SHA), strings.Join(state.Failed, ", "))
}

// queueHealth gives the machine's first worker with health a one-off task
// to make the red default branch green: one task per red commit, kept
// across restarts by the task's name.
func (s *Supervisor) queueHealth(repo string, state backlog.BaseState) {
	worker := ""
	for _, w := range s.Machine.Workers {
		if w.Health && s.Machine.TakesWork(w.Name) == nil {
			worker = w.Name
			break
		}
	}
	if worker == "" {
		return
	}
	id := "health-" + strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(strings.ToLower(repo)) + "-" + strings.ToLower(short(state.SHA))
	if s.healthQueued[id] {
		return
	}
	if s.healthQueued == nil {
		s.healthQueued = map[string]bool{}
	}
	for _, where := range []string{worker, runlog.TaskTaken} {
		if _, err := os.Stat(filepath.Join(s.StateDir, runlog.TasksDir, where, id+".md")); err == nil {
			s.healthQueued[id] = true
			return
		} else if !errors.Is(err, fs.ErrNotExist) {
			s.Logf("health task for %s: %v", repo, err)
			return
		}
	}
	dir := filepath.Join(s.StateDir, runlog.TasksDir, worker)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.Logf("health task for %s: %v", repo, err)
		return
	}
	brief := fmt.Sprintf(`# Make the default branch of %s green

The checks on %s's default branch %s failed at commit %s: %s.
Every new pull request inherits this failure, so shed holds new work in
this repository until the branch is green again.

Find why the checks fail, starting from the failed check's log. Fix it in a
pull request against %s that does nothing else, and mark it ready with
auto-merge. If the fault came from a merged pull request whose issue is
still open, write `+"`%s #<that issue>`"+` on it. If you cannot fix it, say why
in your report and stop.
`, repo, repo, state.Branch, state.SHA, strings.Join(state.Failed, ", "), state.Branch, backlog.RegressionPhrase)
	if err := os.WriteFile(filepath.Join(dir, id+".md"), []byte(brief), 0o644); err != nil {
		s.Logf("health task for %s: %v", repo, err)
		return
	}
	s.healthQueued[id] = true
	s.Logf("the default branch of %s is red at %s: gave %s a task to fix it", repo, short(state.SHA), worker)
}
