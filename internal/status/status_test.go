package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
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
	wantAttention(t, r, "org/app#7 waits on you (decision)")
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
	want := "Waiting on you: 2 review (oldest 5h), 1 eyes (oldest 40m), 1 decision (oldest 2h)."
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
		{"no worker does them", false, []string{"org/app#7 waits on you (eyes)"}, Waiting, "Waiting on you: 1 eyes."},
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
		"30h  decision    org/app#9  Fix the thing\n  2h   review #52  org/app#8  Fix the thing",
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
