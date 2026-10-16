package supervisor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// shedQueue gives work nobody here has started to the fleet while the
// machine stays busy: after capacity.release_after of being busy, one issue
// at a time, each release_after, goes to the account's overflow label, and a
// machine with room takes it. Each machine sees only itself, so it cannot
// know which machine has room; the overflow label lets the first one that
// does take it.
func (s *Supervisor) shedQueue(ctx context.Context, now time.Time) {
	after := s.Machine.Capacity.ReleaseAfterOrZero()
	if after == 0 || s.Account == nil || s.Account.Overflow == "" || s.Releaser == nil {
		return
	}
	reason := s.busy(ctx)
	if reason == "" {
		s.busySince = time.Time{}
		return
	}
	if s.busySince.IsZero() {
		s.busySince = now
	}
	if now.Sub(s.busySince) < after || now.Sub(s.lastShed) < after {
		return
	}
	s.lastShed = now
	issue, remove, ok := s.unstarted(ctx)
	if !ok {
		return
	}
	busyFor := now.Sub(s.busySince).Round(time.Minute)
	comment := fmt.Sprintf("%s %s, busy for %s: %s. Nobody here has started it. A machine with room takes it from `%s`.",
		backlog.ReleasedPhrase, s.Machine.Name, spell(busyFor), reason, s.Account.Overflow)
	if err := s.Releaser.Release(ctx, issue.Repo, issue.Number, remove, s.Account.Overflow, comment); err != nil {
		s.Logf("release unstarted #%d: %v", issue.Number, err)
		return
	}
	s.Logf("released unstarted %s#%d to %s: the machine has been busy for %s: %s", issue.Repo, issue.Number, s.Account.Overflow, spell(busyFor), reason)
}

// unstarted returns the first issue of the machine's queues, in queue order,
// that nobody has started: no worker here was ever handed it, it names no
// pull request, it was never released before, and the owner did not put it
// first here. remove are the machine's queue labels on it.
func (s *Supervisor) unstarted(ctx context.Context) (backlog.Issue, []string, bool) {
	checkins, err := runlog.ReadCheckins(s.StateDir)
	if err != nil {
		s.Logf("release unstarted: %v", err)
		return backlog.Issue{}, nil, false
	}
	for _, w := range s.Machine.Builders() {
		queue, err := s.queue(ctx, w)
		if err != nil {
			s.Logf("release unstarted: worker %s: %v", w.Name, err)
			continue
		}
		for _, item := range queue {
			if !freshWork(item) || s.inHand(item.Number) || runlog.Builder(checkins, item.Number) != "" {
				continue
			}
			issue, found, err := backlog.Find(ctx, s.Lister, s.Machine.SourcesFor(w), item.Number)
			if err != nil || !found || backlog.HandedPR(issue, s.Signals) != 0 || everReleased(issue) {
				continue
			}
			if remove := s.queueLabelsOn(issue); len(remove) > 0 {
				return issue, remove, true
			}
		}
	}
	return backlog.Issue{}, nil, false
}

// freshWork reports whether the queue item is new work a worker could start
// now: not rework, a look, a verification or an answered question, not from
// the backlog, and not put first by the owner.
func freshWork(item QueueItem) bool {
	return item.Workable() && !item.Backfill && !item.Bumped && !item.resumes() && !item.Ruled &&
		item.Look == "" && item.Verify == "" && item.ReleasedBy == ""
}

// inHand reports whether a worker of the machine has the issue now.
func (s *Supervisor) inHand(number int) bool {
	for _, st := range s.workers {
		if st.issue == number {
			return true
		}
	}
	return false
}

// everReleased reports whether the issue was released to the fleet before.
// An issue moves at most once, so two busy machines do not pass it back and
// forth.
func everReleased(issue backlog.Issue) bool {
	return slices.ContainsFunc(issue.Comments, func(c backlog.Comment) bool {
		return strings.HasPrefix(strings.TrimSpace(c.Body), backlog.ReleasedPhrase)
	})
}

// queueLabelsOn returns the labels of the machine's own queues that the
// issue carries. Without one, the issue is in a queue by a search, and no
// label change can take it out.
func (s *Supervisor) queueLabelsOn(issue backlog.Issue) []string {
	var labels []string
	for _, src := range s.Machine.AllSources() {
		if src.Repo == issue.Repo && src.Label != "" && slices.Contains(issue.LabelNames(), src.Label) && !slices.Contains(labels, src.Label) {
			labels = append(labels, src.Label)
		}
	}
	return labels
}
