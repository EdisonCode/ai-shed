package supervisor

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

type release struct {
	repo    string
	number  int
	remove  []string
	add     string
	comment string
}

type fakeReleaser struct{ released []release }

func (f *fakeReleaser) Release(_ context.Context, repo string, number int, remove []string, add, comment string) error {
	f.released = append(f.released, release{repo, number, remove, add, comment})
	return nil
}

var weeklyLimit = Verdict{Verdict: runlog.VerdictLimited, ResumeInMinutes: 3000, Reason: "it reached its weekly limit"}

// spent gives the fixture an account that releases to fleet:ready, and has
// its worker take #12 and then reach a weekly limit. A progress comment
// landed after the hand-over unless bare.
func spentMachine(t *testing.T, bare bool, verdicts ...Verdict) (*fixture, *fakeReleaser) {
	f := started(t, config.Supervisor{}, append([]Verdict{assign(12), weeklyLimit}, verdicts...)...)
	f.Account = &config.Account{Name: "max-1", Machines: []string{"box"}, Overflow: "fleet:ready"}
	releaser := &fakeReleaser{}
	f.Releaser = releaser
	f.lister.issues = []backlog.Issue{labeled(12, "machine:box")}
	f.tick(2 * time.Minute) // #12 is handed over
	if !bare {
		f.lister.issues[0].Comments = []backlog.Comment{{Body: "## Plan\n1. Add the column.", CreatedAt: f.now.Add(time.Minute)}}
	}
	f.term.screen = "You've hit your weekly limit · resets Oct 14, 6pm"
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute) // the review finds the weekly limit
	return f, releaser
}

func TestAWorkerAtItsWeeklyLimitReleasesItsIssueToTheFleet(t *testing.T) {
	f, releaser := spentMachine(t, false)
	f.tick(time.Minute)
	if len(releaser.released) != 1 {
		t.Fatalf("released = %+v, want #12 released once", releaser.released)
	}
	r := releaser.released[0]
	if r.number != 12 || !slices.Equal(r.remove, []string{"machine:box"}) || r.add != "fleet:ready" || !strings.Contains(r.comment, "Released: box/app, weekly limit") {
		t.Fatalf("release = %+v", r)
	}
	if !slices.Contains(f.term.sent, "/clear") {
		t.Fatalf("sent = %q, want the worker's context cleared: the issue is no longer its own", f.term.sent)
	}
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictReleased || c.Issue != 12 {
		t.Fatalf("check-in = %+v", c)
	}
	if issue := runlog.IssueInHand(f.checkins(t), "app"); issue != 0 {
		t.Fatalf("in hand = #%d after the release", issue)
	}
	f.tick(time.Minute)
	if len(releaser.released) != 1 {
		t.Fatalf("released %d times", len(releaser.released))
	}
}

func TestAWorkerWhoseWorkLivesOnlyInItsContextKeepsTheIssue(t *testing.T) {
	f, releaser := spentMachine(t, true)
	f.tick(time.Minute)
	if len(releaser.released) != 0 {
		t.Fatalf("released = %+v: nothing of the work is outside the session", releaser.released)
	}
}

func TestAFiveHourLimitIsWaitedOut(t *testing.T) {
	f, releaser := spentMachine(t, false)
	f.term.screen = "You've hit your 5-hour limit · resets 6pm"
	f.workers["app"].recent[len(f.workers["app"].recent)-1].Limit = runlog.LimitFiveHour
	f.tick(time.Minute)
	if len(releaser.released) != 0 {
		t.Fatalf("released = %+v at a 5-hour limit", releaser.released)
	}
}

func TestWithoutOverflowNothingIsReleased(t *testing.T) {
	f, releaser := spentMachine(t, false)
	f.Account.Overflow = ""
	f.tick(time.Minute)
	if len(releaser.released) != 0 {
		t.Fatalf("released = %+v with no overflow label", releaser.released)
	}
}

func TestAReleasedIssueIsTakenFromTheFleetPoolAndTheWorkerIsToldItWasReleased(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	claimer := &fakeClaimer{}
	f.Claimer, f.QueueLabels = claimer, []string{"machine:box", "machine:mini"}
	f.Machine.Issues = append(f.Machine.Issues, config.IssueSource{Repo: "org/app", Label: "fleet:ready", Backfill: true})
	released := labeled(12, "fleet:ready")
	released.Comments = []backlog.Comment{{Body: "Released: mini/app, weekly limit (resets Wed 18:00)."}}
	f.lister.byLabel = map[string][]backlog.Issue{"machine:box": nil, "fleet:ready": {released}}
	f.tick(2 * time.Minute)
	f.tick(10 * time.Minute)
	if !slices.Equal(claimer.claimed, []string{"org/app#12 +machine:box -fleet:ready"}) {
		t.Fatalf("claimed = %q, want the pool label swapped for the machine's", claimer.claimed)
	}
	c := f.lastCheckin(t)
	if c.Issue != 12 || !strings.Contains(c.Message, "#12 was released by mini/app") || strings.Contains(c.Message, "is from the backlog") {
		t.Fatalf("check-in = %+v", c)
	}
}

func TestAnIssueThatOverlapsWorkInFlightOnAnotherMachineIsHeld(t *testing.T) {
	f := started(t, config.Supervisor{OverlapLabels: "area:"}, done)
	f.Account = &config.Account{Name: "max-1", Machines: []string{"box"}, Overflow: "fleet:ready"}
	f.QueueLabels = []string{"machine:box", "machine:mini"}
	elsewhere := labeled(20, "machine:mini", "area:billing")
	elsewhere.Comments = []backlog.Comment{{Body: "## Plan\n**Paths:** internal/billing/"}}
	handedBack := labeled(21, "machine:mini", "area:auth")
	handedBack.Comments = []backlog.Comment{{Body: "## Plan\n**Paths:** internal/auth/"}, {Body: "## Hand-back\n**PR:** none"}}
	f.lister.byLabel = map[string][]backlog.Issue{
		"machine:box": {labeled(11, "machine:box", "area:billing"), labeled(12, "machine:box", "area:auth")},
		"":            {elsewhere, handedBack}, // the search for other machines' labels
	}
	f.tick(2 * time.Minute)
	prompt := f.reviewer.prompts[0]
	if !strings.Contains(prompt, "org/app#11 Issue [held: overlaps #20, in flight on another machine: both are area:billing]") {
		t.Fatalf("prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "org/app#12 Issue [held:") {
		t.Fatalf("#12 is held by work another machine already handed back:\n%s", prompt)
	}
}
