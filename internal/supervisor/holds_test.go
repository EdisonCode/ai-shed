package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// dependsOn is an issue that waits for another one.
func dependsOn(number, on int, open bool) backlog.Issue {
	i := labeled(number)
	i.Deps = []backlog.Dep{{Repo: "org/app", Number: on, Open: open}}
	return i
}

func TestAnIssueWaitsForItsDependencyToClose(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	f.lister.issues = []backlog.Issue{dependsOn(11, 12, true), labeled(12)}
	f.tick(2 * time.Minute)
	if prompt := f.reviewer.prompts[0]; !strings.Contains(prompt, "org/app#11 Issue [held: waits for #12 to close]") {
		t.Fatalf("prompt does not show the hold:\n%s", prompt)
	}
	f.tick(10 * time.Minute)
	if c := f.lastCheckin(t); c.Issue != 12 {
		t.Fatalf("check-in = %+v, want #12 handed over ahead of #11, which waits for it", c)
	}
}

func TestADependencyThatNamesNothingHoldsOnlyItsIssue(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	missing := labeled(11)
	missing.Deps = []backlog.Dep{{Repo: "org/app", Number: 6300, Missing: true}}
	f.lister.issues = []backlog.Issue{missing, labeled(12)}
	f.tick(2 * time.Minute)
	if prompt := f.reviewer.prompts[0]; !strings.Contains(prompt, "org/app#11 Issue [held: depends on #6300, which is not an issue or pull request]") {
		t.Fatalf("prompt does not show the hold:\n%s", prompt)
	}
	f.tick(10 * time.Minute)
	if c := f.lastCheckin(t); c.Issue != 12 {
		t.Fatalf("check-in = %+v, want #12 handed over", c)
	}
}

func TestAClosedDependencyHoldsNothing(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	f.lister.issues = []backlog.Issue{dependsOn(11, 12, false), labeled(13)}
	f.tick(2 * time.Minute)
	f.tick(10 * time.Minute)
	if c := f.lastCheckin(t); c.Issue != 11 {
		t.Fatalf("check-in = %+v, want #11", c)
	}
}

func TestWithStackingAnIssueIsBuiltOnItsDependencysReadyPullRequest(t *testing.T) {
	f := started(t, config.Supervisor{Stack: true}, done)
	f.lister.issues = []backlog.Issue{dependsOn(11, 12, true), handedBackWith(12, "")}
	f.tick(2 * time.Minute)
	prompt := f.reviewer.prompts[0]
	if want := "org/app#11 Issue [stack on: #12's pull request #41 waits for review; build this on a new branch from it]"; !strings.Contains(prompt, want) {
		t.Fatalf("prompt lacks %q:\n%s", want, prompt)
	}
	f.tick(10 * time.Minute)
	c := f.lastCheckin(t)
	if want := "Start #11 now. It needs #12, whose pull request #41 waits for review: build it on a new branch from that pull request's branch, and open its pull request against that branch."; c.Issue != 11 || !strings.HasSuffix(c.Message, want) {
		t.Fatalf("check-in = %+v, want the message %q", c, want)
	}
}

func TestAnIssueThatOverlapsAnotherWorkersIssueIsHeld(t *testing.T) {
	f := started(t, config.Supervisor{OverlapLabels: "area:"}, done)
	f.Machine.Workers = append(f.Machine.Workers, config.Worker{Name: "docs", Dir: t.TempDir(), Brief: "b"})
	appendCheckin(t, f, runlog.Checkin{Time: f.now, Worker: "docs", Kind: KindIdle, Verdict: runlog.VerdictNudge, Sent: true, Issue: 12})
	paths := labeled(13)
	paths.Body = "**Paths:** internal/billing/invoice.go"
	inHand := labeled(12, "area:billing")
	inHand.Comments = []backlog.Comment{{Body: "## Plan\n**Paths:** internal/billing/"}}
	f.lister.issues = []backlog.Issue{labeled(11, "area:billing"), inHand, paths, labeled(14, "area:auth")}
	f.tick(2 * time.Minute)
	prompt := f.reviewer.prompts[0]
	for _, want := range []string{
		"org/app#11 Issue [held: overlaps #12, which docs has in hand: both are area:billing]",
		"org/app#13 Issue [held: overlaps #12, which docs has in hand: both touch internal/billing/]",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	f.tick(10 * time.Minute)
	var last runlog.Checkin
	for _, c := range f.checkins(t) {
		if c.Worker == "app" {
			last = c
		}
	}
	if last.Issue != 14 {
		t.Fatalf("app's check-in = %+v, want #14, the one that touches nothing in flight", last)
	}
}

func TestTheIssueAWorkerHasInHandIsNeverHeld(t *testing.T) {
	f := started(t, config.Supervisor{}, assign(11), onTrack)
	f.lister.issues = []backlog.Issue{labeled(11)}
	f.tick(2 * time.Minute) // #11 is handed over
	f.lister.issues = []backlog.Issue{dependsOn(11, 12, true)}
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute)
	if prompt := f.reviewer.prompts[1]; strings.Contains(prompt, "[held:") {
		t.Fatalf("the issue in hand is shown held:\n%s", prompt)
	}
}

func TestBriefAsksForThePathsWithThePlanAndSaysHowToNameADependency(t *testing.T) {
	f := newFixture(t, config.Supervisor{})
	brief := f.briefText(f.Machine.Workers[0])
	for _, want := range []string{"**Paths:**", "Depends on #"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("brief lacks %q:\n%s", want, brief)
		}
	}
}

func TestBriefAsksForLessonsAndLeavesTheMemoryToTheOwner(t *testing.T) {
	f := newFixture(t, config.Supervisor{})
	brief := f.briefText(f.Machine.Workers[0])
	if !strings.Contains(brief, "`**Lessons:**`") || !strings.Contains(brief, "Do not edit the repository's instructions or any memory") {
		t.Fatalf("brief lacks the lessons rule:\n%s", brief)
	}
}

// atLimit gives each issue the repository's open pull requests: #41 and #42,
// the limit of org/app.
func atLimit(f *fixture, issues ...backlog.Issue) {
	f.Repos = []config.Repo{{Name: "org/app", MaxOpenPRs: 2}}
	for n := range issues {
		if issues[n].OpenPRs == nil {
			issues[n].OpenPRs = map[int]backlog.PR{}
		}
		for _, pr := range []int{41, 42} {
			if _, ok := issues[n].OpenPRs[pr]; !ok {
				issues[n].OpenPRs[pr] = backlog.PR{}
			}
		}
	}
	f.lister.issues = issues
}

func TestANewIssueIsHeldWhileItsRepositoryIsAtItsOpenPullRequestLimit(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	atLimit(f, labeled(11))
	f.tick(2 * time.Minute)
	if prompt := f.reviewer.prompts[0]; !strings.Contains(prompt, "org/app#11 Issue [held: org/app has 2 open pull requests, at or over its limit of 2: they are finished first]") {
		t.Fatalf("prompt does not show the hold:\n%s", prompt)
	}
	f.tick(15 * time.Minute)
	for _, c := range f.checkins(t) {
		if c.Issue == 11 {
			t.Fatalf("check-in = %+v: #11 was handed over while the repository is at its limit", c)
		}
	}
}

func TestReworkGoesOnWhileItsRepositoryIsAtItsOpenPullRequestLimit(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"},
		Verdict{Verdict: runlog.VerdictNudge, Message: "PR #41 conflicts with main. Rebase it and nothing else.", Issue: 12, Reason: "its pull request went stale"})
	atLimit(f, labeled(5), handedBackWith(12, "conflicts with the base branch"))
	f.tick(2 * time.Minute)
	prompt := f.reviewer.prompts[0]
	if !strings.Contains(prompt, "org/app#5 Issue [held: org/app has 2 open pull requests") || strings.Contains(prompt, "org/app#12 Issue [rework: pull request #41 conflicts with the base branch] [held:") {
		t.Fatalf("want #5 held and the rework not:\n%s", prompt)
	}
	if c := f.lastCheckin(t); c.Issue != 12 || !c.Sent {
		t.Fatalf("check-in = %+v, want the rework of #12 handed over", c)
	}
}
