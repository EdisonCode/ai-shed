package status

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

type fakePulls struct {
	pulls []backlog.OpenPull
	err   error
}

func (f fakePulls) OpenPulls(_ context.Context, repo string) ([]backlog.OpenPull, error) {
	var out []backlog.OpenPull
	for _, p := range f.pulls {
		if p.Repo == repo {
			out = append(out, p)
		}
	}
	return out, f.err
}

func openPull(number int, opened time.Duration, closes ...int) backlog.OpenPull {
	p := backlog.OpenPull{Repo: "org/app", Number: number, Title: "Add the export button", CreatedAt: now.Add(-opened), CI: backlog.CIPassed, LastCommit: now.Add(-opened)}
	for _, n := range closes {
		p.Closes = append(p.Closes, backlog.Dep{Repo: "org/app", Number: n})
	}
	return p
}

// queuedOn is the report of a machine with the issues in its queue.
func queuedOn(issues ...IssueStatus) []MachineReport {
	return []MachineReport{{Name: "box", Issues: issues}}
}

func TestPullRequestsNoQueueCoversAreListedWithTheirAgeAndCI(t *testing.T) {
	noIssue := openPull(41, 3*24*time.Hour)
	noIssue.CI = backlog.CIFailed
	notQueued := openPull(44, 16*time.Hour, 12)
	covered := openPull(45, time.Hour, 7)
	batch := openPull(46, time.Hour)
	batch.Batch = true
	c := Collector{Pulls: fakePulls{pulls: []backlog.OpenPull{covered, notQueued, noIssue, batch}}}

	got := c.collectPulls(context.Background(), oneMachine(), queuedOn(IssueStatus{Repo: "org/app", Number: 7}), now)

	if len(got.Uncovered) != 2 || got.Uncovered[0].Number != 41 || got.Uncovered[1].Number != 44 {
		t.Fatalf("uncovered = %+v, want #41 then #44", got.Uncovered)
	}
	var out bytes.Buffer
	RenderPulls(&out, got, now)
	for _, want := range []string{
		"PRs no queue covers: 2",
		"3d   org/app#41  CI failed  closes no issue",
		"16h  org/app#44  CI passed  #12 is in no queue",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestAPullRequestIsNotCalledUncoveredWhenTheQueueOfItsRepositoryCouldNotBeRead(t *testing.T) {
	c := Collector{Pulls: fakePulls{pulls: []backlog.OpenPull{openPull(41, time.Hour)}}}
	machines := []MachineReport{{Name: "box", unlisted: []string{"org/app"}}}

	got := c.collectPulls(context.Background(), oneMachine(), machines, now)

	if len(got.Uncovered) != 0 {
		t.Fatalf("uncovered = %+v, want none while the queue is unread", got.Uncovered)
	}
}

func TestADraftWithNoCommitFor48HoursIsListedWithItsHold(t *testing.T) {
	fromHandBack := openPull(41, 6*24*time.Hour, 7)
	fromHandBack.Draft = true
	fromBody := openPull(42, 3*24*time.Hour)
	fromBody.Draft, fromBody.Body = true, "Adds the chart.\nWaits for the API change to ship."
	saysNothing := openPull(43, 50*time.Hour)
	saysNothing.Draft = true
	recent := openPull(44, 47*time.Hour)
	recent.Draft = true
	c := Collector{Pulls: fakePulls{pulls: []backlog.OpenPull{recent, saysNothing, fromBody, fromHandBack}}}
	queued := IssueStatus{Repo: "org/app", Number: 7, Drafts: []backlog.Ask{{PR: 41, WaitsFor: "waits for #12"}}}

	got := c.collectPulls(context.Background(), oneMachine(), queuedOn(queued), now)

	if len(got.StaleDrafts) != 3 {
		t.Fatalf("stale drafts = %+v, want #41, #42 and #43", got.StaleDrafts)
	}
	var out bytes.Buffer
	RenderPulls(&out, got, now)
	for _, want := range []string{
		"Drafts quiet for 48h: 3",
		"6d  org/app#41  waits for #12",
		"3d  org/app#42  Waits for the API change to ship.",
		"2d  org/app#43  no hold given",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestAPullRequestListThatFailsIsSaid(t *testing.T) {
	c := Collector{Pulls: fakePulls{err: errors.New("gh: not logged in")}}

	got := c.collectPulls(context.Background(), oneMachine(), queuedOn(), now)

	var out bytes.Buffer
	RenderPulls(&out, got, now)
	if !strings.Contains(out.String(), "! cannot list the open pull requests: gh: not logged in") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestWithNoPullReaderThePullSectionsAreLeftOut(t *testing.T) {
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}}
	if r := c.CollectAll(context.Background(), oneMachine()); r.Pulls != nil {
		t.Fatalf("pulls = %+v, want none", r.Pulls)
	}
}

func TestTheDigestListsPullRequestsNoQueueCovers(t *testing.T) {
	cfg := oneMachine()
	cfg.Machines[0].Issues = []config.IssueSource{{Repo: "org/app", Label: "machine:box"}}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{}, Pulls: fakePulls{pulls: []backlog.OpenPull{openPull(41, 5*time.Hour)}}}

	d := c.Digest(context.Background(), cfg, &fakeMerged{}, now.Add(-24*time.Hour), now)

	if d.Report.Pulls == nil || len(d.Report.Pulls.Uncovered) != 1 {
		t.Fatalf("pulls = %+v", d.Report.Pulls)
	}
	var out bytes.Buffer
	RenderDigest(&out, d)
	if !strings.Contains(out.String(), "PRs no queue covers: 1") {
		t.Fatalf("digest lacks the uncovered pull request:\n%s", out.String())
	}
}
