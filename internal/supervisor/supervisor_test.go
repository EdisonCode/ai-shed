package supervisor

import (
	"context"
	"errors"
	"fmt"
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
	closed []string
}

func (f *fakeTerminal) Close(window string) error {
	f.closed = append(f.closed, window)
	f.obs = Observation{}
	return nil
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

// fakeLister serves one list of issues for every source, or a list per label
// when byLabel is set.
type fakeLister struct {
	issues  []backlog.Issue
	byLabel map[string][]backlog.Issue
}

func (f *fakeLister) List(_ context.Context, src config.IssueSource) ([]backlog.Issue, error) {
	if f.byLabel != nil {
		return f.byLabel[src.Label], nil
	}
	return f.issues, nil
}

type fixture struct {
	*Supervisor
	term     *fakeTerminal
	reviewer *fakeReviewer
	lister   *fakeLister
	now      time.Time
	dir      string

	reviewsBefore int
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
	for _, part := range []string{"Fix audit log bugs only.", "org/app with label `machine:box`", "after the words `Decisions needed:`, in a comment headed `Hand-back`", "write your plan and your progress in the issue"} {
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

func TestRestingWorkerIsNotReviewedForAChangeItCannotActOn(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, Verdict{Verdict: runlog.VerdictDone, Reason: "all handed back"})
	handedBack := backlog.Issue{Repo: "org/app", Number: 12, Title: "Paginate the audit log", OpenPRs: map[int]backlog.PR{41: {}},
		Comments: []backlog.Comment{{Body: "## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** open /audit and check page 2"}}}
	f.lister.issues = []backlog.Issue{handedBack}
	f.tick(2 * time.Minute)

	// The owner did the eye check, then another issue was queued that
	// already waits on them. Neither gives this worker anything to do.
	handedBack.Comments = append(handedBack.Comments, backlog.Comment{Body: "Eyes checked: page 2 is right."})
	f.lister.issues = []backlog.Issue{handedBack}
	f.tick(queueRecheck + time.Minute)
	f.lister.issues = append(f.lister.issues, backlog.Issue{Repo: "org/app", Number: 13, Title: "Plan the export",
		Comments: []backlog.Comment{{Body: "## Hand-back\nDecisions needed: 1. CSV or XLSX?"}}})
	f.tick(queueRecheck + time.Minute)

	if len(f.reviewer.prompts) != 1 {
		t.Fatalf("reviewed %d times, want 1: nothing in the queue became workable", len(f.reviewer.prompts))
	}
}

func TestRestingWorkerWakesWhenTheOwnerAnswersAQuestion(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, Verdict{Verdict: runlog.VerdictDone, Reason: "it asked and stopped"}, assign(12))
	asked := backlog.Issue{Repo: "org/app", Number: 12, Title: "Plan the export",
		Comments: []backlog.Comment{{Body: "## Hand-back\nDecisions needed: 1. CSV or XLSX?"}}}
	f.lister.issues = []backlog.Issue{asked}
	f.tick(2 * time.Minute)

	asked.Comments = append(asked.Comments, backlog.Comment{Body: "Owner ruling: CSV."})
	f.lister.issues = []backlog.Issue{asked}
	f.tick(queueRecheck + time.Minute)

	if len(f.term.sent) != 1 {
		t.Fatalf("sent = %q; the answer made #12 workable again", f.term.sent)
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
	f.reviewer.err = errors.New("exit status 1: not logged in")
	f.tick(2 * time.Minute)
	f.tick(time.Minute)
	f.tick(time.Minute)
	if len(f.reviewer.prompts) != 1 {
		t.Fatalf("reviewer was called %d times inside the retry delay, want 1", len(f.reviewer.prompts))
	}
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictError || !strings.Contains(c.Reason, "not logged in") {
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
		{"limited", `{"verdict":"limited","message":"wait","resume_in_minutes":45,"reason":"r"}`, Verdict{Verdict: "limited", Reason: "r", ResumeInMinutes: 45}, ""},
		{"plain", `{"verdict":"done","message":"","reason":"empty queue"}`, Verdict{Verdict: "done", Reason: "empty queue"}, ""},
		{"wrapped in prose and a fence", "Here you go:\n```json\n{\"verdict\":\"nudge\",\"message\":\"Take #12.\",\"reason\":\"r\"}\n```", Verdict{Verdict: "nudge", Message: "Take #12.", Reason: "r"}, ""},
		{"message becomes one line", `{"verdict":"nudge","message":"Take #12.\nThen #13.","reason":"r"}`, Verdict{Verdict: "nudge", Message: "Take #12. Then #13.", Reason: "r"}, ""},
		{"message is dropped unless nudging", `{"verdict":"needs_owner","message":"y","reason":"r"}`, Verdict{Verdict: "needs_owner", Reason: "r"}, ""},
		{"message never starts with a digit", `{"verdict":"nudge","message":"5227 is next: start it.","issue":5227,"reason":"r"}`, Verdict{Verdict: "nudge", Message: "Next: 5227 is next: start it.", Reason: "r", Issue: 5227}, ""},
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

func TestBriefTellsAWorkerToRefuseAnIssueThatIsAlreadyCovered(t *testing.T) {
	f := newFixture(t, config.Supervisor{}, onTrack)
	f.tick(0)

	brief, _ := os.ReadFile(filepath.Join(f.dir, BriefFile))
	for _, want := range []string{"an open pull request or an unmerged branch already covers it", "do not start it", "as a decision for the owner"} {
		if !strings.Contains(string(brief), want) {
			t.Fatalf("brief lacks %q:\n%s", want, brief)
		}
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
	f.restartAgent() // the agent restarted; the worker did not

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

// restartAgent stands for a new agent process, or a reloaded fleet file: the
// supervisor's memory is gone and only the check-in log is left.
func (f *fixture) restartAgent() {
	f.Supervisor.workers = nil
	f.reviewsBefore = len(f.reviewer.prompts)
	f.term.sent = nil
}

// reviews is how many times the reviewer was asked since the last restart.
func (f *fixture) reviews() int {
	return len(f.reviewer.prompts) - f.reviewsBefore
}

func TestRestartCostsARestingWorkerNothing(t *testing.T) {
	f := started(t, config.Supervisor{}, Verdict{Verdict: runlog.VerdictDone, Reason: "queue is empty"})
	f.tick(2 * time.Minute)
	f.restartAgent()
	f.tick(time.Minute)
	f.tick(10 * time.Minute)

	if f.reviews() != 0 || len(f.term.sent) != 0 {
		t.Fatalf("after a restart: %d review(s), sent %q; want none, the worker was already reviewed", f.reviews(), f.term.sent)
	}
}

func TestRestartCostsABusyWorkerNothing(t *testing.T) {
	f := started(t, config.Supervisor{}, assign(12))
	f.tick(2 * time.Minute) // handed #12
	f.restartAgent()
	f.term.running(f.now.Add(5 * time.Minute)) // working on it
	f.tick(5 * time.Minute)

	if f.reviews() != 0 || len(f.term.sent) != 0 {
		t.Fatalf("after a restart: %d review(s), sent %q; want none until the scope check is due", f.reviews(), f.term.sent)
	}
}

func TestRestartDoesNotResetTheNudgeLimit(t *testing.T) {
	f := started(t, config.Supervisor{}, nudge("Continue with #12."))
	for range maxNudges {
		f.term.running(f.now)
		f.tick(2 * time.Minute)
	}
	f.restartAgent()
	f.term.running(f.now)
	f.tick(2 * time.Minute)

	if len(f.term.sent) != 0 {
		t.Fatalf("sent = %q; a restart must not buy a stuck worker three more nudges", f.term.sent)
	}
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictStuck {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestRestartRemembersTheModel(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h", Models: models}, assign(12), assign(13))
	f.lister.issues = append(f.lister.issues, backlog.Issue{Repo: "org/app", Number: 13, Title: "Another sonnet job"})
	f.tick(2 * time.Minute) // switched to sonnet for #12
	f.restartAgent()

	if sent := f.handOver(); !slices.Equal(sent, []string{"Take the next issue."}) {
		t.Fatalf("sent = %q; #13 is on the same model, so nothing should be cleared or switched", sent)
	}
}

func TestBriefEditDoesNotInterruptAWorkerMidIssue(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12), nudge("Run the failing test again with -v."))
	f.tick(2 * time.Minute) // handed #12
	f.restartAgent()        // a deploy brought an edited brief
	f.Machine.Workers[0].Brief = "Fix export bugs only."
	f.term.running(f.now.Add(5 * time.Minute))
	f.tick(5 * time.Minute)

	if len(f.term.sent) != 0 {
		t.Fatalf("sent = %q; a worker in the middle of an issue must not be interrupted", f.term.sent)
	}
	brief, _ := os.ReadFile(filepath.Join(f.dir, BriefFile))
	if !strings.Contains(string(brief), "Fix export bugs only.") {
		t.Fatal("the brief file was not rewritten")
	}

	f.tick(2 * time.Minute) // it goes quiet; the next message carries the notice
	want := "Your brief changed. Read .shed/BRIEF.md again. Then: Run the failing test again with -v."
	if !slices.Equal(f.term.sent, []string{want}) {
		t.Fatalf("sent = %q", f.term.sent)
	}
}

func TestPendingBriefNoticeSurvivesASecondRestart(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12), nudge("Carry on."))
	f.tick(2 * time.Minute)
	f.restartAgent()
	f.Machine.Workers[0].Brief = "Fix export bugs only."
	f.term.running(f.now.Add(5 * time.Minute))
	f.tick(5 * time.Minute)
	f.restartAgent() // and another deploy before the worker went quiet
	f.tick(2 * time.Minute)

	if len(f.term.sent) != 1 || !strings.HasPrefix(f.term.sent[0], "Your brief changed.") {
		t.Fatalf("sent = %q", f.term.sent)
	}
}

// cancelledReviewer behaves as a reviewer command killed by a stopping agent.
type cancelledReviewer struct{ cancel context.CancelFunc }

func (c cancelledReviewer) Review(ctx context.Context, _ string) (Verdict, error) {
	c.cancel()
	return Verdict{}, ctx.Err()
}

func TestCheckCutShortByAStoppingAgentIsNotAFailure(t *testing.T) {
	f := started(t, config.Supervisor{}, onTrack)
	before := len(f.checkins(t))
	ctx, cancel := context.WithCancel(context.Background())
	f.Reviewer = cancelledReviewer{cancel}
	f.now = f.now.Add(2 * time.Minute)
	f.Tick(ctx)

	if got := len(f.checkins(t)); got != before {
		t.Fatalf("%d check-in(s) recorded for a check the agent itself cut short: %+v", got-before, f.lastCheckin(t))
	}
	// The next agent reviews the worker at once: nothing was backed off.
	f.Reviewer = f.reviewer
	f.restartAgent()
	f.tick(30 * time.Second)
	if f.reviews() != 1 {
		t.Fatalf("reviews after the restart = %d, want 1", f.reviews())
	}
}

func limited(minutes int) Verdict {
	return Verdict{Verdict: runlog.VerdictLimited, ResumeInMinutes: minutes, Reason: "the screen says the usage limit resets at 3pm"}
}

func TestLimitedWorkerIsLeftAloneUntilItsLimitResets(t *testing.T) {
	f := started(t, config.Supervisor{}, limited(90), nudge("Your limit has reset. Continue with #12."))
	f.tick(2 * time.Minute)
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictLimited || !c.Until.Equal(f.now.Add(91*time.Minute)) {
		t.Fatalf("check-in = %+v, want limited until 91 minutes from now", c)
	}

	for range 8 { // 80 minutes: still limited
		f.term.running(f.term.obs.LastActivity)
		f.tick(10 * time.Minute)
	}
	if len(f.reviewer.prompts) != 1 || len(f.term.sent) != 0 {
		t.Fatalf("before the reset: %d review(s), sent %q; want one review and no message", len(f.reviewer.prompts), f.term.sent)
	}

	f.tick(12 * time.Minute) // past the reset, and the screen has not changed
	// Its cache is long cold, but it was cut off mid-task: the context is kept.
	if !slices.Equal(f.term.sent, []string{"Your limit has reset. Continue with #12."}) {
		t.Fatalf("after the reset: sent = %q, want the message alone with no clear", f.term.sent)
	}
	if c := f.lastCheckin(t); !c.Cold || c.Kind != KindLimit {
		t.Fatalf("check-in = %+v", c)
	}
	if !strings.Contains(f.reviewer.prompts[1], "expected to reset at") {
		t.Fatal("the reviewer was not told that the limit was due to reset")
	}
}

func TestLimitWithNoResetTimeIsLookedAtAgainLater(t *testing.T) {
	f := started(t, config.Supervisor{}, limited(0), limited(0))
	f.tick(2 * time.Minute)
	f.tick(limitRecheck - time.Minute)
	if len(f.reviewer.prompts) != 1 {
		t.Fatalf("reviews = %d before the recheck time, want 1", len(f.reviewer.prompts))
	}
	f.tick(2 * time.Minute)
	if len(f.reviewer.prompts) != 2 {
		t.Fatalf("reviews = %d after the recheck time, want 2", len(f.reviewer.prompts))
	}
}

func TestLimitIsRememberedAcrossAnAgentRestart(t *testing.T) {
	f := started(t, config.Supervisor{}, limited(90))
	f.tick(2 * time.Minute)
	f.restartAgent()
	f.tick(30 * time.Minute)
	if f.reviews() != 0 || len(f.term.sent) != 0 {
		t.Fatalf("after a restart: %d review(s), sent %q; the worker is still limited", f.reviews(), f.term.sent)
	}
}

func TestLimitedReviewerIsNotAFailure(t *testing.T) {
	f := started(t, config.Supervisor{}, onTrack)
	f.reviewer.err = errors.New("reviewer command: exit status 1: Claude usage limit reached. Your limit will reset at 3pm.")
	f.tick(2 * time.Minute)
	f.tick(retryAfter + time.Minute) // a plain failure would be retried by now

	if len(f.reviewer.prompts) != 1 {
		t.Fatalf("reviewer was asked %d times inside the limit wait, want 1", len(f.reviewer.prompts))
	}
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictLimited || c.Kind != KindLimit {
		t.Fatalf("check-in = %+v, want limited, not an error", c)
	}

	f.reviewer.err = nil
	f.tick(limitRecheck)
	if len(f.reviewer.prompts) != 2 {
		t.Fatalf("reviewer was asked %d times after the limit wait, want 2", len(f.reviewer.prompts))
	}
}

func labeled(number int, labels ...string) backlog.Issue {
	i := backlog.Issue{Repo: "org/app", Number: number, Title: "Issue"}
	for _, l := range labels {
		i.Labels = append(i.Labels, backlog.Label{Name: l})
	}
	return i
}

// queueShown returns the issue numbers of the queue in the reviewer's prompt,
// in order.
func queueShown(t *testing.T, prompt string) []int {
	t.Helper()
	_, rest, _ := strings.Cut(prompt, "<queue>\n")
	block, _, _ := strings.Cut(rest, "</queue>")
	var numbers []int
	for _, line := range strings.Split(strings.TrimSpace(block), "\n") {
		var n int
		if _, err := fmt.Sscanf(line, "org/app#%d", &n); err == nil {
			numbers = append(numbers, n)
		}
	}
	return numbers
}

func TestQueueIsOrderedByPriorityThenAge(t *testing.T) {
	f := started(t, config.Supervisor{}, onTrack)
	f.Machine.Issues[0].Priority = []string{"p0", "p1"}
	f.lister.issues = []backlog.Issue{labeled(30), labeled(20, "p1"), labeled(10), labeled(40, "p0"), labeled(25, "p1")}
	f.tick(2 * time.Minute)

	if got, want := queueShown(t, f.reviewer.prompts[0]), []int{40, 20, 25, 10, 30}; !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
}

func handedBackWith(number int, problem string) backlog.Issue {
	i := labeled(number)
	i.Comments = []backlog.Comment{{Body: "## Hand-back\n**PR:** #41 (ready)"}}
	i.OpenPRs = map[int]backlog.PR{41: {Problem: problem}}
	return i
}

func TestStalePullRequestGoesBackToAWorkerFirst(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"},
		Verdict{Verdict: runlog.VerdictNudge, Message: "PR #41 conflicts with main. Rebase it and nothing else.", Issue: 12, Reason: "its pull request went stale"})
	f.lister.issues = []backlog.Issue{labeled(5), handedBackWith(12, "conflicts with the base branch")}
	f.tick(2 * time.Minute)

	prompt := f.reviewer.prompts[0]
	if got := queueShown(t, prompt); !slices.Equal(got, []int{12, 5}) {
		t.Fatalf("queue = %v, want the rework item first", got)
	}
	if !strings.Contains(prompt, "[rework: pull request #41 conflicts with the base branch]") {
		t.Fatalf("the reviewer was not told why:\n%s", prompt)
	}
	if c := f.lastCheckin(t); c.Issue != 12 || c.Rework != "pull request #41 conflicts with the base branch" || !c.Sent {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestHealthyPullRequestStaysWithTheOwner(t *testing.T) {
	f := started(t, config.Supervisor{}, onTrack)
	f.lister.issues = []backlog.Issue{handedBackWith(12, "")}
	f.tick(2 * time.Minute)
	if prompt := f.reviewer.prompts[0]; strings.Contains(prompt, "rework") && strings.Contains(prompt, "#12 Issue [rework") || !strings.Contains(prompt, "org/app#12 Issue [waits on the owner: review]") {
		t.Fatalf("prompt queue:\n%s", prompt)
	}
}

func TestRestingWorkerWakesWhenAPullRequestGoesStale(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, Verdict{Verdict: runlog.VerdictDone, Reason: "all handed back"},
		Verdict{Verdict: runlog.VerdictNudge, Message: "Rebase PR #41.", Issue: 12, Reason: "stale"})
	f.lister.issues = []backlog.Issue{handedBackWith(12, "")}
	f.tick(2 * time.Minute)
	f.lister.issues = []backlog.Issue{handedBackWith(12, "has a failed check (integration)")}
	f.tick(queueRecheck + time.Minute)

	if !slices.Equal(f.term.sent, []string{"Rebase PR #41."}) {
		t.Fatalf("sent = %q", f.term.sent)
	}
}

func TestPullRequestIsSentBackOnlySoManyTimes(t *testing.T) {
	back := Verdict{Verdict: runlog.VerdictNudge, Message: "Fix the failed check on PR #41.", Issue: 12, Reason: "stale"}
	f := started(t, config.Supervisor{CacheTTL: "1h"}, back, back, Verdict{Verdict: runlog.VerdictDone, Reason: "nothing workable"})
	f.lister.issues = []backlog.Issue{handedBackWith(12, "has a failed check (flaky)")}
	f.tick(2 * time.Minute)
	f.handOver()
	f.handOver()

	last := f.reviewer.prompts[len(f.reviewer.prompts)-1]
	if strings.Contains(last, "[rework:") || !strings.Contains(last, "waits on the owner: pull request #41 has a failed check (flaky) after 2 tries") {
		t.Fatalf("after two tries the item must go to the owner:\n%s", last)
	}
}

func twoWorkers(t *testing.T, verdicts ...Verdict) *fixture {
	t.Helper()
	f := newFixture(t, config.Supervisor{CacheTTL: "1h"}, verdicts...)
	f.Machine.Workers = append(f.Machine.Workers, config.Worker{Name: "docs", Dir: t.TempDir(), Brief: "Docs only."})
	f.term.running(t0.Add(-10 * time.Minute)) // both sessions are up and silent
	return f
}

func TestTwoWorkersAreNotHandedTheSameIssue(t *testing.T) {
	f := twoWorkers(t, assign(12), Verdict{Verdict: runlog.VerdictDone, Reason: "nothing left"})
	f.tick(0) // app is reviewed first and takes #12; then docs

	if len(f.reviewer.prompts) != 2 {
		t.Fatalf("reviews = %d, want one per worker", len(f.reviewer.prompts))
	}
	if got := queueShown(t, f.reviewer.prompts[1]); len(got) != 0 {
		t.Fatalf("the second worker was shown %v; #12 is in hand for the first", got)
	}
}

func TestReviewerCannotHandOverAnIssueOutsideTheQueue(t *testing.T) {
	f := started(t, config.Supervisor{}, assign(99))
	f.tick(2 * time.Minute)
	if len(f.term.sent) != 0 {
		t.Fatalf("sent = %q; #99 is not in the queue", f.term.sent)
	}
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictError || !strings.Contains(c.Reason, "#99") {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestWorkerWithItsOwnQueueSeesOnlyThat(t *testing.T) {
	f := twoWorkers(t, onTrack)
	f.Machine.Workers[1].Issues = []config.IssueSource{{Repo: "org/app", Label: "area:docs"}}
	f.lister.byLabel = map[string][]backlog.Issue{"machine:box": {labeled(12)}, "area:docs": {labeled(50), labeled(51)}}
	f.tick(0)

	if got := queueShown(t, f.reviewer.prompts[0]); !slices.Equal(got, []int{12}) {
		t.Fatalf("worker app was shown %v, want the machine's queue", got)
	}
	if got := queueShown(t, f.reviewer.prompts[1]); !slices.Equal(got, []int{50, 51}) {
		t.Fatalf("worker docs was shown %v, want its own queue", got)
	}
	brief, _ := os.ReadFile(filepath.Join(f.Machine.Workers[1].Dir, BriefFile))
	if !strings.Contains(string(brief), "label `area:docs`") || strings.Contains(string(brief), "machine:box") {
		t.Fatalf("the docs worker's brief names the wrong queue:\n%s", brief)
	}
}

func (f *fixture) requestRecycle(t *testing.T, mode string) string {
	t.Helper()
	path := filepath.Join(f.StateDir, runlog.RecycleDir, "app")
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(mode), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRecycleWaitsForTheIssueInHand(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12), nudge("The test still fails. Read its output."), assign(13))
	f.lister.issues = append(f.lister.issues, labeled(13))
	f.tick(2 * time.Minute) // handed #12
	request := f.requestRecycle(t, "")

	f.handOver() // mid-issue: a nudge about #12 goes through as usual
	if len(f.term.closed) != 0 || !slices.Equal(f.term.sent, []string{"The test still fails. Read its output."}) {
		t.Fatalf("closed %v, sent %q; a worker with an issue in hand must not be recycled", f.term.closed, f.term.sent)
	}

	f.handOver() // it finished #12; the reviewer would hand over #13
	if !slices.Equal(f.term.closed, []string{"app"}) || len(f.term.sent) != 0 {
		t.Fatalf("closed %v, sent %q; want the session ended and #13 held for the new one", f.term.closed, f.term.sent)
	}
	if _, err := os.Stat(request); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the recycle request was not cleared")
	}
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictRecycled {
		t.Fatalf("check-in = %+v", c)
	}

	f.tick(30 * time.Second) // the next tick starts a fresh session
	if len(f.term.sent) != 1 || !strings.Contains(f.term.sent[0], startPrompt) {
		t.Fatalf("sent = %q, want the worker's command", f.term.sent)
	}
	f.term.running(f.now)
	f.term.sent = nil
	f.tick(2 * time.Minute) // and the new session is handed #13
	if !slices.Equal(f.term.sent, []string{"Take the next issue."}) {
		t.Fatalf("sent = %q", f.term.sent)
	}
	if c := f.lastCheckin(t); c.Issue != 13 {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestRecycleNowDoesNotWait(t *testing.T) {
	f := started(t, config.Supervisor{}, assign(12))
	f.tick(2 * time.Minute)
	f.requestRecycle(t, runlog.RecycleNow)
	f.term.running(f.now.Add(time.Minute)) // busy on #12
	f.tick(time.Minute)
	if !slices.Equal(f.term.closed, []string{"app"}) {
		t.Fatalf("closed = %v", f.term.closed)
	}
}

func TestRecycleOfAWorkerWithNothingInHandIsImmediate(t *testing.T) {
	f := started(t, config.Supervisor{}, onTrack)
	f.requestRecycle(t, "")
	f.tick(30 * time.Second)
	if !slices.Equal(f.term.closed, []string{"app"}) || len(f.reviewer.prompts) != 0 {
		t.Fatalf("closed = %v after %d review(s)", f.term.closed, len(f.reviewer.prompts))
	}
}

func TestOwnerIsNotifiedOnceWhenAWorkerStartsToNeedThem(t *testing.T) {
	needs := Verdict{Verdict: runlog.VerdictNeedsOwner, Reason: "a permission prompt is open"}
	f := started(t, config.Supervisor{}, needs, needs, onTrack, needs)
	var notes []string
	f.Notify = func(worker, message string) { notes = append(notes, worker+"|"+message) }

	f.tick(2 * time.Minute)
	f.term.running(f.now) // the screen changed, and it needs the owner still
	f.tick(2 * time.Minute)
	if want := []string{"app|box: worker app needs you: a permission prompt is open"}; !slices.Equal(notes, want) {
		t.Fatalf("notifications = %q, want %q", notes, want)
	}

	f.term.running(f.now) // the owner answered; it works; later it needs them again
	f.tick(2 * time.Minute)
	f.term.running(f.now)
	f.tick(2 * time.Minute)
	if len(notes) != 2 {
		t.Fatalf("notifications = %q, want a second one for the new problem", notes)
	}
}

func TestOwnerIsNotNotifiedAboutWorkThatIsGoingWell(t *testing.T) {
	f := started(t, config.Supervisor{}, assign(12), Verdict{Verdict: runlog.VerdictDone, Reason: "r"}, limited(30))
	notified := false
	f.Notify = func(string, string) { notified = true }
	f.tick(2 * time.Minute)
	f.handOver()
	f.handOver()
	if notified {
		t.Fatal("a nudge, a finished queue and a usage limit need nobody")
	}
}

func TestOwnerIsNotifiedWhenAWorkerIsStuck(t *testing.T) {
	f := started(t, config.Supervisor{}, nudge("Continue with #12."))
	var notes []string
	f.Notify = func(_, message string) { notes = append(notes, message) }
	for range 5 {
		f.term.running(f.now)
		f.tick(2 * time.Minute)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "worker app is stuck") {
		t.Fatalf("notifications = %q", notes)
	}
}

func TestRecycleOfAWorkerThatHandedBackDoesNotWaitForMoreWork(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12), Verdict{Verdict: runlog.VerdictDone, Reason: "handed back; nothing else is workable"})
	f.tick(2 * time.Minute)
	f.handOver() // it finished #12 and rests, with #12 still its last issue
	f.requestRecycle(t, "")
	f.tick(30 * time.Second)

	if !slices.Equal(f.term.closed, []string{"app"}) {
		t.Fatalf("closed = %v; a resting worker has nothing in progress, so the fresh session must not wait", f.term.closed)
	}
}

func TestReviewerIsToldWhatTheLogSaysIsInHand(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12), onTrack)
	f.tick(2 * time.Minute)
	if !strings.Contains(f.reviewer.prompts[0], "<in_hand>none: you have not handed this worker an issue") {
		t.Fatalf("first review: the prompt does not say that nothing is in hand:\n%s", f.reviewer.prompts[0])
	}
	f.handOver()
	if !strings.Contains(f.reviewer.prompts[1], "<in_hand>#12</in_hand>") {
		t.Fatal("second review: the prompt does not name the issue in hand")
	}
}

// busyMachine makes the fixture's machine busy until the returned flag is
// set to false.
func busyMachine(f *fixture) *bool {
	busy := true
	f.Busy = func(context.Context) string {
		if busy {
			return "the load is 2.50 per core, above the limit of 1.50"
		}
		return ""
	}
	return &busy
}

func TestHandOverIsHeldWhileTheMachineIsBusy(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12))
	busy := busyMachine(f)
	f.tick(2 * time.Minute)

	if len(f.term.sent) != 0 {
		t.Fatalf("sent = %q; the machine is busy", f.term.sent)
	}
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictHeld || c.Issue != 12 || c.Sent || !strings.Contains(c.Reason, "#12 is held") {
		t.Fatalf("check-in = %+v", c)
	}

	f.tick(time.Minute) // still busy: nothing happens, and the reviewer is not asked again
	if len(f.term.sent) != 0 || len(f.reviewer.prompts) != 1 {
		t.Fatalf("sent %q after %d review(s)", f.term.sent, len(f.reviewer.prompts))
	}

	*busy = false
	f.tick(time.Minute)
	if !slices.Equal(f.term.sent, []string{"Take the next issue."}) || len(f.reviewer.prompts) != 1 {
		t.Fatalf("sent %q after %d review(s); want the held hand-over delivered with no second review", f.term.sent, len(f.reviewer.prompts))
	}
	if c := f.lastCheckin(t); c.Kind != KindCapacity || c.Issue != 12 || !c.Sent || !strings.Contains(c.Reason, "room again") {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestMachineThatIsNeverQuietStillHandsOverAfterTheLongestWait(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12))
	f.Machine.Capacity.MaxWait = "10m"
	busyMachine(f)
	f.tick(2 * time.Minute)
	f.tick(9 * time.Minute)
	if len(f.term.sent) != 0 {
		t.Fatalf("sent = %q before the longest wait was over", f.term.sent)
	}
	f.tick(2 * time.Minute)
	if !slices.Equal(f.term.sent, []string{"Take the next issue."}) {
		t.Fatalf("sent = %q; a busy machine must resist new work, not refuse it for ever", f.term.sent)
	}
	if c := f.lastCheckin(t); !strings.Contains(c.Reason, "handed over after waiting") {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestBusyMachineDoesNotHoldANudgeAboutTheIssueInHand(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12), assign(12))
	f.tick(2 * time.Minute) // handed #12 while there is room
	busyMachine(f)
	if sent := f.handOver(); !slices.Equal(sent, []string{"Take the next issue."}) {
		t.Fatalf("sent = %q; work in progress goes on when the machine is busy", sent)
	}
}

func TestHeldHandOverIsDroppedWhenTheWorkerMoves(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12), onTrack)
	busy := busyMachine(f)
	f.tick(2 * time.Minute)
	f.term.running(f.now.Add(time.Minute)) // the owner typed something; the worker is working
	*busy = false
	f.tick(time.Minute)
	if len(f.term.sent) != 0 {
		t.Fatalf("sent = %q; the choice was made for a screen that has since changed", f.term.sent)
	}
}

func TestHeldIssueThatWasClosedMeanwhileIsNotHandedOver(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12), Verdict{Verdict: runlog.VerdictDone, Reason: "nothing left"})
	busy := busyMachine(f)
	f.tick(2 * time.Minute)
	f.lister.issues = nil // #12 was closed while it was held
	*busy = false
	f.tick(time.Minute)
	f.tick(time.Minute)
	if len(f.term.sent) != 0 || len(f.reviewer.prompts) != 2 {
		t.Fatalf("sent %q after %d review(s); want nothing sent and the worker reviewed afresh", f.term.sent, len(f.reviewer.prompts))
	}
}

func TestHeldWorkerIsReviewedAgainByANewAgent(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12))
	busy := busyMachine(f)
	f.tick(2 * time.Minute)
	f.restartAgent()
	*busy = false
	f.tick(time.Minute)
	if f.reviews() != 1 || !slices.Equal(f.term.sent, []string{"Take the next issue."}) {
		t.Fatalf("after a restart: %d review(s), sent %q; a held worker must not be forgotten", f.reviews(), f.term.sent)
	}
}

func TestHeldHandOverDoesNotNotifyTheOwner(t *testing.T) {
	f := started(t, config.Supervisor{}, assign(12))
	busyMachine(f)
	notified := false
	f.Notify = func(string, string) { notified = true }
	f.tick(2 * time.Minute)
	if notified {
		t.Fatal("a busy machine needs nobody")
	}
}

func TestMachineBusy(t *testing.T) {
	load := func(l float64) func() (float64, error) { return func() (float64, error) { return l, nil } }
	failing := func() (float64, error) { return 0, errors.New("no uptime") }
	cases := []struct {
		name     string
		capacity config.Capacity
		load     func() (float64, error)
		want     string
	}{
		{"no test set", config.Capacity{}, load(99), ""},
		{"load under the limit", config.Capacity{MaxLoad: 1.5}, load(8), ""},
		{"load over the limit", config.Capacity{MaxLoad: 1.5}, load(16), "the load is 2.00 per core, above the limit of 1.50"},
		{"load cannot be read: room", config.Capacity{MaxLoad: 1.5}, failing, ""},
		{"busy_when exits 0: busy", config.Capacity{BusyWhen: "true"}, load(0), "the machine's busy_when check says it is busy"},
		{"busy_when exits 1: room", config.Capacity{BusyWhen: "false"}, load(0), ""},
		{"busy_when cannot run: room", config.Capacity{BusyWhen: "no-such-command-xyz"}, load(0), ""},
		{"either test is enough", config.Capacity{MaxLoad: 1.5, BusyWhen: "false"}, load(16), "the load is 2.00 per core, above the limit of 1.50"},
	}
	for _, tc := range cases {
		busy := MachineBusy(config.Machine{Capacity: tc.capacity}, tc.load, 8)
		if got := busy(context.Background()); got != tc.want {
			t.Errorf("%s: busy = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestBusyWhenRunsAfterTheMachinesInitLine(t *testing.T) {
	m := config.Machine{Init: "export SHED_TEST_BUSY=yes", Capacity: config.Capacity{BusyWhen: `test "$SHED_TEST_BUSY" = yes`}}
	if got := MachineBusy(m, nil, 8)(context.Background()); got == "" {
		t.Fatal("the busy_when command did not see what the init line set")
	}
}

func TestSystemLoadCanBeRead(t *testing.T) {
	if l, err := systemLoad(); err != nil || l < 0 {
		t.Fatalf("load = %v, %v", l, err)
	}
}
