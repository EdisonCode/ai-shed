package supervisor

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// Readier marks a draft pull request ready for review, and says so on its
// issue.
type Readier interface {
	MarkReady(ctx context.Context, repo string, issue, pr int, comment string) error
}

// UnparkedPhrase starts the comment on an issue whose parked draft shed
// marked ready.
const UnparkedPhrase = "Unparked:"

// unpark marks the oldest parked drafts ready while their repository has
// room under its limit of ready pull requests: a worker parked each as a
// draft at the limit, and its CI starts once it is ready. Only the machine
// whose log shows it built the work does it, so two machines do not act on
// one pull request.
func (s *Supervisor) unpark(ctx context.Context, open []backlog.Issue, checkins []runlog.Checkin) {
	if s.Readier == nil {
		return
	}
	for _, repo := range s.Repos {
		if !repo.ParksAtPRLimit() {
			continue
		}
		type parkedPR struct {
			issue, pr int
			since     time.Time
		}
		var parked []parkedPR
		ready, read := 0, false
		for _, i := range open {
			if i.Repo != repo.Name || i.OpenPRs == nil {
				continue
			}
			if !read {
				ready, read = backlog.ReadyPRs(i), true
				// A draft marked ready a moment ago may still be listed as
				// one: it counts as ready.
				for pr, p := range i.OpenPRs {
					if p.Draft && s.unparked[fmt.Sprintf("%s#%d", repo.Name, pr)] {
						ready++
					}
				}
			}
			pr, since := backlog.Parked(i, s.Signals)
			if pr == 0 || s.unparked[fmt.Sprintf("%s#%d", repo.Name, pr)] || runlog.Builder(checkins, i.Number) == "" {
				continue
			}
			parked = append(parked, parkedPR{issue: i.Number, pr: pr, since: since})
		}
		sort.SliceStable(parked, func(a, b int) bool { return parked[a].since.Before(parked[b].since) })
		for _, p := range parked {
			if ready >= repo.MaxOpenPRs {
				break
			}
			comment := fmt.Sprintf("%s pull request #%d is marked ready, so its CI runs: %s had %d ready pull request(s), under its limit of %d. It was parked as a draft at the limit.", UnparkedPhrase, p.pr, repo.Name, ready, repo.MaxOpenPRs)
			if err := s.Readier.MarkReady(ctx, repo.Name, p.issue, p.pr, comment); err != nil {
				s.Logf("unpark %s#%d: %v", repo.Name, p.pr, err)
				continue
			}
			if s.unparked == nil {
				s.unparked = map[string]bool{}
			}
			s.unparked[fmt.Sprintf("%s#%d", repo.Name, p.pr)] = true
			s.Logf("unparked %s#%d (#%d): %d ready pull request(s), under the limit of %d", repo.Name, p.pr, p.issue, ready, repo.MaxOpenPRs)
			ready++
		}
	}
}
