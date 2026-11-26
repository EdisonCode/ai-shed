package supervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// goalCheckEvery is how often a machine looks for goals whose last slice
// has closed: a goal ends in days, not minutes.
const goalCheckEvery = 10 * time.Minute

// offerGoalChecks gives a crew worker the check of each goal whose slices
// are all closed: does the goal's done-when hold, across its repositories,
// on staging? Every machine works out the same crew worker, the first of
// the goal's crew in order, so only one offers it, and each ask for a check
// is offered once.
func (s *Supervisor) offerGoalChecks(ctx context.Context, now time.Time) {
	if s.Settings.GoalLabels == "" || len(s.CrewRepos) == 0 || now.Sub(s.lastGoalCheck) < goalCheckEvery {
		return
	}
	s.lastGoalCheck = now
	mine := map[string]config.Worker{}
	for _, w := range s.Machine.Workers {
		if w.Crew {
			mine[w.Session] = w
		}
	}
	if len(mine) == 0 {
		return
	}
	crew := slices.Sorted(func(yield func(string) bool) {
		for session := range s.crewSessions() {
			if !yield(session) {
				return
			}
		}
	})
	w, ok := mine[crew[0]]
	if !ok {
		return
	}
	for _, repo := range s.CrewRepos {
		briefs, err := s.Lister.List(ctx, config.IssueSource{Repo: repo, Query: fmt.Sprintf("%q in:title", backlog.GoalBriefPrefix)})
		if err != nil {
			s.Logf("goal checks: %v", err)
			continue
		}
		for _, b := range briefs {
			goal := backlog.GoalLabel(b.LabelNames(), s.Settings.GoalLabels)
			state, asked := backlog.GoalCheck(b)
			if goal == "" || !backlog.IsGoalBrief(b.Title) || state != backlog.GoalCheckDue {
				continue
			}
			open := slices.DeleteFunc(slices.Clone(s.crewIssues(ctx, []string{goal})[goal]), func(i backlog.Issue) bool { return backlog.IsGoalBrief(i.Title) })
			if len(open) > 0 {
				continue
			}
			key := fmt.Sprintf("goalcheck-%s-%d-%d", slug(b.Repo), b.Number, asked+1)
			if s.taskOffered(key) {
				continue
			}
			id := fmt.Sprintf("%d-%s", now.Unix(), key)
			path := filepath.Join(s.StateDir, runlog.TasksDir, w.Name, id+".md")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				err = os.WriteFile(path, []byte(goalCheckBrief(b, goal)), 0o644)
			} else {
				s.Logf("goal check of %s: %v", goal, err)
				continue
			}
			s.Logf("every slice of %s is closed: worker %s is asked to check its done-when on staging (task %s)", goal, w.Name, id)
		}
	}
}

// goalCheckBrief is the task that checks a whole goal once its slices have
// closed. It checks; it does not fix.
func goalCheckBrief(b backlog.Issue, goal string) string {
	brief := backlog.ParseGoalBrief(b)
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Check that %s works as a whole\n\nEvery slice of %s is closed. Check, on staging, that the parts work together as the goal brief %s#%d says they should.\n\n", goal, goal, b.Repo, b.Number)
	fmt.Fprintf(&sb, "1. Read the brief: gh issue view %d --repo %s --comments\n", b.Number, b.Repo)
	sb.WriteString("2. Check each line of Done when, end to end on staging, across every repository the goal touches. Do it as a user would: sign in, click, compare. Note what you saw for each line.\n")
	fmt.Fprintf(&sb, "3. Post one comment on the brief. When every line held, start it with %q and list each line with what you saw. When one did not, start it with %q, and say which line failed, what you saw, and where in which repository the fault seems to be.\n", backlog.GoalCheckedPhrase+":", backlog.GoalFailedPhrase+":")
	sb.WriteString("4. Change no code and open no pull request: a failure is the owner's to plan. Write in your report file what you found, and stop.\n")
	if len(brief.DoneWhen) > 0 {
		sb.WriteString("\n## Done when\n\n")
		for _, d := range brief.DoneWhen {
			sb.WriteString("- " + d + "\n")
		}
	} else {
		sb.WriteString("\nThe brief has no Done when: check that each contract it lists works between its provider and its users, and say in your comment that the brief should have one.\n")
	}
	return sb.String()
}
