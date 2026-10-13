package supervisor

import (
	"context"
	"fmt"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/looks"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

const (
	// deployWait is how long a started deploy may take before staging serves
	// a new commit. After it the deploy is over, and the owner is told.
	deployWait = time.Hour
	// deployOutput is how much of the deploy command's output is kept.
	deployOutput = 2000
)

// deployState is where a repository's staging deploy stands on this machine.
type deployState struct {
	read bool
	// since is set while a deploy runs, and before is the commit staging
	// served when it started.
	since  time.Time
	before string
	// nextTry holds the next deploy back after one failed or was not served.
	nextTry time.Time
}

// stagingDeploy deploys staging when looks have piled up waiting for it, on
// the machine the fleet file names, and reports whether a deploy runs: no
// look of the repository is handed out until staging serves the new commit.
func (s *Supervisor) stagingDeploy(ctx context.Context, repo config.Repo, report looks.Report) bool {
	if repo.Staging.Deploy == "" || repo.Staging.DeployFrom != s.Machine.Name || s.RunDeploy == nil {
		return false
	}
	now := s.Now()
	st := s.deployState(repo.Name)
	when := repo.Staging.DeployWhen
	if !st.since.IsZero() {
		switch {
		case report.Staging != "" && report.Staging != st.before:
			s.recordDeploy(runlog.Deploy{Repo: repo.Name, Start: st.since, End: now, Before: st.before, After: report.Staging, Result: runlog.DeployServed})
			st.since = time.Time{}
			return false
		case now.Sub(st.since) >= deployWait:
			s.recordDeploy(runlog.Deploy{Repo: repo.Name, Start: st.since, End: now, Before: st.before, After: report.Staging, Result: runlog.DeployNotServed})
			s.tellDeploy(fmt.Sprintf("%s: the staging deploy of %s started %s ago, and staging still serves %s", s.Machine.Name, repo.Name, spell(deployWait), short(st.before)))
			st.since, st.nextTry = time.Time{}, now.Add(when.OrAfterOrDefault())
			return false
		}
		return true
	}
	if now.Before(st.nextTry) || report.Unchecked != "" || report.Staging == "" {
		return false
	}
	var waiting int
	var oldest time.Time
	for _, l := range report.Looks {
		if l.State == looks.Awaiting {
			waiting++
			if oldest.IsZero() || l.Since.Before(oldest) {
				oldest = l.Since
			}
		}
	}
	if waiting == 0 || (waiting < when.WaitingLooksOrDefault() && now.Sub(oldest) < when.OrAfterOrDefault()) {
		return false
	}
	if s.lookInProgress(report) {
		// A deploy restarts the app under the worker's look.
		return false
	}
	output, exit := s.RunDeploy(ctx, repo.Staging.Deploy)
	d := runlog.Deploy{Repo: repo.Name, Start: now, Before: report.Staging, Waiting: waiting, ExitCode: exit, Output: tailOf(output, deployOutput)}
	if exit != 0 {
		d.End, d.Result = s.Now(), runlog.DeployFailed
		s.recordDeploy(d)
		s.tellDeploy(fmt.Sprintf("%s: the staging deploy of %s failed (exit %d); tried again in %s", s.Machine.Name, repo.Name, exit, spell(when.OrAfterOrDefault())))
		st.nextTry = now.Add(when.OrAfterOrDefault())
		return false
	}
	d.Result = runlog.DeployStarted
	s.recordDeploy(d)
	st.since, st.before = now, report.Staging
	return true
}

// deployState returns the repository's deploy state, read back from the
// log the first time, so a restart of the agent in the middle of a deploy
// goes on waiting for it.
func (s *Supervisor) deployState(repo string) *deployState {
	if s.deploys == nil {
		s.deploys = map[string]*deployState{}
	}
	st := s.deploys[repo]
	if st == nil {
		st = &deployState{}
		s.deploys[repo] = st
	}
	if !st.read {
		st.read = true
		records, err := runlog.ReadDeploys(s.StateDir)
		if err != nil {
			s.Logf("staging deploys: %v", err)
		}
		for _, d := range records {
			if d.Repo != repo {
				continue
			}
			st.since, st.before = time.Time{}, ""
			if d.Result == runlog.DeployStarted {
				st.since, st.before = d.Start, d.Before
			}
		}
	}
	return st
}

// lookInProgress reports whether a worker of this machine has a look of the
// report's repository in hand that is still due: it has not posted what it
// saw.
func (s *Supervisor) lookInProgress(report looks.Report) bool {
	for _, st := range s.workers {
		if !st.look || st.issue == 0 {
			continue
		}
		for _, l := range report.Looks {
			if l.Number == st.issue && l.State == looks.Due {
				return true
			}
		}
	}
	return false
}

func (s *Supervisor) recordDeploy(d runlog.Deploy) {
	s.Logf("staging deploy of %s: %s", d.Repo, d.Result)
	if err := runlog.AppendDeploy(s.StateDir, d); err != nil {
		s.Logf("staging deploy of %s: %v", d.Repo, err)
	}
}

func (s *Supervisor) tellDeploy(message string) {
	s.Logf("%s", message)
	if s.Notify != nil {
		s.Notify("", message)
	}
}

func short(commit string) string { return commit[:min(len(commit), 7)] }

func tailOf(s string, n int) string { return s[max(0, len(s)-n):] }
