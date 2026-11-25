package supervisor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// GoalCounter counts the issues of a repository with a goal label: how many
// are closed, of how many in all.
type GoalCounter interface {
	GoalCounts(ctx context.Context, repo, label string) (closed, total int, err error)
}

// goalEvery is how long a goal's count is kept before it is read again: a
// goal moves by issues closed, not by the minute.
const goalEvery = time.Hour

type goalCount struct {
	at   time.Time
	done float64
}

// goalCache keeps each goal's closed share, by "repo label".
type goalCache struct {
	mu     sync.Mutex
	shares map[string]goalCount
}

// shareGoals sets on each item the closed share of its issue's goal, for
// goal_labels. A goal that cannot be counted counts as no goal: the queue
// does not wait on it.
func (s *Supervisor) shareGoals(ctx context.Context, items []QueueItem, open []backlog.Issue) {
	prefix := s.Settings.GoalLabels
	if prefix == "" || s.Goals == nil {
		return
	}
	goalOf := map[string]string{}
	for _, i := range open {
		if goal := backlog.GoalLabel(i.LabelNames(), prefix); goal != "" {
			goalOf[issueRef(i.Repo, i.Number)] = goal
		}
	}
	for n := range items {
		if goal := goalOf[issueRef(items[n].Repo, items[n].Number)]; goal != "" {
			items[n].goalDone = s.goalDone(ctx, items[n].Repo, goal)
		}
	}
}

// applyGoalModes applies the owner's focus and tabled goals to a worker's
// queue. An issue of a goal in focus goes ahead of every other goal's, so the
// fleet finishes the goal before it spreads over more. An issue of a tabled
// goal that nobody has started is held; the issue in hand, rework, a stack,
// a look, a verification and a bump go on, so work started on it ends.
func (s *Supervisor) applyGoalModes(w config.Worker, items []QueueItem, open []backlog.Issue, modes map[string]string, checkins []runlog.Checkin) {
	prefix := s.Settings.GoalLabels
	if prefix == "" || len(modes) == 0 {
		return
	}
	goalOf := map[string]string{}
	for _, i := range open {
		if goal := backlog.GoalLabel(i.LabelNames(), prefix); goal != "" {
			goalOf[issueRef(i.Repo, i.Number)] = goal
		}
	}
	inHand := runlog.IssueInHand(checkins, w.Name)
	for n := range items {
		item := &items[n]
		goal := goalOf[issueRef(item.Repo, item.Number)]
		switch modes[goal] {
		case runlog.GoalFocus:
			item.focus = true
		case runlog.GoalTabled:
			if item.Number == inHand || item.Bumped || item.Blocked != "" || item.resumes() || item.Look != "" || item.Verify != "" {
				continue
			}
			item.Blocked = fmt.Sprintf("the owner tabled goal %q; work already started on it goes on (shed goal clear to take it up again)", goal)
		}
	}
}

func (s *Supervisor) goalDone(ctx context.Context, repo, goal string) float64 {
	s.goals.mu.Lock()
	defer s.goals.mu.Unlock()
	key := repo + " " + goal
	if c, ok := s.goals.shares[key]; ok && s.Now().Sub(c.at) < goalEvery {
		return c.done
	}
	closed, total, err := s.Goals.GoalCounts(ctx, repo, goal)
	c := goalCount{at: s.Now()}
	switch {
	case err != nil:
		// Kept as no goal until the next read, so a failing count is not
		// asked and logged on every check-in.
		s.Logf("goal %s of %s: %v", goal, repo, err)
	case total > 0:
		c.done = float64(closed) / float64(total)
	}
	if s.goals.shares == nil {
		s.goals.shares = map[string]goalCount{}
	}
	s.goals.shares[key] = c
	return c.done
}

func issueRef(repo string, number int) string {
	return fmt.Sprintf("%s#%d", repo, number)
}
