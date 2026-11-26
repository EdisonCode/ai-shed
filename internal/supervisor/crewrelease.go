package supervisor

import (
	"context"
	"fmt"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// shedCrewSlices gives a goal's unstarted slice to the goal's crew on the
// other machines while every builder here has work in hand: after
// crew_release_after (half that for a goal in focus), one slice at a time
// goes to the account's overflow label, marked for the goal's crew, and a
// crew worker with room on any machine takes it. The holder releases;
// nobody takes a slice from a holder.
func (s *Supervisor) shedCrewSlices(ctx context.Context, now time.Time) {
	after := s.Settings.CrewReleaseAfterOrZero()
	if after == 0 || s.Account == nil || s.Account.Overflow == "" || s.Releaser == nil || s.Settings.GoalLabels == "" || !s.crewElsewhere() {
		return
	}
	if !s.everyBuilderBusy() {
		s.crewBusySince = time.Time{}
		return
	}
	if s.crewBusySince.IsZero() {
		s.crewBusySince = now
	}
	modes := s.goalModes()
	slice := func(i backlog.Issue) bool {
		goal := backlog.GoalLabel(i.LabelNames(), s.Settings.GoalLabels)
		if goal == "" {
			return false
		}
		wait := after
		if modes[goal] == runlog.GoalFocus {
			wait = after / 2
		}
		return now.Sub(s.crewBusySince) >= wait && now.Sub(s.lastCrewShed) >= wait
	}
	issue, remove, ok := s.unstarted(ctx, slice)
	if !ok {
		return
	}
	s.lastCrewShed = now
	goal := backlog.GoalLabel(issue.LabelNames(), s.Settings.GoalLabels)
	busyFor := now.Sub(s.crewBusySince).Round(time.Minute)
	comment := fmt.Sprintf("%s %s, every worker busy for %s while this slice of %s waits: released %s. A crew worker with room on another machine takes it from `%s`.",
		backlog.ReleasedPhrase, s.Machine.Name, spell(busyFor), goal, backlog.CrewReleasePhrase, s.Account.Overflow)
	if err := s.Releaser.Release(ctx, issue.Repo, issue.Number, remove, s.Account.Overflow, comment); err != nil {
		s.Logf("release slice #%d of %s: %v", issue.Number, goal, err)
		return
	}
	s.Logf("released %s#%d of %s to its crew on %s: every worker here has been busy for %s", issue.Repo, issue.Number, goal, s.Account.Overflow, spell(busyFor))
}

// goalModes reads the owner's focus and tabled goals on this machine; none
// when they cannot be read.
func (s *Supervisor) goalModes() map[string]string {
	modes, err := runlog.GoalModes(s.StateDir)
	if err != nil {
		s.Logf("goal modes: %v", err)
		return nil
	}
	return modes
}

// crewElsewhere reports whether a crew worker here has a crew-mate on
// another machine: only then can a slice go to the crew elsewhere.
func (s *Supervisor) crewElsewhere() bool {
	for _, w := range s.Machine.Workers {
		if !w.Crew {
			continue
		}
		for _, p := range w.PeerList {
			if p.Machine != s.Machine.Name {
				return true
			}
		}
	}
	return false
}

// everyBuilderBusy reports whether every worker here that builds has work
// in hand: no builder could start a waiting slice now.
func (s *Supervisor) everyBuilderBusy() bool {
	builders := s.Machine.Builders()
	if len(builders) == 0 {
		return false
	}
	for _, w := range builders {
		st := s.workers[w.Name]
		if st == nil || (st.issue == 0 && st.task == "") || !st.doneSince.IsZero() {
			return false
		}
	}
	return true
}
