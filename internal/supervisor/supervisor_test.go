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
	"github.com/edisoncode/ai-shed/internal/looks"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// t0 is on a clock that is not UTC, as a machine's is. A time from GitHub is
// in UTC: a test that mixes the two shows the wrong hour.
var t0 = time.Date(2026, 10, 4, 15, 0, 0, 0, time.FixedZone("PDT", -7*60*60))

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

func TestWorkerThatFinishesShortIssuesIsNotStuck(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12), assign(13), assign(14), assign(15))
	f.lister.issues = []backlog.Issue{labeled(12), labeled(13), labeled(14), labeled(15)}
	for range 4 {
		// It finishes each issue within minutes and waits for the next.
		f.term.running(f.now)
		f.tick(2 * time.Minute)
	}
	if len(f.term.sent) != 4 {
		t.Fatalf("sent %d hand-overs, want 4: each one got the worker moving", len(f.term.sent))
	}
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictNudge || c.Issue != 15 {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestNudgesToAnEndedSessionDoNotCountAgainstTheNewOne(t *testing.T) {
	for _, restart := range []bool{false, true} {
		f := started(t, config.Supervisor{}, nudge("Continue with #12."))
		for range maxNudges + 1 {
			f.term.running(f.now)
			f.tick(2 * time.Minute)
		}
		if c := f.lastCheckin(t); c.Verdict != runlog.VerdictStuck {
			t.Fatalf("check-in = %+v, want the old session stuck", c)
		}
		f.requestRecycle(t, runlog.RecycleNow)
		f.tick(time.Minute) // the session is ended
		f.tick(time.Minute) // and a new one starts
		if restart {
			f.restartAgent()
		}
		f.term.sent = nil
		f.term.running(f.now)
		f.tick(2 * time.Minute)

		if !slices.Equal(f.term.sent, []string{"Continue with #12."}) {
			t.Fatalf("agent restarted=%v: sent = %q; the new session has not been nudged before (%+v)", restart, f.term.sent, f.lastCheckin(t))
		}
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
		{"a tool error keeps its message", `{"verdict":"tool_error","message":"Continue; dispatch the test-runner again.","issue":12,"reason":"r"}`, Verdict{Verdict: "tool_error", Message: "Continue; dispatch the test-runner again.", Reason: "r"}, ""},
		{"a tool error with no message gets one", `{"verdict":"tool_error","reason":"r"}`, Verdict{Verdict: "tool_error", Message: continueMessage, Reason: "r"}, ""},
		{"plain", `{"verdict":"done","message":"","reason":"empty queue"}`, Verdict{Verdict: "done", Reason: "empty queue"}, ""},
		{"wrapped in prose and a fence", "Here you go:\n```json\n{\"verdict\":\"nudge\",\"message\":\"Take #12.\",\"reason\":\"r\"}\n```", Verdict{Verdict: "nudge", Message: "Take #12.", Reason: "r"}, ""},
		{"message becomes one line", `{"verdict":"nudge","message":"Take #12.\nThen #13.","reason":"r"}`, Verdict{Verdict: "nudge", Message: "Take #12. Then #13.", Reason: "r"}, ""},
		{"message is dropped unless nudging", `{"verdict":"needs_owner","message":"y","reason":"r"}`, Verdict{Verdict: "needs_owner", Reason: "r"}, ""},
		{"message never starts with a digit", `{"verdict":"nudge","message":"127 is next: start it.","issue":127,"reason":"r"}`, Verdict{Verdict: "nudge", Message: "Next: 127 is next: start it.", Reason: "r", Issue: 127}, ""},
		{"nudge without message", `{"verdict":"nudge","message":" ","reason":"r"}`, Verdict{}, "no message"},
		{"unknown verdict", `{"verdict":"approve","reason":"r"}`, Verdict{}, "unknown verdict"},
		{"no json", "I think it is fine.", Verdict{}, "no JSON"},
		{"a remark after the answer is ignored", "{\"verdict\":\"done\",\"reason\":\"r\"}\n\nWhy: every item {in the queue} waits on the owner.", Verdict{Verdict: "done", Reason: "r"}, ""},
		{"the last of two answers is the one it means", `{"verdict":"done","reason":"r"} Wait, the queue has #13. {"verdict":"nudge","message":"Take #13.","issue":13,"reason":"r"}`, Verdict{Verdict: "nudge", Message: "Take #13.", Reason: "r", Issue: 13}, ""},
		{"a second answer that was cut short does not count", `{"verdict":"done","reason":"r"} {"verdict":"nudge","message":"Take {the next`, Verdict{Verdict: "done", Reason: "r"}, ""},
		{"an answer after a brace in a remark", `The queue {as shown} is empty. {"verdict":"done","reason":"r"}`, Verdict{Verdict: "done", Reason: "r"}, ""},
		{"an object that was cut short", `{"verdict":"done","reason":"every item`, Verdict{}, "bad JSON"},
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

func TestBriefTellsAWorkerSentBackToHandBackAgainEvenWithNothingToChange(t *testing.T) {
	f := newFixture(t, config.Supervisor{}, onTrack)
	f.tick(0)

	brief, _ := os.ReadFile(filepath.Join(f.dir, BriefFile))
	for _, want := range []string{"sent back to an issue you already handed back", "even when nothing needed to change", "names the pull request"} {
		if !strings.Contains(string(brief), want) {
			t.Fatalf("brief lacks %q:\n%s", want, brief)
		}
	}
}

func TestReviewerIsToldWhatToDoWhenASentBackWorkerFindsNothingToChange(t *testing.T) {
	if !strings.Contains(instructions, "tell it once to post a new hand-back that says so") {
		t.Fatal("the reviewer would nudge such a worker until it is marked stuck")
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

// fakeLooks serves one report for every repository.
type fakeLooks struct {
	looks []looks.Look
	err   error
}

func (f *fakeLooks) Read(context.Context, config.Repo, []config.IssueSource, []backlog.Issue, []config.Signal) (looks.Report, error) {
	return looks.Report{Repo: "org/app", Looks: f.looks}, f.err
}

func look(number, pr int, state string) looks.Look {
	return looks.Look{Repo: "org/app", Number: number, Title: "Merged work", PR: pr, State: state}
}

// withStaging gives the fixture's repository a staging environment and its
// first worker a browser.
func withStaging(f *fixture, found ...looks.Look) *fakeLooks {
	reader := &fakeLooks{looks: found}
	f.Looks = reader
	f.Repos = []config.Repo{{Name: "org/app", Staging: config.Environment{Commit: "true", URL: "https://staging.example.com",
		Orders: "Open pages and cancel a draft.\nNever send a message.", Viewport: "1440x900"}}}
	f.Machine.Workers[0].EyeChecks = true
	return reader
}

func TestLookOnStagingIsHandedToAWorkerThatDoesEyeChecks(t *testing.T) {
	f := started(t, config.Supervisor{}, assign(7))
	withStaging(f, look(7, 41, looks.Due), look(8, 42, looks.Awaiting), look(9, 43, looks.Failed))
	f.tick(2 * time.Minute)

	prompt := f.reviewer.prompts[0]
	if got, want := queueShown(t, prompt), []int{7, 12}; !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want %v: the look that is due comes first, the others are not for a worker", got, want)
	}
	if !strings.Contains(prompt, "org/app#7 Merged work [eye check: pull request #41 is on staging]") {
		t.Fatalf("the look is not marked in the queue:\n%s", prompt)
	}
	want := "Do the eye check of #7 on staging: pull request #41 is on staging. Follow the Eye checks section of .shed/BRIEF.md and change no code."
	if !slices.Equal(f.term.sent, []string{want}) {
		t.Fatalf("sent = %q, want the fixed eye-check order", f.term.sent)
	}
	if c := f.lastCheckin(t); !c.Look || c.Issue != 7 {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestWorkerWithoutABrowserIsNotShownLooks(t *testing.T) {
	f := started(t, config.Supervisor{}, onTrack)
	withStaging(f, look(7, 41, looks.Due))
	f.Machine.Workers[0].EyeChecks = false
	f.tick(2 * time.Minute)

	if got := queueShown(t, f.reviewer.prompts[0]); !slices.Equal(got, []int{12}) {
		t.Fatalf("queue = %v, want only the issue to build", got)
	}
}

func TestLookGoesToAWorkerThatDidNotBuildTheWork(t *testing.T) {
	done := Verdict{Verdict: runlog.VerdictDone, Reason: "nothing for it"}
	for _, tc := range []struct {
		name       string
		bothLook   bool
		wantForApp []int
	}{
		{"another worker can look: not the builder", true, nil},
		{"the builder is the only one with a browser", false, []int{7}},
	} {
		f := twoWorkers(t, done)
		f.lister.issues = nil
		withStaging(f, look(7, 41, looks.Due))
		f.Machine.Workers[1].EyeChecks = tc.bothLook
		built := runlog.Checkin{Time: t0.Add(-3 * time.Hour), Worker: "app", Kind: KindIdle, Verdict: runlog.VerdictNudge, Issue: 7, Sent: true}
		if err := runlog.AppendCheckin(f.StateDir, built); err != nil {
			t.Fatal(err)
		}
		lookedByDocs := runlog.Checkin{Time: t0.Add(-time.Hour), Worker: "docs", Kind: KindIdle, Verdict: runlog.VerdictNudge, Issue: 8, Sent: true, Look: true}
		if err := runlog.AppendCheckin(f.StateDir, lookedByDocs); err != nil {
			t.Fatal(err)
		}
		f.tick(0)

		if got := queueShown(t, f.reviewer.prompts[0]); !slices.Equal(got, tc.wantForApp) {
			t.Errorf("%s: the builder was shown %v, want %v", tc.name, got, tc.wantForApp)
		}
		if tc.bothLook {
			if got := queueShown(t, f.reviewer.prompts[1]); !slices.Equal(got, []int{7}) {
				t.Errorf("%s: the other worker was shown %v, want the look", tc.name, got)
			}
		}
	}
}

func TestBuilderWithTheIssueStillInHandIsHandedItsLookAsNewWork(t *testing.T) {
	f := freshWorker(t, config.Supervisor{}, assign(12), assign(12))
	f.tick(2 * time.Minute) // it builds #12
	f.lister.issues = nil   // merged and closed
	withStaging(f, look(12, 41, looks.Due))

	sent := f.handOver()
	if len(sent) != 2 || sent[0] != "/clear" || !strings.Contains(sent[1], "Do the eye check of #12 on staging") {
		t.Fatalf("sent = %q, want a clear context and the eye-check order", sent)
	}
}

func TestRestingWorkerWakesWhenMergedWorkReachesStaging(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, Verdict{Verdict: runlog.VerdictDone, Reason: "nothing to do"}, assign(7))
	f.lister.issues = nil
	reader := withStaging(f, look(7, 41, looks.Awaiting))
	f.tick(2 * time.Minute)
	f.tick(queueRecheck + time.Minute)
	if len(f.reviewer.prompts) != 1 {
		t.Fatalf("reviewed %d times while the look waited for a staging deploy, want 1", len(f.reviewer.prompts))
	}

	reader.looks = []looks.Look{look(7, 41, looks.Due)}
	f.tick(queueRecheck + time.Minute)
	if len(f.term.sent) != 1 || !strings.HasSuffix(f.term.sent[0], "change no code.") {
		t.Fatalf("sent = %q; staging now serves the work", f.term.sent)
	}
}

func TestLooksThatCannotBeReadDoNotStopTheBuilding(t *testing.T) {
	f := started(t, config.Supervisor{}, assign(12))
	withStaging(f).err = errors.New("org/app: read staging's commit: exit status 7")
	f.tick(2 * time.Minute)

	if !slices.Equal(f.term.sent, []string{"Take the next issue."}) {
		t.Fatalf("sent = %q; the queue of issues to build does not depend on staging", f.term.sent)
	}
}

func TestBriefSaysHowAnEyeCheckPassesFailsAndIsBlocked(t *testing.T) {
	f := newFixture(t, config.Supervisor{}, onTrack)
	withStaging(f)
	f.tick(0)

	brief, _ := os.ReadFile(filepath.Join(f.dir, BriefFile))
	for _, want := range []string{
		"## Eye checks",
		"- org/app: staging is https://staging.example.com.",
		"What you may do there: Open pages and cancel a draft.\n  Never send a message.",
		"is after `Needs eyes:`",
		"Viewport: look at 1440x900, unless the hand-back names another size.",
		"Whatever you post, say that size in it, as in `viewport 1440x900`.",
		"If it does, you are blocked, and your comment says the size you had and the size that was asked.",
		"starts with `Eyes checked:`",
		"headed `Hand-back`. Write `Eyes failed:`",
		"then `Decisions needed:`",
		"Post a comment on the issue that starts with `Eyes blocked:`",
		"the supervisor gives you your next item. A check you could not do has not passed.",
	} {
		if !strings.Contains(string(brief), want) {
			t.Fatalf("brief lacks %q:\n%s", want, brief)
		}
	}
}

func TestBriefOfAWorkerWithoutABrowserHasNoEyeChecks(t *testing.T) {
	f := newFixture(t, config.Supervisor{}, onTrack)
	f.tick(0)

	if brief, _ := os.ReadFile(filepath.Join(f.dir, BriefFile)); strings.Contains(string(brief), "Eye checks") {
		t.Fatalf("brief:\n%s", brief)
	}
}

func TestBlockedLookLeavesTheQueueAndTheOwnerIsToldOnce(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12), onTrack)
	blocked := look(7, 41, looks.Blocked)
	blocked.Note = "staging asks for a sign-in"
	withStaging(f, blocked)
	var told []string
	f.Notify = func(worker, message string) { told = append(told, message) }
	f.tick(2 * time.Minute)
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute)

	if got := queueShown(t, f.reviewer.prompts[0]); !slices.Equal(got, []int{12}) {
		t.Fatalf("queue = %v; a blocked look is the owner's, not a worker's", got)
	}
	if !slices.Equal(f.term.sent, []string{"Take the next issue."}) {
		t.Fatalf("sent = %q; the worker takes the next workable item", f.term.sent)
	}
	if want := []string{"box: the eye check of org/app#7 is blocked and waits for you: staging asks for a sign-in"}; !slices.Equal(told, want) {
		t.Fatalf("told = %q, want %q once", told, want)
	}
}

func TestReviewerIsToldABlockedEyeCheckDoesNotHoldTheWorker(t *testing.T) {
	if !strings.Contains(instructions, "A blocked check is no reason to leave a worker idle beside an item it can act on.") {
		t.Fatal("the reviewer is not told what to do with an eye check that could not be done")
	}
}

// mark records what the worker's tool reports about itself.
func (f *fixture) mark(t *testing.T, state, detail string, at time.Time) {
	t.Helper()
	if err := runlog.WriteMark(f.StateDir, "app", runlog.Mark{Time: at, State: state, Detail: detail}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) kinds(t *testing.T) []string {
	t.Helper()
	var kinds []string
	for _, c := range f.checkins(t) {
		kinds = append(kinds, c.Kind+" "+c.Verdict)
	}
	return kinds
}

// askedForPermission is a started worker whose tool stopped a minute in to
// ask a person, with the prompt the last thing on its screen.
func askedForPermission(t *testing.T) *fixture {
	t.Helper()
	f := started(t, config.Supervisor{}, onTrack)
	asked := f.now.Add(time.Minute)
	f.term.running(asked)
	f.mark(t, runlog.MarkWaiting, "Claude needs your permission to use Bash", asked)
	return f
}

func TestToolThatWaitsForAPersonNeedsTheOwnerWithoutAReview(t *testing.T) {
	f := askedForPermission(t)
	f.tick(2 * time.Minute)
	f.tick(10 * time.Minute)
	f.tick(10 * time.Minute)

	if len(f.reviewer.prompts) != 0 {
		t.Fatalf("the reviewer was asked %d times; the tool already said what the worker waits for", len(f.reviewer.prompts))
	}
	if got, want := f.kinds(t), []string{"start started", "hook needs_owner"}; !slices.Equal(got, want) {
		t.Fatalf("check-ins = %v, want %v: it is reported once", got, want)
	}
	if c := f.lastCheckin(t); c.Reason != "its tool waits for a person: Claude needs your permission to use Bash" {
		t.Fatalf("reason = %q", c.Reason)
	}
}

func TestWaitingIsOverWhenTheScreenMovesOn(t *testing.T) {
	f := askedForPermission(t)
	f.tick(2 * time.Minute)
	f.term.running(f.now.Add(20 * time.Second)) // the owner answered and the work goes on
	f.tick(30 * time.Second)

	if c := f.lastCheckin(t); c.Kind != KindHook || c.Verdict != runlog.VerdictOnTrack {
		t.Fatalf("check-in = %+v, want the worker back on track", c)
	}
	if len(f.reviewer.prompts) != 0 {
		t.Fatalf("the reviewer was asked %d times", len(f.reviewer.prompts))
	}
}

func TestWaitingIsReportedOnceAcrossAnAgentRestart(t *testing.T) {
	f := askedForPermission(t)
	f.tick(2 * time.Minute)
	f.restartAgent()
	f.tick(time.Minute)

	if got, want := f.kinds(t), []string{"start started", "hook needs_owner"}; !slices.Equal(got, want) {
		t.Fatalf("check-ins = %v, want %v", got, want)
	}
}

func TestTurnInProgressIsNotReviewedAsIdle(t *testing.T) {
	f := started(t, config.Supervisor{}, onTrack)
	f.mark(t, runlog.MarkWorking, "", f.now)
	f.tick(5 * time.Minute) // a long test run prints nothing
	f.tick(5 * time.Minute)
	if len(f.reviewer.prompts) != 0 {
		t.Fatalf("reviewed %d times while its tool reports a turn in progress", len(f.reviewer.prompts))
	}

	f.tick(config.DefaultScopeEvery)
	if len(f.reviewer.prompts) != 1 || !strings.Contains(f.reviewer.prompts[0], "its tool reports a turn in progress") {
		t.Fatalf("after scope_every it gets its scope check, told that a turn is in progress: %q", f.reviewer.prompts)
	}
}

func TestWorkerWhoseTurnEndedIsReviewedAsBefore(t *testing.T) {
	f := started(t, config.Supervisor{}, Verdict{Verdict: runlog.VerdictDone, Reason: "queue is empty"})
	f.mark(t, runlog.MarkIdle, "", f.now)
	f.tick(2 * time.Minute)
	if len(f.reviewer.prompts) != 1 {
		t.Fatalf("reviewed %d times, want 1", len(f.reviewer.prompts))
	}
}

func TestNewSessionStartsWithNoMarkOfTheLastOne(t *testing.T) {
	f := newFixture(t, config.Supervisor{}, onTrack)
	f.mark(t, runlog.MarkWaiting, "a prompt of the session that ended", t0.Add(-time.Hour))
	f.tick(0)

	if m, ok := runlog.ReadMark(f.StateDir, "app"); ok {
		t.Fatalf("mark = %+v after a new session started", m)
	}
}

// pushTask queues a one-off task on the machine, for a worker or for any
// (where = runlog.TaskAny), as `shed task` does.
func (f *fixture) pushTask(t *testing.T, where, id, brief string) {
	t.Helper()
	path := filepath.Join(f.StateDir, runlog.TasksDir, where, id+".md")
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(brief), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, f.now, f.now); err != nil {
		t.Fatal(err)
	}
}

const taskMessage = "One-off task from the owner, ahead of your queue: read .shed/tasks/t1.md in full and carry it out. When it is done, write your report in .shed/tasks/t1.report.md, say so here, and stop."

var done = Verdict{Verdict: runlog.VerdictDone, Reason: "nothing it can act on"}

func TestRestingWorkerIsHandedAOneOffTaskWithoutAReview(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, done)
	f.tick(2 * time.Minute) // found done; it rests
	f.pushTask(t, runlog.TaskAny, "t1", "# Audit the export\n\nRead only. Report what you find.")
	f.tick(time.Minute)

	if !slices.Equal(f.term.sent, []string{taskMessage}) {
		t.Fatalf("sent = %q", f.term.sent)
	}
	if len(f.reviewer.prompts) != 1 {
		t.Fatalf("reviewed %d times; a resting worker needs no review to be handed a task", len(f.reviewer.prompts))
	}
	if brief, _ := os.ReadFile(filepath.Join(f.dir, TaskDir, "t1.md")); string(brief) != "# Audit the export\n\nRead only. Report what you find." {
		t.Fatalf("the brief in the worker's directory = %q", brief)
	}
	if c := f.lastCheckin(t); c.Kind != KindTask || c.Task != "t1" || !c.Sent {
		t.Fatalf("check-in = %+v", c)
	}
	if _, pending := runlog.PendingTask(f.StateDir, "app"); pending {
		t.Fatal("the task still waits, and would be handed over again")
	}
}

func TestTaskForAnotherWorkerIsLeftForIt(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	f.tick(2 * time.Minute)
	f.pushTask(t, "docs", "t1", "Docs work.")
	f.tick(time.Minute)

	if len(f.term.sent) != 0 {
		t.Fatalf("sent = %q; the task was pushed to another worker", f.term.sent)
	}
}

func TestTaskWaitsForTheIssueInHandThenComesBeforeTheQueue(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12), onTrack, assign(13))
	f.lister.issues = append(f.lister.issues, labeled(13))
	f.tick(2 * time.Minute) // handed #12
	f.pushTask(t, "app", "t1", "Do this next.")
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute) // still on #12: left alone
	if len(f.term.sent) != 1 {
		t.Fatalf("sent = %q; a worker is not interrupted for a task", f.term.sent)
	}

	sent := f.handOver() // it finished #12 and the reviewer would hand over #13
	if !slices.Equal(sent, []string{taskMessage}) {
		t.Fatalf("sent = %q, want the task before the next issue", sent)
	}
}

func TestReviewerJudgesAWorkerAgainstItsTasksBrief(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, done, onTrack)
	f.tick(2 * time.Minute)
	f.pushTask(t, runlog.TaskAny, "t1", "Compare the two export paths and say which to keep.")
	f.tick(time.Minute)
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute)

	prompt := f.reviewer.prompts[len(f.reviewer.prompts)-1]
	want := "<in_hand>a one-off task from the owner (t1), not a queue item. The owner's brief for it:\nCompare the two export paths and say which to keep.\n</in_hand>"
	if !strings.Contains(prompt, want) {
		t.Fatalf("the reviewer was not shown the task:\n%s", prompt)
	}
}

func TestAfterATaskTheWorkerGoesBackToItsQueueOnAClearContext(t *testing.T) {
	f := freshWorker(t, config.Supervisor{CacheTTL: "1h"}, done, assign(12))
	f.tick(2 * time.Minute)
	f.pushTask(t, runlog.TaskAny, "t1", "A task.")
	f.tick(time.Minute)

	sent := f.handOver() // the task is done; the reviewer hands over #12
	if len(sent) != 2 || sent[0] != "/clear" || !strings.HasSuffix(sent[1], "Take the next issue.") {
		t.Fatalf("sent = %q; the task's conversation must not follow the worker into #12", sent)
	}
	if got := runlog.TaskInHand(f.checkins(t), "app"); got != "" {
		t.Fatalf("task in hand = %q after the worker moved on", got)
	}
}

func TestTaskInHandIsRememberedAcrossAnAgentRestart(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, done, onTrack)
	f.tick(2 * time.Minute)
	f.pushTask(t, runlog.TaskAny, "t1", "A task.")
	f.tick(time.Minute)
	f.restartAgent()
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute)

	if len(f.term.sent) != 0 {
		t.Fatalf("sent = %q after a restart", f.term.sent)
	}
	if !strings.Contains(f.reviewer.prompts[len(f.reviewer.prompts)-1], "a one-off task from the owner (t1)") {
		t.Fatal("the new agent does not know the worker has the task in hand")
	}
}

func TestTaskOfASessionThatEndedIsHandedToTheNextOne(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, done, done)
	f.tick(2 * time.Minute)
	f.pushTask(t, runlog.TaskAny, "t1", "A task.")
	f.tick(time.Minute) // handed over
	f.term.exitedToShell(f.now)
	f.tick(time.Minute) // the session ended; a new one is started
	f.term.sent = nil
	f.term.running(f.now)
	f.tick(2 * time.Minute) // the new session is found with nothing to do
	f.tick(time.Minute)

	if !slices.Contains(f.term.sent, taskMessage) {
		t.Fatalf("sent = %q; the task the old session did not finish is handed over again", f.term.sent)
	}
}

func TestBusyMachineHoldsATaskLikeAHandOver(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, done)
	f.Machine.Capacity = config.Capacity{MaxLoad: 1, MaxWait: "20m"}
	f.Busy = func(context.Context) string { return "the load is high" }
	f.tick(2 * time.Minute)
	f.pushTask(t, runlog.TaskAny, "t1", "A task.")
	f.tick(5 * time.Minute)
	if len(f.term.sent) != 0 {
		t.Fatalf("sent = %q on a busy machine", f.term.sent)
	}
	f.tick(20 * time.Minute)
	if !slices.Equal(f.term.sent, []string{taskMessage}) {
		t.Fatalf("sent = %q; after max_wait the task goes through", f.term.sent)
	}
}

func TestTaskPushedWhileAHandOverIsHeldComesBeforeIt(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12))
	busy := busyMachine(f)
	f.tick(2 * time.Minute) // #12 is held
	f.pushTask(t, runlog.TaskAny, "t1", "A task.")
	*busy = false
	f.tick(time.Minute)

	if !slices.Equal(f.term.sent, []string{taskMessage}) {
		t.Fatalf("sent = %q; a task goes ahead of the queue, held or not", f.term.sent)
	}
}

func TestRecycleWaitsForATaskInHand(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, done, onTrack)
	f.tick(2 * time.Minute)
	f.pushTask(t, runlog.TaskAny, "t1", "A task.")
	f.tick(time.Minute)
	f.requestRecycle(t, "")
	f.term.running(f.now.Add(30 * time.Second))
	f.tick(time.Minute)

	if len(f.term.closed) != 0 {
		t.Fatalf("closed = %v; a fresh session waits for the task in hand", f.term.closed)
	}
}

func TestBriefTellsAWorkerAboutOneOffTasks(t *testing.T) {
	f := newFixture(t, config.Supervisor{}, onTrack)
	f.tick(0)
	brief, _ := os.ReadFile(filepath.Join(f.dir, BriefFile))
	for _, want := range []string{"## One-off tasks", "names a file under `.shed/tasks/`", "write a\nreport in the file the supervisor named"} {
		if !strings.Contains(string(brief), want) {
			t.Fatalf("brief lacks %q:\n%s", want, brief)
		}
	}
}

// bump puts an issue first in line, as `shed bump` does: for the named
// worker, or with worker empty for the workers whose queue it is in.
func (f *fixture) bump(t *testing.T, issue int, worker string) string {
	t.Helper()
	path := filepath.Join(f.StateDir, runlog.BumpsDir, fmt.Sprint(issue))
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(worker), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBumpedIssueGoesFirstInTheQueue(t *testing.T) {
	f := started(t, config.Supervisor{}, onTrack)
	f.Machine.Issues[0].Priority = []string{"p0"}
	f.lister.issues = []backlog.Issue{labeled(30), labeled(40, "p0"), handedBackWith(12, "conflicts with the base branch")}
	f.bump(t, 30, "")
	f.tick(2 * time.Minute)

	prompt := f.reviewer.prompts[0]
	if got, want := queueShown(t, prompt), []int{30, 12, 40}; !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
	if !strings.Contains(prompt, "org/app#30 Issue [the owner put this first]") {
		t.Fatalf("the reviewer was not told the owner put #30 first:\n%s", prompt)
	}
}

func TestBumpedIssueIsHandedOverWithTheOwnersWord(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(30), nudge("The test still fails."))
	f.lister.issues = []backlog.Issue{labeled(12), labeled(30)}
	f.bump(t, 30, "")
	f.tick(2 * time.Minute)
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute)

	want := []string{"Take the next issue. The owner put #30 first in your queue: it is yours even where your brief does not cover it.", "The test still fails."}
	if !slices.Equal(f.term.sent, want) {
		t.Fatalf("sent = %q, want %q", f.term.sent, want)
	}
}

// docsQueue gives the second worker a queue of its own.
func docsQueue(f *fixture) {
	f.Machine.Workers[1].Issues = []config.IssueSource{{Repo: "org/app", Label: "area:docs"}}
	f.lister.byLabel = map[string][]backlog.Issue{"machine:box": {labeled(12), labeled(13)}, "area:docs": {labeled(50)}}
}

func TestIssueMovedToAnotherWorkerLeavesTheQueueItWasIn(t *testing.T) {
	f := twoWorkers(t, onTrack)
	docsQueue(f)
	f.bump(t, 13, "docs")
	f.tick(0)

	if got := queueShown(t, f.reviewer.prompts[0]); !slices.Equal(got, []int{12}) {
		t.Fatalf("worker app was shown %v; #13 was moved to docs", got)
	}
	if got := queueShown(t, f.reviewer.prompts[1]); !slices.Equal(got, []int{13, 50}) {
		t.Fatalf("worker docs was shown %v, want the moved issue first", got)
	}
}

func TestIssueAWorkerHasInHandIsNotMovedAway(t *testing.T) {
	f := twoWorkers(t, assign(13), onTrack)
	docsQueue(f)
	f.tick(0) // app takes #13
	path := f.bump(t, 13, "docs")
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute)

	last := f.reviewer.prompts[len(f.reviewer.prompts)-1]
	if got := queueShown(t, last); !slices.Equal(got, []int{50}) {
		t.Fatalf("worker docs was shown %v; #13 is in hand for app", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the bump was dropped while its issue is still open: %v", err)
	}
}

func TestRestingWorkerWakesWhenAnIssueIsMovedToIt(t *testing.T) {
	f := twoWorkers(t, onTrack, done, onTrack, assign(13))
	docsQueue(f)
	f.lister.byLabel["area:docs"] = nil
	f.tick(0) // app works; docs has an empty queue and rests
	f.term.sent = nil
	f.bump(t, 13, "docs")
	f.tick(queueRecheck + time.Minute)

	if len(f.term.sent) != 1 || !strings.HasPrefix(f.term.sent[0], "Take the next issue. The owner put #13 first") {
		t.Fatalf("sent = %q, want #13 handed to docs", f.term.sent)
	}
	if c := f.lastCheckin(t); c.Worker != "docs" || c.Issue != 13 || c.Kind != KindQueue {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestBumpOfAnIssueThatLeftEveryQueueIsDropped(t *testing.T) {
	f := twoWorkers(t, onTrack)
	docsQueue(f)
	path := f.bump(t, 99, "docs") // closed, or its label was removed
	f.tick(0)

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the bump of #99 is still on the machine: %v", err)
	}
	if got := queueShown(t, f.reviewer.prompts[1]); !slices.Equal(got, []int{50}) {
		t.Fatalf("worker docs was shown %v", got)
	}
}

func TestBumpToAWorkerTheMachineDoesNotHaveMovesNothing(t *testing.T) {
	f := started(t, config.Supervisor{}, onTrack)
	f.lister.issues = []backlog.Issue{labeled(12), labeled(30)}
	f.bump(t, 30, "renamed-away")
	f.tick(2 * time.Minute)

	if got := queueShown(t, f.reviewer.prompts[0]); !slices.Equal(got, []int{30, 12}) {
		t.Fatalf("queue = %v; an issue must not drop out of every queue", got)
	}
}

func TestBriefTellsAWorkerAboutBumpedIssues(t *testing.T) {
	f := started(t, config.Supervisor{}, onTrack)
	brief, _ := os.ReadFile(filepath.Join(f.dir, BriefFile))
	if !strings.Contains(string(brief), "The owner may put an issue first in your queue") {
		t.Fatalf("brief:\n%s", brief)
	}
}

func TestReviewerIsToldABumpedIssueIsInScope(t *testing.T) {
	if !strings.Contains(instructions, "An item marked as put first by the owner") {
		t.Fatal("the reviewer would hold a moved issue against the worker's brief")
	}
}

// pause pauses the machine for a while, as `shed pause` does.
func (f *fixture) pause(t *testing.T, d time.Duration) string {
	t.Helper()
	path := filepath.Join(f.StateDir, runlog.PauseFile)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", f.now.Add(d).Unix())), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPausedMachineIsLeftAloneUntilThePauseRunsOut(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12))
	f.pause(t, 30*time.Minute)
	f.tick(2 * time.Minute)
	f.tick(20 * time.Minute)
	if len(f.term.sent) != 0 || len(f.reviewer.prompts) != 0 {
		t.Fatalf("sent %q after %d review(s) on a paused machine", f.term.sent, len(f.reviewer.prompts))
	}

	f.tick(10 * time.Minute)
	if !slices.Equal(f.term.sent, []string{"Take the next issue."}) {
		t.Fatalf("sent = %q; the pause ran out and the idle worker has work", f.term.sent)
	}
}

func TestResumeEndsAPauseEarly(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12))
	path := f.pause(t, 2*time.Hour)
	f.tick(2 * time.Minute)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	f.tick(time.Minute)
	if !slices.Equal(f.term.sent, []string{"Take the next issue."}) {
		t.Fatalf("sent = %q; the owner resumed the machine", f.term.sent)
	}
}

func TestPausedMachineStartsNoWorker(t *testing.T) {
	f := newFixture(t, config.Supervisor{}, onTrack)
	f.pause(t, 30*time.Minute)
	f.tick(0)
	if len(f.term.opened) != 0 || len(f.term.sent) != 0 {
		t.Fatalf("opened %q, sent %q; a session that starts adds to the load", f.term.opened, f.term.sent)
	}
}

func TestBriefNamesNoViewportWhenTheFleetFileHasNone(t *testing.T) {
	f := newFixture(t, config.Supervisor{}, onTrack)
	withStaging(f)
	f.Repos[0].Staging.Viewport = ""
	f.tick(0)

	brief, _ := os.ReadFile(filepath.Join(f.dir, BriefFile))
	if strings.Contains(string(brief), "Viewport: look at") || !strings.Contains(string(brief), "say that size in it") {
		t.Fatalf("brief:\n%s", brief)
	}
}

// resume leaves the request that `shed resume` writes for a worker.
func (f *fixture) resume(t *testing.T, worker string) {
	t.Helper()
	path := filepath.Join(f.StateDir, runlog.ResumeDir, worker)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLimitHoldIsDroppedWhenTheWorkerIsSeenWorkingAgain(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, limited(360), assign(12))
	f.tick(2 * time.Minute)

	f.term.running(f.now.Add(5 * time.Minute)) // the owner reset the usage and told it to continue
	f.tick(6 * time.Minute)
	if c := f.lastCheckin(t); c.Kind != KindLimit || c.Verdict != runlog.VerdictOnTrack {
		t.Fatalf("check-in = %+v; the worker printed after the limit was seen, so the limit is over", c)
	}
	if len(f.reviewer.prompts) != 1 {
		t.Fatalf("reviews = %d; a worker in the middle of its turn needs no review", len(f.reviewer.prompts))
	}

	f.tick(2 * time.Minute) // it finished its turn
	if !slices.Equal(f.term.sent, []string{"Take the next issue."}) {
		t.Fatalf("sent = %q; the worker is no longer held and has work", f.term.sent)
	}
}

func TestDroppedLimitHoldStaysDroppedAcrossAnAgentRestart(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, limited(360), assign(12))
	f.tick(2 * time.Minute)
	f.term.running(f.now.Add(5 * time.Minute))
	f.tick(6 * time.Minute)

	f.restartAgent()
	f.tick(2 * time.Minute)
	if !slices.Equal(f.term.sent, []string{"Take the next issue."}) {
		t.Fatalf("sent = %q; the restart must not bring the hold back", f.term.sent)
	}
}

func TestResumeEndsALimitHoldAndTheReviewerIsToldTheLimitIsOver(t *testing.T) {
	f := started(t, config.Supervisor{}, limited(360), nudge("The usage limit was reset. Continue."))
	f.tick(2 * time.Minute)
	f.term.running(f.term.obs.LastActivity)
	f.tick(3 * time.Hour) // far past the cache lifetime, and the screen has not changed

	f.resume(t, "app")
	f.tick(time.Minute)
	// It was cut off mid-task: its context is kept, however cold.
	if !slices.Equal(f.term.sent, []string{"The usage limit was reset. Continue."}) {
		t.Fatalf("sent = %q, want the message alone with no clear", f.term.sent)
	}
	if last := f.reviewer.prompts[len(f.reviewer.prompts)-1]; !strings.Contains(last, "the owner said the usage limit is over") {
		t.Fatalf("the reviewer was not told why the hold ended:\n%s", last)
	}
	if _, err := os.Stat(filepath.Join(f.StateDir, runlog.ResumeDir, "app")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the request is still there and would end the next hold too: %v", err)
	}
}

func TestResumeOfAWorkerThatIsNotHeldCostsNoReview(t *testing.T) {
	f := started(t, config.Supervisor{}, Verdict{Verdict: runlog.VerdictDone, Reason: "queue is empty"})
	f.tick(2 * time.Minute)
	f.term.running(f.term.obs.LastActivity)
	f.resume(t, "app")
	f.tick(time.Minute)
	if len(f.reviewer.prompts) != 1 || len(f.term.sent) != 0 {
		t.Fatalf("%d review(s), sent %q; a resting worker was at no limit", len(f.reviewer.prompts), f.term.sent)
	}
}

func TestResumeTriesALimitedReviewerAgainAtOnce(t *testing.T) {
	f := started(t, config.Supervisor{}, nudge("The usage limit was reset. Continue."))
	f.reviewer.err = errors.New("reviewer command: exit status 1: You've hit your weekly limit")
	f.tick(2 * time.Minute)

	f.reviewer.err = nil
	f.resume(t, "app")
	f.tick(time.Minute)
	if len(f.reviewer.prompts) != 2 || !strings.Contains(f.reviewer.prompts[1], "the owner said the usage limit is over") {
		t.Fatalf("reviews = %d; the owner said the limit is over, so the worker is looked at now", len(f.reviewer.prompts))
	}
	if !slices.Equal(f.term.sent, []string{"The usage limit was reset. Continue."}) {
		t.Fatalf("sent = %q", f.term.sent)
	}
}

func TestReviewerThatAnswersAgainEndsEveryLimitHold(t *testing.T) {
	done := Verdict{Verdict: runlog.VerdictDone, Reason: "nothing it can act on"}
	f := twoWorkers(t, limited(360), done, done, done, nudge("The usage limit was reset. Continue."))
	f.tick(0) // app is found at its limit and held; docs rests

	// The queue changes, so docs is looked at: now the reviewer is limited too.
	f.lister.issues = append(f.lister.issues, backlog.Issue{Repo: "org/app", Number: 13, Title: "Export the audit log"})
	f.reviewer.err = errors.New("reviewer command: exit status 1: You've hit your weekly limit")
	f.tick(queueRecheck)
	if c := f.lastCheckin(t); c.Worker != "docs" || c.Verdict != runlog.VerdictLimited {
		t.Fatalf("check-in = %+v, want the reviewer limited on docs", c)
	}

	f.reviewer.err = nil
	f.tick(limitRecheck) // docs is tried again and the reviewer answers
	f.tick(time.Minute)
	if !slices.Equal(f.term.sent, []string{"The usage limit was reset. Continue."}) {
		t.Fatalf("sent = %q; the limit that stopped the reviewer is the account's, and it is over for app too", f.term.sent)
	}
	if last := f.reviewer.prompts[len(f.reviewer.prompts)-1]; !strings.Contains(last, "<worker>app</worker>") || !strings.Contains(last, "the reviewer answers again") {
		t.Fatalf("the reviewer was not told why app's hold ended:\n%s", last)
	}
}

func TestLimitMessageNamesTheLatestTimeNotThePromisedOne(t *testing.T) {
	prompt := Prompt(ReviewInput{Worker: "app", Now: t0, LimitedUntil: t0.Add(-time.Minute)})
	if !strings.Contains(prompt, "at the latest") {
		t.Fatalf("the reviewer must know that a limit may end before the time its screen names:\n%s", prompt)
	}
}

func toolError(message string) Verdict {
	return Verdict{Verdict: runlog.VerdictToolError, Message: message, Reason: "the screen shows an API error 403"}
}

func TestToolErrorIsRetriedAfterABackoffWithNoPerson(t *testing.T) {
	f := started(t, config.Supervisor{}, toolError("Continue from where you stopped."), onTrack)
	var notes []string
	f.Notify = func(worker, message string) { notes = append(notes, message) }
	f.tick(2 * time.Minute)
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictToolError || !c.Until.Equal(f.now.Add(5*time.Minute)) {
		t.Fatalf("check-in = %+v, want tool_error with a retry due in 5 minutes", c)
	}

	f.tick(4 * time.Minute)
	if len(f.term.sent) != 0 {
		t.Fatalf("sent = %q before the backoff was over", f.term.sent)
	}
	f.tick(2 * time.Minute)
	if !slices.Equal(f.term.sent, []string{"Continue from where you stopped."}) {
		t.Fatalf("sent = %q, want the retry alone with no clear", f.term.sent)
	}
	if c := f.lastCheckin(t); c.Kind != KindRetry || !c.Sent {
		t.Fatalf("check-in = %+v, want a sent retry", c)
	}
	if len(f.reviewer.prompts) != 1 || len(notes) != 0 {
		t.Fatalf("%d review(s), notifications %q; a retry needs neither a review nor the owner", len(f.reviewer.prompts), notes)
	}
}

func TestToolErrorNeedsTheOwnerOnlyAfterTheLastRetryFails(t *testing.T) {
	f := started(t, config.Supervisor{}, toolError("Continue."))
	var waits []time.Duration
	for range len(retryBackoff) {
		f.tick(2 * time.Minute) // silent again: reviewed, the error is still there
		c := f.lastCheckin(t)
		if c.Verdict != runlog.VerdictToolError {
			t.Fatalf("check-in = %+v, want tool_error", c)
		}
		waits = append(waits, c.Until.Sub(f.now))
		f.tick(c.Until.Sub(f.now)) // the retry is sent
		f.term.running(f.now)      // and the error is printed again
	}
	if !slices.Equal(waits, retryBackoff) {
		t.Fatalf("waits = %v, want %v", waits, retryBackoff)
	}
	if len(f.term.sent) != len(retryBackoff) {
		t.Fatalf("sent = %q, want %d retries", f.term.sent, len(retryBackoff))
	}

	f.tick(2 * time.Minute)
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictNeedsOwner || !strings.Contains(c.Reason, "3 retries") {
		t.Fatalf("check-in = %+v, want needs_owner after the last retry failed", c)
	}
	f.tick(time.Hour)
	if len(f.term.sent) != len(retryBackoff) {
		t.Fatalf("sent = %q; nothing is typed once the owner is needed", f.term.sent)
	}
}

func TestToolErrorIsRetriedAtOnceWhenAnotherWorkerIsSeenWorking(t *testing.T) {
	f := twoWorkers(t, toolError("Continue."), onTrack)
	f.tick(0) // app shows the error; docs is on track, after the error
	if len(f.term.sent) != 0 {
		t.Fatalf("sent = %q", f.term.sent)
	}
	f.tick(30 * time.Second)
	if !slices.Equal(f.term.sent, []string{"Continue."}) {
		t.Fatalf("sent = %q, want the retry at once: the account works for the other worker", f.term.sent)
	}
}

func TestToolErrorThatClearsByItselfIsNotRetried(t *testing.T) {
	f := started(t, config.Supervisor{}, toolError("Continue."), onTrack)
	f.tick(2 * time.Minute)
	f.term.running(f.now.Add(time.Minute)) // it went on, or somebody typed
	f.tick(6 * time.Minute)
	if len(f.term.sent) != 0 {
		t.Fatalf("sent = %q; the worker moved, so it is reviewed, not retried", f.term.sent)
	}
	if len(f.reviewer.prompts) != 2 {
		t.Fatalf("reviews = %d, want 2", len(f.reviewer.prompts))
	}
}

func TestPendingRetryAndItsCountSurviveAnAgentRestart(t *testing.T) {
	f := started(t, config.Supervisor{}, toolError("Continue."))
	f.tick(2 * time.Minute)
	f.restartAgent()
	f.tick(4 * time.Minute)
	if f.reviews() != 0 || len(f.term.sent) != 0 {
		t.Fatalf("after a restart: %d review(s), sent %q; the retry is not due", f.reviews(), f.term.sent)
	}
	f.tick(2 * time.Minute)
	if !slices.Equal(f.term.sent, []string{"Continue."}) {
		t.Fatalf("sent = %q, want the retry", f.term.sent)
	}

	f.term.running(f.now)
	f.restartAgent()
	f.tick(2 * time.Minute)
	if c := f.lastCheckin(t); !c.Until.Equal(f.now.Add(retryBackoff[1])) {
		t.Fatalf("check-in = %+v, want the second backoff: the first retry is remembered", c)
	}
}

// asksADecision is an issue whose last hand-back asked the owner a question
// at the time given.
func asksADecision(number int, at time.Time, more ...string) backlog.Issue {
	i := labeled(number)
	i.Comments = []backlog.Comment{{Body: "## Hand-back\n**PR:** none\n**Decisions needed:**\n1. Drop the legacy column?", CreatedAt: at.UTC()}}
	for _, body := range more {
		i.Comments = append(i.Comments, backlog.Comment{Body: body})
	}
	return i
}

func TestDecisionKeepsItsWorkerUntilTheGracePeriodIsOverThenParks(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12), onTrack, assign(13))
	f.lister.issues = []backlog.Issue{labeled(12), labeled(13)}
	f.tick(2 * time.Minute) // it takes #12

	f.lister.issues = []backlog.Issue{asksADecision(12, f.now.Add(time.Minute)), labeled(13)}
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute) // silent after its question: reviewed, 2 minutes into the 15
	if prompt := f.reviewer.prompts[1]; !strings.Contains(prompt, "#12 Issue [waits on the owner: decision; not parked yet: the owner has until 15:18 PDT to answer]") {
		t.Fatalf("the reviewer was not told that the clock runs:\n%s", prompt)
	}

	f.tick(10 * time.Minute)
	if len(f.reviewer.prompts) != 2 || len(f.term.sent) != 1 {
		t.Fatalf("inside the grace period: %d review(s), sent %q; want it left on #12", len(f.reviewer.prompts), f.term.sent)
	}

	f.tick(6 * time.Minute) // the owner did not answer in time
	if len(f.reviewer.prompts) != 3 || !strings.Contains(f.reviewer.prompts[2], "[waits on the owner: decision; parked since 15:18 PDT]") {
		t.Fatalf("after the grace period: %d review(s); want one that shows #12 parked", len(f.reviewer.prompts))
	}
	if c := f.lastCheckin(t); c.Issue != 13 || !c.Sent {
		t.Fatalf("check-in = %+v, want #13 handed over", c)
	}
}

// A hand-back's time comes from GitHub in UTC. The reviewer reads the
// machine's clock, so the deadline is written on that clock.
func TestParkDeadlineIsShownOnTheReviewersClock(t *testing.T) {
	west := time.FixedZone("PDT", -7*60*60)
	parksAt := time.Date(2026, 10, 4, 23, 18, 0, 0, time.UTC)
	queue := []QueueItem{{Repo: "org/app", Number: 12, Title: "Issue", Waiting: []string{"decision"}, ParksAt: parksAt}}

	before := Prompt(ReviewInput{Queue: queue, Now: parksAt.Add(-12 * time.Minute).In(west)})
	if !strings.Contains(before, "; not parked yet: the owner has until 16:18 PDT to answer]") {
		t.Fatalf("the deadline is not on the reviewer's clock:\n%s", before)
	}

	after := Prompt(ReviewInput{Queue: queue, Now: parksAt.Add(4 * time.Hour).In(west)})
	if !strings.Contains(after, "[waits on the owner: decision; parked since 16:18 PDT]") {
		t.Fatalf("the reviewer was not told since when #12 is parked:\n%s", after)
	}
}

func TestCheckinTimesAreShownOnTheReviewersClock(t *testing.T) {
	west := time.FixedZone("PDT", -7*60*60)
	at := time.Date(2026, 10, 4, 23, 6, 0, 0, time.UTC)

	prompt := Prompt(ReviewInput{Now: at.Add(time.Hour).In(west), Recent: []runlog.Checkin{{Time: at, Verdict: runlog.VerdictOnTrack, Reason: "it waits"}}})
	if !strings.Contains(prompt, "16:06 on_track: it waits") {
		t.Fatalf("the check-in is not on the reviewer's clock:\n%s", prompt)
	}
}

func TestReviewerThatLeavesAWorkerOnAParkedIssueIsAskedAgain(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12), onTrack, onTrack, assign(13))
	f.lister.issues = []backlog.Issue{labeled(12), labeled(13)}
	f.tick(2 * time.Minute) // it takes #12

	f.lister.issues = []backlog.Issue{asksADecision(12, f.now.Add(time.Minute)), labeled(13)}
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute)
	f.tick(16 * time.Minute) // #12 is parked, and the reviewer hands over nothing
	if len(f.reviewer.prompts) != 3 || len(f.term.sent) != 1 {
		t.Fatalf("at the end of the grace period: %d review(s), sent %q; want a review that hands over nothing", len(f.reviewer.prompts), f.term.sent)
	}

	f.tick(10 * time.Minute)
	if len(f.reviewer.prompts) != 3 {
		t.Fatalf("%d review(s) 10 minutes later; want the reviewer asked no more often than for a busy worker", len(f.reviewer.prompts))
	}

	f.tick(20 * time.Minute)
	if c := f.lastCheckin(t); c.Issue != 13 || !c.Sent {
		t.Fatalf("check-in = %+v, want the reviewer asked again and #13 handed over", c)
	}
}

func TestIssueTheOwnerRuledOnComesBeforeNewWork(t *testing.T) {
	f := started(t, config.Supervisor{}, onTrack)
	f.Machine.Issues[0].Priority = []string{"p0"}
	f.lister.issues = []backlog.Issue{labeled(5, "p0"), asksADecision(12, t0, "Owner ruling: drop it."), labeled(7)}
	f.tick(2 * time.Minute)

	prompt := f.reviewer.prompts[0]
	if got := queueShown(t, prompt); !slices.Equal(got, []int{12, 5, 7}) {
		t.Fatalf("queue = %v, want the ruled issue first", got)
	}
	if !strings.Contains(prompt, "#12 Issue [the owner answered its question") {
		t.Fatalf("the reviewer was not told of the answer:\n%s", prompt)
	}
}

func TestBriefSaysWhichChoicesAreTheWorkersOwn(t *testing.T) {
	f := newFixture(t, config.Supervisor{})
	f.Signals[0].Risky = "anything that touches billing"
	f.Signals[0].Grace = "10m"
	brief := f.briefText(f.Machine.Workers[0])
	for _, part := range []string{"Risky: anything that touches billing.", "The owner has 10m to answer", "after the words `Decided without you:`", "Safe: every other choice."} {
		if !strings.Contains(brief, part) {
			t.Fatalf("brief lacks %q:\n%s", part, brief)
		}
	}
	// The reviewer judges a worker that stopped against the same words.
	if prompt := Prompt(ReviewInput{Brief: f.scope(f.Machine.Workers[0])}); !strings.Contains(prompt, "anything that touches billing") || !strings.Contains(prompt, "this is a safe default") {
		t.Fatal("the reviewer is not told which choices are risky, or what to say to a worker that stopped on a safe one")
	}
}

type fakeClaimer struct {
	claimed []string
	err     error
}

func (f *fakeClaimer) Claim(_ context.Context, repo string, number int, label string) error {
	if f.err == nil {
		f.claimed = append(f.claimed, fmt.Sprintf("%s#%d %s", repo, number, label))
	}
	return f.err
}

// withBackfill gives the machine a backfill source. The labelled queue holds
// the issues given; the backlog holds #50 and #51, and #52, which another
// machine of the fleet took.
func withBackfill(f *fixture, queue ...backlog.Issue) *fakeClaimer {
	claimer := &fakeClaimer{}
	f.Claimer, f.QueueLabels = claimer, []string{"machine:box", "machine:other"}
	f.Machine.Issues = append(f.Machine.Issues, config.IssueSource{Repo: "org/app", Query: `label:"p1" no:assignee`, Backfill: true, Priority: []string{"p1"}})
	f.lister.issues = nil
	f.lister.byLabel = map[string][]backlog.Issue{
		"machine:box": queue,
		"":            {labeled(51), labeled(50, "p1"), labeled(52, "p1", "machine:other")},
	}
	return claimer
}

func TestWorkerWithNothingWorkableIsHandedAnIssueFromTheBacklog(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(50))
	claimer := withBackfill(f, asksADecision(12, t0.Add(-time.Hour)))
	f.tick(2 * time.Minute)

	prompt := f.reviewer.prompts[0]
	if got := queueShown(t, prompt); !slices.Equal(got, []int{12, 50, 51}) {
		t.Fatalf("queue = %v, want the parked issue, then the backlog by priority, without the one another machine took", got)
	}
	if !strings.Contains(prompt, "#50 Issue [backlog]") {
		t.Fatalf("the reviewer was not told where #50 comes from:\n%s", prompt)
	}
	if !slices.Equal(claimer.claimed, []string{"org/app#50 machine:box"}) {
		t.Fatalf("claimed = %q, want #50 put in this machine's queue", claimer.claimed)
	}
	c := f.lastCheckin(t)
	if c.Issue != 50 || !c.Backfill || !c.Sent || !strings.Contains(c.Message, "#50 is from the backlog") {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestBacklogIsNotTouchedWhileTheQueueHasWork(t *testing.T) {
	f := started(t, config.Supervisor{}, assign(12))
	claimer := withBackfill(f, labeled(12))
	f.tick(2 * time.Minute)

	if got := queueShown(t, f.reviewer.prompts[0]); !slices.Equal(got, []int{12}) {
		t.Fatalf("queue = %v, want the labelled issue alone", got)
	}
	if len(claimer.claimed) != 0 {
		t.Fatalf("claimed = %q", claimer.claimed)
	}
}

func TestBacklogIssueThatCannotBeMarkedIsNotHandedOver(t *testing.T) {
	f := started(t, config.Supervisor{}, assign(50))
	withBackfill(f).err = errors.New("gh issue edit: HTTP 403")
	f.tick(2 * time.Minute)

	if len(f.term.sent) != 0 {
		t.Fatalf("sent = %q; an issue that is not marked could be taken twice", f.term.sent)
	}
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictError {
		t.Fatalf("check-in = %+v, want the failure recorded", c)
	}
}

func TestBacklogIsHandedOutOnlySoOftenInAnHour(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(50), assign(51), assign(50), assign(51), Verdict{Verdict: runlog.VerdictDone, Reason: "nothing workable"})
	withBackfill(f)
	for range runlog.BackfillLimit {
		f.term.running(f.now) // it said the issue is not ready, and stopped
		f.tick(2 * time.Minute)
	}
	if len(f.term.sent) != runlog.BackfillLimit {
		t.Fatalf("sent = %q, want %d hand-overs", f.term.sent, runlog.BackfillLimit)
	}

	f.term.running(f.now)
	f.tick(2 * time.Minute)
	if got := queueShown(t, f.reviewer.prompts[runlog.BackfillLimit]); len(got) != 0 {
		t.Fatalf("queue = %v; the backlog is closed to this worker for the rest of the hour", got)
	}
}

func TestBriefTellsAWorkerHowToSayABacklogIssueIsNotReady(t *testing.T) {
	f := newFixture(t, config.Supervisor{})
	if brief := f.briefText(f.Machine.Workers[0]); strings.Contains(brief, "backlog") {
		t.Fatalf("a worker with no backfill source is told of a backlog:\n%s", brief)
	}
	withBackfill(f)
	brief := f.briefText(f.Machine.Workers[0])
	for _, part := range []string{"that match `label:\"p1\" no:assignee`", "post one comment headed `Hand-back` with `Not ready:` and what is missing"} {
		if !strings.Contains(brief, part) {
			t.Fatalf("brief lacks %q:\n%s", part, brief)
		}
	}
}

func TestOwnerIsToldOnceWhenAWorkerHasHadNoWorkForTooLong(t *testing.T) {
	f := started(t, config.Supervisor{NoWorkAfter: "30m"}, Verdict{Verdict: runlog.VerdictDone, Reason: "every item waits on the owner"})
	f.lister.issues = []backlog.Issue{asksADecision(12, t0.Add(-time.Hour))}
	var notes []string
	f.Notify = func(worker, message string) { notes = append(notes, message) }

	f.tick(2 * time.Minute) // found with nothing to do
	f.tick(20 * time.Minute)
	if len(notes) != 0 {
		t.Fatalf("notifications = %q before the time set", notes)
	}
	f.tick(15 * time.Minute)
	f.tick(15 * time.Minute)
	if want := []string{"box: worker app has had no work for 35m: 1 issue(s) of its queue wait on you"}; !slices.Equal(notes, want) {
		t.Fatalf("notifications = %q, want %q", notes, want)
	}
}

func TestOwnerIsToldOnceWhenAQueueRunsShort(t *testing.T) {
	f := started(t, config.Supervisor{LowQueue: 2}, onTrack)
	f.lister.issues = []backlog.Issue{labeled(12), labeled(13), asksADecision(14, t0)}
	var notes []string
	f.Notify = func(worker, message string) { notes = append(notes, message) }

	f.tick(2 * time.Minute)
	if len(notes) != 0 {
		t.Fatalf("notifications = %q with two issues to work", notes)
	}
	f.lister.issues = f.lister.issues[1:]
	for range 3 {
		f.tick(queueRecheck)
	}
	if want := []string{"box: the queue of worker app has 1 issue(s) a worker can act on, fewer than 2; 1 wait on you"}; !slices.Equal(notes, want) {
		t.Fatalf("notifications = %q, want %q", notes, want)
	}
}
