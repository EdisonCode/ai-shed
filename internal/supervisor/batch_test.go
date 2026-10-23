package supervisor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/risk"
)

type fakeBatcher struct {
	strict   bool
	conflict int
	next     int
	built    [][]int
	rebuilt  []backlog.Rebuilt
	closed   []int
	open     []backlog.Batch
	// moved is what the base has that a batch does not; batchFiles what
	// every batch changes.
	moved      backlog.Comparison
	batchFiles []string
}

func (f *fakeBatcher) RequiresUpToDate(context.Context, string, string) (bool, error) {
	return f.strict, nil
}

func (f *fakeBatcher) Compare(_ context.Context, _, from, _ string) (backlog.Comparison, error) {
	if from == "main" {
		return backlog.Comparison{Files: f.batchFiles}, nil
	}
	return f.moved, nil
}

func (f *fakeBatcher) BuildBatch(_ context.Context, _, _, _ string, cars []backlog.Car, rebuilt backlog.Rebuilt) (int, error) {
	var prs []int
	for _, c := range cars {
		if c.PR == f.conflict {
			return 0, &backlog.MergeConflict{PR: c.PR}
		}
		prs = append(prs, c.PR)
	}
	f.built = append(f.built, prs)
	f.rebuilt = append(f.rebuilt, rebuilt)
	f.next++
	return 100 + f.next, nil
}

func (f *fakeBatcher) Batches(context.Context, string) ([]backlog.Batch, error) {
	return f.open, nil
}

func (f *fakeBatcher) CloseBatch(_ context.Context, _ string, pr int) error {
	f.closed = append(f.closed, pr)
	return nil
}

// verifiedAs is an issue whose hand-back of the pull request a verifier
// passed at low risk.
func verifiedAs(issue, pr int) backlog.Issue {
	i := labeled(issue)
	i.Comments = []backlog.Comment{
		{Body: fmt.Sprintf("## Hand-back\n**PR:** #%d (ready)", pr)},
		{Body: "Verified (low): ran the export tests"},
	}
	i.OpenPRs = map[int]backlog.PR{pr: {}}
	return i
}

// withBatches sets up a batch train on org/app with these issues, each
// built here and verified, pull request #<issue+29> on issue #<issue>.
func withBatches(t *testing.T, f *fixture, verify config.Verify, issues ...int) (*fakeTrain, *fakeBatcher) {
	verify.MergeLow, verify.MergeTrain = true, config.TrainBatch
	f.Repos = []config.Repo{{Name: "org/app", Verify: &verify}}
	train := &fakeTrain{heads: map[int]backlog.Head{}}
	batcher := &fakeBatcher{}
	f.Train, f.Batch, f.Merger = train, batcher, &fakeMerger{}
	f.lister.issues = nil
	for _, n := range issues {
		built(t, f, n)
		f.lister.issues = append(f.lister.issues, verifiedAs(n, n+29))
		train.heads[n+29] = headAt(n + 29)
	}
	return train, batcher
}

// ride moves the train once, as step does, and adds what shed commented to
// the issues, as GitHub would.
func (f *fixture) ride(t *testing.T, train *fakeTrain) {
	t.Helper()
	before := len(train.comments)
	f.step(t)
	for _, c := range train.comments[before:] {
		var n int
		fmt.Sscanf(c, "#%d ", &n)
		for i := range f.lister.issues {
			if f.lister.issues[i].Number == n {
				f.lister.issues[i].Comments = append(f.lister.issues[i].Comments, backlog.Comment{Body: strings.SplitN(c, " ", 2)[1], CreatedAt: f.now})
			}
		}
	}
}

// movedUnder puts the pull requests' heads behind a base that moved in
// files they change, unless the test said how the base moved: by itself,
// each is tested in a batch, not merged directly.
func movedUnder(train *fakeTrain, batcher *fakeBatcher, prs ...int) {
	if batcher.moved.Commits == nil {
		batcher.moved = backlog.Comparison{Commits: []string{"d00d0000000"}, Files: []string{"export.go"}}
		batcher.batchFiles = []string{"export.go"}
	}
	for _, pr := range prs {
		h := train.heads[pr]
		h.Behind = 1
		train.heads[pr] = h
	}
}

func headAt(pr int) backlog.Head {
	return backlog.Head{Open: true, SHA: fmt.Sprintf("c0ffee%d", pr), Base: "main", Checks: 1}
}

var (
	batchRunning = backlog.Head{Open: true, SHA: "ba7c4", Base: "main", Checks: 1, Pending: true}
	batchGreen   = backlog.Head{Open: true, SHA: "ba7c4", Base: "main", Checks: 1}
	batchRed     = backlog.Head{Open: true, SHA: "ba7c4", Base: "main", Checks: 1, Problem: "has a failed check (test)"}
)

func TestABatchTrainTestsVerifiedPullRequestsTogetherAndMergesEachWhenTheBatchIsGreen(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{MergeMethod: "squash"}, 12, 13, 14)
	f.ride(t, train)
	if len(train.comments) != 3 || !strings.HasPrefix(train.comments[0], "#12 "+backlog.QueuedPhrase+" pull request #41,") {
		t.Fatalf("comments = %q, want each pull request boarded", train.comments)
	}
	if len(batcher.built) != 1 || !slices.Equal(batcher.built[0], []int{41, 42, 43}) {
		t.Fatalf("built = %v, want one batch of #41, #42, #43 in order", batcher.built)
	}
	train.heads[101] = batchRunning
	f.ride(t, train)
	if len(train.merged) != 0 {
		t.Fatalf("merged %q before the batch's checks passed", train.merged)
	}
	train.heads[101] = batchGreen
	f.ride(t, train)
	want := []string{"#41 squash at c0ffee41", "#42 squash at c0ffee42", "#43 squash at c0ffee43"}
	if !slices.Equal(train.merged, want) {
		t.Fatalf("merged = %q, want %q", train.merged, want)
	}
	if !slices.Equal(batcher.closed, []int{101}) {
		t.Fatalf("closed = %v, want the batch closed", batcher.closed)
	}
	if len(train.updated) != 0 {
		t.Fatalf("updated = %q: a batch train does not update branches", train.updated)
	}
}

func TestABatchWhoseEveryCheckWasSkippedIsNotMerged(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, _ := withBatches(t, f, config.Verify{}, 12, 13)
	f.ride(t, train)
	train.heads[101] = backlog.Head{Open: true, SHA: "ba7c4", Base: "main", Skipped: 5}
	f.ride(t, train)
	f.ride(t, train)
	if len(train.merged) != 0 {
		t.Fatalf("merged %q on a batch its CI did not run on", train.merged)
	}
}

func TestARedBatchIsSplitUntilThePullRequestThatFailsIsFound(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{}, 12, 13)
	f.ride(t, train) // #101: #41, #42
	train.heads[101] = batchRed
	f.ride(t, train)
	f.ride(t, train) // #102: #41
	if len(batcher.built) != 2 || !slices.Equal(batcher.built[1], []int{41}) {
		t.Fatalf("built = %v, want the first half tested next", batcher.built)
	}
	train.heads[102] = batchGreen
	f.ride(t, train)
	if !slices.Equal(train.merged, []string{"#41 merge at c0ffee41"}) {
		t.Fatalf("merged = %q, want the first half merged", train.merged)
	}
	f.ride(t, train) // #103: #42
	train.heads[103] = batchRed
	f.ride(t, train)
	last := train.comments[len(train.comments)-1]
	if !strings.HasPrefix(last, "#13 "+backlog.OffTrainPhrase+" pull request #42,") || !strings.Contains(last, "has a failed check (test)") {
		t.Fatalf("last comment = %q, want #42 taken off with its failing check named", last)
	}
	if !strings.Contains(last, "the split began when batch pull request #101 of #41, #42 has a failed check (test)") {
		t.Fatalf("last comment = %q, want the batch that failed first named with its pull requests", last)
	}
	if reason := backlog.Rework(f.lister.issues[1], f.Signals); !strings.Contains(reason, "off the merge train") {
		t.Fatalf("rework = %q, want #42 back with a worker", reason)
	}
	if !slices.Equal(batcher.closed, []int{101, 102, 103}) {
		t.Fatalf("closed = %v", batcher.closed)
	}
	f.ride(t, train)
	if len(batcher.built) != 3 {
		t.Fatalf("built = %v: the train is empty", batcher.built)
	}
}

func TestABatchTrainDoesNotRunWhereTheBaseRequiresUpToDateBranches(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{}, 12)
	batcher.strict = true
	var logged []string
	f.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	f.ride(t, train)
	if len(train.comments) != 0 || len(batcher.built) != 0 {
		t.Fatalf("comments = %q, built = %v: nothing boards", train.comments, batcher.built)
	}
	if !slices.ContainsFunc(logged, func(l string) bool { return strings.Contains(l, "main requires a pull request to be up to date") }) {
		t.Fatalf("logged = %q, want the reason", logged)
	}
}

func TestAPullRequestTouchingAnAlonePathIsABatchOfItsOwn(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{MergeTrainAlone: []string{"db/migrations"}}, 12, 13, 14)
	f.Diffs = &fakeDiffs{diffs: map[int]risk.Diff{42: {Files: []string{"db/migrations/0042_orders.sql"}}}}
	movedUnder(train, batcher, 41, 42)
	f.ride(t, train)
	if !slices.Equal(batcher.built[0], []int{41}) {
		t.Fatalf("built = %v, want #41 alone, before the migration", batcher.built)
	}
	train.heads[101] = batchGreen
	f.ride(t, train)
	f.ride(t, train)
	if !slices.Equal(batcher.built[1], []int{42}) {
		t.Fatalf("built = %v, want the migration by itself", batcher.built)
	}
}

func TestAPullRequestWhoseHeadMovesLeavesTheBatchAndTheBatchIsBuiltAgain(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{}, 12, 13)
	movedUnder(train, batcher, 42)
	f.ride(t, train)
	train.heads[101] = batchGreen
	train.heads[41] = backlog.Head{Open: true, SHA: "f1x", Base: "main", Checks: 1}
	f.ride(t, train)
	if len(train.merged) != 0 {
		t.Fatalf("merged = %q from a batch that no longer tests what it holds", train.merged)
	}
	last := train.comments[len(train.comments)-1]
	if !strings.HasPrefix(last, "#12 "+backlog.OffTrainPhrase+" pull request #41,") {
		t.Fatalf("last comment = %q, want #41 taken off", last)
	}
	reboard := f.lister.issues[0]
	reboard.Verify = true
	if v := backlog.Verification(reboard, f.Signals); v.State != backlog.VerifyDue || v.Since != "c0ffee4" {
		t.Fatalf("verification = %+v, want #41 due a verifier again, since the head that boarded", v)
	}
	f.ride(t, train)
	if len(batcher.built) != 2 || !slices.Equal(batcher.built[1], []int{42}) {
		t.Fatalf("built = %v, want the batch built again without #41", batcher.built)
	}
}

func TestABatchWhoseBaseMovedInItsFilesIsBuiltAgainAndCountsIt(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{}, 12)
	batcher.batchFiles = []string{"export.go", "export_test.go"}
	batcher.moved = backlog.Comparison{Commits: []string{"d00d1230000", "d00d4560000"}, Files: []string{"export.go"}}
	movedUnder(train, batcher, 41)
	f.ride(t, train)
	moved := batchGreen
	moved.Behind = 2
	train.heads[101] = moved
	f.ride(t, train)
	f.ride(t, train) // #102
	if len(train.merged) != 0 || len(batcher.built) != 2 {
		t.Fatalf("merged = %q, built = %v: want it tested again on the new base", train.merged, batcher.built)
	}
	if want := (backlog.Rebuilt{Times: 1, Green: 1, Last: "d00d456"}); batcher.rebuilt[1] != want {
		t.Fatalf("rebuilt = %+v, want %+v", batcher.rebuilt[1], want)
	}
	running := batchRunning
	running.Behind = 1
	train.heads[102] = running
	f.ride(t, train)
	f.ride(t, train) // #103
	if want := (backlog.Rebuilt{Times: 2, Green: 1, Last: "d00d456"}); len(batcher.rebuilt) != 3 || batcher.rebuilt[2] != want {
		t.Fatalf("rebuilt = %+v, want the third batch to say %+v", batcher.rebuilt, want)
	}
	train.heads[103] = batchGreen
	f.ride(t, train)
	f.ride(t, train)
	if len(batcher.built) != 3 || len(train.merged) != 1 {
		t.Fatalf("built = %v, merged = %q: want the third batch merged", batcher.built, train.merged)
	}
}

func TestABatchWhoseBaseMovedApartFromItsFilesMergesWhenGreen(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{}, 12, 13)
	batcher.batchFiles = []string{"export.go"}
	batcher.moved = backlog.Comparison{Commits: []string{"d00d1230000"}, Files: []string{"README.md", "billing.go"}}
	f.ride(t, train)
	moved := batchRunning
	moved.Behind = 1
	train.heads[101] = moved
	f.ride(t, train)
	moved.Pending = false
	train.heads[101] = moved
	f.ride(t, train)
	if len(batcher.built) != 1 || !slices.Equal(train.merged, []string{"#41 merge at c0ffee41", "#42 merge at c0ffee42"}) {
		t.Fatalf("built = %v, merged = %q: want the batch merged, the base's commits touching none of its files", batcher.built, train.merged)
	}
}

func TestABatchIsBuiltAgainWhenTheBaseMovedByMoreThanGitHubLists(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{}, 12)
	batcher.batchFiles = []string{"export.go"}
	batcher.moved = backlog.Comparison{Commits: []string{"d00d1230000"}, Files: []string{"vendor/a.go"}, Partial: true}
	movedUnder(train, batcher, 41)
	f.ride(t, train)
	moved := batchGreen
	moved.Behind = 1
	train.heads[101] = moved
	f.ride(t, train)
	if len(train.merged) != 0 || !slices.Equal(batcher.closed, []int{101}) {
		t.Fatalf("merged = %q, closed = %v: a base change too large to list may touch the batch's files", train.merged, batcher.closed)
	}
}

func TestARunningBatchFinishesBeforeANewlyVerifiedPullRequestBoards(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{}, 12)
	movedUnder(train, batcher, 41)
	f.ride(t, train) // #101: #41
	train.heads[101] = batchRunning
	built(t, f, 13)
	f.lister.issues = append(f.lister.issues, verifiedAs(13, 42))
	train.heads[42] = headAt(42)
	movedUnder(train, batcher, 42)
	f.ride(t, train)
	f.ride(t, train)
	if len(batcher.built) != 1 || len(batcher.closed) != 0 {
		t.Fatalf("built = %v, closed = %v: the running batch was thrown away for a newcomer", batcher.built, batcher.closed)
	}
	train.heads[101] = batchGreen
	f.ride(t, train)
	f.ride(t, train) // #102: #42
	if !slices.Equal(train.merged, []string{"#41 merge at c0ffee41"}) || len(batcher.built) != 2 || !slices.Equal(batcher.built[1], []int{42}) {
		t.Fatalf("merged = %q, built = %v: want #41 merged, then #42 in the next batch", train.merged, batcher.built)
	}
}

func TestAPullRequestThatConflictsWithTheBatchIsTakenOff(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{}, 12, 13)
	batcher.conflict = 42
	movedUnder(train, batcher, 41)
	f.ride(t, train)
	last := train.comments[len(train.comments)-1]
	if !strings.HasPrefix(last, "#13 "+backlog.OffTrainPhrase+" pull request #42,") || !strings.Contains(last, "does not merge cleanly") {
		t.Fatalf("last comment = %q", last)
	}
	f.ride(t, train)
	if len(batcher.built) != 1 || !slices.Equal(batcher.built[0], []int{41}) {
		t.Fatalf("built = %v, want #41 without #42", batcher.built)
	}
}

func TestAnotherMachinesBatchHoldsThisMachinesBack(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{}, 12)
	batcher.open = []backlog.Batch{{PR: 90, Branch: backlog.BatchBranch("other", f.now), Created: f.now}}
	movedUnder(train, batcher, 41)
	f.ride(t, train)
	if len(train.comments) != 0 || len(batcher.built) != 0 {
		t.Fatalf("comments = %q, built = %v while another machine's batch runs", train.comments, batcher.built)
	}
	f.now = f.now.Add(trainHold)
	f.ride(t, train)
	if len(batcher.built) != 1 {
		t.Fatalf("built = %v: the train does not wait for a machine that is gone", batcher.built)
	}
}

func TestABatchThisMachineOpenedBeforeARestartIsTakenInHandAgain(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{}, 12, 13)
	batcher.open = []backlog.Batch{{PR: 90, Branch: backlog.BatchBranch(f.Machine.Name, f.now), Created: f.now,
		Cars: []backlog.Car{{Issue: 12, PR: 41, SHA: "c0ffee41"}, {Issue: 13, PR: 42, SHA: "c0ffee42"}}}}
	train.heads[90] = batchGreen
	f.ride(t, train)
	if len(batcher.built) != 0 || !slices.Equal(train.merged, []string{"#41 merge at c0ffee41", "#42 merge at c0ffee42"}) {
		t.Fatalf("built = %v, merged = %q, want the open batch merged", batcher.built, train.merged)
	}
}

func TestARedBaseHoldsTheBatchTrainWithoutSplittingOrTakingAnythingOff(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{}, 12, 13)
	f.ride(t, train) // #101: #41, #42
	red := batchRed
	red.Failed = []string{"test"}
	train.heads[101] = red
	train.base = backlog.BaseState{Branch: "main", SHA: "ba5e1", Failed: []string{"test"}}
	f.ride(t, train)
	f.ride(t, train)
	f.ride(t, train)
	if len(batcher.built) != 1 || !slices.Equal(batcher.closed, []int{101}) {
		t.Fatalf("built = %v, closed = %v: want the batch closed and none built while the base is red", batcher.built, batcher.closed)
	}
	for _, c := range train.comments {
		if strings.Contains(c, backlog.OffTrainPhrase) {
			t.Fatalf("comment %q: nothing is to blame on the pull requests", c)
		}
	}
	train.base = backlog.BaseState{Branch: "main", SHA: "f1xed"}
	f.ride(t, train)
	if len(batcher.built) != 2 || !slices.Equal(batcher.built[1], []int{41, 42}) {
		t.Fatalf("built = %v, want the whole batch built again once the base is green", batcher.built)
	}
}

func TestAPullRequestAloneOnTheTrainWhoseHeadHasTheBaseIsMergedWithoutABatch(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{MergeMethod: "squash"}, 12)
	f.ride(t, train)
	if len(batcher.built) != 0 || !slices.Equal(train.merged, []string{"#41 squash at c0ffee41"}) {
		t.Fatalf("built = %v, merged = %q: want #41 merged at the head its own checks passed on", batcher.built, train.merged)
	}
}

func TestAPullRequestAloneOnTheTrainIsMergedWithoutABatchWhenTheBaseMovedApartFromItsFiles(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{}, 12)
	batcher.batchFiles = []string{"export.go"}
	batcher.moved = backlog.Comparison{Commits: []string{"d00d1230000"}, Files: []string{"README.md"}}
	movedUnder(train, batcher, 41)
	f.ride(t, train)
	if len(batcher.built) != 0 || !slices.Equal(train.merged, []string{"#41 merge at c0ffee41"}) {
		t.Fatalf("built = %v, merged = %q: want #41 merged, the base's commits touching none of its files", batcher.built, train.merged)
	}
}

func TestAPullRequestAloneOnTheTrainWhoseChecksStillRunIsNotMergedDirectly(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{}, 12)
	h := train.heads[41]
	h.Pending = true
	train.heads[41] = h
	f.ride(t, train)
	if len(train.merged) != 0 || len(batcher.built) != 1 {
		t.Fatalf("merged = %q, built = %v: want a batch, its own checks not passed", train.merged, batcher.built)
	}
}

func TestFewerPullRequestsThanTheMinimumMergeEachOnItsOwnChecksWithoutABatch(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{MergeTrainMin: 3}, 12, 13)
	movedUnder(train, batcher, 41, 42) // the base moved in their files: the minimum still merges them
	f.ride(t, train)
	if len(batcher.built) != 0 || !slices.Equal(train.merged, []string{"#41 merge at c0ffee41", "#42 merge at c0ffee42"}) {
		t.Fatalf("built = %v, merged = %q: want both merged on their own checks", batcher.built, train.merged)
	}
}

func TestAsManyPullRequestsAsTheMinimumAreTestedInABatch(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{MergeTrainMin: 2}, 12, 13)
	f.ride(t, train)
	if len(train.merged) != 0 || len(batcher.built) != 1 || !slices.Equal(batcher.built[0], []int{41, 42}) {
		t.Fatalf("merged = %q, built = %v: want one batch of both", train.merged, batcher.built)
	}
}

func TestBelowTheMinimumAPullRequestWhoseChecksStillRunIsBatchedWhenNothingMerges(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	train, batcher := withBatches(t, f, config.Verify{MergeTrainMin: 3}, 12)
	h := train.heads[41]
	h.Pending = true
	train.heads[41] = h
	f.ride(t, train)
	if len(train.merged) != 0 || len(batcher.built) != 1 {
		t.Fatalf("merged = %q, built = %v: want a batch, its own checks not passed", train.merged, batcher.built)
	}
}
