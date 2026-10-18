package supervisor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/risk"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// Batcher builds and closes the batches of a batch train: draft pull
// requests that test several verified pull requests together.
type Batcher interface {
	RequiresUpToDate(ctx context.Context, repo, base string) (bool, error)
	BuildBatch(ctx context.Context, repo, base, branch string, cars []backlog.Car) (int, error)
	Batches(ctx context.Context, repo string) ([]backlog.Batch, error)
	CloseBatch(ctx context.Context, repo string, pr int) error
}

// strictEvery is how often a batch train reads again whether a base branch
// requires pull requests to be up to date. The owner may change the rule.
const strictEvery = time.Hour

// batchTrain is where a repository's batch train stands on this machine.
type batchTrain struct {
	// cars are the pull requests on the train, in the order they boarded.
	// The first testing of them are in the batch in hand.
	cars []batchCar
	// draft is the batch's draft pull request; 0 for none.
	draft   int
	testing int
	// half, once a batch failed, is how many pull requests the next batch
	// tests: the first half of the one that failed. 0 tests a full batch.
	half int
	// next is when the train moves again.
	next time.Time
	// looked is set once the train looked on GitHub for a batch and pull
	// requests this machine put on it before the agent started.
	looked bool
	// strict holds what was read of each base branch: whether it requires
	// pull requests to be up to date, and when that was read.
	strict map[string]strictRead
}

type strictRead struct {
	strict bool
	at     time.Time
}

// batchCar is a pull request on a batch train.
type batchCar struct {
	backlog.Car
	base string
	// alone is set for a pull request that touches a merge_train_alone
	// path: it is tested in a batch of its own.
	alone bool
}

// runBatch moves a repository's batch train one step. With no batch in
// hand, it boards verified pull requests this machine built, up to a batch,
// and builds a draft pull request of them on the base branch. With one in
// hand, it merges each of its pull requests once its checks pass, and
// splits it in half when they fail, until the pull request that fails is
// found and goes back to a worker.
func (s *Supervisor) runBatch(ctx context.Context, repo config.Repo, ready []boardable, checkins []runlog.Checkin) {
	now := s.Now()
	if s.batches == nil {
		s.batches = map[string]*batchTrain{}
	}
	bt := s.batches[repo.Name]
	if bt == nil {
		bt = &batchTrain{strict: map[string]strictRead{}}
		s.batches[repo.Name] = bt
	}
	if now.Before(bt.next) {
		return
	}
	bt.next = now.Add(trainEvery)
	if !bt.looked {
		if !s.recoverBatch(ctx, repo, bt, checkins) {
			return
		}
		bt.looked = true
	}
	if bt.draft != 0 {
		s.driveBatch(ctx, repo, bt)
		return
	}
	if len(ready) == 0 && len(bt.cars) == 0 {
		return
	}
	if s.otherBatch(ctx, repo.Name, now) {
		return
	}
	s.boardBatch(ctx, repo, bt, ready)
	if len(bt.cars) > 0 {
		s.buildBatch(ctx, repo, bt)
	}
}

// recoverBatch takes in hand again what this machine left on the train
// before the agent started: its open batch, and the pull requests it
// boarded. It reports whether the train may move.
func (s *Supervisor) recoverBatch(ctx context.Context, repo config.Repo, bt *batchTrain, checkins []runlog.Checkin) bool {
	batches, err := s.Batch.Batches(ctx, repo.Name)
	if err != nil {
		s.Logf("merge train %s: look for this machine's batch: %v", repo.Name, err)
		return false
	}
	for _, b := range batches {
		if !backlog.BatchOf(b.Branch, s.Machine.Name) {
			continue
		}
		if bt.draft != 0 || len(b.Cars) == 0 {
			s.closeBatch(ctx, repo.Name, b.PR)
			continue
		}
		bt.draft, bt.testing = b.PR, len(b.Cars)
		for _, c := range b.Cars {
			// The batch tested the head it names; the base is read again.
			h, err := s.Train.ReadHead(ctx, repo.Name, c.PR)
			if err != nil {
				s.Logf("merge train %s: read #%d: %v", repo.Name, c.PR, err)
				bt.draft, bt.testing, bt.cars = 0, 0, nil
				return false
			}
			bt.cars = append(bt.cars, batchCar{Car: c, base: h.Base})
		}
	}
	issues, err := s.Lister.List(ctx, config.IssueSource{Repo: repo.Name, Query: fmt.Sprintf("%q in:comments", backlog.QueuedPhrase)})
	if err != nil {
		s.Logf("merge train %s: look for the pull requests on the train: %v", repo.Name, err)
		return false
	}
	type boarded struct {
		issue, pr int
		at        time.Time
	}
	var mine []boarded
	for _, i := range issues {
		if pr, at := backlog.Boarded(i, s.Signals); pr != 0 && runlog.Builder(checkins, i.Number) != "" && !bt.has(pr) {
			mine = append(mine, boarded{i.Number, pr, at})
		}
	}
	sort.SliceStable(mine, func(a, b int) bool { return mine[a].at.Before(mine[b].at) })
	for _, b := range mine {
		h, err := s.Train.ReadHead(ctx, repo.Name, b.pr)
		if err != nil {
			s.Logf("merge train %s: read #%d: %v", repo.Name, b.pr, err)
			return false
		}
		if h.Open {
			bt.cars = append(bt.cars, batchCar{Car: backlog.Car{Issue: b.issue, PR: b.pr, SHA: h.SHA}, base: h.Base})
		}
	}
	return true
}

// otherBatch reports whether another machine's batch of the repository,
// opened lately, holds this machine's next batch back. After trainHold the
// other machine is taken to be gone.
func (s *Supervisor) otherBatch(ctx context.Context, repo string, now time.Time) bool {
	batches, err := s.Batch.Batches(ctx, repo)
	if err != nil {
		s.Logf("merge train %s: look for other machines' batches: %v", repo, err)
		return true
	}
	for _, b := range batches {
		if !backlog.BatchOf(b.Branch, s.Machine.Name) && now.Sub(b.Created) < trainHold {
			return true
		}
	}
	return false
}

// boardBatch puts verified pull requests on the train, the first verified
// first, until it holds a full batch.
func (s *Supervisor) boardBatch(ctx context.Context, repo config.Repo, bt *batchTrain, ready []boardable) {
	sort.SliceStable(ready, func(a, b int) bool { return ready[a].v.At.Before(ready[b].v.At) })
	for _, next := range ready {
		if len(bt.cars) >= repo.Verify.BatchSizeOrDefault() {
			return
		}
		if bt.has(next.v.PR) {
			continue
		}
		h, err := s.Train.ReadHead(ctx, repo.Name, next.v.PR)
		if err != nil {
			s.Logf("merge train %s: read #%d: %v", repo.Name, next.v.PR, err)
			continue
		}
		if !h.Open {
			continue
		}
		if s.refuses(ctx, repo.Name, bt, h.Base) {
			return
		}
		alone, err := s.alone(ctx, repo, next.v.PR)
		if err != nil {
			s.Logf("merge train %s: read what #%d changes: %v", repo.Name, next.v.PR, err)
			continue
		}
		comment := fmt.Sprintf("%s pull request #%d, verified at %s risk with green checks. The verifier said: %s\n\nIt is on the merge train: shed tests it in a batch with other verified pull requests, on a draft pull request, and merges it when the batch's checks pass. The merge is no longer yours on this repository: shed promote lists it with what reaches production.", backlog.QueuedPhrase, next.v.PR, next.v.Tier, next.v.Note)
		if err := s.Train.Comment(ctx, repo.Name, next.issue.Number, comment); err != nil {
			s.Logf("merge train %s: board #%d: %v", repo.Name, next.v.PR, err)
			return
		}
		bt.cars = append(bt.cars, batchCar{Car: backlog.Car{Issue: next.issue.Number, PR: next.v.PR, SHA: h.SHA}, base: h.Base, alone: alone})
		s.Logf("merge train %s: #%d boarded at %s: verified at %s risk", repo.Name, next.v.PR, short(h.SHA), next.v.Tier)
	}
}

// refuses reports whether the base branch requires pull requests to be up to
// date before they merge. A batch train cannot run there: it merges each
// pull request as it stands, without bringing it up to date.
func (s *Supervisor) refuses(ctx context.Context, repo string, bt *batchTrain, base string) bool {
	now := s.Now()
	if read, ok := bt.strict[base]; ok && now.Sub(read.at) < strictEvery {
		return read.strict
	}
	strict, err := s.Batch.RequiresUpToDate(ctx, repo, base)
	if err != nil {
		s.Logf("merge train %s: read whether %s requires pull requests to be up to date: %v", repo, base, err)
		return true
	}
	bt.strict[base] = strictRead{strict: strict, at: now}
	if strict {
		s.Logf("merge train %s: merge_train: batch cannot run: %s requires a pull request to be up to date with it before it merges, and a batch train merges each pull request as it stands. Turn that requirement off, or use merge_train: true", repo, base)
	}
	return strict
}

// alone reports whether the pull request touches a path that is tested in a
// batch of its own.
func (s *Supervisor) alone(ctx context.Context, repo config.Repo, pr int) (bool, error) {
	if len(repo.Verify.MergeTrainAlone) == 0 || s.Diffs == nil {
		return false, nil
	}
	d, err := s.Diffs.Diff(ctx, repo.Name, pr)
	if err != nil {
		return false, err
	}
	return risk.Touches(d.Files, repo.Verify.MergeTrainAlone), nil
}

// group is the pull requests the next batch tests: the first on the train,
// up to a batch (or half the last one, after it failed), all on one base.
// A pull request that goes alone ends the batch before it, or is a batch of
// its own.
func (bt *batchTrain) group(size int) []batchCar {
	n := min(len(bt.cars), size)
	if bt.half > 0 {
		n = min(n, bt.half)
	}
	if bt.cars[0].alone {
		return bt.cars[:1]
	}
	for i := 1; i < n; i++ {
		if bt.cars[i].alone || bt.cars[i].base != bt.cars[0].base {
			return bt.cars[:i]
		}
	}
	return bt.cars[:n]
}

// buildBatch opens the next batch as a draft pull request.
func (s *Supervisor) buildBatch(ctx context.Context, repo config.Repo, bt *batchTrain) {
	group := bt.group(repo.Verify.BatchSizeOrDefault())
	var cars []backlog.Car
	for _, c := range group {
		cars = append(cars, c.Car)
	}
	branch := backlog.BatchBranch(s.Machine.Name, s.Now())
	pr, err := s.Batch.BuildBatch(ctx, repo.Name, group[0].base, branch, cars)
	var conflict *backlog.MergeConflict
	switch {
	case errors.As(err, &conflict):
		if c, ok := bt.car(conflict.PR); ok {
			s.takeOff(ctx, repo.Name, bt, c, "does not merge cleanly into the base branch together with the pull requests before it on the train")
		}
		return
	case err != nil:
		s.Logf("merge train %s: build a batch of %s: %v", repo.Name, prList(cars), err)
		return
	}
	bt.draft, bt.testing = pr, len(group)
	s.Logf("merge train %s: batch #%d of %s opened on %s", repo.Name, pr, prList(cars), group[0].base)
}

// driveBatch moves the batch in hand: it waits for its checks, then merges
// each of its pull requests, or splits it when they fail.
func (s *Supervisor) driveBatch(ctx context.Context, repo config.Repo, bt *batchTrain) {
	h, err := s.Train.ReadHead(ctx, repo.Name, bt.draft)
	if err != nil {
		s.Logf("merge train %s: read batch #%d: %v", repo.Name, bt.draft, err)
		return
	}
	if !h.Open {
		s.Logf("merge train %s: batch #%d was closed; the next is built afresh", repo.Name, bt.draft)
		bt.draft, bt.testing = 0, 0
		return
	}
	// A pull request whose head moved is not what the batch tests: it
	// leaves the train, and the batch is built again without it.
	changed := false
	for _, c := range slices.Clone(bt.cars[:bt.testing]) {
		ch, err := s.Train.ReadHead(ctx, repo.Name, c.PR)
		if err != nil {
			s.Logf("merge train %s: read #%d: %v", repo.Name, c.PR, err)
			return
		}
		switch {
		case !ch.Open:
			s.Logf("merge train %s: #%d is no longer open", repo.Name, c.PR)
			bt.remove(c.PR)
			changed = true
		case ch.SHA != c.SHA:
			s.takeOff(ctx, repo.Name, bt, c, fmt.Sprintf("had new commits (%s, was %s) while batch pull request #%d tested it", short(ch.SHA), short(c.SHA), bt.draft))
			changed = true
		}
	}
	if changed {
		s.dropBatch(ctx, repo.Name, bt)
		return
	}
	switch {
	case h.Behind > 0:
		// The base moved under the batch: what it tested is not what would
		// land, and a failure may be the base's. It is built again.
		s.Logf("merge train %s: the base moved under batch #%d, %d commit(s); it is built again", repo.Name, bt.draft, h.Behind)
		s.dropBatch(ctx, repo.Name, bt)
	case h.Problem != "" && bt.testing == 1:
		c, draft := bt.cars[0], bt.draft
		s.dropBatch(ctx, repo.Name, bt)
		bt.half = 0
		s.takeOff(ctx, repo.Name, bt, c, fmt.Sprintf("tested by itself on the base branch in batch pull request #%d, %s", draft, h.Problem))
	case h.Problem != "":
		half := bt.testing / 2
		s.Logf("merge train %s: batch #%d %s; testing the first %d of its %d pull requests", repo.Name, bt.draft, h.Problem, half, bt.testing)
		s.dropBatch(ctx, repo.Name, bt)
		bt.half = half
	case !h.Green():
		// Its checks run.
	default:
		s.mergeBatch(ctx, repo, bt)
	}
}

// mergeBatch merges each pull request of a batch whose checks passed, in
// order, at the head the batch tested, and closes the batch.
func (s *Supervisor) mergeBatch(ctx context.Context, repo config.Repo, bt *batchTrain) {
	method := repo.Verify.MergeMethodOrDefault()
	for _, c := range slices.Clone(bt.cars[:bt.testing]) {
		if err := s.Train.MergeHead(ctx, repo.Name, c.PR, method, c.SHA); err != nil {
			// What merged is off the train; the rest are tested again on
			// the base that now has it.
			s.Logf("merge train %s: merge #%d at %s from batch #%d: %v", repo.Name, c.PR, short(c.SHA), bt.draft, err)
			s.dropBatch(ctx, repo.Name, bt)
			return
		}
		s.Logf("merge train %s: #%d merged at %s, tested in batch #%d", repo.Name, c.PR, short(c.SHA), bt.draft)
		bt.remove(c.PR)
		bt.testing--
	}
	s.dropBatch(ctx, repo.Name, bt)
	bt.half = 0
}

// dropBatch closes the batch in hand. Its pull requests stay on the train.
func (s *Supervisor) dropBatch(ctx context.Context, repo string, bt *batchTrain) {
	s.closeBatch(ctx, repo, bt.draft)
	bt.draft, bt.testing = 0, 0
}

func (s *Supervisor) closeBatch(ctx context.Context, repo string, pr int) {
	if err := s.Batch.CloseBatch(ctx, repo, pr); err != nil {
		s.Logf("merge train %s: close batch #%d and delete its branch: %v", repo, pr, err)
	}
}

// takeOff takes a pull request off the train, with a comment on its issue
// that says why. It goes back to a worker, and boards again once its next
// hand-back is verified.
func (s *Supervisor) takeOff(ctx context.Context, repo string, bt *batchTrain, c batchCar, why string) {
	comment := fmt.Sprintf("%s pull request #%d, at %s, %s. It goes back to a worker, and boards again once its next hand-back is verified.", backlog.OffTrainPhrase, c.PR, short(c.SHA), why)
	if err := s.Train.Comment(ctx, repo, c.Issue, comment); err != nil {
		s.Logf("merge train %s: take #%d off: %v", repo, c.PR, err)
		return
	}
	s.Logf("merge train %s: #%d taken off: %s", repo, c.PR, why)
	bt.remove(c.PR)
}

func (bt *batchTrain) has(pr int) bool {
	_, ok := bt.car(pr)
	return ok
}

func (bt *batchTrain) car(pr int) (batchCar, bool) {
	for _, c := range bt.cars {
		if c.PR == pr {
			return c, true
		}
	}
	return batchCar{}, false
}

func (bt *batchTrain) remove(pr int) {
	bt.cars = slices.DeleteFunc(bt.cars, func(c batchCar) bool { return c.PR == pr })
}

func prList(cars []backlog.Car) string {
	var prs []string
	for _, c := range cars {
		prs = append(prs, fmt.Sprintf("#%d", c.PR))
	}
	return strings.Join(prs, ", ")
}
