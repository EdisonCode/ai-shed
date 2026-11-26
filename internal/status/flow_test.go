package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/looks"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

type fakeFlow struct{ issues map[string][]looks.FlowIssue }

func (f fakeFlow) IssuesSince(_ context.Context, repo string, _ time.Time) ([]looks.FlowIssue, error) {
	return f.issues[repo], nil
}

// countingClosed is a ClosedLister that counts the times it is asked.
type countingClosed struct {
	issues []backlog.Issue
	asked  int
}

func (c *countingClosed) ClosedSince(context.Context, config.IssueSource, time.Time) ([]backlog.Issue, error) {
	c.asked++
	return c.issues, nil
}

func digestText(t *testing.T, d Digest) string {
	t.Helper()
	var out bytes.Buffer
	RenderDigest(&out, d)
	return out.String()
}

func wantLines(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Fatalf("digest lacks %q:\n%s", want, out)
		}
	}
}

func TestTheFlowCountsIssuesOpenedClosedAndNetAndThoseWorkersOpened(t *testing.T) {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	flow := fakeFlow{issues: map[string][]looks.FlowIssue{"org/app": {
		{Number: 20, CreatedAt: ago(2 * time.Hour), Body: "Found while building #9"},
		{Number: 21, CreatedAt: ago(3 * time.Hour), Body: "Found while building #9"},
		{Number: 22, CreatedAt: ago(4 * time.Hour)},
		{Number: 9, CreatedAt: ago(9 * 24 * time.Hour), ClosedAt: ago(time.Hour), Pulls: []int{41, 44}},
		{Number: 10, CreatedAt: ago(9 * 24 * time.Hour), ClosedAt: ago(time.Hour), Pulls: []int{45}},
		{Number: 3, CreatedAt: ago(30 * 24 * time.Hour), ClosedAt: ago(30 * time.Hour)}, // closed before the window
	}}}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Flow: flow}

	d := c.Digest(context.Background(), oneMachine(), &fakeMerged{}, ago(24*time.Hour), now)

	got := d.Flow["org/app"]
	if got.Opened != 3 || got.Closed != 2 || got.Net != 1 || got.OpenedByWorkers != 2 {
		t.Fatalf("flow = %+v", got)
	}
	wantLines(t, digestText(t, d),
		"Flow: org/app",
		"issues         opened 3, closed 2, net +1; 2 opened by workers (67%)",
		"pull requests  1.5 per closed issue, most #9 (2)",
	)
}

func TestTheFlowGivesReworkSendsPerMergedPullRequestWithTheWorstIssue(t *testing.T) {
	sent := func(ago time.Duration, issue int) runlog.Checkin {
		return runlog.Checkin{Time: now.Add(-ago), Worker: "app", Sent: true, Issue: issue, Repo: "org/app", Rework: "pull request #41 has a failed check (test)"}
	}
	res := healthy()
	res.Checkins = []runlog.Checkin{sent(30*time.Hour, 7), sent(5*time.Hour, 7), sent(3*time.Hour, 7), sent(2*time.Hour, 8),
		{Time: now.Add(-time.Hour), Worker: "app", Sent: true, Issue: 9, Repo: "org/app"}}
	d := Digest{Since: now.Add(-24 * time.Hour), Now: now,
		Merged: []RepoMerged{{Repo: "org/app", Pulls: []looks.Pull{{Number: 41}, {Number: 42}}}},
		Report: Report{Machines: []MachineReport{{Name: "box", Probe: res}}}}
	c := Collector{Flow: fakeFlow{}}

	c.collectFlow(context.Background(), oneMachine(), &d)

	if got := d.Flow["org/app"]; got.ReworkSends != 3 || got.MergedPulls != 2 || got.MostRework == nil || got.MostRework.Number != 7 {
		t.Fatalf("flow = %+v", got)
	}
	wantLines(t, digestText(t, d), "rework         1.5 sends per merged pull request, most #7 (2)")
}

func TestTheFlowGivesTheShareOfEyeChecksThatPassedOnTheFirstLook(t *testing.T) {
	ask := "## Hand-back\n**PR:** #41 (ready)\nNeeds eyes: the export button"
	first := issue(5, ask, "Eyes checked")
	second := issue(6, ask, "Eyes failed: grey", ask, "Eyes checked")
	closed := &countingClosed{issues: []backlog.Issue{first, second}}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Flow: fakeFlow{}, Closed: closed}

	d := c.Digest(context.Background(), oneMachine(), &fakeMerged{}, now.Add(-24*time.Hour), now)

	wantLines(t, digestText(t, d), "eye checks     1 of 3 passed on the first look (33%)")
}

func TestTheDigestReadsTheClosedIssuesOfASourceOnce(t *testing.T) {
	closed := &countingClosed{}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Flow: fakeFlow{}, Closed: closed}

	c.Digest(context.Background(), oneMachine(), &fakeMerged{}, now.Add(-24*time.Hour), now)

	if closed.asked != 1 {
		t.Fatalf("closed issues were asked for %d times, want once", closed.asked)
	}
}

func TestTheFlowGivesTheLongestOpenDependencyChainAmongQueuedIssues(t *testing.T) {
	dep := func(n int) backlog.Dep { return backlog.Dep{Repo: "org/app", Number: n, Open: true} }
	root, middle, last, alone := issue(12), issue(14), issue(19), issue(30)
	middle.Deps = []backlog.Dep{dep(12)}
	last.Deps = []backlog.Dep{dep(14), {Repo: "org/app", Number: 3}} // #3 is closed
	alone.Deps = []backlog.Dep{dep(12)}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{issues: []backlog.Issue{alone, last, middle, root}}, Flow: fakeFlow{}}

	d := c.Digest(context.Background(), oneMachine(), &fakeMerged{}, now.Add(-24*time.Hour), now)

	if got := d.Flow["org/app"].Chain; !slices.Equal(got, []int{12, 14, 19}) {
		t.Fatalf("chain = %v, want #12, #14, #19", got)
	}
	wantLines(t, digestText(t, d), "longest chain  #12 → #14 → #19 (3)")
}

func TestTheFlowRepeatsTheAgeOfTheOldestItemWaitingOnTheOwner(t *testing.T) {
	d := Digest{Since: now.Add(-24 * time.Hour), Now: now, Report: Report{Machines: []MachineReport{{Name: "box", Issues: []IssueStatus{
		{Repo: "org/app", Number: 9, Asks: []backlog.Ask{{Name: "decision", Since: now.Add(-30 * time.Hour)}}},
		{Repo: "org/app", Number: 8, Asks: []backlog.Ask{{Name: "review", Since: now.Add(-2 * time.Hour)}}},
	}}}}}
	c := Collector{Flow: fakeFlow{}}

	c.collectFlow(context.Background(), oneMachine(), &d)

	wantLines(t, digestText(t, d), "oldest wait    30h  org/app#9")
}

func TestTheDigestJSONCarriesTheFlowPerRepository(t *testing.T) {
	flow := fakeFlow{issues: map[string][]looks.FlowIssue{"org/app": {{Number: 20, CreatedAt: now.Add(-time.Hour)}}}}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Flow: flow}
	d := c.Digest(context.Background(), oneMachine(), &fakeMerged{}, now.Add(-24*time.Hour), now)

	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Flow map[string]struct {
			Opened int `json:"opened"`
			Net    int `json:"net"`
		} `json:"flow"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if f := got.Flow["org/app"]; f.Opened != 1 || f.Net != 1 {
		t.Fatalf("flow = %s", data)
	}
}

func TestTheDigestJSONListsWhatWaitsOnTheOwnerInTheOrderToAnswerIt(t *testing.T) {
	ask := func(name, effect string, ago time.Duration) []backlog.Ask {
		return []backlog.Ask{{Name: name, Effect: effect, Since: now.Add(-ago)}}
	}
	report := Report{Machines: []MachineReport{{Name: "box", Issues: []IssueStatus{
		{Repo: "org/app", Number: 5, Asks: ask("review", backlog.EffectMerge, 50*time.Hour)},
		{Repo: "org/app", Number: 6, Asks: ask("eyes", backlog.EffectRead, 40*time.Hour)},
		{Repo: "org/app", Number: 7, Asks: ask("decision", backlog.EffectReturns, 30*time.Hour), ReturnsTo: "app"},
		{Repo: "org/app", Number: 8, Asks: ask("decision", backlog.EffectReturns, 2*time.Hour), ReturnsTo: "api", ReturnsIdle: true},
	}}, {Name: "mini", Issues: []IssueStatus{
		{Repo: "org/app", Number: 6, Asks: ask("eyes", backlog.EffectRead, 40*time.Hour)},
	}}}}

	d := Collector{}.DigestFrom(context.Background(), oneMachine(), &fakeMerged{}, report, now.Add(-24*time.Hour), now)

	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Waiting []struct {
			Number int `json:"number"`
		} `json:"waiting"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	var order []int
	for _, w := range got.Waiting {
		order = append(order, w.Number)
	}
	// The return to an idle worker, the other return, the read, the merge;
	// the issue in two queues once.
	if !slices.Equal(order, []int{8, 7, 6, 5}) {
		t.Fatalf("waiting = %v, want #8, #7, #6, #5: %s", order, data)
	}
}

func TestTheDigestJSONCarriesWhatTheReleaseStateNeedsFromTheOwner(t *testing.T) {
	report := Report{Repos: []RepoReport{{Report: looks.Report{Repo: "org/app", Looks: []looks.Look{
		{Repo: "org/app", Number: 4, PR: 44, State: looks.Blocked, Note: "staging was down"},
	}}}}}

	d := Collector{}.DigestFrom(context.Background(), oneMachine(), &fakeMerged{}, report, now.Add(-24*time.Hour), now)

	want := []string{"org/app: the eye check of org/app#4 (pull request #44) is blocked: staging was down"}
	if !slices.Equal(d.RepoAttention, want) || !slices.Equal(RepoAttention(report), want) {
		t.Fatalf("repo attention = %q, want %q", d.RepoAttention, want)
	}
}

func TestWithNoFlowReaderTheFlowIsLeftOut(t *testing.T) {
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}}
	d := c.Digest(context.Background(), oneMachine(), &fakeMerged{}, now.Add(-24*time.Hour), now)
	if d.Flow != nil || strings.Contains(digestText(t, d), "Flow:") {
		t.Fatalf("flow = %+v", d.Flow)
	}
}

type failingFlow struct{}

func (failingFlow) IssuesSince(context.Context, string, time.Time) ([]looks.FlowIssue, error) {
	return nil, errors.New("gh issue list org/app: timed out")
}

func TestAFlowWhoseIssuesCannotBeReadSaysSoInsteadOfCountingZero(t *testing.T) {
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Flow: failingFlow{}}
	d := c.Digest(context.Background(), oneMachine(), &fakeMerged{}, now.Add(-24*time.Hour), now)
	wantLines(t, digestText(t, d), "issues         cannot be read (see Also needs you)", "! gh issue list org/app: timed out")
}
