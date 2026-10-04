package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

var t0 = time.Date(2026, 10, 4, 22, 0, 0, 0, time.UTC)

type fakeTerminal struct {
	obs    Observation
	screen string
	opened []string
	sent   []string
}

func (f *fakeTerminal) Observe(string) (Observation, error) { return f.obs, nil }
func (f *fakeTerminal) Capture(string, int) (string, error) { return f.screen, nil }
func (f *fakeTerminal) Open(window, dir string) error {
	f.opened = append(f.opened, window)
	return nil
}
func (f *fakeTerminal) Send(window, text string) error { f.sent = append(f.sent, text); return nil }
func (f *fakeTerminal) running(lastActivity time.Time) {
	f.obs = Observation{Exists: true, Command: "claude", LastActivity: lastActivity}
}
func (f *fakeTerminal) exitedToShell(lastActivity time.Time) {
	f.obs = Observation{Exists: true, Command: "zsh", LastActivity: lastActivity}
}

// fakeReviewer gives its verdicts in order and repeats the last one.
type fakeReviewer struct {
	verdicts []Verdict
	err      error
	prompts  []string
}

func (f *fakeReviewer) Review(_ context.Context, prompt string) (Verdict, error) {
	f.prompts = append(f.prompts, prompt)
	if f.err != nil {
		return Verdict{}, f.err
	}
	v := f.verdicts[min(len(f.prompts), len(f.verdicts))-1]
	return v, nil
}

type fakeLister struct{ issues []backlog.Issue }

func (f *fakeLister) List(context.Context, config.IssueSource) ([]backlog.Issue, error) {
	return f.issues, nil
}

type fixture struct {
	*Supervisor
	term     *fakeTerminal
	reviewer *fakeReviewer
	lister   *fakeLister
	now      time.Time
	dir      string
}

// tick moves the clock forward and runs one supervisor tick.
func (f *fixture) tick(after time.Duration) {
	f.now = f.now.Add(after)
	f.Tick(context.Background())
}

func (f *fixture) checkins(t *testing.T) []runlog.Checkin {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.StateDir, runlog.CheckinsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []runlog.Checkin
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		c, err := runlog.ParseCheckin(line)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func (f *fixture) lastCheckin(t *testing.T) runlog.Checkin {
	t.Helper()
	all := f.checkins(t)
	if len(all) == 0 {
		t.Fatal("no check-in was recorded")
	}
	return all[len(all)-1]
}

func nudge(message string) Verdict {
	return Verdict{Verdict: runlog.VerdictNudge, Message: message, Reason: "it stopped with work left"}
}

var onTrack = Verdict{Verdict: runlog.VerdictOnTrack, Reason: "working on the brief"}

func newFixture(t *testing.T, settings config.Supervisor, verdicts ...Verdict) *fixture {
	t.Helper()
	f := &fixture{term: &fakeTerminal{}, reviewer: &fakeReviewer{verdicts: verdicts}, lister: &fakeLister{}, now: t0, dir: t.TempDir()}
	f.lister.issues = []backlog.Issue{{Repo: "org/app", Number: 12, Title: "Paginate the audit log"}}
	f.Supervisor = &Supervisor{
		Machine: config.Machine{
			Name:    "box",
			Issues:  []config.IssueSource{{Repo: "org/app", Label: "machine:box"}},
			Workers: []config.Worker{{Name: "app", Dir: f.dir, Command: "claude --permission-mode auto", Brief: "Fix audit log bugs only."}},
		},
		Settings: settings,
		Signals:  config.DefaultSignals(),
		Terminal: f.term,
		Reviewer: f.reviewer,
		Lister:   f.lister,
		StateDir: t.TempDir(),
		Now:      func() time.Time { return f.now },
		Sleep:    func(time.Duration) {},
		Logf:     t.Logf,
	}
	return f
}

// started returns a fixture whose worker is already running, with its brief
// in place and its last review at t0.
func started(t *testing.T, settings config.Supervisor, verdicts ...Verdict) *fixture {
	t.Helper()
	f := newFixture(t, settings, verdicts...)
	f.tick(0)
	f.term.sent = nil
	f.term.running(f.now)
	return f
}

func TestMissingWorkerIsStartedWithItsBrief(t *testing.T) {
	f := newFixture(t, config.Supervisor{})
	f.tick(0)

	if !slices.Equal(f.term.opened, []string{"app"}) {
		t.Fatalf("opened = %v", f.term.opened)
	}
	want := "claude --permission-mode auto 'You are an unattended worker. Read .shed/BRIEF.md in full and carry it out.'"
	if !slices.Equal(f.term.sent, []string{want}) {
		t.Fatalf("sent = %q", f.term.sent)
	}
	brief, err := os.ReadFile(filepath.Join(f.dir, BriefFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"Fix audit log bugs only.", "org/app with label `machine:box`", "after the words `Decisions needed:`", "write your plan and your progress in the issue"} {
		if !strings.Contains(string(brief), part) {
			t.Fatalf("brief lacks %q:\n%s", part, brief)
		}
	}
	if c := f.lastCheckin(t); c.Kind != KindStart || c.Verdict != runlog.VerdictStarted {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestExitedSessionIsRestartedInItsWindow(t *testing.T) {
	f := started(t, config.Supervisor{})
	f.term.opened = nil
	f.term.exitedToShell(f.now)
	f.tick(time.Minute)

	if len(f.term.opened) != 0 || len(f.term.sent) != 1 {
		t.Fatalf("opened = %v, sent = %q; want the command typed into the existing window", f.term.opened, f.term.sent)
	}
	if c := f.lastCheckin(t); c.Kind != KindRestart {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestSessionThatKeepsExitingIsNotRestartedForever(t *testing.T) {
	f := newFixture(t, config.Supervisor{})
	f.term.exitedToShell(t0)
	for range 6 {
		f.tick(time.Minute)
	}
	if len(f.term.sent) != maxStarts {
		t.Fatalf("started %d times, want %d", len(f.term.sent), maxStarts)
	}
	all := f.checkins(t)
	if last := all[len(all)-1]; last.Verdict != runlog.VerdictStuck || len(all) != maxStarts+1 {
		t.Fatalf("check-ins = %+v; want %d starts then one stuck", all, maxStarts)
	}
}

func TestBusyWorkerIsLeftAloneBetweenScopeChecks(t *testing.T) {
	f := started(t, config.Supervisor{}, onTrack)
	f.term.running(f.now.Add(10 * time.Minute))
	f.tick(10 * time.Minute)
	if len(f.reviewer.prompts) != 0 {
		t.Fatal("a busy worker was reviewed before its scope check was due")
	}
}

func TestBusyWorkerGetsAScopeCheckAndIsPulledBack(t *testing.T) {
	f := started(t, config.Supervisor{}, nudge("The billing refactor is outside your brief. Go back to #12."))
	f.term.running(f.now.Add(31 * time.Minute))
	f.term.screen = "● Refactoring the billing module..."
	f.tick(31 * time.Minute)

	if !slices.Equal(f.term.sent, []string{"The billing refactor is outside your brief. Go back to #12."}) {
		t.Fatalf("sent = %q", f.term.sent)
	}
	prompt := f.reviewer.prompts[0]
	for _, part := range []string{"Fix audit log bugs only.", "org/app#12 Paginate the audit log", "Refactoring the billing module", "it is working"} {
		if !strings.Contains(prompt, part) {
			t.Fatalf("prompt lacks %q", part)
		}
	}
	if c := f.lastCheckin(t); c.Kind != KindScope || !c.Sent || c.Cold {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestIdleWorkerIsNudgedWhileItsCacheIsWarm(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "5m"}, nudge("Take #12 next."))
	f.tick(2 * time.Minute) // silent for 2m: idle, and inside the 5m cache

	if !slices.Equal(f.term.sent, []string{"Take #12 next."}) {
		t.Fatalf("sent = %q", f.term.sent)
	}
	if c := f.lastCheckin(t); c.Kind != KindIdle || c.Cold {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestIdleWorkerIsReviewedOncePerSilence(t *testing.T) {
	f := started(t, config.Supervisor{}, Verdict{Verdict: runlog.VerdictDone, Reason: "queue is empty"})
	f.tick(2 * time.Minute)
	f.tick(time.Minute)
	f.tick(time.Minute)
	if len(f.reviewer.prompts) != 1 {
		t.Fatalf("reviewed %d times, want 1", len(f.reviewer.prompts))
	}
}

func TestRestingWorkerWakesWhenItsQueueChanges(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"},
		Verdict{Verdict: runlog.VerdictDone, Reason: "queue is empty"}, nudge("Take #13 next."))
	f.tick(2 * time.Minute)
	f.tick(10 * time.Minute) // same queue: GitHub is asked, the reviewer is not
	if len(f.reviewer.prompts) != 1 {
		t.Fatalf("reviewed %d times with an unchanged queue, want 1", len(f.reviewer.prompts))
	}

	f.lister.issues = append(f.lister.issues, backlog.Issue{Repo: "org/app", Number: 13, Title: "New work"})
	f.tick(10 * time.Minute)
	if !slices.Equal(f.term.sent, []string{"Take #13 next."}) {
		t.Fatalf("sent = %q", f.term.sent)
	}
	if c := f.lastCheckin(t); c.Kind != KindQueue || c.Cold {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestColdWorkerIsClearedBeforeANudge(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "5m"},
		Verdict{Verdict: runlog.VerdictDone, Reason: "queue is empty"}, nudge("Take #13 next."))
	f.tick(2 * time.Minute)
	f.lister.issues = append(f.lister.issues, backlog.Issue{Repo: "org/app", Number: 13, Title: "New work"})
	f.tick(20 * time.Minute) // silent for 22m: past the 5m cache

	if len(f.term.sent) != 2 || f.term.sent[0] != "/clear" {
		t.Fatalf("sent = %q, want /clear first", f.term.sent)
	}
	if got := f.term.sent[1]; !strings.HasPrefix(got, "Your context was cleared") || !strings.HasSuffix(got, "Take #13 next.") {
		t.Fatalf("message = %q", got)
	}
	if c := f.lastCheckin(t); !c.Cold || !c.Sent {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestColdWorkerKeepsItsContextWhenConfiguredToResume(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "5m", WhenCold: config.ColdResume},
		Verdict{Verdict: runlog.VerdictDone, Reason: "queue is empty"}, nudge("Take #13 next."))
	f.tick(2 * time.Minute)
	f.lister.issues = append(f.lister.issues, backlog.Issue{Repo: "org/app", Number: 13, Title: "New work"})
	f.tick(20 * time.Minute)

	if !slices.Equal(f.term.sent, []string{"Take #13 next."}) {
		t.Fatalf("sent = %q", f.term.sent)
	}
	if c := f.lastCheckin(t); !c.Cold {
		t.Fatalf("check-in = %+v, want it marked cold", c)
	}
}

func TestNothingIsTypedWhenTheOwnerIsNeeded(t *testing.T) {
	f := started(t, config.Supervisor{}, Verdict{Verdict: runlog.VerdictNeedsOwner, Reason: "a permission prompt is open"})
	f.tick(2 * time.Minute)
	if len(f.term.sent) != 0 {
		t.Fatalf("sent = %q", f.term.sent)
	}
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictNeedsOwner || c.Sent {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestNudgesThatDoNotHelpStop(t *testing.T) {
	f := started(t, config.Supervisor{}, nudge("Continue with #12."))
	for range 5 {
		// Each nudge echoes in the terminal, then the worker goes silent again.
		f.term.running(f.now)
		f.tick(2 * time.Minute)
	}
	if len(f.term.sent) != maxNudges {
		t.Fatalf("sent %d nudges, want %d", len(f.term.sent), maxNudges)
	}
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictStuck || c.Sent {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestReviewerFailureIsRecordedOnceAndRetriedLater(t *testing.T) {
	f := started(t, config.Supervisor{}, onTrack)
	f.reviewer.err = errors.New("usage limit reached")
	f.tick(2 * time.Minute)
	f.tick(time.Minute)
	f.tick(time.Minute)
	if len(f.reviewer.prompts) != 1 {
		t.Fatalf("reviewer was called %d times inside the retry delay, want 1", len(f.reviewer.prompts))
	}
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictError || !strings.Contains(c.Reason, "usage limit") {
		t.Fatalf("check-in = %+v", c)
	}

	f.reviewer.err = nil
	f.tick(retryAfter)
	if len(f.reviewer.prompts) != 2 {
		t.Fatalf("reviewer was called %d times after the retry delay, want 2", len(f.reviewer.prompts))
	}
}

func TestRunningWorkerIsToldWhenItsBriefChanges(t *testing.T) {
	f := started(t, config.Supervisor{}, onTrack)
	// A new supervisor, as after a config reload, with an edited brief.
	f.Supervisor.workers = nil
	f.Machine.Workers[0].Brief = "Fix export bugs only."
	f.tick(10 * time.Second)

	if !slices.Equal(f.term.sent, []string{briefChanged}) {
		t.Fatalf("sent = %q", f.term.sent)
	}
	brief, _ := os.ReadFile(filepath.Join(f.dir, BriefFile))
	if !strings.Contains(string(brief), "Fix export bugs only.") {
		t.Fatalf("brief was not rewritten:\n%s", brief)
	}
}

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name, answer string
		want         Verdict
		wantErr      string
	}{
		{"issue is kept on a nudge", `{"verdict":"nudge","message":"Take #12.","issue":12,"reason":"r"}`, Verdict{Verdict: "nudge", Message: "Take #12.", Reason: "r", Issue: 12}, ""},
		{"plain", `{"verdict":"done","message":"","reason":"empty queue"}`, Verdict{Verdict: "done", Reason: "empty queue"}, ""},
		{"wrapped in prose and a fence", "Here you go:\n```json\n{\"verdict\":\"nudge\",\"message\":\"Take #12.\",\"reason\":\"r\"}\n```", Verdict{Verdict: "nudge", Message: "Take #12.", Reason: "r"}, ""},
		{"message becomes one line", `{"verdict":"nudge","message":"Take #12.\nThen #13.","reason":"r"}`, Verdict{Verdict: "nudge", Message: "Take #12. Then #13.", Reason: "r"}, ""},
		{"message is dropped unless nudging", `{"verdict":"needs_owner","message":"y","reason":"r"}`, Verdict{Verdict: "needs_owner", Reason: "r"}, ""},
		{"nudge without message", `{"verdict":"nudge","message":" ","reason":"r"}`, Verdict{}, "no message"},
		{"unknown verdict", `{"verdict":"approve","reason":"r"}`, Verdict{}, "unknown verdict"},
		{"no json", "I think it is fine.", Verdict{}, "no JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseVerdict(tc.answer)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("verdict = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestCommandReviewerPipesThePromptThroughTheCommand(t *testing.T) {
	// The command echoes a verdict whose reason is the prompt's first word.
	r := CommandReviewer{Command: `read first rest; printf '{"verdict":"on_track","reason":"%s"}' "$first"`, Dir: t.TempDir()}
	v, err := r.Review(context.Background(), "hello world\n")
	if err != nil || v.Reason != "hello" {
		t.Fatalf("verdict = %+v, %v", v, err)
	}
}

func TestCommandReviewerReportsAFailingCommand(t *testing.T) {
	r := CommandReviewer{Command: "echo limit reached >&2; exit 1", Dir: t.TempDir()}
	if _, err := r.Review(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("error = %v", err)
	}
}

var models = config.Models{Default: "sonnet", Labels: map[string]string{"model:opus": "opus"}}

func assign(issue int) Verdict {
	return Verdict{Verdict: runlog.VerdictNudge, Message: "Take the next issue.", Issue: issue, Reason: "it finished its item"}
}

func TestFirstIssueSetsTheModelOnACleanContext(t *testing.T) {
	f := started(t, config.Supervisor{Models: models}, assign(12))
	f.tick(2 * time.Minute)

	if len(f.term.sent) != 3 || f.term.sent[0] != "/clear" || f.term.sent[1] != "/model sonnet" || !strings.HasSuffix(f.term.sent[2], "Take the next issue.") {
		t.Fatalf("sent = %q", f.term.sent)
	}
	if c := f.lastCheckin(t); c.Issue != 12 || c.Model != "sonnet" {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestNextIssueOnTheSameModelKeepsTheWarmContext(t *testing.T) {
	f := started(t, config.Supervisor{Models: models}, assign(12), assign(13))
	f.lister.issues = append(f.lister.issues, backlog.Issue{Repo: "org/app", Number: 13, Title: "Another sonnet job"})
	f.tick(2 * time.Minute)
	f.term.sent = nil
	f.term.running(f.now.Add(40 * time.Minute))
	f.tick(42 * time.Minute)

	if !slices.Equal(f.term.sent, []string{"Take the next issue."}) {
		t.Fatalf("sent = %q, want the plain message with no clear and no model switch", f.term.sent)
	}
}

func TestIssueLabelSwitchesTheModel(t *testing.T) {
	f := started(t, config.Supervisor{Models: models}, assign(12), assign(13))
	f.lister.issues = append(f.lister.issues, backlog.Issue{Repo: "org/app", Number: 13, Title: "Design the export pipeline",
		Labels: []backlog.Label{{Name: "machine:box"}, {Name: "model:opus"}}})
	f.tick(2 * time.Minute)
	f.term.sent = nil
	f.term.running(f.now.Add(40 * time.Minute))
	f.tick(42 * time.Minute)

	if len(f.term.sent) != 3 || f.term.sent[0] != "/clear" || f.term.sent[1] != "/model opus" {
		t.Fatalf("sent = %q, want clear, then the model switch, then the message", f.term.sent)
	}
	if c := f.lastCheckin(t); c.Issue != 13 || c.Model != "opus" {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestNudgeThatAssignsNothingLeavesTheModelAlone(t *testing.T) {
	f := started(t, config.Supervisor{Models: models}, nudge("Run the failing test again with -v."))
	f.tick(2 * time.Minute)
	if !slices.Equal(f.term.sent, []string{"Run the failing test again with -v."}) {
		t.Fatalf("sent = %q", f.term.sent)
	}
}

func TestWithoutModelsNoModelCommandIsSent(t *testing.T) {
	f := started(t, config.Supervisor{}, assign(12))
	f.tick(2 * time.Minute)
	if !slices.Equal(f.term.sent, []string{"Take the next issue."}) {
		t.Fatalf("sent = %q", f.term.sent)
	}
}

func TestStandingOrdersReachTheWorkerAndTheReviewer(t *testing.T) {
	f := newFixture(t, config.Supervisor{}, onTrack)
	f.StandingOrders = "State changes only through domain events."
	f.tick(0)
	f.term.running(f.now)
	f.tick(2 * time.Minute)

	brief, _ := os.ReadFile(filepath.Join(f.dir, BriefFile))
	if !strings.Contains(string(brief), "## Standing orders\n\nState changes only through domain events.") {
		t.Fatalf("brief lacks the standing orders:\n%s", brief)
	}
	if !strings.Contains(f.reviewer.prompts[0], "State changes only through domain events.") {
		t.Fatal("the reviewer was not shown the standing orders")
	}
}

func TestOrchestratorIsToldTheModelAndNotSwitched(t *testing.T) {
	tell := config.Models{Default: "sonnet", Labels: models.Labels, Apply: config.ModelTell}
	f := started(t, config.Supervisor{Models: tell}, assign(12))
	f.tick(2 * time.Minute)

	if !slices.Equal(f.term.sent, []string{"Take the next issue. The model for this issue is sonnet."}) {
		t.Fatalf("sent = %q, want one message and no clear or model command", f.term.sent)
	}
	if c := f.lastCheckin(t); c.Issue != 12 || c.Model != "sonnet" {
		t.Fatalf("check-in = %+v", c)
	}
}

// handOver runs the worker through one more issue: it works, goes silent, and
// the supervisor checks in. It returns what was typed.
func (f *fixture) handOver() []string {
	f.term.sent = nil
	f.term.running(f.now.Add(40 * time.Minute))
	f.tick(42 * time.Minute)
	return f.term.sent
}

func freshWorker(t *testing.T, settings config.Supervisor, verdicts ...Verdict) *fixture {
	t.Helper()
	f := started(t, settings, verdicts...)
	f.Machine.Workers[0].FreshPerIssue = true
	f.lister.issues = append(f.lister.issues, backlog.Issue{Repo: "org/app", Number: 13, Title: "Another job"})
	return f
}

func TestFreshWorkerStartsEachNewIssueOnAClearContext(t *testing.T) {
	f := freshWorker(t, config.Supervisor{CacheTTL: "1h"}, assign(12), assign(13))
	f.tick(2 * time.Minute)
	if !slices.Equal(f.term.sent, []string{"Take the next issue."}) {
		t.Fatalf("first issue: sent = %q, want no clear on a session that has had no issue", f.term.sent)
	}

	sent := f.handOver()
	if len(sent) != 2 || sent[0] != "/clear" || !strings.HasPrefix(sent[1], "Your context was cleared") {
		t.Fatalf("second issue: sent = %q, want a clear and then the re-orienting message", sent)
	}
	if c := f.lastCheckin(t); c.Issue != 13 || c.Cold {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestNudgeAboutTheIssueInHandKeepsTheContext(t *testing.T) {
	f := freshWorker(t, config.Supervisor{CacheTTL: "1h"}, assign(12))
	f.tick(2 * time.Minute)
	if sent := f.handOver(); !slices.Equal(sent, []string{"Take the next issue."}) {
		t.Fatalf("sent = %q, want no clear: issue 12 is the issue in hand", sent)
	}
}

func TestIssueInHandIsRememberedAcrossAnAgentRestart(t *testing.T) {
	f := freshWorker(t, config.Supervisor{CacheTTL: "1h"}, assign(12), assign(12), assign(13))
	f.tick(2 * time.Minute)
	f.Supervisor.workers = nil // the agent restarted; the worker did not

	if sent := f.handOver(); !slices.Equal(sent, []string{"Take the next issue."}) {
		t.Fatalf("after restart, same issue: sent = %q, want no clear", sent)
	}
	if sent := f.handOver(); len(sent) != 2 || sent[0] != "/clear" {
		t.Fatalf("after restart, new issue: sent = %q, want a clear first", sent)
	}
}

func TestRestartedSessionHasNoIssueInHand(t *testing.T) {
	f := freshWorker(t, config.Supervisor{CacheTTL: "1h"}, assign(12), assign(13))
	f.tick(2 * time.Minute)
	f.term.exitedToShell(f.now)
	f.tick(time.Minute) // the supervisor starts a new session
	f.term.running(f.now)
	f.term.sent = nil
	f.tick(2 * time.Minute)

	if !slices.Equal(f.term.sent, []string{"Take the next issue."}) {
		t.Fatalf("sent = %q, want no clear: the new session's context is already empty", f.term.sent)
	}
}

func TestFreshOrchestratorIsClearedAndToldTheModel(t *testing.T) {
	tell := config.Models{Default: "sonnet", Apply: config.ModelTell}
	f := freshWorker(t, config.Supervisor{CacheTTL: "1h", Models: tell}, assign(12), assign(13))
	f.tick(2 * time.Minute)
	sent := f.handOver()
	if len(sent) != 2 || sent[0] != "/clear" || !strings.HasSuffix(sent[1], "Take the next issue. The model for this issue is sonnet.") {
		t.Fatalf("sent = %q", sent)
	}
}

func TestWorkerThatKeepsItsContextIsNotClearedBetweenIssues(t *testing.T) {
	f := freshWorker(t, config.Supervisor{CacheTTL: "1h"}, assign(12), assign(13))
	f.Machine.Workers[0].FreshPerIssue = false
	f.tick(2 * time.Minute)
	if sent := f.handOver(); !slices.Equal(sent, []string{"Take the next issue."}) {
		t.Fatalf("sent = %q", sent)
	}
}
