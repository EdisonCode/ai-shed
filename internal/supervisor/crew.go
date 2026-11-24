package supervisor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// crewKept is how long the open issues of a goal in one repository are
// reused. A goal spans repositories other machines build in, so each read
// is a GitHub search; the crew changes in hours, not minutes.
const crewKept = 5 * time.Minute

type crewRead struct {
	issues []backlog.Issue
	at     time.Time
}

// crewGoals returns the goals a crew worker builds: the goal of the issue
// it has in hand, and of each open issue whose plan its session signed.
func (s *Supervisor) crewGoals(w config.Worker, open []backlog.Issue, checkins []runlog.Checkin) []string {
	if !w.Crew || s.Settings.GoalLabels == "" {
		return nil
	}
	inHand := runlog.BuildingInHand(checkins, w.Name)
	var goals []string
	for _, i := range open {
		if i.Number != inHand && backlog.PlanSession(i) != w.Session {
			continue
		}
		if g := backlog.GoalLabel(i.LabelNames(), s.Settings.GoalLabels); g != "" && !slices.Contains(goals, g) {
			goals = append(goals, g)
		}
	}
	slices.Sort(goals)
	return goals
}

// crewIssues lists the open issues of each goal in every repository a crew
// worker of the fleet builds in, reused for crewKept.
func (s *Supervisor) crewIssues(ctx context.Context, goals []string) map[string][]backlog.Issue {
	out := map[string][]backlog.Issue{}
	for _, goal := range goals {
		for _, repo := range s.CrewRepos {
			key := repo + "\x00" + goal
			read, ok := s.crew[key]
			if !ok || s.Now().Sub(read.at) >= crewKept {
				issues, err := s.Lister.List(ctx, config.IssueSource{Repo: repo, Query: fmt.Sprintf("label:%q", goal)})
				if err != nil {
					s.Logf("crew issues of %s in %s: %v", goal, repo, err)
					continue
				}
				read = crewRead{issues: issues, at: s.Now()}
				if s.crew == nil {
					s.crew = map[string]crewRead{}
				}
				s.crew[key] = read
			}
			out[goal] = append(out[goal], read.issues...)
		}
	}
	return out
}

// crewText is the part of a crew worker's roster that names, for each goal
// it builds, which crew-mate builds which part: the open issues of the goal
// whose plan a crew-mate's session signed. "" when it builds no goal.
func crewText(w config.Worker, goals []string, issues map[string][]backlog.Issue) string {
	if len(goals) == 0 {
		return ""
	}
	mates := map[string]bool{}
	for _, p := range w.PeerList {
		mates[p.Session] = true
	}
	var b strings.Builder
	b.WriteString("\n# Your crew\n\nThe crew-mates who build other parts of the goals you build, and what each has signed. Before you push a change to something one of them builds on (an API, an event, a schema, a shared type), message them what changes and why.\n")
	for _, goal := range goals {
		var lines []string
		for _, i := range issues[goal] {
			session := backlog.PlanSession(i)
			if session == "" || session == w.Session || !mates[session] {
				continue
			}
			line := fmt.Sprintf("- `%s` builds %s#%d %s", session, i.Repo, i.Number, i.Title)
			if paths := backlog.Paths(i); len(paths) > 0 {
				line += " (paths: " + strings.Join(paths, ", ") + ")"
			}
			lines = append(lines, line)
		}
		fmt.Fprintf(&b, "\n## %s\n\n", goal)
		if len(lines) == 0 {
			b.WriteString("No crew-mate has signed a plan on this goal yet.\n")
			continue
		}
		b.WriteString(strings.Join(lines, "\n") + "\n")
	}
	return b.String()
}
