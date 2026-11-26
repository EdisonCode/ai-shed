package supervisor

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

// Commenter posts a comment on an issue.
type Commenter interface {
	Comment(ctx context.Context, repo string, issue int, body string) error
}

// markProviders puts first, among a crew worker's items of a goal, the
// slices its brief names as providing a contract: the slices that use one
// wait on it.
func (s *Supervisor) markProviders(ctx context.Context, items []QueueItem, open []backlog.Issue, goals []string) {
	if len(goals) == 0 {
		return
	}
	byGoal := s.crewIssues(ctx, goals)
	var briefs []backlog.GoalBrief
	for _, goal := range goals {
		if i, ok := backlog.BriefOf(byGoal[goal]); ok {
			briefs = append(briefs, backlog.ParseGoalBrief(i))
		}
	}
	for n := range items {
		items[n].provides = slices.ContainsFunc(briefs, func(b backlog.GoalBrief) bool { return b.Provides(items[n].Repo, items[n].Number) })
	}
}

// tellConsumers writes, on each slice of a goal that this machine owns and a
// worker has started, that a contract it uses changed: its provider is no
// longer open. Once per contract and slice: the comment is what lasts, and
// the slice's next hand-back must say it checked.
func (s *Supervisor) tellConsumers(ctx context.Context) {
	if s.Settings.GoalLabels == "" || s.Contracts == nil || !slices.ContainsFunc(s.Machine.Workers, func(w config.Worker) bool { return w.Crew }) {
		return
	}
	own := map[string]backlog.Issue{}
	var goals []string
	for _, src := range s.Machine.AllSources() {
		issues, err := s.Lister.List(ctx, src)
		if err != nil {
			s.Logf("contracts: %v", err)
			return
		}
		for _, i := range issues {
			goal := backlog.GoalLabel(i.LabelNames(), s.Settings.GoalLabels)
			if goal == "" || backlog.IsGoalBrief(i.Title) || s.firstQueue(i) != "" {
				continue
			}
			own[issueRef(i.Repo, i.Number)] = i
			if !slices.Contains(goals, goal) {
				goals = append(goals, goal)
			}
		}
	}
	slices.Sort(goals)
	byGoal := s.crewIssues(ctx, goals)
	for _, goal := range goals {
		issues := byGoal[goal]
		b, ok := backlog.BriefOf(issues)
		if !ok {
			continue
		}
		brief := backlog.ParseGoalBrief(b)
		openRefs := map[string]bool{}
		for _, i := range issues {
			openRefs[issueRef(i.Repo, i.Number)] = true
		}
		for _, c := range brief.Contracts {
			provider := brief.Ref(c.Provider)
			if c.Provider == "" || openRefs[provider] {
				continue
			}
			for ref, i := range own {
				if ref == provider || backlog.GoalLabel(i.LabelNames(), s.Settings.GoalLabels) != goal || !uses(brief, c, ref) || !sliceStarted(i, s) || told(i, c) {
					continue
				}
				body := fmt.Sprintf("%s %s\n\n%s, which provides it, is closed. Check this slice against it and against the goal brief (%s#%d). Change what must change, and say %q in your next hand-back once your part fits it.",
					backlog.ContractChangedPhrase, c.Text, provider, brief.Repo, brief.Number, backlog.ContractCheckedPhrase)
				if err := s.Contracts.Comment(ctx, i.Repo, i.Number, body); err != nil {
					s.Logf("contracts: tell %s: %v", ref, err)
					continue
				}
				s.Logf("told %s that a contract it uses changed: %s", ref, c.Text)
			}
		}
	}
}

// uses reports whether a slice uses a contract: the brief names it after
// "used by", or names no user at all.
func uses(b backlog.GoalBrief, c backlog.Contract, ref string) bool {
	if len(c.Users) == 0 {
		return true
	}
	return slices.ContainsFunc(c.Users, func(u string) bool { return b.Ref(u) == ref })
}

// sliceStarted reports whether a worker has started the slice: its plan is
// signed, or it was handed back.
func sliceStarted(i backlog.Issue, s *Supervisor) bool {
	return backlog.PlanSession(i) != "" || backlog.HandedPR(i, s.Signals) != 0
}

// told reports whether the slice was told of the contract's change.
func told(i backlog.Issue, c backlog.Contract) bool {
	return slices.ContainsFunc(i.Comments, func(cm backlog.Comment) bool {
		body := strings.TrimSpace(cm.Body)
		return strings.HasPrefix(body, backlog.ContractChangedPhrase) && strings.Contains(body, c.Text)
	})
}
