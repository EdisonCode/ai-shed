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
		brief := briefText(issues[goal])
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
		} else {
			b.WriteString(strings.Join(lines, "\n") + "\n")
		}
		b.WriteString("\n" + brief)
	}
	return b.String()
}

// briefMax bounds each part of a goal brief in the roster.
const briefMax = 12

// briefText is a goal's brief as a crew worker's roster gives it: the shape
// of the whole goal, and how a change to a contract is made. A goal with no
// brief says so, so the crew asks the owner for one.
func briefText(issues []backlog.Issue) string {
	i, ok := backlog.BriefOf(issues)
	if !ok {
		return fmt.Sprintf("This goal has no brief: the crew has no shared plan of its slices and the contracts between them. Tell the owner in your hand-back, as a decision, that an issue titled %q with Slices, Contracts and Done when would let the crew build it together.\n\n", backlog.GoalBriefPrefix+" <the goal>")
	}
	brief := backlog.ParseGoalBrief(i)
	var b strings.Builder
	fmt.Fprintf(&b, "Goal brief: %s#%d. It is the crew's shared plan, and it outlives every session. Build your slice to it. When a contract must change, edit the brief and comment on it what changed and why, then message the crew-mates who use it: a message alone is lost when a session is cleared.\n", i.Repo, i.Number)
	part := func(title string, entries []string) {
		if len(entries) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s:\n", title)
		for n, e := range entries {
			if n == briefMax {
				fmt.Fprintf(&b, "- … %d more in the brief\n", len(entries)-briefMax)
				break
			}
			b.WriteString("- " + e + "\n")
		}
	}
	part("Slices", brief.Slices)
	var contracts []string
	for _, c := range brief.Contracts {
		contracts = append(contracts, c.Text)
	}
	part("Contracts", contracts)
	part("Done when", brief.DoneWhen)
	b.WriteString("\n")
	return b.String()
}

// crewSessions are the sessions of every crew worker of the fleet, as this
// machine knows them: its own crew workers and their peers.
func (s *Supervisor) crewSessions() map[string]bool {
	out := map[string]bool{}
	for _, w := range s.Machine.Workers {
		if !w.Crew {
			continue
		}
		out[w.Session] = true
		for _, p := range w.PeerList {
			out[p.Session] = true
		}
	}
	return out
}

// crewReviewer returns the session of the crew-mate who reviews the pull
// request of an issue: among the crew of its goal, the first after its
// author's session in order, wrapping around. Every machine works it out
// the same way from GitHub, so no two hand out the same review. "" when
// the issue has no goal, its author is not crew, or the goal has no other
// crew-mate.
func (s *Supervisor) crewReviewer(ctx context.Context, i backlog.Issue) string {
	goal := backlog.GoalLabel(i.LabelNames(), s.Settings.GoalLabels)
	author := backlog.PlanSession(i)
	crew := s.crewSessions()
	if goal == "" || !crew[author] {
		return ""
	}
	var sessions []string
	for _, other := range s.crewIssues(ctx, []string{goal})[goal] {
		if session := backlog.PlanSession(other); crew[session] && !slices.Contains(sessions, session) {
			sessions = append(sessions, session)
		}
	}
	if !slices.Contains(sessions, author) {
		sessions = append(sessions, author)
	}
	if len(sessions) < 2 {
		return ""
	}
	slices.Sort(sessions)
	n := slices.Index(sessions, author)
	return sessions[(n+1)%len(sessions)]
}

// withCrewReviews adds to a crew worker's queue the pull requests of its
// goals that it is the crew-mate to review, each at its risk tier.
func (s *Supervisor) withCrewReviews(ctx context.Context, w config.Worker, items []QueueItem, open []backlog.Issue, checkins []runlog.Checkin) []QueueItem {
	goals := s.crewGoals(w, open, checkins)
	byGoal := s.crewIssues(ctx, goals)
	for _, goal := range goals {
		for _, i := range byGoal[goal] {
			c := backlog.CrewReview(i, s.Signals)
			if c.State != backlog.CrewDue || s.crewReviewer(ctx, i) != w.Session {
				continue
			}
			tier := config.TierLow
			if s.Diffs != nil {
				t, _, err := s.tierOf(ctx, i.Repo, c.PR)
				if err != nil {
					s.Logf("worker %s: tier of %s#%d: %v", w.Name, i.Repo, c.PR, err)
					continue
				}
				tier = t
			}
			author := backlog.PlanSession(i)
			item := QueueItem{Repo: i.Repo, Number: i.Number, Title: i.Title, Verify: fmt.Sprintf("crew review for %s of %s's pull request", goal, author),
				VerifyPR: c.PR, Tier: tier, CrewReview: author}
			items = slices.DeleteFunc(items, func(q QueueItem) bool { return q.Repo == i.Repo && q.Number == i.Number })
			items = append(items, item)
		}
	}
	return items
}
