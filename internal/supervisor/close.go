package supervisor

import (
	"context"
	"fmt"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// Closer reads what closing a finished issue needs from GitHub, and closes
// it.
type Closer interface {
	MergeOf(ctx context.Context, repo string, pr int) (backlog.Merge, error)
	Regressions(ctx context.Context, repo string, number int) ([]int, error)
	Close(ctx context.Context, repo string, number int, comment string) error
}

// closeRecheck is how long a finished issue that could not be closed yet
// waits before GitHub is asked about it again.
const closeRecheck = 30 * time.Minute

// closeFinished closes the issues of repositories with close_finished whose
// work is done: every pull request merged and nothing left to do, verified
// where the repository verifies, its eye check passed, the checks on the
// merge commit green, and no regression named in the quiet period since the
// merge, or the passed look if later. A finished issue stays open while
// anything is missing, and a step left for the owner keeps it open too.
// Only the machine whose log shows it built the work does it, so two
// machines do not act on one issue.
func (s *Supervisor) closeFinished(ctx context.Context, open []backlog.Issue, checkins []runlog.Checkin) {
	if s.Closer == nil {
		return
	}
	now := s.Now()
	for _, i := range open {
		repo, ok := s.repoConfig(i.Repo)
		if !ok || !repo.CloseFinished || runlog.Builder(checkins, i.Number) == "" {
			continue
		}
		pr, wait, since := backlog.CloseWait(i, s.Signals)
		key := fmt.Sprintf("%s#%d", i.Repo, i.Number)
		if pr == 0 || wait != "" || s.closed[key] || now.Sub(s.closeChecked[key]) < closeRecheck {
			continue
		}
		if s.closeChecked == nil {
			s.closeChecked, s.closed = map[string]time.Time{}, map[string]bool{}
		}
		s.closeChecked[key] = now
		m, err := s.Closer.MergeOf(ctx, i.Repo, pr)
		if err != nil {
			s.Logf("close %s: %v", key, err)
			continue
		}
		if m.At.After(since) {
			since = m.At
		}
		after := repo.CloseAfterOrDefault()
		if now.Before(since.Add(after)) {
			continue
		}
		if m.Checks == backlog.CIFailed || m.Checks == backlog.CIPending {
			s.Logf("not closing %s yet: the checks on merge commit %s of pull request #%d %s", key, short(m.Commit), pr, m.Checks)
			continue
		}
		regressions, err := s.Closer.Regressions(ctx, i.Repo, i.Number)
		if err != nil {
			s.Logf("close %s: %v", key, err)
			continue
		}
		if len(regressions) > 0 {
			s.Logf("not closing %s: %s names a regression of it", key, backlog.DepWords(deps(i.Repo, regressions), i.Repo))
			continue
		}
		comment := closeComment(i, s.Signals, pr, m, now, after)
		if err := s.Closer.Close(ctx, i.Repo, i.Number, comment); err != nil {
			s.Logf("close %s: %v", key, err)
			continue
		}
		s.closed[key] = true
		s.Logf("closed %s: pull request #%d merged, and nothing reopened it in %s", key, pr, spell(after))
	}
}

// closeComment says why shed closed the issue, and how to undo it.
func closeComment(i backlog.Issue, signals []config.Signal, pr int, m backlog.Merge, now time.Time, after time.Duration) string {
	evidence := fmt.Sprintf("pull request #%d merged %s ago", pr, spell(now.Sub(m.At).Round(time.Hour)))
	if i.Verify {
		evidence += ", it was verified"
	}
	if state, _, _, _ := backlog.EyeCheck(i, signals); state == backlog.EyesPassed {
		evidence += ", its eye check passed"
	}
	checks := "reported nothing"
	if m.Checks == backlog.CIPassed {
		checks = "passed"
	}
	return fmt.Sprintf("%s %s, the checks on its merge commit %s %s, and no regression was named in the %s since. Nothing was left to do. If that is wrong, reopen it, or write `%s #%d` on the issue that found the fault.",
		backlog.ClosedPhrase, evidence, short(m.Commit), checks, spell(after), backlog.RegressionPhrase, i.Number)
}

// deps names issue numbers of one repository as dependencies, for DepWords.
func deps(repo string, numbers []int) []backlog.Dep {
	out := make([]backlog.Dep, 0, len(numbers))
	for _, n := range numbers {
		out = append(out, backlog.Dep{Repo: repo, Number: n})
	}
	return out
}
