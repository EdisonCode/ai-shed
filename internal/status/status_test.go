package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/diraccess"
	"github.com/edisoncode/ai-shed/internal/looks"
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
	wantAttention(t, Assess(config.Machine{Name: "box"}, signals, now, healthy(), nil))
}

func TestFailedCheckNeedsOwner(t *testing.T) {
	res := healthy()
	res.Checks = []probe.CheckResult{{Name: "gh auth", OK: true}, {Name: "claude", Detail: "not found"}}
	wantAttention(t, Assess(config.Machine{}, signals, now, res, nil), "check failed: claude (not found)")
}

func TestFullDiskNeedsOwner(t *testing.T) {
	res := healthy()
	res.DiskUsedPct = 93
	wantAttention(t, Assess(config.Machine{}, signals, now, res, nil), "disk is 93% full")
}

func TestIssueWithWorkerIsWorking(t *testing.T) {
	res := healthy()
	res.Windows = []probe.Window{window("app-123", time.Minute)}
	r := Assess(config.Machine{}, signals, now, res, []backlog.Issue{issue(123)})
	wantAttention(t, r)
	if r.Issues[0].State != Working || r.Issues[0].Worker != "app-123" {
		t.Fatalf("issue = %+v", r.Issues[0])
	}
}

func TestWorkerMatchNeedsTheWholeNumber(t *testing.T) {
	res := healthy()
	res.Windows = []probe.Window{window("app-1234", time.Minute)}
	r := Assess(config.Machine{}, signals, now, res, []backlog.Issue{issue(123)})
	if r.Issues[0].State != Queued {
		t.Fatalf("issue 123 matched window app-1234: %+v", r.Issues[0])
	}
}

func TestQueuedIssuesWithNoWorkerNeedOwner(t *testing.T) {
	r := Assess(config.Machine{}, signals, now, healthy(), []backlog.Issue{issue(1), issue(2)})
	wantAttention(t, r, "2 issue(s) queued and no worker is running")
}

func TestQueuedIssuesBehindABusyWorkerAreFine(t *testing.T) {
	res := healthy()
	res.Windows = []probe.Window{window("app-1", time.Minute)}
	wantAttention(t, Assess(config.Machine{}, signals, now, res, []backlog.Issue{issue(1), issue(2)}))
}

func TestIssueWaitingOnDecisionNeedsOwner(t *testing.T) {
	res := healthy()
	res.Windows = []probe.Window{window("app-7", 3*time.Hour)}
	r := Assess(config.Machine{}, signals, now, res, []backlog.Issue{issue(7, "## Hand-back\nDecisions needed: 1. which one?")})
	wantAttention(t, r, "org/app#7 waits on you (decision; returns to a worker)")
	if r.Issues[0].State != Waiting {
		t.Fatalf("state = %s", r.Issues[0].State)
	}
}

func TestWaitingIssueSaysHowLongAndWhichPullRequest(t *testing.T) {
	handedBack := issue(7)
	handedBack.OpenPRs = map[int]backlog.PR{41: {}}
	handedBack.Comments = []backlog.Comment{
		{Body: "## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** open /orders", CreatedAt: now.Add(-3 * time.Hour)}}
	r := Assess(config.Machine{}, signals, now, healthy(), []backlog.Issue{handedBack})
	wantAttention(t, r, "org/app#7 waits on you (eyes 3h, review #41 3h): Fix the thing")
}

func TestRenderTotalsWhatWaitsOnTheOwnerByKind(t *testing.T) {
	asked := func(number int, ago time.Duration, body string) backlog.Issue {
		i := issue(number)
		i.Comments = []backlog.Comment{{Body: "## Hand-back\n" + body, CreatedAt: now.Add(-ago)}}
		return i
	}
	issues := []backlog.Issue{
		asked(7, 5*time.Hour, "**PR:** #41 (ready)"),
		asked(8, 40*time.Minute, "**PR:** #42 (ready)\n**Needs eyes:** open /orders"),
		asked(9, 2*time.Hour, "Decisions needed: 1. which one?"),
	}
	var out bytes.Buffer
	// The same issues are in the queue of two machines. They wait once.
	Render(&out, []MachineReport{Assess(config.Machine{Name: "a"}, signals, now, healthy(), issues), Assess(config.Machine{Name: "b"}, signals, now, healthy(), issues)}, nil)
	want := "Waiting on you: 1 return to a worker (oldest 2h), 2 to read (oldest 5h)."
	if !strings.Contains(out.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, out.String())
	}
}

func TestQuietWorkerNeedsOwner(t *testing.T) {
	res := healthy()
	res.Windows = []probe.Window{window("app-9", 2*time.Hour)}
	wantAttention(t, Assess(config.Machine{}, signals, now, res, []backlog.Issue{issue(9)}), "worker app-9 on #9 has been quiet for 2h")
}

func TestTasksWithoutAgentNeedOwner(t *testing.T) {
	res := healthy()
	res.Heartbeat = time.Time{}
	wantAttention(t, Assess(config.Machine{Tasks: []config.Task{nightly}}, signals, now, res, nil), "agent is not running")
}

func TestStaleHeartbeatNeedsOwner(t *testing.T) {
	res := healthy()
	res.Heartbeat = now.Add(-2 * time.Hour)
	res.Runs = []runlog.Record{ran(0, 30*time.Hour)}
	r := Assess(config.Machine{Tasks: []config.Task{nightly}}, signals, now, res, nil)
	wantAttention(t, r, "agent stopped 2h ago: 1 task(s) will not run")
	if !r.Tasks[0].Missed {
		t.Fatal("the task is late and must be marked missed")
	}
}

func TestMachineWithoutTasksNeedsNoAgent(t *testing.T) {
	res := healthy()
	res.Heartbeat = time.Time{}
	wantAttention(t, Assess(config.Machine{}, signals, now, res, nil))
}

func TestTaskThatNeverRanIsNotAnAlarm(t *testing.T) {
	r := Assess(config.Machine{Tasks: []config.Task{nightly}}, signals, now, healthy(), nil)
	wantAttention(t, r)
	if r.Tasks[0].State != TaskNever {
		t.Fatalf("state = %s", r.Tasks[0].State)
	}
}

func TestFailedTaskNeedsOwner(t *testing.T) {
	res := healthy()
	res.Runs = []runlog.Record{ran(0, 30*time.Hour), ran(2, 9*time.Hour)}
	wantAttention(t, Assess(config.Machine{Tasks: []config.Task{nightly}}, signals, now, res, nil), "task nightly failed (exit 2) 8h ago (boom)")
}

func TestMissedTaskNeedsOwner(t *testing.T) {
	res := healthy()
	res.Runs = []runlog.Record{ran(0, 30*time.Hour)}
	wantAttention(t, Assess(config.Machine{Tasks: []config.Task{nightly}}, signals, now, res, nil), "task nightly missed its")
}

func TestTaskOnScheduleIsFine(t *testing.T) {
	res := healthy()
	res.Runs = []runlog.Record{ran(0, 9*time.Hour)}
	r := Assess(config.Machine{Tasks: []config.Task{nightly}}, signals, now, res, nil)
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
		Lister: fakeLister{issues: []backlog.Issue{issue(7, "## Hand-back\nDecisions needed: 1. which one?"), issue(8)}},
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
	Render(&out, []MachineReport{Assess(config.Machine{Name: "box", Host: "me@box"}, signals, now, res, nil)}, nil)
	for _, want := range []string{"box  me@box  load 0.50/8  disk 40%", "FAIL claude", "! check failed: claude", "1 item(s) need you."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestRenderSaysWhenAllIsWell(t *testing.T) {
	var out bytes.Buffer
	Render(&out, []MachineReport{Assess(config.Machine{Name: "box"}, signals, now, healthy(), nil)}, nil)
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
	wantAttention(t, Assess(appWorker, signals, now, res, nil), "agent is not running: 0 task(s) will not run and 1 worker(s) are not supervised")
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
		wantAttention(t, Assess(appWorker, signals, now, res, nil), want)
	}
}

func TestWorkerDirThatTmuxCannotReadNeedsOwner(t *testing.T) {
	res := healthy()
	res.DirProblems = []diraccess.Problem{{Worker: "app", Via: diraccess.ViaTmux, Err: "Operation not permitted"}}
	res.DirHost = diraccess.Host{OS: "Linux", Tmux: "/usr/bin/tmux"}
	wantAttention(t, Assess(appWorker, signals, now, res, nil), "worker app: dir /tmp is not readable by tmux (/usr/bin/tmux) (Operation not permitted)")
}

func TestWorkerThatIsOnTrackNeedsNothing(t *testing.T) {
	for _, verdict := range []string{runlog.VerdictOnTrack, runlog.VerdictNudge, runlog.VerdictDone, runlog.VerdictStarted, runlog.VerdictLimited, runlog.VerdictHeld, runlog.VerdictRecycled} {
		res := healthy()
		res.Checkins = []runlog.Checkin{checkin(runlog.VerdictNeedsOwner, "older"), checkin(verdict, "fine")}
		r := Assess(appWorker, signals, now, res, nil)
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
	r := Assess(appWorker, signals, now, res, []backlog.Issue{issue(7), issue(8)})

	wantAttention(t, r) // no "queued and no worker", and no "quiet" alarm: the supervisor owns both
	if r.Issues[0].State != Working || r.Issues[0].Worker != "app" || r.Issues[1].State != Queued {
		t.Fatalf("issues = %+v", r.Issues)
	}
}

func TestWaitingReviewWithNoPullRequestSaysSo(t *testing.T) {
	r := Assess(config.Machine{}, signals, now, healthy(), []backlog.Issue{issue(7, "## Hand-back\n**PR:** not opened yet, branch pushed")})
	wantAttention(t, r, "org/app#7 waits on you (review (no pull request named)): Fix the thing")
}

func TestQueueBehindAnIdleSupervisedWorkerIsNotAnAlarm(t *testing.T) {
	res := healthy()
	res.Checkins = []runlog.Checkin{checkin(runlog.VerdictOnTrack, "waiting for its first issue")}
	wantAttention(t, Assess(appWorker, signals, now, res, []backlog.Issue{issue(7), issue(8)}))
}

func TestHandedBackIssueWaitsOnOwnerEvenWhileInHand(t *testing.T) {
	res := healthy()
	res.Checkins = []runlog.Checkin{handedOver(7)}
	r := Assess(appWorker, signals, now, res, []backlog.Issue{issue(7, "## Hand-back\n**PR:** #41 (ready)")})
	wantAttention(t, r, "#7 waits on you (review #41)")
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
	Render(&out, []MachineReport{Assess(m, signals, now, res, nil)}, nil)
	for _, want := range []string{"shed:app    claude", "work:notes  2.1.289", "app  #7  nudge  20m ago"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestRenderShowsWhatEachWorkersProcessesUse(t *testing.T) {
	res := healthy()
	res.Checkins = []runlog.Checkin{handedOver(7)}
	res.Usage = map[string]probe.Usage{"app": {CPU: 325.5, MemKB: 7340032, Top: probe.Process{PID: 102, MemKB: 6291456, Command: "/usr/bin/node /repo/node_modules/.bin/vitest run"}}}
	m := config.Machine{Name: "box", Workers: []config.Worker{{Name: "app", Dir: "/tmp", Brief: "b"}, {Name: "web", Dir: "/tmp", Brief: "b"}}}
	var out bytes.Buffer
	Render(&out, []MachineReport{Assess(m, signals, now, res, nil)}, nil)
	if want := "app  #7  cpu 326% 7.0G, most node vitest 6.0G  nudge"; !strings.Contains(out.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, out.String())
	}
}

func TestAgentSessionsOutsideTheFleetAreMarkedUnsupervised(t *testing.T) {
	res := healthy()
	res.Windows = []probe.Window{
		{Session: "shed", Name: "app", Command: "2.1.289", LastActivity: now},
		{Session: "shed", Name: "preflight-app", Command: "2.1.289", LastActivity: now},
		{Session: "work", Name: "notes", Command: "2.1.289", LastActivity: now.Add(-6 * 24 * time.Hour)},
		{Session: "work", Name: "old", Command: "claude", LastActivity: now.Add(-3 * time.Hour)},
		{Session: "work", Name: "edit", Command: "vim", LastActivity: now},
	}
	m := config.Machine{Name: "box", Workers: []config.Worker{{Name: "app", Dir: "/tmp", Brief: "b"}}}
	r := Assess(m, signals, now, res, nil)
	wantAttention(t, r)
	if want := []string{"work:notes", "work:old"}; !slices.Equal(r.Unsupervised, want) {
		t.Fatalf("unsupervised = %q, want %q", r.Unsupervised, want)
	}
	var out bytes.Buffer
	Render(&out, []MachineReport{r}, nil)
	for _, want := range []string{
		"work:notes          2.1.289  active 6d ago  unsupervised",
		"2 unsupervised agent session(s) share this machine's account and CPU with its workers",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if (strings.Contains(line, "shed:app ") || strings.Contains(line, "shed:preflight-app")) && strings.Contains(line, "unsupervised") {
			t.Fatalf("a window of shed's own is marked: %q", line)
		}
	}
}

func TestMachineWithNoSupervisedWorkersHasNoUnsupervisedSessions(t *testing.T) {
	res := healthy()
	res.Windows = []probe.Window{window("notes", time.Hour)}
	if r := Assess(config.Machine{Name: "box"}, signals, now, res, nil); len(r.Unsupervised) != 0 {
		t.Fatalf("unsupervised = %q on a machine where every session is the owner's", r.Unsupervised)
	}
}

func TestIssueWhosePullRequestMergedIsNotQueued(t *testing.T) {
	done := issue(7, "## Hand-back\n**PR:** #41 (ready)")
	done.OpenPRs, done.MergedPRs = map[int]backlog.PR{}, map[int]bool{41: true}
	r := Assess(config.Machine{Name: "box"}, signals, now, healthy(), []backlog.Issue{done})
	wantAttention(t, r) // not "1 issue(s) queued and no worker is running"
	if r.Issues[0].State != Merged {
		t.Fatalf("state = %q, want %q", r.Issues[0].State, Merged)
	}
}

func stalePR(number int, problem string) backlog.Issue {
	i := issue(number, "## Hand-back\n**PR:** #41 (ready)")
	i.OpenPRs = map[int]backlog.PR{41: {Problem: problem}}
	return i
}

func sentBack(issueNumber int, ago time.Duration) runlog.Checkin {
	return runlog.Checkin{Time: now.Add(-ago), Worker: "app", Kind: "queue", Verdict: runlog.VerdictNudge, Sent: true, Issue: issueNumber, Rework: "pull request #41 conflicts with the base branch"}
}

func TestStalePullRequestOnASupervisedMachineIsTheSupervisors(t *testing.T) {
	r := Assess(appWorker, signals, now, healthy(), []backlog.Issue{stalePR(7, "conflicts with the base branch")})
	wantAttention(t, r)
	if r.Issues[0].State != Rework || r.Issues[0].Rework != "pull request #41 conflicts with the base branch" {
		t.Fatalf("issue = %+v", r.Issues[0])
	}
}

func TestStalePullRequestWithNoSupervisorNeedsOwner(t *testing.T) {
	r := Assess(config.Machine{}, signals, now, healthy(), []backlog.Issue{stalePR(7, "has a failed check (integration)")})
	wantAttention(t, r, "org/app#7: pull request #41 has a failed check (integration)")
}

func TestPullRequestStillStaleAfterTheAllowedTriesNeedsOwner(t *testing.T) {
	res := healthy()
	res.Checkins = []runlog.Checkin{sentBack(7, 3*time.Hour), sentBack(7, time.Hour), checkin(runlog.VerdictDone, "nothing left")}
	r := Assess(appWorker, signals, now, res, []backlog.Issue{stalePR(7, "conflicts with the base branch")})
	wantAttention(t, r, "org/app#7: pull request #41 conflicts with the base branch after 2 tries by a worker")
}

func TestOldTriesDoNotCountAgainstAPullRequest(t *testing.T) {
	res := healthy()
	res.Checkins = []runlog.Checkin{sentBack(7, 20*time.Hour), sentBack(7, 19*time.Hour), checkin(runlog.VerdictDone, "nothing left")}
	wantAttention(t, Assess(appWorker, signals, now, res, []backlog.Issue{stalePR(7, "conflicts with the base branch")}))
}

func TestPendingRecycleIsShownAndNeedsNothing(t *testing.T) {
	res := healthy()
	res.Checkins = []runlog.Checkin{handedOver(7)}
	res.Recycle = []string{"app"}
	r := Assess(appWorker, signals, now, res, nil)
	wantAttention(t, r)
	var out bytes.Buffer
	Render(&out, []MachineReport{r}, nil)
	if !r.Workers[0].RecyclePending || !strings.Contains(out.String(), "[fresh session pending]") {
		t.Fatalf("worker = %+v\n%s", r.Workers[0], out.String())
	}
}

type fakeLooks struct {
	report looks.Report
	err    error
	open   []backlog.Issue
}

func (f *fakeLooks) Read(_ context.Context, _ config.Repo, _ []config.IssueSource, open []backlog.Issue, _ []config.Signal) (looks.Report, error) {
	f.open = open
	return f.report, f.err
}

func withRepo(eyeChecks bool) *config.Config {
	cfg := oneMachine()
	cfg.Machines[0].Workers = []config.Worker{{Name: "app", Dir: "/tmp", Brief: "b", EyeChecks: eyeChecks}}
	cfg.Repos = []config.Repo{{Name: "org/app", Staging: config.Environment{Commit: "true"}}}
	return cfg
}

func releaseState() looks.Report {
	asked := func(number, pr int, state string, ago time.Duration) looks.Look {
		return looks.Look{Repo: "org/app", Number: number, Title: "Fix the thing", PR: pr, State: state, Since: now.Add(-ago)}
	}
	return looks.Report{Repo: "org/app", Staging: "aaaaaaa1111", Production: "bbbbbbb2222", Since: now.Add(-48 * time.Hour), SinceProduction: true, Merged: 14,
		Looks: []looks.Look{asked(1, 41, looks.Due, 5*time.Hour), asked(2, 42, looks.Awaiting, 2*time.Hour), asked(4, 44, looks.Failed, time.Hour)}}
}

func TestRenderGivesTheGoOrNoGoForAProductionDeploy(t *testing.T) {
	var out bytes.Buffer
	Render(&out, nil, []RepoReport{{Report: releaseState(), Lookers: 1, Now: now}})
	for _, want := range []string{
		"org/app  staging aaaaaaa  production bbbbbbb",
		"14 pull request(s) merged since production's commit, 2d ago",
		"eye checks: 2 not done (1 on staging, 1 wait for a staging deploy), 1 failed",
		"#1  PR #41  on staging         5h  Fix the thing",
		"#2  PR #42  waits for staging  2h  Fix the thing",
		"#4  PR #44  FAILED             1h  Fix the thing",
		"All machines are on track.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestLooksThatAreDueAreTheOwnersWhenNoWorkerDoesThem(t *testing.T) {
	reader := &fakeLooks{report: releaseState()}
	c := Collector{Lister: fakeLister{issues: []backlog.Issue{issue(7)}}, Looks: reader}

	withBrowser := c.CollectRepos(context.Background(), withRepo(true))
	if NeedsOwner(nil, withBrowser) || len(reader.open) != 1 {
		t.Fatalf("attention = %q with a worker that does eye checks (open issues passed on: %d)", withBrowser[0].Attention(), len(reader.open))
	}
	without := c.CollectRepos(context.Background(), withRepo(false))
	if got := without[0].Attention(); len(got) != 1 || !strings.Contains(got[0], "1 eye check(s) are due on staging and no worker does eye checks") {
		t.Fatalf("attention = %q", got)
	}
}

func TestReleaseStateThatCannotBeReadNeedsOwner(t *testing.T) {
	c := Collector{Lister: fakeLister{}, Looks: &fakeLooks{err: errors.New("org/app: read staging's commit: exit status 7")}}
	repos := c.CollectRepos(context.Background(), withRepo(true))
	var out bytes.Buffer
	Render(&out, nil, repos)
	if !NeedsOwner(nil, repos) || !strings.Contains(out.String(), "! cannot read the release state: org/app: read staging's commit: exit status 7") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestStagingThatCannotBeAskedStillGivesTheEyeChecks(t *testing.T) {
	state := releaseState()
	state.Unchecked = "gh api repos/org/app/compare/a...b: timed out at shed's deadline after 45s"
	repos := []RepoReport{{Report: state, Lookers: 1, Now: now}}
	var out bytes.Buffer
	Render(&out, nil, repos)
	for _, want := range []string{
		"eye checks: 2 not done (1 on staging, 1 wait for a staging deploy), 1 failed",
		"! cannot tell which merged work staging serves, so none is counted as on staging: gh api repos/org/app/compare/a...b: timed out",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if !NeedsOwner(nil, repos) {
		t.Fatal("a release state read in part does not need the owner")
	}
}

func TestJSONReportCarriesTheMachinesAndTheReleaseState(t *testing.T) {
	report := Report{
		Machines: []MachineReport{Assess(config.Machine{Name: "box"}, signals, now, healthy(), nil)},
		Repos:    []RepoReport{{Report: releaseState(), Lookers: 1, Now: now}},
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Machines []struct{ Name string }
		Repos    []struct {
			Repo, Staging, Production string
			Merged                    int
			Looks                     []struct {
				Number, PR int
				State      string
			}
		}
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Machines) != 1 || got.Machines[0].Name != "box" {
		t.Fatalf("machines = %+v in %s", got.Machines, data)
	}
	if r := got.Repos[0]; r.Repo != "org/app" || r.Staging != "aaaaaaa1111" || r.Merged != 14 || len(r.Looks) != 3 || r.Looks[0].State != looks.Due || r.Looks[0].PR != 41 {
		t.Fatalf("repos = %+v in %s", got.Repos, data)
	}
}

func TestStatusShowsWhatEachWorkersToolReports(t *testing.T) {
	m := config.Machine{Name: "box", Workers: []config.Worker{
		{Name: "app", Dir: "/tmp", Brief: "b"}, {Name: "docs", Dir: "/tmp", Brief: "b"}, {Name: "ops", Dir: "/tmp", Brief: "b"}, {Name: "old", Dir: "/tmp", Brief: "b"}}}
	res := healthy()
	shedWindow := func(name string, quietFor time.Duration) probe.Window {
		return probe.Window{Session: "shed", Name: name, Command: "claude", LastActivity: now.Add(-quietFor)}
	}
	res.Windows = []probe.Window{shedWindow("app", 20*time.Minute), shedWindow("docs", 10*time.Second), shedWindow("ops", 4*time.Minute), shedWindow("old", time.Hour)}
	res.Marks = map[string]runlog.Mark{
		"app":  {Time: now.Add(-20 * time.Minute), State: runlog.MarkWaiting, Detail: "Allow Bash?"},
		"docs": {Time: now.Add(-20 * time.Minute), State: runlog.MarkWaiting}, // answered: its screen moved on
		"ops":  {Time: now.Add(-4 * time.Minute), State: runlog.MarkIdle},
	}
	for _, name := range []string{"app", "docs", "ops", "old"} {
		c := checkin(runlog.VerdictOnTrack, "fine")
		c.Worker = name
		res.Checkins = append(res.Checkins, c)
	}

	r := Assess(m, signals, now, res, nil)
	got := map[string]string{}
	for _, w := range r.Workers {
		got[w.Name] = w.Tool + "|" + w.State
	}
	want := map[string]string{"app": "claude|waiting", "docs": "claude|working", "ops": "claude|idle", "old": "claude|"}
	for name, state := range want {
		if got[name] != state {
			t.Errorf("worker %s = %q, want %q", name, got[name], state)
		}
	}

	var out bytes.Buffer
	Render(&out, []MachineReport{r}, nil)
	for _, line := range []string{
		"app   no issue  waiting 20m  on_track  5m ago  fine",
		"docs  no issue  working      on_track  5m ago  fine",
		"ops   no issue  idle 4m      on_track  5m ago  fine",
		"old   no issue  -            on_track  5m ago  fine",
	} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("output lacks %q:\n%s", line, out.String())
		}
	}
}

func TestLookAWorkerWillDoIsNotTheOwnersBacklog(t *testing.T) {
	// Merged work that asks for eyes: the pull request is closed, the ask is open.
	merged := issue(7, "## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** open /orders")
	merged.OpenPRs = map[int]backlog.PR{}
	state := looks.Report{Repo: "org/app", Looks: []looks.Look{{Repo: "org/app", Number: 7, PR: 41, State: looks.Due}}}

	for _, tc := range []struct {
		name          string
		eyeChecks     bool
		wantAttention []string
		wantState     string
		wantTotals    string
	}{
		{"a worker does eye checks", true, nil, Look, ""},
		{"no worker does them", false, []string{"org/app#7 waits on you (eyes)"}, Waiting, "Waiting on you: 1 to read."},
	} {
		c := Collector{Runner: fakeRunner{}, Lister: fakeLister{issues: []backlog.Issue{merged}}, Looks: &fakeLooks{report: state}}
		report := c.CollectAll(context.Background(), withRepo(tc.eyeChecks))
		machine := report.Machines[0]
		var mine []string
		for _, a := range machine.Attention {
			if strings.Contains(a, "#7") {
				mine = append(mine, a)
			}
		}
		if len(mine) != len(tc.wantAttention) || (len(mine) == 1 && !strings.Contains(mine[0], tc.wantAttention[0])) {
			t.Errorf("%s: attention = %q, want %q", tc.name, mine, tc.wantAttention)
		}
		if machine.Issues[0].State != tc.wantState {
			t.Errorf("%s: state = %q, want %q", tc.name, machine.Issues[0].State, tc.wantState)
		}
		if got := waitingTotals(report.Machines); got != tc.wantTotals {
			t.Errorf("%s: totals = %q, want %q", tc.name, got, tc.wantTotals)
		}
	}
}

type fakeMerged struct {
	pulls map[int]looks.Pull
	since time.Time
	err   error
}

func (f *fakeMerged) MergedSince(_ context.Context, _ string, since time.Time) (map[int]looks.Pull, error) {
	f.since = since
	return f.pulls, f.err
}

func TestDigestSaysWhatMergedHowTheLooksWentAndWhatWaits(t *testing.T) {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	state := releaseState()
	state.Passed = []looks.Look{
		{Repo: "org/app", Number: 3, PR: 43, State: looks.Passed, Since: ago(3 * time.Hour), Title: "Show the total"},
		{Repo: "org/app", Number: 2, PR: 40, State: looks.Passed, Since: ago(40 * time.Hour), Title: "Looked at before this window"},
	}
	review := issue(8)
	review.OpenPRs = map[int]backlog.PR{52: {}}
	review.Comments = []backlog.Comment{{Body: "## Hand-back\n**PR:** #52 (ready)", CreatedAt: ago(2 * time.Hour)}}
	decision := issue(9)
	decision.Comments = []backlog.Comment{{Body: "## Hand-back\nDecisions needed: 1. which one?", CreatedAt: ago(30 * time.Hour)}}

	merged := &fakeMerged{pulls: map[int]looks.Pull{
		51: {Number: 51, Title: "Paginate the audit log", MergedAt: ago(time.Hour)},
		50: {Number: 50, Title: "Retry the export", MergedAt: ago(5 * time.Hour)},
	}}
	c := Collector{Runner: fakeRunner{err: errors.New("connection timed out")}, Lister: fakeLister{issues: []backlog.Issue{review, decision}}, Looks: &fakeLooks{report: state}}
	since := ago(24 * time.Hour)
	d := c.Digest(context.Background(), withRepo(true), merged, since, now)
	if !merged.since.Equal(since) {
		t.Fatalf("merged pull requests were asked for since %s, want %s", merged.since, since)
	}
	// The collector stamps the report with the real clock; the test's
	// comments are dated by the test's.
	var out bytes.Buffer
	RenderDigest(&out, d)

	for _, want := range []string{
		"shed digest: the last 24h",
		"Merged: 2 pull request(s)",
		"org/app#50  5h ago  Retry the export\n  org/app#51  1h ago  Paginate the audit log",
		"Eye checks: org/app",
		"1 passed in this window; now 2 not done (1 on staging, 1 wait for a staging deploy), 1 failed",
		"#3  PR #43  passed  3h ago  Show the total",
		"#4  PR #44  FAILED  1h ago  Fix the thing",
		"Waiting on you: 2",
		"  returns to a worker when you answer\n    30h  decision  org/app#9  Fix the thing\n  to read\n    2h  review #52  org/app#8  Fix the thing",
		"Also needs you: 1\n  ! box: unreachable: connection timed out",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("digest lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "Looked at before this window") {
		t.Fatalf("a look that passed before the window is in the digest:\n%s", out.String())
	}
}

func TestDigestSaysWhatItCouldNotRead(t *testing.T) {
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Looks: &fakeLooks{}}
	d := c.Digest(context.Background(), oneMachine(), &fakeMerged{err: errors.New("gh: not logged in")}, now.Add(-time.Hour), now)
	var out bytes.Buffer
	RenderDigest(&out, d)
	if !strings.Contains(out.String(), "! gh: not logged in") || !strings.Contains(out.String(), "Merged: 0 pull request(s)") {
		t.Fatalf("digest:\n%s", out.String())
	}
}

func TestDigestSaysWhenStagingCouldNotBeAsked(t *testing.T) {
	state := releaseState()
	state.Unchecked = "gh api compare: timed out at shed's deadline after 45s"
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Looks: &fakeLooks{report: state}}
	d := c.Digest(context.Background(), withRepo(true), &fakeMerged{}, now.Add(-time.Hour), now)
	var out bytes.Buffer
	RenderDigest(&out, d)
	if want := "! cannot tell which merged work staging serves: gh api compare: timed out"; !strings.Contains(out.String(), want) {
		t.Fatalf("digest lacks %q:\n%s", want, out.String())
	}
}

func TestStatusShowsWhereEachOneOffTaskStands(t *testing.T) {
	res := healthy()
	handed := func(worker, task string, ago time.Duration) runlog.Checkin {
		return runlog.Checkin{Time: now.Add(-ago), Worker: worker, Kind: "task", Verdict: runlog.VerdictNudge, Sent: true, Task: task, Reason: "the owner pushed a one-off task"}
	}
	res.Checkins = []runlog.Checkin{
		handed("app", "t-old", 3*time.Hour),
		{Time: now.Add(-2 * time.Hour), Worker: "app", Kind: "idle", Verdict: runlog.VerdictDone, Reason: "it wrote its report"},
		handed("app", "t-now", 10*time.Minute),
	}
	res.Tasks = []probe.Task{
		{Where: runlog.TaskTaken, ID: "t-old"}, {Where: runlog.TaskTaken, ID: "t-now"}, {Where: runlog.TaskTaken, ID: "t-forgotten"},
		{Where: "app", ID: "t-next"}, {Where: runlog.TaskAny, ID: "t-any"},
	}
	r := Assess(appWorker, signals, now, res, nil)
	var out bytes.Buffer
	Render(&out, []MachineReport{r}, nil)
	for _, want := range []string{
		"app  task t-now  nudge  10m ago  the owner pushed a one-off task",
		"one-off tasks\n    t-old   finished  app\n    t-now   in hand   app\n    t-next  queued    app\n    t-any   queued    any worker\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "t-forgotten") {
		t.Fatalf("a task the log no longer knows is listed:\n%s", out.String())
	}
}

func TestBlockedLookIsCountedAsNotDoneAndNeedsOwner(t *testing.T) {
	state := releaseState()
	state.Looks = append(state.Looks, looks.Look{Repo: "org/app", Number: 5, Title: "Fix the thing", PR: 45, State: looks.Blocked,
		Since: now.Add(-30 * time.Minute), Note: "staging asks for a sign-in"})
	repos := []RepoReport{{Report: state, Lookers: 1, Now: now}}
	var out bytes.Buffer
	Render(&out, nil, repos)
	for _, want := range []string{
		"eye checks: 3 not done (1 on staging, 1 wait for a staging deploy, 1 blocked), 1 failed",
		"#5  PR #45  BLOCKED",
		"! the eye check of org/app#5 (pull request #45) is blocked: staging asks for a sign-in",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if !NeedsOwner(nil, repos) {
		t.Fatal("a blocked look is the owner's to clear")
	}
}

func TestBumpedIssueIsMarkedAndNeedsNothing(t *testing.T) {
	res := healthy()
	res.Checkins = []runlog.Checkin{checkin(runlog.VerdictOnTrack, "working")}
	res.Bumps = map[int]string{7: "", 8: "app", 9: "renamed-away"}
	r := Assess(appWorker, signals, now, res, []backlog.Issue{issue(7), issue(8), issue(9), issue(10)})
	wantAttention(t, r)

	if i := r.Issues; !i[0].Bumped || i[0].BumpedTo != "" || i[1].BumpedTo != "app" || !i[2].Bumped || i[2].BumpedTo != "" || i[3].Bumped {
		t.Fatalf("issues = %+v", i)
	}
	var out bytes.Buffer
	Render(&out, []MachineReport{r}, nil)
	if got := out.String(); strings.Count(got, "(bumped)") != 2 || strings.Count(got, "(bumped to app)") != 1 {
		t.Fatalf("report:\n%s", got)
	}
}

func TestPausedMachineSaysSoAndNeedsNothing(t *testing.T) {
	res := healthy()
	res.PausedUntil = now.Add(22 * time.Minute)
	r := Assess(config.Machine{Name: "box"}, signals, now, res, nil)
	wantAttention(t, r)
	var out bytes.Buffer
	Render(&out, []MachineReport{r}, nil)
	if !strings.Contains(out.String(), "paused   for another 22m: nothing is handed out and no worker is nudged") {
		t.Fatalf("report:\n%s", out.String())
	}
}

func limitedReport(t *testing.T, quietFor time.Duration) string {
	t.Helper()
	res := healthy()
	c := checkin(runlog.VerdictLimited, "the screen says the limit resets Oct 7 at 6pm")
	c.Until = now.Add(4 * time.Hour)
	res.Checkins = []runlog.Checkin{c}
	win := window("app", quietFor)
	win.Session = workerSession
	res.Windows = []probe.Window{win}
	machine := appWorker
	machine.Name = "box"
	var out bytes.Buffer
	Render(&out, []MachineReport{Assess(machine, signals, now, res, nil)}, nil)
	return out.String()
}

func TestLimitedWorkerShowsHowLongItIsHeldAndHowToEndIt(t *testing.T) {
	out := limitedReport(t, 10*time.Minute)
	for _, want := range []string{"held for up to another 4h", "last active 10m ago", "shed resume box app", "resets Oct 7 at 6pm"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

func weeklyLimited(worker string) runlog.Checkin {
	c := checkin(runlog.VerdictLimited, "the screen says the weekly limit resets Wed at 6pm")
	c.Worker, c.Until, c.Limit, c.Resets = worker, now.Add(6*time.Hour), runlog.LimitWeekly, now.Add(46*time.Hour)
	return c
}

func TestLimitedWorkerSaysWhichLimitAndWhenItResets(t *testing.T) {
	res := healthy()
	res.Checkins = []runlog.Checkin{weeklyLimited("app")}
	var out bytes.Buffer
	Render(&out, []MachineReport{Assess(appWorker, signals, now, res, nil)}, nil)
	if want := "weekly limit, resets " + now.Add(46*time.Hour).Format("Mon 15:04") + " (in 46h), held for up to another 6h"; !strings.Contains(out.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, out.String())
	}
}

func TestMachineWithEveryWorkerAtItsWeeklyLimitNeedsTheOwner(t *testing.T) {
	two := config.Machine{Name: "box", Workers: []config.Worker{{Name: "app", Dir: "/tmp", Brief: "b"}, {Name: "api", Dir: "/tmp", Brief: "b"}}}
	res := healthy()
	fiveHour := weeklyLimited("api")
	fiveHour.Limit = runlog.LimitFiveHour
	res.Checkins = []runlog.Checkin{weeklyLimited("app"), fiveHour}
	wantAttention(t, Assess(two, signals, now, res, nil))

	res.Checkins = []runlog.Checkin{weeklyLimited("app"), weeklyLimited("api")}
	wantAttention(t, Assess(two, signals, now, res, nil),
		"every worker is at its weekly limit: switch the machine's account or wait until "+now.Add(46*time.Hour).Format("Mon 15:04"))

	// A worker that only answers questions does not keep the machine busy.
	two.Workers[1].Ask = true
	res.Checkins = []runlog.Checkin{weeklyLimited("app")}
	wantAttention(t, Assess(two, signals, now, res, nil), "every worker is at its weekly limit")
}

func TestLimitedWorkerThatWorkedSinceDoesNotShowTheOldReason(t *testing.T) {
	out := limitedReport(t, 2*time.Minute) // the limit was seen 5 minutes ago
	if !strings.Contains(out, "but active 2m ago, after the limit was seen") || strings.Contains(out, "resets Oct 7") {
		t.Fatalf("a reason older than the worker's last output reads as if it were current:\n%s", out)
	}
}

func TestChoicesAWorkerMadeAreShownAndAskNothing(t *testing.T) {
	handedBack := issue(7, "## Hand-back\n**PR:** none\n**Decided without you:**\n- Kept the old column; revert abc123 to drop it.\n**Decisions needed:** none")
	r := Assess(config.Machine{Name: "box"}, signals, now, healthy(), []backlog.Issue{handedBack})
	wantAttention(t, r, "1 issue(s) queued and no worker is running")
	var out bytes.Buffer
	Render(&out, []MachineReport{r}, nil)
	if want := "org/app#7 decided without you: Kept the old column; revert abc123 to drop it."; !strings.Contains(out.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, out.String())
	}

	out.Reset()
	RenderDigest(&out, Digest{Since: now.Add(-time.Hour), Now: now, Report: Report{Machines: []MachineReport{r, r}}})
	if want := "Decided without you: 1\n  org/app#7  Kept the old column"; !strings.Contains(out.String(), want) {
		t.Fatalf("digest lacks %q:\n%s", want, out.String())
	}
}

func TestDraftOnPurposeIsShownAndAsksNothing(t *testing.T) {
	draft := issue(7, "## Hand-back\n**PR:** #41 (draft: waits for #40 to merge)")
	draft.OpenPRs = map[int]backlog.PR{41: {Draft: true}}
	r := Assess(config.Machine{Name: "box"}, signals, now, healthy(), []backlog.Issue{draft})
	wantAttention(t, r)
	if r.Issues[0].State != Draft || len(r.Issues[0].Asks) != 0 {
		t.Fatalf("issue = %+v, want a draft that asks nothing", r.Issues[0])
	}
	var out bytes.Buffer
	Render(&out, []MachineReport{r}, nil)
	if want := "(draft #41 (waits for #40 to merge))"; !strings.Contains(out.String(), want) || strings.Contains(out.String(), "Waiting on you") {
		t.Fatalf("output lacks %q, or counts the draft as waiting:\n%s", want, out.String())
	}
}

func TestStatusSaysInOneLineHowManyWorkersHaveNoWorkAndWhy(t *testing.T) {
	m := config.Machine{Name: "box", Workers: []config.Worker{{Name: "app"}, {Name: "docs"}, {Name: "api"}}}
	res := healthy()
	done := func(worker string) runlog.Checkin {
		return runlog.Checkin{Time: now.Add(-time.Hour), Worker: worker, Kind: "idle", Verdict: runlog.VerdictDone, Reason: "nothing workable"}
	}
	res.Checkins = []runlog.Checkin{done("app"), done("docs"), {Time: now.Add(-time.Minute), Worker: "api", Kind: "scope", Verdict: runlog.VerdictOnTrack}}
	asked := issue(7)
	asked.Comments = []backlog.Comment{{Body: "## Hand-back\nDecisions needed: 1. which one?", CreatedAt: now.Add(-27 * time.Hour)}}

	r := Assess(m, signals, now, res, []backlog.Issue{asked})
	wantAttention(t, r, "2 of 3 workers have no work: every queued issue waits on you (oldest 27h)", "org/app#7 waits on you")

	// A queue that is simply empty is not the owner's backlog.
	wantAttention(t, Assess(m, signals, now, res, nil))
}

func TestDigestTotalsIdleWorkerHoursByCause(t *testing.T) {
	at := func(ago time.Duration, worker, kind, verdict string) runlog.Checkin {
		return runlog.Checkin{Time: now.Add(-ago), Worker: worker, Kind: kind, Verdict: verdict}
	}
	idle := runlog.IdleByCause([]runlog.Checkin{
		at(30*time.Hour, "app", "idle", runlog.VerdictDone), // only the 24 hours of the window count
		at(4*time.Hour, "docs", "idle", runlog.VerdictNudge),
		at(3*time.Hour, "docs", "idle", runlog.VerdictToolError),
		at(2*time.Hour, "docs", "retry", runlog.VerdictNudge),
		at(time.Hour, "docs", "idle", runlog.VerdictNeedsOwner),
	}, now.Add(-24*time.Hour), now)
	want := map[string]time.Duration{runlog.IdleNoWork: 24 * time.Hour, runlog.IdleToolError: time.Hour, runlog.IdleOwner: time.Hour}
	if !maps.Equal(idle, want) {
		t.Fatalf("idle = %v, want %v", idle, want)
	}

	var out bytes.Buffer
	RenderDigest(&out, Digest{Since: now.Add(-24 * time.Hour), Now: now, IdleHours: map[string]float64{runlog.IdleNoWork: 24, runlog.IdleToolError: 1, runlog.IdleOwner: 1}})
	if want := "Idle worker-hours: no work 24.0h, tool error 1.0h, waits on you 1.0h\n"; !strings.Contains(out.String(), want) {
		t.Fatalf("digest lacks %q:\n%s", want, out.String())
	}
}

func TestAskWorkerIsShownAsOneAndIsNotCountedAsIdle(t *testing.T) {
	m := config.Machine{Name: "box", Workers: []config.Worker{{Name: "app"}, {Name: "ask", Ask: true}}}
	res := healthy()
	res.Checkins = []runlog.Checkin{
		{Time: now.Add(-time.Hour), Worker: "app", Kind: "idle", Verdict: runlog.VerdictDone, Reason: "nothing workable"},
		{Time: now.Add(-3 * time.Hour), Worker: "ask", Kind: "start", Verdict: runlog.VerdictStarted, Sent: true},
	}
	asked := issue(7)
	asked.Comments = []backlog.Comment{{Body: "## Hand-back\nDecisions needed: 1. which one?", CreatedAt: now.Add(-27 * time.Hour)}}

	r := Assess(m, signals, now, res, []backlog.Issue{asked})
	wantAttention(t, r, "1 of 1 workers have no work: every queued issue waits on you (oldest 27h)", "org/app#7 waits on you")
	var out bytes.Buffer
	Render(&out, []MachineReport{r}, nil)
	if want := "ask  answers questions  started  3h ago"; !strings.Contains(out.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, out.String())
	}
}

type fakeUsage struct {
	byMachine map[string]runlog.UsageSummary
	err       error
}

func (f fakeUsage) ReadUsage(_ context.Context, m config.Machine, _ time.Time) (runlog.UsageSummary, error) {
	return f.byMachine[m.Name], f.err
}

func TestDigestTotalsUsageByWorkerAndModelAndPricesTheMergedWork(t *testing.T) {
	summary := runlog.UsageSummary{
		Workers: []runlog.WorkerUsage{{Worker: "app", Dollars: 14.5, Models: map[string]runlog.ModelTokens{
			"opus-5-5":   {Input: 1_200_000, CacheWrite: 300_000, CacheRead: 15_700_000, Output: 52_000, Dollars: 12.25, Priced: true},
			"sonnet-5-5": {Input: 100_000, Output: 10_000, Dollars: 2.25, Priced: true},
		}}},
		PRs: []runlog.PRCost{
			{Repo: "org/app", PR: 51, Dollars: 9.2},
			{Repo: "org/app", PR: 50, Dollars: 1.1},
			{Repo: "org/app", PR: 50, Worker: "docs", Dollars: 0.9}, // a rework by another worker
			{Repo: "org/app", PR: 7, Dollars: 99},                   // not merged in the window
		},
	}
	merged := &fakeMerged{pulls: map[int]looks.Pull{
		51: {Number: 51, Title: "Paginate the audit log", MergedAt: now.Add(-time.Hour)},
		50: {Number: 50, Title: "Retry the export", MergedAt: now.Add(-5 * time.Hour)},
		49: {Number: 49, Title: "Merged by hand", MergedAt: now.Add(-6 * time.Hour)},
	}}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Looks: &fakeLooks{}, Usage: fakeUsage{byMachine: map[string]runlog.UsageSummary{"box": summary}}}
	d := c.Digest(context.Background(), withRepo(true), merged, now.Add(-24*time.Hour), now)
	var out bytes.Buffer
	RenderDigest(&out, d)
	for _, want := range []string{
		"Usage at API rates: $14.50",
		"box  app  opus-5-5    in 1.2M  write 300k  read 15.7M  out 52k  $12.25",
		"box  app  sonnet-5-5  in 100k  write 0     read 0      out 10k  $2.25",
		"by model: opus-5-5 $12.25, sonnet-5-5 $2.25",
		"Merged pull requests with a cost: 2 of 3, median $5.60",
		"org/app#51  $9.20  Paginate the audit log",
		"org/app#50  $2.00  Retry the export",
		"The reviewer's own calls are not counted: its command must print JSON",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("digest lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "$99") {
		t.Fatalf("a pull request merged outside the window was counted:\n%s", out.String())
	}
}

func TestDigestSaysWhichMachineItCouldNotReadUsageFrom(t *testing.T) {
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Looks: &fakeLooks{}, Usage: fakeUsage{err: errors.New("unknown command \"usage\"")}}
	d := c.Digest(context.Background(), withRepo(true), &fakeMerged{}, now.Add(-time.Hour), now)
	var out bytes.Buffer
	RenderDigest(&out, d)
	if want := "usage: unknown command \"usage\""; !strings.Contains(out.String(), want) {
		t.Fatalf("digest lacks %q:\n%s", want, out.String())
	}
}

func TestStatusNamesEachMachinesAccountAndWhichShareOne(t *testing.T) {
	report := func(name, account string) MachineReport {
		res := healthy()
		res.Account = account
		return Assess(config.Machine{Name: name}, signals, now, res, nil)
	}
	var out bytes.Buffer
	Render(&out, []MachineReport{report("box", "a@example.com"), report("mini", "a@example.com"), report("pc", "b@example.com")}, nil)
	for _, want := range []string{
		"account  a@example.com  (one usage limit with mini)",
		"account  a@example.com  (one usage limit with box)",
		"account  b@example.com\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestDigestCountsTheReviewerAsItsOwnLine(t *testing.T) {
	summary := runlog.UsageSummary{Workers: []runlog.WorkerUsage{{Worker: runlog.ReviewerLedger, Dollars: 0.5,
		Models: map[string]runlog.ModelTokens{"sonnet-5-5": {Output: 50_000, Dollars: 0.5, Priced: true}}}}}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Looks: &fakeLooks{}, Usage: fakeUsage{byMachine: map[string]runlog.UsageSummary{"box": summary}}}
	d := c.Digest(context.Background(), withRepo(true), &fakeMerged{}, now.Add(-time.Hour), now)
	var out bytes.Buffer
	RenderDigest(&out, d)
	if !strings.Contains(out.String(), "box  (reviewer)  sonnet-5-5") || strings.Contains(out.String(), "not counted") {
		t.Fatalf("digest:\n%s", out.String())
	}
}

func TestStatusShowsEachMachinesPaceAndEachAccountsTotal(t *testing.T) {
	report := func(name string, spend, line float64, state string) MachineReport {
		res := healthy()
		res.Pace = &runlog.Pace{Account: "max", Budget: 400, Share: 200, Spend: spend, Line: line, State: state}
		return Assess(config.Machine{Name: name}, signals, now, res, nil)
	}
	var out bytes.Buffer
	Render(&out, []MachineReport{report("box", 120, 100, runlog.PaceAhead), report("mini", 60, 100, runlog.PaceOn)}, nil)
	for _, want := range []string{
		"pace     $120.00 of a $200.00 share this week, pace line $100.00: ahead, only priority work is handed out",
		"pace     $60.00 of a $200.00 share this week, pace line $100.00: on pace",
		"Account max: $180.00 of $400.00 this week, pace line $200.00 (box, mini)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestStatusShowsTheLastGateRunOfEachPullRequest(t *testing.T) {
	res := healthy()
	gate := func(pr int, commit string, exit int, endedAgo time.Duration) runlog.Gate {
		end := now.Add(-endedAgo)
		return runlog.Gate{Repo: "org/app", PR: pr, Commit: commit, Worker: "app", Start: end.Add(-2 * time.Minute), End: end, ExitCode: exit}
	}
	res.Gates = []runlog.Gate{gate(41, "aaa1111aaa", 1, time.Hour), gate(42, "bbb2222bbb", 0, 30*time.Minute), gate(41, "ccc3333ccc", 0, 5*time.Minute)}
	var out bytes.Buffer
	Render(&out, []MachineReport{Assess(config.Machine{Name: "box"}, signals, now, res, nil)}, nil)
	for _, want := range []string{"  gates\n", "org/app#41  ccc3333  passed  5m ago, took 2m", "org/app#42  bbb2222  passed  30m ago, took 2m"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "aaa1111") {
		t.Fatalf("an older run of #41 is shown:\n%s", out.String())
	}
}

func TestDigestSplitsTheSpendByKindAndFlagsALargeCacheRatio(t *testing.T) {
	summary := runlog.UsageSummary{
		Workers: []runlog.WorkerUsage{
			{Worker: "app", Dollars: 40, Models: map[string]runlog.ModelTokens{"opus-5-5": {CacheRead: 31_000_000, Output: 100_000, Dollars: 40, Priced: true}}},
			{Worker: "docs", Dollars: 2, Models: map[string]runlog.ModelTokens{"haiku-5-5": {CacheRead: 1_200_000, Output: 10_000, Dollars: 2, Priced: true}}},
		},
		Kinds: []runlog.KindSpend{
			{Kind: runlog.KindPR, Items: []float64{4, 10, 3}},
			{Kind: runlog.KindLook, Items: []float64{1, 2}},
			{Kind: runlog.KindNoItem, Items: []float64{22}},
		},
	}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Looks: &fakeLooks{}, Usage: fakeUsage{byMachine: map[string]runlog.UsageSummary{"box": summary}}}
	d := c.Digest(context.Background(), withRepo(true), &fakeMerged{}, now.Add(-24*time.Hour), now)
	var out bytes.Buffer
	RenderDigest(&out, d)
	for _, want := range []string{
		"Spend by kind of work:",
		"pull request  $17.00  40%  3 item(s), median $4.00",
		"eye check     $3.00   7%   2 item(s), median $1.50",
		"no item       $22.00  52%  nothing in hand",
		"Cache reads per output token: app 310 (high), docs 120",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("digest lacks %q:\n%s", want, out.String())
		}
	}
}

func TestStatusShowsTheLastStagingDeployAndAFailureNeedsTheOwner(t *testing.T) {
	res := healthy()
	res.Deploys = []runlog.Deploy{
		{Repo: "org/app", Start: now.Add(-5 * time.Hour), Before: "aaa1111", Result: runlog.DeployStarted},
		{Repo: "org/app", Start: now.Add(-5 * time.Hour), End: now.Add(-4 * time.Hour), Before: "aaa1111", After: "bbb2222", Result: runlog.DeployServed},
		{Repo: "org/api", Start: now.Add(-time.Hour), End: now.Add(-time.Hour), ExitCode: 1, Result: runlog.DeployFailed},
	}
	r := Assess(config.Machine{Name: "box"}, signals, now, res, nil)
	wantAttention(t, r, "the staging deploy of org/api failed (exit 1) 1h ago")
	var out bytes.Buffer
	Render(&out, []MachineReport{r}, nil)
	for _, want := range []string{"  staging deploys\n", "org/app  served bbb2222   5h ago", "org/api  failed (exit 1)  1h ago"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status lacks %q:\n%s", want, out.String())
		}
	}
}

func TestStatusShowsWhyAQueuedIssueIsHeld(t *testing.T) {
	deps := issue(11, "")
	deps.Deps = []backlog.Dep{{Repo: "org/app", Number: 9, Open: true}}
	inHand := issue(12, "")
	inHand.Labels = []backlog.Label{{Name: "area:billing"}}
	same := issue(13, "")
	same.Labels = []backlog.Label{{Name: "area:billing"}}
	free := issue(14, "")
	issues := []backlog.Issue{deps, inHand, same, free}
	report := MachineReport{Issues: []IssueStatus{
		{Repo: "org/app", Number: 11, State: Queued}, {Repo: "org/app", Number: 12, State: Working, Worker: "docs"},
		{Repo: "org/app", Number: 13, State: Queued}, {Repo: "org/app", Number: 14, State: Queued},
	}}
	MarkHeld(&report, issues, "area:")
	want := []struct{ state, held string }{
		{Held, "waits for #9 to close"}, {Working, ""}, {Held, "overlaps #12 in hand: both are area:billing"}, {Queued, ""},
	}
	for n, w := range want {
		if got := report.Issues[n]; got.State != w.state || got.Held != w.held {
			t.Errorf("#%d = %s %q, want %s %q", got.Number, got.State, got.Held, w.state, w.held)
		}
	}
}

func TestAPullRequestWaitingForItsVerificationIsNotTheOwners(t *testing.T) {
	i := issue(12, "## Hand-back\n**PR:** #41 (ready)")
	i.OpenPRs = map[int]backlog.PR{41: {}}
	i.Verify = true
	r := Assess(config.Machine{Name: "box", Workers: []config.Worker{{Name: "app", Dir: "/tmp", Brief: "b"}}}, signals, now, healthy(), []backlog.Issue{i})
	wantAttention(t, r)
	if st := r.Issues[0]; st.State != Verifying || st.Held != "pull request #41 waits for a verifier" {
		t.Fatalf("issue = %+v", st)
	}
	i.Comments = append(i.Comments, backlog.Comment{Body: "Verified (low): ran the tests"})
	r = Assess(config.Machine{Name: "box", Workers: []config.Worker{{Name: "app", Dir: "/tmp", Brief: "b"}}}, signals, now, healthy(), []backlog.Issue{i})
	if st := r.Issues[0]; st.State != Waiting {
		t.Fatalf("issue after it was verified = %+v, want it waiting on the owner's review", st)
	}
}

type fakeClosed struct{ issues []backlog.Issue }

func (f fakeClosed) ClosedSince(context.Context, config.IssueSource, time.Time) ([]backlog.Issue, error) {
	return f.issues, nil
}

func TestDigestGivesTheEscapeRateOfEachVerificationTier(t *testing.T) {
	cfg := withRepo(true)
	cfg.Repos[0].Verify = &config.Verify{}
	escaped := issue(5, "## Hand-back\n**PR:** #40", "Verified (low): ran the tests", "Owner ruling: the export misses a column")
	clean := issue(6, "## Hand-back\n**PR:** #41", "Verified (low): fine")
	caught := issue(7, "## Hand-back\n**PR:** #42", "Verification failed (high): accepts a refund of 0", "## Hand-back\n**PR:** #42", "Verified (high): refunds checked")
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Looks: &fakeLooks{}, Closed: fakeClosed{issues: []backlog.Issue{escaped, clean, caught}}}
	d := c.Digest(context.Background(), cfg, &fakeMerged{}, now.Add(-168*time.Hour), now)
	var out bytes.Buffer
	RenderDigest(&out, d)
	for _, want := range []string{
		"Verification of the issues closed in this window:",
		"low   2 passed  1 escaped (50%)  0 sent back",
		"high  1 passed  0 escaped (0%)   1 sent back",
		"escaped: org/app#5: the owner sent it back after it was verified",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("digest lacks %q:\n%s", want, out.String())
		}
	}
}

func TestDigestListsTheLessonsToPromoteOncePerLesson(t *testing.T) {
	handBack := func(n int, lessons string, ago time.Duration) backlog.Issue {
		i := issue(n)
		i.Comments = []backlog.Comment{{Body: "## Hand-back\n**PR:** none\n**Lessons:**\n" + lessons, CreatedAt: now.Add(-ago)}}
		return i
	}
	settled := handBack(9, "- Old news.", 50*time.Hour)
	settled.Comments = append(settled.Comments, backlog.Comment{Body: "Lessons promoted."})
	open := []backlog.Issue{
		handBack(5, "- The suite needs `make db-reset` first.", 3*time.Hour),
		handBack(6, "- the suite needs `make db-reset` first\n- Staging logs are in /var/log/app.", 30*time.Hour),
		settled,
	}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{issues: open}, Looks: &fakeLooks{}}
	d := c.Digest(context.Background(), withRepo(true), &fakeMerged{}, now.Add(-24*time.Hour), now)
	var out bytes.Buffer
	RenderDigest(&out, d)
	for _, want := range []string{
		"Lessons to promote: 2",
		"30h  org/app#6             Staging logs are in /var/log/app.",
		"30h  org/app#5, org/app#6  The suite needs `make db-reset` first.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("digest lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "Old news") {
		t.Fatalf("a settled lesson is listed:\n%s", out.String())
	}
}

func TestStatusGivesEachWorkersContextAndSaysWhenOneIsOverBudget(t *testing.T) {
	res := healthy()
	res.Context = map[string]int{"app": 31000, "docs": 9000}
	m := config.Machine{Name: "box", Workers: []config.Worker{{Name: "app", Dir: "/tmp", Brief: "b"}, {Name: "docs", Dir: "/tmp", Brief: "b"}}}
	r := Assess(m, signals, now, res, nil)
	AssessContext(&r, m, *res, 20000)
	wantAttention(t, r, "worker app loads about 31k tokens before its first item, over the context_budget of 20k: shed context box app")
	r.Workers[0].Last = &runlog.Checkin{Time: now, Verdict: runlog.VerdictOnTrack}
	r.Workers[1].Last = &runlog.Checkin{Time: now, Verdict: runlog.VerdictOnTrack}
	var out bytes.Buffer
	Render(&out, []MachineReport{r}, nil)
	if !strings.Contains(out.String(), "context  app 31k, docs 9k tokens before the first item") {
		t.Fatalf("status:\n%s", out.String())
	}
}

func TestWhatWaitsOnTheOwnerLeadsWithWhatAWorkerTakesAndCollapsesMerges(t *testing.T) {
	asked := func(number int, ago time.Duration, comments ...string) backlog.Issue {
		i := issue(number)
		i.OpenPRs = map[int]backlog.PR{40 + number: {}}
		i.Verify = true
		for n, body := range comments {
			i.Comments = append(i.Comments, backlog.Comment{Body: body, CreatedAt: now.Add(-ago + time.Duration(n)*time.Minute)})
		}
		return i
	}
	pr := func(n int) string { return "## Hand-back\n**PR:** #" + strconv.Itoa(40+n) + " (ready)" }
	issues := []backlog.Issue{
		asked(1, 30*time.Hour, pr(1), "Verified (low): ran the tests"),
		asked(2, 20*time.Hour, pr(2), "Verified (high): tried to break it"),
		asked(3, 2*time.Hour, "## Hand-back\nDecisions needed: 1. which one?"),
		asked(4, 10*time.Hour, pr(4), "Verified (low): fine"),
	}
	res := healthy()
	res.Checkins = []runlog.Checkin{
		{Time: now.Add(-3 * time.Hour), Worker: "app", Kind: "idle", Verdict: runlog.VerdictNudge, Sent: true, Issue: 3},
		{Time: now.Add(-time.Hour), Worker: "app", Kind: "idle", Verdict: runlog.VerdictDone},
	}
	m := config.Machine{Name: "box", Workers: []config.Worker{{Name: "app", Dir: "/tmp", Brief: "b"}}}
	r := Assess(m, signals, now, res, issues)
	if len(r.Attention) < 3 {
		t.Fatalf("attention = %q", r.Attention)
	}
	wantInOrder := []string{
		"org/app#3 waits on you (decision 2h; returns to app, which is idle)",
		"org/app#2 waits on you (review #42 20h)",
		"2 pull request(s) verified at low risk with green checks wait for your merge, oldest 30h: #1 #4",
	}
	for n, want := range wantInOrder {
		if !strings.Contains(r.Attention[len(r.Attention)-3+n], want) {
			t.Fatalf("attention = %q, want %q at %d", r.Attention, want, n)
		}
	}
	if got := waitingTotals([]MachineReport{r}); got != "Waiting on you: 1 return to a worker (oldest 2h), 1 to read (oldest 20h), 2 to merge (oldest 30h)." {
		t.Fatalf("totals = %q", got)
	}
}

func TestDigestCountsTheSafeDefaultsTheOwnerOverruledByArea(t *testing.T) {
	cfg := withRepo(true)
	cfg.Supervisor.OverlapLabels = "area:"
	billing := func(n int, comments ...string) backlog.Issue {
		i := issue(n, comments...)
		i.Labels = []backlog.Label{{Name: "area:billing"}}
		return i
	}
	issues := []backlog.Issue{
		billing(5, "## Hand-back\n**PR:** #40\n**Decided without you:**\n- Kept the old column.", "Owner ruling: drop it"),
		billing(6, "## Hand-back\n**PR:** #41\n**Decided without you:**\n- Rounded half up."),
		issue(7, "## Hand-back\n**PR:** #42\n**Decided without you:**\n- Logged at info.", "Owner ruling: accepted"),
	}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Looks: &fakeLooks{}, Closed: fakeClosed{issues: issues}}
	d := c.Digest(context.Background(), cfg, &fakeMerged{}, now.Add(-168*time.Hour), now)
	var out bytes.Buffer
	RenderDigest(&out, d)
	for _, want := range []string{
		"Safe defaults on the issues closed in this window:",
		"area:billing  1 held  1 overruled (50%)",
		"org/app       1 held  0 overruled (0%)",
		"overruled: org/app#5  Kept the old column. — drop it",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("digest lacks %q:\n%s", want, out.String())
		}
	}
}

func TestStatusListsWhatWasReleasedToTheFleet(t *testing.T) {
	cfg := oneMachine()
	cfg.Machines[0].Issues = []config.IssueSource{{Repo: "org/app", Label: "machine:box"}}
	cfg.Accounts = []config.Account{{Name: "max-1", Machines: []string{"box"}, Overflow: "fleet:ready"}}
	released := issue(12)
	released.Title = "Paginate the audit log"
	released.Comments = []backlog.Comment{{Body: "Released: box/app, weekly limit.", CreatedAt: now.Add(-3 * time.Hour)}}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{issues: []backlog.Issue{released}}, Looks: &fakeLooks{}}
	got := c.collectReleased(context.Background(), cfg)
	if len(got) != 1 || got[0].By != "box/app" || got[0].Label != "fleet:ready" {
		t.Fatalf("released = %+v", got)
	}
	var out bytes.Buffer
	RenderReleased(&out, got, now)
	if !strings.Contains(out.String(), "3h  org/app#12  from box/app  Paginate the audit log") {
		t.Fatalf("output:\n%s", out.String())
	}
}

type fakeStaging struct{ serves map[string]bool }

// Contains answers by "head@commit".
func (f fakeStaging) Contains(_ context.Context, _, head, commit string) (bool, error) {
	return f.serves[head+"@"+commit], nil
}

func TestThePromotionBriefSplitsWhatShedMergedFromWhatTheOwnerDid(t *testing.T) {
	cfg := withRepo(true)
	state := looks.Report{Repo: "org/app", Staging: "ccc3333", Production: "aaa1111", Since: now.Add(-48 * time.Hour)}
	queued := issue(5, "## Hand-back\n**PR:** #41 (ready)", "Verified (low): ran the export tests", backlog.QueuedPhrase+" pull request #41, verified at low risk with green checks.")
	mine := issue(6, "## Hand-back\n**PR:** #42 (ready)", "Verified (high): tried to break the refund")
	merged := &fakeMerged{pulls: map[int]looks.Pull{
		41: {Number: 41, Title: "Retry the export", MergedAt: now.Add(-5 * time.Hour), Commit: "m41"},
		42: {Number: 42, Title: "Refund a partial order", MergedAt: now.Add(-2 * time.Hour), Commit: "m42"},
	}}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{issues: []backlog.Issue{mine}}, Looks: &fakeLooks{report: state}, Closed: fakeClosed{issues: []backlog.Issue{queued}}}
	merged.pulls[40] = looks.Pull{Number: 40, Title: "Already in production", MergedAt: now.Add(-48 * time.Hour), Commit: "aaa1111"}
	p, err := c.Promote(context.Background(), cfg, "org/app", merged, fakeStaging{serves: map[string]bool{"ccc3333@m41": true, "aaa1111@aaa1111": true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ByShed) != 1 || p.ByShed[0].PR != 41 || p.ByShed[0].Verified != "ran the export tests" || len(p.ByOwner) != 1 || p.ByOwner[0].Tier != "high" {
		t.Fatalf("promotion = %+v", p)
	}
	if !p.Blocked() {
		t.Fatal("a pull request staging does not serve did not stop the promotion")
	}
	var out bytes.Buffer
	RenderPromotion(&out, p, now)
	for _, want := range []string{
		"org/app: 2 pull request(s) merged since production (production aaa1111, 2d ago; staging ccc3333)",
		"No go yet",
		"Queued by shed on the verifier's word, low risk: 1",
		"#41  #5  on staging  low  Retry the export",
		"verified: ran the export tests",
		"#42  #6  NOT ON STAGING  high  Refund a partial order",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("brief lacks %q:\n%s", want, out.String())
		}
	}
}
