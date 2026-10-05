package runlog

import (
	"os"
	"path/filepath"
	"testing"
)

func queueTask(t *testing.T, dir, where, id, brief string) {
	t.Helper()
	path := filepath.Join(dir, TasksDir, where, id+".md")
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(brief), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerTakesItsOwnTaskBeforeOneForAnyWorker(t *testing.T) {
	dir := t.TempDir()
	if _, ok := PendingTask(dir, "app"); ok {
		t.Fatal("a task is pending on a machine that was pushed none")
	}
	queueTask(t, dir, TaskAny, "20261004-100000", "for anyone")
	queueTask(t, dir, "app", "20261004-120000", "for app, pushed later")
	queueTask(t, dir, "app", "20261004-110000", "for app")
	queueTask(t, dir, "docs", "20261004-090000", "for docs")

	var got []string
	for {
		task, ok := PendingTask(dir, "app")
		if !ok {
			break
		}
		got = append(got, task.Brief)
		if err := TakeTask(dir, task); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"for app", "for app, pushed later", "for anyone"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("taken in order %q, want %q", got, want)
	}
	if brief, ok := TakenTask(dir, "20261004-110000"); !ok || brief != "for app" {
		t.Fatalf("taken task = %q, %v", brief, ok)
	}
	if task, ok := PendingTask(dir, "docs"); !ok || task.Worker != "docs" {
		t.Fatalf("the other worker's task = %+v, %v", task, ok)
	}
}

func TestTaskOfASessionThatEndedGoesBackToItsWorker(t *testing.T) {
	dir := t.TempDir()
	queueTask(t, dir, TaskAny, "20261004-100000", "for anyone")
	task, _ := PendingTask(dir, "app")
	if err := TakeTask(dir, task); err != nil {
		t.Fatal(err)
	}
	if err := ReturnTask(dir, task.ID, "app"); err != nil {
		t.Fatal(err)
	}
	if back, ok := PendingTask(dir, "app"); !ok || back.ID != task.ID || back.Worker != "app" {
		t.Fatalf("pending = %+v, %v; the worker that had it takes it up again", back, ok)
	}
	if _, ok := PendingTask(dir, "docs"); ok {
		t.Fatal("another worker would now take a task that is half done elsewhere")
	}
}

func TestTaskInHandEndsWhenTheWorkerIsDoneOrMovesOn(t *testing.T) {
	handed := Checkin{Worker: "app", Verdict: VerdictNudge, Sent: true, Task: "t1"}
	for _, tc := range []struct {
		name  string
		after []Checkin
		want  string
	}{
		{"just handed", nil, "t1"},
		{"nudged about it", []Checkin{{Worker: "app", Verdict: VerdictNudge, Sent: true}}, "t1"},
		{"another worker is done", []Checkin{{Worker: "docs", Verdict: VerdictDone}}, "t1"},
		{"found done", []Checkin{{Worker: "app", Verdict: VerdictDone}}, ""},
		{"handed an issue", []Checkin{{Worker: "app", Verdict: VerdictNudge, Sent: true, Issue: 12}}, ""},
		{"its session started again", []Checkin{{Worker: "app", Verdict: VerdictStarted, Sent: true}}, ""},
	} {
		log := append([]Checkin{{Worker: "app", Verdict: VerdictNudge, Sent: true, Issue: 7}, handed}, tc.after...)
		if got := TaskInHand(log, "app"); got != tc.want {
			t.Errorf("%s: task in hand = %q, want %q", tc.name, got, tc.want)
		}
	}
	if issue := IssueInHand([]Checkin{{Worker: "app", Sent: true, Issue: 7}, handed}, "app"); issue != 0 {
		t.Errorf("issue in hand = #%d during a one-off task; another worker may take #7", issue)
	}
	if got := TaskWorker([]Checkin{handed}, "t1"); got != "app" {
		t.Errorf("task worker = %q", got)
	}
}
