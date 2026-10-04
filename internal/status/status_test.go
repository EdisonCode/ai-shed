package status

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

var (
	now     = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	signals = config.DefaultSignals()
	nightly = config.Task{Name: "nightly", Schedule: "0 3 * * *", Run: "true"}
)

// healthy is a probe result with nothing wrong and a live agent.
func healthy() *probe.Result {
	return &probe.Result{Now: now, Cores: 8, Load1: 0.5, DiskUsedPct: 40, Heartbeat: now.Add(-20 * time.Second)}
}

func issue(number int, comments ...string) backlog.Issue {
	i := backlog.Issue{Repo: "org/app", Number: number, Title: "Fix the thing"}
	for _, body := range comments {
		i.Comments = append(i.Comments, backlog.Comment{Body: body})
	}
	return i
}

func window(name string, quietFor time.Duration) probe.Window {
	return probe.Window{Session: "work", Name: name, Command: "claude", LastActivity: now.Add(-quietFor)}
}

func ran(exit int, ago time.Duration) runlog.Record {
	start := now.Add(-ago)
	return runlog.Record{Task: "nightly", Start: start, End: start.Add(time.Second), ExitCode: exit, Next: start.Add(24 * time.Hour), Output: "step 1\nboom\n"}
}

func wantAttention(t *testing.T, r MachineReport, substrings ...string) {
	t.Helper()
	if len(r.Attention) != len(substrings) {
		t.Fatalf("attention = %q, want %d item(s)", r.Attention, len(substrings))
	}
	for i, want := range substrings {
		if !strings.Contains(r.Attention[i], want) {
			t.Fatalf("attention[%d] = %q, want it to mention %q", i, r.Attention[i], want)
		}
	}
}

func TestHealthyIdleMachineNeedsNothing(t *testing.T) {
	wantAttention(t, Assess(config.Machine{Name: "box"}, signals, healthy(), nil))
}

func TestFailedCheckNeedsOwner(t *testing.T) {
	res := healthy()
	res.Checks = []probe.CheckResult{{Name: "gh auth", OK: true}, {Name: "claude", Detail: "not found"}}
	wantAttention(t, Assess(config.Machine{}, signals, res, nil), "check failed: claude (not found)")
}

func TestFullDiskNeedsOwner(t *testing.T) {
	res := healthy()
	res.DiskUsedPct = 93
	wantAttention(t, Assess(config.Machine{}, signals, res, nil), "disk is 93% full")
}

func TestIssueWithWorkerIsWorking(t *testing.T) {
	res := healthy()
	res.Windows = []probe.Window{window("app-123", time.Minute)}
	r := Assess(config.Machine{}, signals, res, []backlog.Issue{issue(123)})
	wantAttention(t, r)
	if r.Issues[0].State != Working || r.Issues[0].Worker != "app-123" {
		t.Fatalf("issue = %+v", r.Issues[0])
	}
}

func TestWorkerMatchNeedsTheWholeNumber(t *testing.T) {
	res := healthy()
	res.Windows = []probe.Window{window("app-1234", time.Minute)}
	r := Assess(config.Machine{}, signals, res, []backlog.Issue{issue(123)})
	if r.Issues[0].State != Queued {
		t.Fatalf("issue 123 matched window app-1234: %+v", r.Issues[0])
	}
}

func TestQueuedIssuesWithNoWorkerNeedOwner(t *testing.T) {
	r := Assess(config.Machine{}, signals, healthy(), []backlog.Issue{issue(1), issue(2)})
	wantAttention(t, r, "2 issue(s) queued and no worker is running")
}

func TestQueuedIssuesBehindABusyWorkerAreFine(t *testing.T) {
	res := healthy()
	res.Windows = []probe.Window{window("app-1", time.Minute)}
	wantAttention(t, Assess(config.Machine{}, signals, res, []backlog.Issue{issue(1), issue(2)}))
}

func TestIssueWaitingOnDecisionNeedsOwner(t *testing.T) {
	res := healthy()
	res.Windows = []probe.Window{window("app-7", 3*time.Hour)}
	r := Assess(config.Machine{}, signals, res, []backlog.Issue{issue(7, "Decisions needed: 1. which one?")})
	wantAttention(t, r, "org/app#7 waits on you (decision)")
	if r.Issues[0].State != Waiting {
		t.Fatalf("state = %s", r.Issues[0].State)
	}
}

func TestQuietWorkerNeedsOwner(t *testing.T) {
	res := healthy()
	res.Windows = []probe.Window{window("app-9", 2*time.Hour)}
	wantAttention(t, Assess(config.Machine{}, signals, res, []backlog.Issue{issue(9)}), "worker app-9 on #9 has been quiet for 2h")
}

func TestTasksWithoutAgentNeedOwner(t *testing.T) {
	res := healthy()
	res.Heartbeat = time.Time{}
	wantAttention(t, Assess(config.Machine{Tasks: []config.Task{nightly}}, signals, res, nil), "agent is not running")
}

func TestStaleHeartbeatNeedsOwner(t *testing.T) {
	res := healthy()
	res.Heartbeat = now.Add(-2 * time.Hour)
	res.Runs = []runlog.Record{ran(0, 30*time.Hour)}
	r := Assess(config.Machine{Tasks: []config.Task{nightly}}, signals, res, nil)
	wantAttention(t, r, "agent stopped 2h ago: 1 task(s) will not run")
	if !r.Tasks[0].Missed {
		t.Fatal("the task is late and must be marked missed")
	}
}

func TestMachineWithoutTasksNeedsNoAgent(t *testing.T) {
	res := healthy()
	res.Heartbeat = time.Time{}
	wantAttention(t, Assess(config.Machine{}, signals, res, nil))
}

func TestTaskThatNeverRanIsNotAnAlarm(t *testing.T) {
	r := Assess(config.Machine{Tasks: []config.Task{nightly}}, signals, healthy(), nil)
	wantAttention(t, r)
	if r.Tasks[0].State != TaskNever {
		t.Fatalf("state = %s", r.Tasks[0].State)
	}
}

func TestFailedTaskNeedsOwner(t *testing.T) {
	res := healthy()
	res.Runs = []runlog.Record{ran(0, 30*time.Hour), ran(2, 9*time.Hour)}
	wantAttention(t, Assess(config.Machine{Tasks: []config.Task{nightly}}, signals, res, nil), "task nightly failed (exit 2) 8h ago (boom)")
}

func TestMissedTaskNeedsOwner(t *testing.T) {
	res := healthy()
	res.Runs = []runlog.Record{ran(0, 30*time.Hour)}
	wantAttention(t, Assess(config.Machine{Tasks: []config.Task{nightly}}, signals, res, nil), "task nightly missed its")
}

func TestTaskOnScheduleIsFine(t *testing.T) {
	res := healthy()
	res.Runs = []runlog.Record{ran(0, 9*time.Hour)}
	r := Assess(config.Machine{Tasks: []config.Task{nightly}}, signals, res, nil)
	wantAttention(t, r)
	if r.Tasks[0].State != TaskOK {
		t.Fatalf("state = %s", r.Tasks[0].State)
	}
}

type fakeRunner struct{ err error }

func (f fakeRunner) Run(context.Context, config.Machine, string, io.Reader) ([]byte, error) {
	return []byte("@@now 1790000000\n@@end\n"), f.err
}

type fakeLister struct {
	issues []backlog.Issue
	err    error
}

func (f fakeLister) List(context.Context, config.IssueSource) ([]backlog.Issue, error) {
	return f.issues, f.err
}

func oneMachine() *config.Config {
	return &config.Config{Signals: signals, Machines: []config.Machine{{
		Name: "box", Host: "me@box", Issues: []config.IssueSource{{Repo: "org/app", Label: "machine:box"}},
	}}}
}

func TestUnreachableMachineStillReportsWaitingIssues(t *testing.T) {
	c := Collector{
		Runner: fakeRunner{err: errors.New("connection timed out")},
		Lister: fakeLister{issues: []backlog.Issue{issue(7, "Decisions needed: 1. which one?"), issue(8)}},
	}
	r := c.Collect(context.Background(), oneMachine())[0]
	wantAttention(t, r, "unreachable: connection timed out", "#7 waits on you")
}

func TestIssueListFailureNeedsOwner(t *testing.T) {
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{err: errors.New("gh: not logged in")}}
	wantAttention(t, c.Collect(context.Background(), oneMachine())[0], "cannot list issues: gh: not logged in")
}

func TestRenderSummarizesWhatNeedsTheOwner(t *testing.T) {
	res := healthy()
	res.Checks = []probe.CheckResult{{Name: "claude"}}
	var out bytes.Buffer
	Render(&out, []MachineReport{Assess(config.Machine{Name: "box", Host: "me@box"}, signals, res, nil)})
	for _, want := range []string{"box  me@box  load 0.50/8  disk 40%", "FAIL claude", "! check failed: claude", "1 item(s) need you."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestRenderSaysWhenAllIsWell(t *testing.T) {
	var out bytes.Buffer
	Render(&out, []MachineReport{Assess(config.Machine{Name: "box"}, signals, healthy(), nil)})
	if !strings.Contains(out.String(), "All machines are on track.") {
		t.Fatalf("output:\n%s", out.String())
	}
}

var appWorker = config.Machine{Workers: []config.Worker{{Name: "app", Dir: "/tmp", Brief: "b"}}}

func checkin(verdict, reason string) runlog.Checkin {
	return runlog.Checkin{Time: now.Add(-5 * time.Minute), Worker: "app", Kind: "idle", Verdict: verdict, Reason: reason}
}

func TestWorkersWithoutAgentNeedOwner(t *testing.T) {
	res := healthy()
	res.Heartbeat = time.Time{}
	wantAttention(t, Assess(appWorker, signals, res, nil), "agent is not running: 0 task(s) will not run and 1 worker(s) are not supervised")
}

func TestWorkerVerdictsThatNeedOwner(t *testing.T) {
	cases := map[string]string{
		runlog.VerdictNeedsOwner: "worker app needs you: a permission prompt is open",
		runlog.VerdictStuck:      "worker app is stuck: a permission prompt is open",
		runlog.VerdictError:      "worker app could not be checked: a permission prompt is open",
	}
	for verdict, want := range cases {
		res := healthy()
		res.Checkins = []runlog.Checkin{checkin(runlog.VerdictNudge, "older"), checkin(verdict, "a permission prompt is open")}
		wantAttention(t, Assess(appWorker, signals, res, nil), want)
	}
}

func TestWorkerThatIsOnTrackNeedsNothing(t *testing.T) {
	for _, verdict := range []string{runlog.VerdictOnTrack, runlog.VerdictNudge, runlog.VerdictDone, runlog.VerdictStarted} {
		res := healthy()
		res.Checkins = []runlog.Checkin{checkin(runlog.VerdictNeedsOwner, "older"), checkin(verdict, "fine")}
		r := Assess(appWorker, signals, res, nil)
		wantAttention(t, r)
		if r.Workers[0].Last == nil || r.Workers[0].Last.Verdict != verdict {
			t.Fatalf("worker = %+v", r.Workers[0])
		}
	}
}

func handedOver(issue int) runlog.Checkin {
	return runlog.Checkin{Time: now.Add(-20 * time.Minute), Worker: "app", Kind: "idle", Verdict: runlog.VerdictNudge, Sent: true, Issue: issue, Reason: "next in the queue"}
}

func TestIssueInHandOfASupervisedWorkerIsWorking(t *testing.T) {
	res := healthy()
	res.Windows = []probe.Window{{Session: "shed", Name: "app", Command: "2.1.289", LastActivity: now.Add(-2 * time.Hour)}}
	res.Checkins = []runlog.Checkin{handedOver(7)}
	r := Assess(appWorker, signals, res, []backlog.Issue{issue(7), issue(8)})

	wantAttention(t, r) // no "queued and no worker", and no "quiet" alarm: the supervisor owns both
	if r.Issues[0].State != Working || r.Issues[0].Worker != "app" || r.Issues[1].State != Queued {
		t.Fatalf("issues = %+v", r.Issues)
	}
}

func TestQueueBehindAnIdleSupervisedWorkerIsNotAnAlarm(t *testing.T) {
	res := healthy()
	res.Checkins = []runlog.Checkin{checkin(runlog.VerdictOnTrack, "waiting for its first issue")}
	wantAttention(t, Assess(appWorker, signals, res, []backlog.Issue{issue(7), issue(8)}))
}

func TestHandedBackIssueWaitsOnOwnerEvenWhileInHand(t *testing.T) {
	res := healthy()
	res.Checkins = []runlog.Checkin{handedOver(7)}
	r := Assess(appWorker, signals, res, []backlog.Issue{issue(7, "**PR:** #41 (ready)")})
	wantAttention(t, r, "#7 waits on you (review)")
}

func TestRenderNamesASupervisedWorkersToolAndIssue(t *testing.T) {
	res := healthy()
	res.Windows = []probe.Window{
		{Session: "shed", Name: "app", Command: "2.1.289", LastActivity: now},
		{Session: "work", Name: "notes", Command: "2.1.289", LastActivity: now},
	}
	res.Checkins = []runlog.Checkin{handedOver(7)}
	m := config.Machine{Name: "box", Workers: []config.Worker{{Name: "app", Dir: "/tmp", Brief: "b", Command: "claude --permission-mode auto"}}}
	var out bytes.Buffer
	Render(&out, []MachineReport{Assess(m, signals, res, nil)})
	for _, want := range []string{"shed:app    claude", "work:notes  2.1.289", "app  #7  nudge  20m ago"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}
