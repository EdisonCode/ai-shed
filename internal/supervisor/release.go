package supervisor

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// Releaser moves an issue off this machine's queue onto the fleet's overflow
// label, with a comment that says who released it and why.
type Releaser interface {
	Release(ctx context.Context, repo string, number int, remove []string, add, comment string) error
}

// KindRelease is the check-in of a worker that gave its issue to the fleet.
const KindRelease = "release"

// maybeRelease gives the issue in hand to the fleet when its worker cannot
// go on for a long time, and another machine may: it is at its weekly limit,
// or stuck. The holder releases; nobody takes an issue from a holder. Only
// work that lives outside the session is released: a pull request, or a
// comment after the hand-over that holds the plan or the progress. It
// reports whether it released.
func (s *Supervisor) maybeRelease(ctx context.Context, w config.Worker, st *workerState, now time.Time, toolState string) bool {
	if s.Account == nil || s.Account.Overflow == "" || s.Releaser == nil || st.issue == 0 || st.look || st.verify || st.task != "" || st.repo == "" {
		return false
	}
	if toolState == runlog.MarkWorking || now.Before(st.releaseAfter) || len(st.recent) == 0 {
		return false
	}
	last := st.recent[len(st.recent)-1]
	why := ""
	switch {
	case last.Verdict == runlog.VerdictLimited && last.Limit == runlog.LimitWeekly:
		why = "weekly limit"
		if !last.Resets.IsZero() {
			why += " (resets " + last.Resets.In(now.Location()).Format("Mon 15:04") + ")"
		}
	case last.Verdict == runlog.VerdictStuck:
		why = "stuck: " + last.Reason
	default:
		return false
	}
	if bumps, _ := s.bumps(); bumps != nil {
		if _, bumped := bumps[st.issue]; bumped {
			return false
		}
	}
	issue, found, err := backlog.Find(ctx, s.Lister, s.Machine.SourcesFor(w), st.issue)
	if err != nil || !found {
		if err != nil {
			s.Logf("worker %s: release #%d: %v", w.Name, st.issue, err)
		}
		st.releaseAfter = now.Add(retryAfter)
		return false
	}
	if !s.survives(issue, w) {
		// Its only copy is the session: another machine would start over.
		st.releaseAfter = now.Add(retryAfter)
		return false
	}
	var remove []string
	for _, src := range s.Machine.AllSources() {
		if src.Repo == issue.Repo && src.Label != "" && slices.Contains(issue.LabelNames(), src.Label) && !slices.Contains(remove, src.Label) {
			remove = append(remove, src.Label)
		}
	}
	comment := fmt.Sprintf("%s %s/%s, %s. The plan and the progress are in this issue, and the work is on its branch and pull request. A machine with room takes it from `%s`.",
		backlog.ReleasedPhrase, s.Machine.Name, w.Name, why, s.Account.Overflow)
	if err := s.Releaser.Release(ctx, issue.Repo, issue.Number, remove, s.Account.Overflow, comment); err != nil {
		s.Logf("worker %s: release #%d: %v", w.Name, issue.Number, err)
		st.releaseAfter = now.Add(retryAfter)
		return false
	}
	// The session must not go on with an issue that is no longer its own
	// when its limit resets.
	if err := s.command(w, s.Settings.ClearCommandOrDefault()); err != nil {
		s.Logf("worker %s: clear after the release of #%d: %v", w.Name, issue.Number, err)
	}
	s.record(st, runlog.Checkin{Time: now, Worker: w.Name, Kind: KindRelease, Verdict: runlog.VerdictReleased, Issue: issue.Number, Repo: issue.Repo,
		Reason: fmt.Sprintf("released #%d to %s: %s", issue.Number, s.Account.Overflow, why)})
	st.issue, st.repo, st.look, st.verify, st.contextSince = 0, "", false, false, now
	return true
}

// survives reports whether the issue's work lives outside the worker's
// session: a pull request its hand-back names, or a comment written after
// it was handed over.
func (s *Supervisor) survives(issue backlog.Issue, w config.Worker) bool {
	if backlog.HandedPR(issue, s.Signals) != 0 {
		return true
	}
	checkins, err := runlog.ReadCheckins(s.StateDir)
	if err != nil {
		return false
	}
	var handed time.Time
	for _, c := range checkins {
		if c.Worker == w.Name && c.Sent && c.Issue == issue.Number {
			handed = c.Time
		}
	}
	for _, c := range issue.Comments {
		if !handed.IsZero() && c.CreatedAt.After(handed) {
			return true
		}
	}
	return false
}
