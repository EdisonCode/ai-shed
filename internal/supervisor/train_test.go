package supervisor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

type fakeTrain struct {
	heads    map[int]backlog.Head
	comments []string
	updated  []string
	merged   []string
	// base is where the base branch's checks stand.
	base backlog.BaseState
}

func (f *fakeTrain) BaseChecks(context.Context, string, string) (backlog.BaseState, error) {
	return f.base, nil
}

func (f *fakeTrain) ReadHead(_ context.Context, _ string, pr int) (backlog.Head, error) {
	return f.heads[pr], nil
}

func (f *fakeTrain) UpdateBranch(_ context.Context, _ string, pr int, rebase bool) error {
	f.updated = append(f.updated, fmt.Sprintf("#%d rebase=%v", pr, rebase))
	return nil
}

func (f *fakeTrain) MergeHead(_ context.Context, _ string, pr int, method, sha string) error {
	f.merged = append(f.merged, fmt.Sprintf("#%d %s at %s", pr, method, sha))
	return nil
}

func (f *fakeTrain) Comment(_ context.Context, _ string, issue int, body string) error {
	f.comments = append(f.comments, fmt.Sprintf("#%d %s", issue, body))
	return nil
}

var (
	behind    = backlog.Head{Open: true, SHA: "aaaaaaa1", Behind: 3, Checks: 1}
	running   = backlog.Head{Open: true, SHA: "bbbbbbb2", Checks: 1, Pending: true}
	notYet    = backlog.Head{Open: true, SHA: "bbbbbbb2"}
	green     = backlog.Head{Open: true, SHA: "bbbbbbb2", Checks: 1}
	redOnHead = backlog.Head{Open: true, SHA: "bbbbbbb2", Checks: 1, Problem: "has a failed check (test)"}
)

func withTrain(t *testing.T, f *fixture, method string) (*fakeTrain, *fakeMerger) {
	f.Repos = []config.Repo{{Name: "org/app", Verify: &config.Verify{MergeLow: true, MergeTrain: config.TrainOne, MergeMethod: method}}}
	train := &fakeTrain{heads: map[int]backlog.Head{}}
	merger := &fakeMerger{}
	f.Train, f.Merger = train, merger
	built(t, f, 12)
	return train, merger
}

// step moves the clock past the train's pace and moves the train once, on
// the issues as a queue check reads them.
func (f *fixture) step(t *testing.T) {
	t.Helper()
	f.now = f.now.Add(trainEvery)
	open := slices.Clone(f.lister.issues)
	for n := range open {
		open[n].Verify = f.verifies(open[n].Repo)
	}
	f.queueMerges(context.Background(), open, f.checkins(t))
}

// boarded is verifiedLow after shed put #41 on the train.
func boarded() backlog.Issue {
	i := verifiedLow("low")
	i.Comments = append(i.Comments, backlog.Comment{Body: backlog.QueuedPhrase + " pull request #41, verified at low risk with green checks."})
	return i
}

func TestOnAMergeTrainAVerifiedPullRequestIsMergedAtTheHeadItsChecksPassedOn(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, merger := withTrain(t, f, "squash")
	f.lister.issues = []backlog.Issue{verifiedLow("low")}
	train.heads[41] = behind
	f.tick(2 * time.Minute)
	if len(merger.queued) != 0 {
		t.Fatalf("auto-merge turned on for %+v: the train merges it", merger.queued)
	}
	if len(train.comments) != 1 || !strings.HasPrefix(train.comments[0], "#12 "+backlog.QueuedPhrase+" pull request #41,") {
		t.Fatalf("comments = %q, want #41 boarded on #12", train.comments)
	}
	if len(train.updated) != 1 || train.updated[0] != "#41 rebase=false" {
		t.Fatalf("updated = %q, want #41 brought up to date", train.updated)
	}

	f.lister.issues = []backlog.Issue{boarded()}
	for _, h := range []backlog.Head{notYet, running} {
		train.heads[41] = h
		f.step(t)
		if len(train.merged) != 0 {
			t.Fatalf("merged %q before its checks passed on the new head", train.merged)
		}
	}
	train.heads[41] = green
	f.step(t)
	if len(train.merged) != 1 || train.merged[0] != "#41 squash at bbbbbbb2" {
		t.Fatalf("merged = %q", train.merged)
	}
}

func TestARebaseMergeIsBroughtUpToDateByARebase(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, _ := withTrain(t, f, "rebase")
	f.lister.issues = []backlog.Issue{verifiedLow("low")}
	train.heads[41] = behind
	f.step(t)
	if len(train.updated) != 1 || train.updated[0] != "#41 rebase=true" {
		t.Fatalf("updated = %q", train.updated)
	}
}

func TestAPullRequestWhoseChecksFailOnTheUpdatedHeadIsTakenOffAndNotBoardedAgain(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, _ := withTrain(t, f, "")
	f.lister.issues = []backlog.Issue{boarded()}
	train.heads[41] = redOnHead
	f.step(t)
	if len(train.comments) != 1 || !strings.HasPrefix(train.comments[0], "#12 "+backlog.OffTrainPhrase) || !strings.Contains(train.comments[0], "has a failed check (test)") {
		t.Fatalf("comments = %q, want #41 taken off with its failing check named", train.comments)
	}
	// Its worker pushes a fix and the checks pass, before any new hand-back.
	off := boarded()
	off.Comments = append(off.Comments, backlog.Comment{Body: backlog.OffTrainPhrase + " pull request #41, brought up to date, has a failed check (test)."})
	f.lister.issues = []backlog.Issue{off}
	train.heads[41] = green
	f.step(t)
	if len(train.comments) != 1 || len(train.merged) != 0 {
		t.Fatalf("comments = %q, merged = %q: it boards again only after a new hand-back is verified", train.comments, train.merged)
	}
}

func TestOneOnTheTrainAtATime(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, _ := withTrain(t, f, "")
	built(t, f, 13)
	next := verifiedLow("low")
	next.Number = 13
	f.lister.issues = []backlog.Issue{boarded(), next}
	train.heads[41] = running
	f.step(t)
	if len(train.comments) != 0 {
		t.Fatalf("comments = %q: #13 boards when #12 is merged", train.comments)
	}
}

func TestAPullRequestAnotherMachinePutOnTheTrainLatelyHoldsItBack(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, _ := withTrain(t, f, "")
	theirs := boarded()
	theirs.Number = 20
	theirs.Comments[len(theirs.Comments)-1].CreatedAt = f.now
	f.lister.issues = []backlog.Issue{theirs, verifiedLow("low")}
	train.heads[41] = behind
	f.step(t)
	if len(train.comments) != 0 || len(train.updated) != 0 {
		t.Fatalf("comments = %q, updated = %q while another machine's pull request is in hand", train.comments, train.updated)
	}
	f.now = f.now.Add(trainHold)
	f.step(t)
	if len(train.comments) != 1 {
		t.Fatalf("comments = %q: the train does not wait for a machine that is gone", train.comments)
	}
}

func TestAPullRequestRedOnlyWhereTheBaseIsRedStaysInHandAndIsTestedAgainOnTheFix(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, _ := withTrain(t, f, "")
	f.lister.issues = []backlog.Issue{boarded()}
	red := redOnHead
	red.Failed = []string{"test"}
	train.heads[41] = red
	train.base = backlog.BaseState{Branch: "main", SHA: "ba5e1", Failed: []string{"test"}}
	f.step(t)
	f.step(t)
	if len(train.comments) != 0 || len(train.merged) != 0 {
		t.Fatalf("comments = %q, merged = %q: want #41 kept in hand while the base is red", train.comments, train.merged)
	}
	// The base gets its fix and goes green; #41 is now behind it.
	train.base = backlog.BaseState{Branch: "main", SHA: "f1xed"}
	red.Behind = 1
	train.heads[41] = red
	f.step(t)
	if len(train.comments) != 0 || !slices.Equal(train.updated, []string{"#41 rebase=false"}) {
		t.Fatalf("comments = %q, updated = %q: want #41 brought up to date with the fix, not taken off", train.comments, train.updated)
	}
}

func TestAPullRequestRedOnACheckTheBaseIsGreenOnIsTakenOff(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, _ := withTrain(t, f, "")
	f.lister.issues = []backlog.Issue{boarded()}
	red := redOnHead
	red.Failed = []string{"test", "lint"}
	train.heads[41] = red
	train.base = backlog.BaseState{Branch: "main", SHA: "ba5e1", Failed: []string{"test"}}
	f.step(t)
	if len(train.comments) != 1 || !strings.HasPrefix(train.comments[0], "#12 "+backlog.OffTrainPhrase) {
		t.Fatalf("comments = %q: lint fails on #41 alone, want it taken off", train.comments)
	}
}
