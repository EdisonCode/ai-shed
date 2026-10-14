package supervisor

import (
	"context"
	"fmt"
	"slices"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// Merger puts a pull request on its way to the merge, the way the
// repository merges: into its merge queue, or by itself once its required
// checks pass. It comments on the issue that shed did so.
type Merger interface {
	QueueMerge(ctx context.Context, repo string, issue, pr int, method, comment string) error
}

// queueMerges moves the owner's approval of low-risk work from the merge to
// the promotion, where the repository says so: a pull request a verifier
// passed at low risk, with green checks, nothing else waiting on the owner
// and not yet on its way, is queued for merge. A look on staging that a
// worker of the fleet does waits for nothing: it can only happen after the
// merge, and a failed one reopens the issue for the owner. Only the machine whose log
// shows it built the work does it, so two machines do not act on one pull
// request. Where the repository runs a merge train, the pull request waits
// for it instead. The owner approves what reaches production, with shed
// promote.
func (s *Supervisor) queueMerges(ctx context.Context, open []backlog.Issue, checkins []runlog.Checkin) {
	trains := map[string][]boardable{}
	for _, i := range open {
		repo, v, ok := s.mergeReady(i, checkins)
		if !ok {
			continue
		}
		if repo.Verify.MergeTrain {
			trains[i.Repo] = append(trains[i.Repo], boardable{issue: i, v: v})
			continue
		}
		if s.Merger == nil {
			continue
		}
		key := fmt.Sprintf("%s#%d", i.Repo, v.PR)
		comment := fmt.Sprintf("%s pull request #%d, verified at low risk with green checks. The verifier said: %s\n\nThe merge is no longer yours on this repository: shed promote lists it with what reaches production.", backlog.QueuedPhrase, v.PR, v.Note)
		if err := s.Merger.QueueMerge(ctx, i.Repo, i.Number, v.PR, repo.Verify.MergeMethodOrDefault(), comment); err != nil {
			s.Logf("queue %s for merge: %v", key, err)
			continue
		}
		if s.mergeQueued == nil {
			s.mergeQueued = map[string]bool{}
		}
		s.mergeQueued[key] = true
		s.Logf("queued %s for merge: verified at low risk", key)
	}
	if s.Train == nil {
		return
	}
	for _, repo := range s.Repos {
		if repo.Trains() {
			s.runTrain(ctx, repo, trains[repo.Name], checkins)
		}
	}
}

// mergeReady reports whether the issue's pull request may be queued for
// merge, with its repository and its verification.
func (s *Supervisor) mergeReady(i backlog.Issue, checkins []runlog.Checkin) (config.Repo, backlog.VerificationState, bool) {
	repo, ok := s.repoConfig(i.Repo)
	if !ok || repo.Verify == nil || !repo.Verify.MergeLow || !i.Verify {
		return repo, backlog.VerificationState{}, false
	}
	v, ready := backlog.MergeReady(i, s.Signals, s.Now(), slices.Contains(s.EyeCheckRepos, i.Repo))
	if !ready || s.mergeQueued[fmt.Sprintf("%s#%d", i.Repo, v.PR)] {
		return repo, v, false
	}
	if runlog.Builder(checkins, i.Number) == "" {
		return repo, v, false // another machine built it
	}
	return repo, v, true
}

// repoConfig is what the fleet file says about a repository.
func (s *Supervisor) repoConfig(name string) (config.Repo, bool) {
	for _, r := range s.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return config.Repo{}, false
}
