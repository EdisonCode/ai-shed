package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

type fakeBase struct {
	state backlog.BaseState
	asked int
}

func (f *fakeBase) BaseChecks(context.Context, string, string) (backlog.BaseState, error) {
	f.asked++
	return f.state, nil
}

func red() backlog.BaseState {
	return backlog.BaseState{Branch: "main", SHA: "abc1234def", Failed: []string{"test"}}
}

func breaking(t *testing.T, base *fakeBase, health bool) *fixture {
	t.Helper()
	f := started(t, config.Supervisor{}, done, done)
	f.Base = base
	f.Repos = []config.Repo{{Name: "org/app", CircuitBreaker: true}}
	f.Machine.Workers[0].Health = health
	return f
}

func TestARedDefaultBranchHoldsNewWorkButNotRework(t *testing.T) {
	f := breaking(t, &fakeBase{state: red()}, false)
	f.lister.issues = []backlog.Issue{labeled(11), handedBackWith(12, "conflicts with the base branch")}
	f.tick(2 * time.Minute)
	prompt := f.reviewer.prompts[0]
	if !strings.Contains(prompt, "org/app#11 Issue [held: the default branch main of org/app is red at abc1234 (test): new work waits until it is green]") {
		t.Fatalf("new work is not held:\n%s", prompt)
	}
	if strings.Contains(prompt, "org/app#12 Issue [rework: pull request #41 conflicts with the base branch] [held") {
		t.Fatalf("rework is held:\n%s", prompt)
	}
}

func TestAGreenDefaultBranchOrNoBreakerHoldsNothing(t *testing.T) {
	f := breaking(t, &fakeBase{state: backlog.BaseState{Branch: "main", SHA: "abc1234"}}, false)
	f.lister.issues = []backlog.Issue{labeled(11)}
	f.tick(2 * time.Minute)
	if prompt := f.reviewer.prompts[0]; strings.Contains(prompt, "is red at") {
		t.Fatalf("a green branch holds:\n%s", prompt)
	}
	off := breaking(t, &fakeBase{state: red()}, false)
	off.Repos = []config.Repo{{Name: "org/app", Gates: config.Gates{Command: "make"}}}
	off.lister.issues = []backlog.Issue{labeled(11)}
	off.tick(2 * time.Minute)
	if prompt := off.reviewer.prompts[0]; strings.Contains(prompt, "is red at") {
		t.Fatalf("without circuit_breaker, a red branch holds:\n%s", prompt)
	}
}

func TestTheDefaultBranchIsReadAtMostEveryFiveMinutes(t *testing.T) {
	base := &fakeBase{state: red()}
	f := breaking(t, base, false)
	f.lister.issues = []backlog.Issue{labeled(11)}
	f.tick(2 * time.Minute)
	f.tick(2 * time.Minute)
	if base.asked != 1 {
		t.Fatalf("asked = %d, want 1", base.asked)
	}
}

func TestAHealthWorkerIsGivenOneTaskPerRedCommit(t *testing.T) {
	f := breaking(t, &fakeBase{state: red()}, true)
	f.lister.issues = []backlog.Issue{labeled(11)}
	f.tick(2 * time.Minute)
	// The idle worker is handed it at once.
	brief, err := os.ReadFile(filepath.Join(f.StateDir, runlog.TasksDir, runlog.TaskTaken, "health-org-app-abc1234.md"))
	if err != nil || !strings.Contains(string(brief), "abc1234def") || !strings.Contains(string(brief), "test") {
		t.Fatalf("brief = %q, %v; want a health task naming the commit and the failed check", brief, err)
	}
	if c := f.lastCheckin(t); c.Task != "health-org-app-abc1234" {
		t.Fatalf("check-in = %+v, want the health task handed to app", c)
	}
	// The agent restarts: the same red commit is not queued again.
	f.healthQueued = nil
	f.breakerRead = nil
	f.tick(10 * time.Minute)
	entries, _ := os.ReadDir(filepath.Join(f.StateDir, runlog.TasksDir, "app"))
	if len(entries) != 0 {
		t.Fatalf("a second health task for the same commit: %v", entries)
	}
}

func TestNoHealthTaskWithoutAHealthWorker(t *testing.T) {
	f := breaking(t, &fakeBase{state: red()}, false)
	f.lister.issues = []backlog.Issue{labeled(11)}
	f.tick(2 * time.Minute)
	if task, ok := runlog.PendingTask(f.StateDir, "app"); ok {
		t.Fatalf("task = %+v, want none", task)
	}
}
