package supervisor

import (
	"context"
	"fmt"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// CIRestarter starts a pull request's CI on its head again.
type CIRestarter interface {
	RestartCI(ctx context.Context, repo string, pr int) error
}

// restartStuckCI starts the CI again of each ready pull request handed back
// here that nothing else would move:
//   - its CI skipped its head: its last commit came while it was a draft,
//     say, and the ready_for_review run went to the commit before. It is not
//     red, so it is not rework, and it is not pending, so nothing waits for it.
//   - it was handed back with a failed check in front of the worker. Sending
//     it back for that would change nothing, so it is not rework either. A new
//     run that passes clears it; one that fails again finishes after the
//     hand-back, and that is rework.
//
// Without this either waits in no queue. Each head is started once; only the
// machine whose log shows it built the work does it.
func (s *Supervisor) restartStuckCI(ctx context.Context, open []backlog.Issue, checkins []runlog.Checkin) {
	if s.CI == nil {
		return
	}
	for _, i := range open {
		pr := backlog.HandedPR(i, s.Signals)
		p, isOpen := i.OpenPRs[pr]
		if pr == 0 || !isOpen || p.Draft || runlog.Builder(checkins, i.Number) == "" {
			continue
		}
		red := backlog.HandedBackRed(i, s.Signals) != ""
		if !p.NoCI && !red {
			continue
		}
		key := fmt.Sprintf("%s#%d@%s", i.Repo, pr, p.Head)
		if s.ciRestarted[key] {
			continue
		}
		if s.ciRestarted == nil {
			s.ciRestarted = map[string]bool{}
		}
		s.ciRestarted[key] = true
		if err := s.CI.RestartCI(ctx, i.Repo, pr); err != nil {
			s.Logf("start the CI of %s#%d again: %v", i.Repo, pr, err)
			continue
		}
		why := "every check on its head %s was skipped or canceled"
		if red {
			why = "it was handed back with a failed check on its head %s"
		}
		s.Logf("started the CI of %s#%d again: "+why, i.Repo, pr, short(p.Head))
	}
}
