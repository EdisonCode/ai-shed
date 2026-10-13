package supervisor

import (
	"fmt"
	"slices"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// applyHolds holds the items that would make work for another: an issue
// that depends on one still open, and an issue that touches the same code
// as one another worker of the machine has in hand. Two branches on the same
// code go stale on each other's merge, and cost a rebase and a full run of
// the checks. With stacking, an issue whose one open dependency has a ready
// pull request is built on that pull request's branch instead.
//
// Started work is never held: the worker's own issue in hand, rework and
// eye checks.
func (s *Supervisor) applyHolds(w config.Worker, items []QueueItem, open []backlog.Issue, checkins []runlog.Checkin) {
	byNumber := map[int]backlog.Issue{}
	for _, i := range open {
		byNumber[i.Number] = i
	}
	inHand := runlog.IssueInHand(checkins, w.Name)
	others := map[int]string{}
	for _, other := range s.Machine.Workers {
		if n := runlog.IssueInHand(checkins, other.Name); other.Name != w.Name && n != 0 {
			others[n] = other.Name
		}
	}
	inFlight := make([]int, 0, len(others))
	for n := range others {
		inFlight = append(inFlight, n)
	}
	slices.Sort(inFlight)
	for n := range items {
		item := &items[n]
		issue, ok := byNumber[item.Number]
		if !ok || item.Look != "" || item.Rework != "" || item.Stack != "" || item.Number == inHand {
			continue
		}
		if deps := backlog.OpenDeps(issue); len(deps) > 0 {
			if s.stackOnDep(item, deps, byNumber) {
				continue
			}
			item.Blocked = fmt.Sprintf("waits for %s to close", backlog.DepWords(deps, issue.Repo))
			continue
		}
		for _, number := range inFlight {
			other, ok := byNumber[number]
			if !ok {
				continue
			}
			if why := backlog.Overlap(issue, other, s.Settings.OverlapLabels); why != "" {
				item.Blocked = fmt.Sprintf("overlaps #%d, which %s has in hand: %s", number, others[number], why)
				break
			}
		}
	}
}

// stackOnDep lets an item go ahead on top of its one open dependency, when
// the fleet stacks and that dependency has a ready pull request that waits
// for review.
func (s *Supervisor) stackOnDep(item *QueueItem, deps []backlog.Dep, byNumber map[int]backlog.Issue) bool {
	if !s.Settings.Stack || len(deps) != 1 || deps[0].Repo != item.Repo {
		return false
	}
	dep, ok := byNumber[deps[0].Number]
	if !ok {
		return false
	}
	pr := backlog.HandedPR(dep, s.Signals)
	state, open := dep.OpenPRs[pr]
	if pr == 0 || !open || state.Draft || state.Problem != "" {
		return false
	}
	item.StackOn = fmt.Sprintf("#%d's pull request #%d waits for review", dep.Number, pr)
	item.StackDep, item.StackPR = dep.Number, pr
	return true
}
