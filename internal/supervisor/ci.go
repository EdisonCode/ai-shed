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

// restartSkippedCI starts the CI again of each ready pull request handed
// back here whose head its CI skipped: its last commit came while it was a
// draft, say, and the ready_for_review run went to the commit before. Such
// a pull request is not red, so it is not rework, and it is not pending, so
// nothing waits for it: without this it waits in no queue. Each head is
// started once; only the machine whose log shows it built the work does it.
func (s *Supervisor) restartSkippedCI(ctx context.Context, open []backlog.Issue, checkins []runlog.Checkin) {
	if s.CI == nil {
		return
	}
	for _, i := range open {
		pr := backlog.HandedPR(i, s.Signals)
		p, isOpen := i.OpenPRs[pr]
		if pr == 0 || !isOpen || p.Draft || !p.NoCI || runlog.Builder(checkins, i.Number) == "" {
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
		s.Logf("started the CI of %s#%d again: every check on its head %s was skipped or cancelled", i.Repo, pr, short(p.Head))
	}
}
