package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/looks"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

func spend(t *testing.T, f *fixture) {
	t.Helper()
	if err := runlog.RecordTranscript(f.StateDir, "app", writeUsageTranscript(t, f.now), "sess"); err != nil {
		t.Fatal(err)
	}
}

func TestAnEyeCheckPostsItsCostOnTheIssueWhenTheWorkerMovesOn(t *testing.T) {
	cost := &fakeCost{}
	f := started(t, config.Supervisor{}, assign(7), done)
	f.Cost = cost
	reader := withStaging(f, look(7, 41, looks.Due))
	f.tick(2 * time.Minute) // the look of #7 is handed over
	spend(t, f)
	reader.looks = nil // it posted what it saw; the look leaves the queue
	f.lister.issues = nil
	f.term.running(f.now)
	f.tick(2 * time.Minute) // done
	if len(cost.bodies) != 1 || cost.repos[0] != "org/app" || cost.prs[0] != 7 || cost.markers[0] != "<!-- shed-cost app -->" {
		t.Fatalf("posted %v %v %q, want the cost on issue #7 under app's marker", cost.repos, cost.prs, cost.markers)
	}
	if !strings.Contains(cost.bodies[0], "<!-- shed-cost app -->") || !strings.Contains(cost.bodies[0], "output 85k") || strings.Contains(cost.bodies[0], backlog.CostMarker) {
		t.Fatalf("body:\n%s", cost.bodies[0])
	}
}

func TestAPlanOnlyPassPostsItsCostOnTheIssue(t *testing.T) {
	cost := &fakeCost{}
	f := started(t, config.Supervisor{}, assign(12), done)
	f.Cost = cost
	f.tick(2 * time.Minute)
	spend(t, f)
	f.lister.issues[0].Comments = []backlog.Comment{{Body: "## Hand-back\n**PR:** none\n## Plan\n1. Add the column."}}
	f.term.running(f.now)
	f.tick(2 * time.Minute)
	if len(cost.bodies) != 1 || cost.prs[0] != 12 {
		t.Fatalf("posted %v, want the cost on issue #12", cost.prs)
	}
	f.term.running(f.now)
	f.tick(2 * time.Minute)
	if len(cost.bodies) != 1 {
		t.Fatalf("posted %d times with nothing new", len(cost.bodies))
	}
}

func TestAOneOffTaskWritesItsCostIntoItsReport(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	f.Cost = &fakeCost{}
	f.pushTask(t, runlog.TaskAny, "t-1", "Compare the two exporters.")
	f.tick(2 * time.Minute) // the task is handed over
	spend(t, f)
	report := filepath.Join(f.dir, TaskDir, "t-1"+taskReport)
	if err := os.WriteFile(report, []byte("# Report\nThe new one is faster.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.term.running(f.now)
	f.tick(2 * time.Minute) // done with the task
	data, _ := os.ReadFile(report)
	if !strings.Contains(string(data), "The new one is faster.") || !strings.Contains(string(data), "## Cost") || !strings.Contains(string(data), "output 85k") {
		t.Fatalf("report:\n%s", data)
	}
	f.term.running(f.now)
	f.tick(2 * time.Minute)
	again, _ := os.ReadFile(report)
	if strings.Count(string(again), "## Cost") != 1 {
		t.Fatalf("the cost was written twice:\n%s", again)
	}
}
