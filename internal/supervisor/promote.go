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
// request. The owner approves what reaches production, with shed promote.
func (s *Supervisor) queueMerges(ctx context.Context, open []backlog.Issue, checkins []runlog.Checkin) {
	if s.Merger == nil {
		return
	}
	for _, i := range open {
		repo, ok := s.repoConfig(i.Repo)
		if !ok || repo.Verify == nil || !repo.Verify.MergeLow || !i.Verify {
			continue
		}
		v := backlog.Verification(i, s.Signals)
		pr := i.OpenPRs[v.PR]
		key := fmt.Sprintf("%s#%d", i.Repo, v.PR)
		if v.State != backlog.VerifyPassed || v.Tier != "low" || pr.Draft || pr.Pending || pr.Problem != "" || pr.AutoMerge || s.mergeQueued[key] {
			continue
		}
		lookAfter := slices.Contains(s.EyeCheckRepos, i.Repo)
		if slices.ContainsFunc(backlog.Effects(i, s.Signals, s.Now()), func(a backlog.Ask) bool {
			return a.Effect != backlog.EffectMerge && !(lookAfter && a.Name == config.EyesSignal)
		}) {
			continue // something else on the issue waits on the owner
		}
		if runlog.Builder(checkins, i.Number) == "" {
			continue // another machine built it
		}
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
