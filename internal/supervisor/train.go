package supervisor

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// Trainer runs a merge train: it reads a pull request at its head, brings
// it up to date with its base branch, and merges it at the head whose checks
// passed.
type Trainer interface {
	ReadHead(ctx context.Context, repo string, pr int) (backlog.Head, error)
	UpdateBranch(ctx context.Context, repo string, pr int, rebase bool) error
	MergeHead(ctx context.Context, repo string, pr int, method, sha string) error
	Comment(ctx context.Context, repo string, issue int, body string) error
}

// trainEvery is how often a repository's merge train moves.
const trainEvery = time.Minute

// trainHold is how long a pull request another machine put on the train
// holds this machine's next one back. After that, the other machine is taken
// to be gone: the train does not stop for it.
const trainHold = time.Hour

// train is where a repository's merge train stands on this machine.
type train struct {
	// issue and pr are the pull request in hand; 0 for none.
	issue, pr int
	// next is when the train moves again.
	next time.Time
	// looked is set once the train looked for a pull request this machine
	// put on it before the agent started.
	looked bool
}

// boardable is a pull request ready for the train, with when its
// verification passed: the first passed boards first.
type boardable struct {
	issue backlog.Issue
	v     backlog.VerificationState
}

// runTrain moves a repository's merge train one step: it takes the next
// verified pull request this machine built when nothing is in hand, brings
// the one in hand up to date with its base branch, and merges it when its
// checks passed at that head. A pull request whose checks fail there is
// taken off: it goes back to a worker as rework. One pull request is in hand
// at a time, so each merges on the main the one before it left.
func (s *Supervisor) runTrain(ctx context.Context, repo config.Repo, ready []boardable, checkins []runlog.Checkin) {
	now := s.Now()
	if s.trains == nil {
		s.trains = map[string]*train{}
	}
	t := s.trains[repo.Name]
	if t == nil {
		t = &train{}
		s.trains[repo.Name] = t
	}
	if now.Before(t.next) {
		return
	}
	t.next = now.Add(trainEvery)
	if t.pr == 0 && (len(ready) > 0 || !t.looked) {
		t.looked = true
		if held := s.findCar(ctx, repo.Name, t, checkins, now); held {
			return
		}
	}
	if t.pr == 0 {
		if len(ready) == 0 {
			return
		}
		sort.SliceStable(ready, func(a, b int) bool { return ready[a].v.At.Before(ready[b].v.At) })
		next := ready[0]
		comment := fmt.Sprintf("%s pull request #%d, verified at %s risk with green checks. The verifier said: %s\n\nIt is on the merge train: shed brings it up to date with the base branch and merges it when its checks pass on that head. The merge is no longer yours on this repository: shed promote lists it with what reaches production.", backlog.QueuedPhrase, next.v.PR, next.v.Tier, next.v.Note)
		if err := s.Train.Comment(ctx, repo.Name, next.issue.Number, comment); err != nil {
			s.Logf("merge train %s: board #%d: %v", repo.Name, next.v.PR, err)
			return
		}
		t.issue, t.pr = next.issue.Number, next.v.PR
		s.Logf("merge train %s: #%d boarded: verified at %s risk", repo.Name, t.pr, next.v.Tier)
	}
	s.drive(ctx, repo, t)
}

// findCar looks on GitHub for a pull request on the repository's train. One
// this machine put there is taken in hand again. It reports whether one
// another machine put there lately holds the train back.
func (s *Supervisor) findCar(ctx context.Context, repo string, t *train, checkins []runlog.Checkin, now time.Time) bool {
	issues, err := s.Lister.List(ctx, config.IssueSource{Repo: repo, Query: fmt.Sprintf("%q in:comments", backlog.QueuedPhrase)})
	if err != nil {
		s.Logf("merge train %s: look for the pull request in hand: %v", repo, err)
		return true
	}
	held := false
	for _, i := range issues {
		pr, at := backlog.Boarded(i, s.Signals)
		switch {
		case pr == 0:
		case runlog.Builder(checkins, i.Number) != "":
			t.issue, t.pr = i.Number, pr
			return false
		case now.Sub(at) < trainHold:
			held = true
		}
	}
	return held
}

// drive moves the pull request in hand: up to date, then checks, then merge.
func (s *Supervisor) drive(ctx context.Context, repo config.Repo, t *train) {
	method := repo.Verify.MergeMethodOrDefault()
	h, err := s.Train.ReadHead(ctx, repo.Name, t.pr)
	if err != nil {
		s.Logf("merge train %s: read #%d: %v", repo.Name, t.pr, err)
		return
	}
	switch {
	case !h.Open:
		s.Logf("merge train %s: #%d is no longer open", repo.Name, t.pr)
		t.issue, t.pr = 0, 0
	case h.Problem != "":
		comment := fmt.Sprintf("%s pull request #%d, brought up to date with its base branch, %s at %s. It goes back to a worker, and boards again once its next hand-back is verified.", backlog.OffTrainPhrase, t.pr, h.Problem, short(h.SHA))
		if err := s.Train.Comment(ctx, repo.Name, t.issue, comment); err != nil {
			s.Logf("merge train %s: take #%d off: %v", repo.Name, t.pr, err)
			return
		}
		s.Logf("merge train %s: #%d taken off: %s", repo.Name, t.pr, h.Problem)
		t.issue, t.pr = 0, 0
	case h.Behind > 0:
		if err := s.Train.UpdateBranch(ctx, repo.Name, t.pr, method == "rebase"); err != nil {
			s.Logf("merge train %s: bring #%d up to date: %v", repo.Name, t.pr, err)
			return
		}
		s.Logf("merge train %s: #%d brought up to date, %d commit(s) behind", repo.Name, t.pr, h.Behind)
	case !h.Green():
		// Its checks run on the head the train brought up to date.
	default:
		if err := s.Train.MergeHead(ctx, repo.Name, t.pr, method, h.SHA); err != nil {
			s.Logf("merge train %s: merge #%d at %s: %v", repo.Name, t.pr, short(h.SHA), err)
			return
		}
		s.Logf("merge train %s: #%d merged at %s", repo.Name, t.pr, short(h.SHA))
		t.issue, t.pr = 0, 0
	}
}
