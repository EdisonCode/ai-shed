package supervisor

import (
	"context"
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
	if prompt := f.reviewer.prompts[0]; !strings.Contains(prompt, "org/app#11 Issue [held: waits for #12 to close (GitHub's blocked-by)]") {
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
	if prompt := f.reviewer.prompts[0]; !strings.Contains(prompt, "org/app#11 Issue [held: depends on #6300, which is not an issue or pull request (GitHub's blocked-by)]") {
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

func TestTheReserveIsOfferedWhenTheQueueIsHeldOrReworkOnCodeInHand(t *testing.T) {
	f := started(t, config.Supervisor{OverlapLabels: "area:"}, done)
	f.Machine.Workers = append(f.Machine.Workers, config.Worker{Name: "docs", Dir: t.TempDir(), Brief: "b"})
	appendCheckin(t, f, runlog.Checkin{Time: f.now, Worker: "docs", Kind: KindIdle, Verdict: runlog.VerdictNudge, Sent: true, Issue: 12})
	rework := handedBackWith(13, "has a failed check (test)")
	rework.Labels = append(rework.Labels, backlog.Label{Name: "area:billing"})
	withBackfill(f, labeled(11, "area:billing"), labeled(12, "area:billing"), rework)
	f.tick(2 * time.Minute)
	prompt := f.reviewer.prompts[0]
	for _, want := range []string{"org/app#11 Issue [held: overlaps #12", "#50 Issue [backlog]"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
}

func TestTheReserveIsNotOfferedBesideReworkAWorkerCanDo(t *testing.T) {
	f := started(t, config.Supervisor{OverlapLabels: "area:"}, done)
	withBackfill(f, handedBackWith(13, "has a failed check (test)"))
	f.tick(2 * time.Minute)
	if prompt := f.reviewer.prompts[0]; strings.Contains(prompt, "[backlog]") {
		t.Fatalf("the reserve is offered beside rework a worker can do:\n%s", prompt)
	}
}

// keptQueue reads the queue the agent kept for the worker.
func keptQueue(t *testing.T, f *fixture, worker string) runlog.QueueView {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.StateDir, runlog.QueuesDir, worker))
	if err != nil {
		t.Fatal(err)
	}
	v, err := runlog.ParseQueue(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTheAgentKeepsEachWorkersQueueWithWhyItHoldsAnItem(t *testing.T) {
	f := started(t, config.Supervisor{OverlapLabels: "area:"}, done)
	f.Machine.Workers = append(f.Machine.Workers, config.Worker{Name: "docs", Dir: t.TempDir(), Brief: "b"})
	appendCheckin(t, f, runlog.Checkin{Time: f.now, Worker: "docs", Kind: KindIdle, Verdict: runlog.VerdictNudge, Sent: true, Issue: 12})
	f.lister.issues = []backlog.Issue{labeled(11, "area:billing"), labeled(12, "area:billing"), labeled(14)}
	f.tick(2 * time.Minute)
	v := keptQueue(t, f, "app")
	holds := map[int]string{}
	for _, e := range v.Items {
		holds[e.Number] = e.Hold
	}
	if holds[11] != "held: overlaps #12, which docs has in hand: both are area:billing" || holds[14] != "" || v.Workable() != 1 {
		t.Fatalf("kept queue = %+v, want #11 held for its overlap and #14 workable", v)
	}
}

func TestTheKeptQueueSaysWhyTheReserveWasNotListed(t *testing.T) {
	f := started(t, config.Supervisor{OverlapLabels: "area:"}, done)
	withBackfill(f, labeled(13))
	f.tick(2 * time.Minute)
	if v := keptQueue(t, f, "app"); v.Reserve != "not listed: #13 can be acted on" {
		t.Fatalf("reserve = %q", v.Reserve)
	}
}

func TestTheKeptQueueSaysWhatTheReserveOfferedAndWhatItLeftOut(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	withBackfill(f, asksADecision(12, t0.Add(-time.Hour)))
	f.tick(2 * time.Minute)
	v := keptQueue(t, f, "app")
	if want := "listed: 2 issue(s) offered; left out: 1 labelled machine:other, a queue of the fleet (#52)"; v.Reserve != want {
		t.Fatalf("reserve = %q, want %q", v.Reserve, want)
	}
	for _, e := range v.Items {
		if e.Number == 50 && !e.Backfill {
			t.Fatalf("#50 is not marked as from the reserve: %+v", v)
		}
	}
}

// reserveOf replaces the backlog the reserve lists.
func reserveOf(f *fixture, issues ...backlog.Issue) {
	f.lister.byLabel[""] = issues
}

// withOpenPRs gives the issue the open pull requests of its repository.
func withOpenPRs(i backlog.Issue, prs ...int) backlog.Issue {
	i.OpenPRs = map[int]backlog.PR{}
	for _, pr := range prs {
		i.OpenPRs[pr] = backlog.PR{}
	}
	return i
}

func TestTheReserveIsHeldWhileItsRepositoryIsAtItsOpenPullRequestLimit(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	f.Repos = []config.Repo{{Name: "org/app", MaxOpenPRs: 2}}
	withBackfill(f, withOpenPRs(labeled(11), 41, 42))
	reserveOf(f, withOpenPRs(labeled(50, "p1"), 41, 42))
	f.tick(2 * time.Minute)

	const hold = "held: org/app has 2 open pull requests, at or over its limit of 2"
	prompt := f.reviewer.prompts[0]
	for _, want := range []string{"org/app#11 Issue [" + hold, "#50 Issue [" + hold} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	f.tick(15 * time.Minute)
	for _, c := range f.checkins(t) {
		if c.Issue == 11 || c.Issue == 50 {
			t.Fatalf("check-in = %+v: an issue was handed over while the repository is at its limit", c)
		}
	}
}

func TestTheReserveIsHandedOverWhileItsRepositoryIsUnderItsOpenPullRequestLimit(t *testing.T) {
	f := started(t, config.Supervisor{}, assign(50))
	f.Repos = []config.Repo{{Name: "org/app", MaxOpenPRs: 2}}
	claimer := withBackfill(f)
	reserveOf(f, withOpenPRs(labeled(50, "p1"), 41))
	f.tick(2 * time.Minute)

	if c := f.lastCheckin(t); c.Issue != 50 || !c.Backfill || !c.Sent {
		t.Fatalf("check-in = %+v, want #50 handed over from the reserve", c)
	}
	if !slices.Equal(claimer.claimed, []string{"org/app#50 machine:box"}) {
		t.Fatalf("claimed = %q", claimer.claimed)
	}
}

func TestTheReserveHoldsAnIssueThatWaitsForADependency(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	withBackfill(f)
	reserveOf(f, dependsOn(50, 12, true))
	f.tick(2 * time.Minute)

	if prompt := f.reviewer.prompts[0]; !strings.Contains(prompt, "org/app#50 Issue [held: waits for #12 to close (GitHub's blocked-by)] [backlog]") {
		t.Fatalf("prompt does not show the hold:\n%s", prompt)
	}
	f.tick(10 * time.Minute)
	for _, c := range f.checkins(t) {
		if c.Issue == 50 {
			t.Fatalf("check-in = %+v: #50 was handed over while it waits for #12", c)
		}
	}
}

func TestTheReserveIsOfferedWhenThePaceHoldsTheQueue(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	paced(f, 200) // half the week gone: the pace line is $100
	spent(t, f, 120)
	withBackfill(f, labeled(5))
	f.tick(2 * time.Minute)
	prompt := f.reviewer.prompts[0]
	for _, want := range []string{"org/app#5 Issue [held: the account is ahead of its weekly pace", "#50 Issue [backlog]"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
}

// A hand-back that left no worker step must not be handed out again, and
// must not hide the reserve: nothing beside it can be acted on.
func TestAnIssueWithNoWorkerStepWaitsOnTheOwnerAndTheReserveIsOffered(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	rested := labeled(12)
	rested.Comments = []backlog.Comment{{Body: "## Hand-back\n**PR:** none\n**Decisions needed:** none\n**Owner act:** run the backfill"}}
	rested.OpenPRs = map[int]backlog.PR{}
	withBackfill(f, rested)
	f.tick(2 * time.Minute)
	prompt := f.reviewer.prompts[0]
	for _, want := range []string{"#12 Issue [waits on the owner: no worker step]", "#50 Issue [backlog]"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
}

type fakeFiled struct {
	filed []backlog.Filed
	asked int
}

func (f *fakeFiled) FiledSince(context.Context, string, time.Time) ([]backlog.Filed, error) {
	f.asked++
	return f.filed, nil
}

func workerFiled(number int) backlog.Issue {
	i := labeled(number)
	i.Body = "Found while building #5\n\nThe export times out."
	return i
}

// Splitting ends a worker's issue now and leaves the cost to the chain, so
// the issues workers open each day are scarce: past the quota they wait
// for the owner to admit them.
func TestAnIssueAWorkerOpenedPastTheDaysQuotaWaitsForTheOwner(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	f.Repos = []config.Repo{{Name: "org/app", IssueQuota: 1}}
	f.Filed = &fakeFiled{filed: []backlog.Filed{{Number: 11, CreatedAt: f.now.Add(-2 * time.Hour)}, {Number: 12, CreatedAt: f.now.Add(-time.Hour)}}}
	admitted := workerFiled(13)
	admitted.Comments = []backlog.Comment{{Body: "Ready now: this one is worth it."}}
	f.Filed.(*fakeFiled).filed = append(f.Filed.(*fakeFiled).filed, backlog.Filed{Number: 13, CreatedAt: f.now.Add(-30 * time.Minute)})
	f.lister.issues = []backlog.Issue{workerFiled(11), workerFiled(12), admitted}
	f.tick(2 * time.Minute)
	prompt := f.reviewer.prompts[0]
	if !strings.Contains(prompt, "org/app#12 Issue [held: over the quota of 1 issue(s) workers open a day") {
		t.Fatalf("#12 is not held:\n%s", prompt)
	}
	for _, free := range []string{"org/app#11 Issue\n", "org/app#13 Issue\n"} {
		if !strings.Contains(prompt, free) {
			t.Fatalf("prompt lacks %q, which is within the quota or admitted:\n%s", free, prompt)
		}
	}
}

func TestTheIssuesWorkersOpenedAreReadOnceInTenMinutes(t *testing.T) {
	f := started(t, config.Supervisor{}, done, done)
	f.Repos = []config.Repo{{Name: "org/app", IssueQuota: 1}}
	filed := &fakeFiled{}
	f.Filed = filed
	f.lister.issues = []backlog.Issue{labeled(11)}
	f.tick(2 * time.Minute)
	f.tick(3 * time.Minute)
	if filed.asked != 1 {
		t.Fatalf("asked = %d, want 1", filed.asked)
	}
}

func TestTheBriefSetsTheBarForOpeningAnIssue(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	f.tick(2 * time.Minute)
	brief, err := os.ReadFile(filepath.Join(f.Machine.Workers[0].Dir, BriefFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Found while building #<your issue>", "Not ready: too big:", "never a new issue"} {
		if !strings.Contains(string(brief), want) {
			t.Errorf("brief lacks %q", want)
		}
	}
}

func TestTheFilingRuleNamesEachRepositorysQuota(t *testing.T) {
	s := &Supervisor{Repos: []config.Repo{{Name: "org/app", IssueQuota: 2}, {Name: "org/api"}}}
	rule := s.filingRule("Still to do:")
	if !strings.Contains(rule, "Workers may open 2 a day in org/app, all workers together") || strings.Contains(rule, "org/api") {
		t.Fatalf("rule = %q", rule)
	}
}
