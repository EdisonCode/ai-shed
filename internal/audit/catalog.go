package audit

import (
	"slices"

	"github.com/edisoncode/ai-shed/internal/config"
)

// Setting is one thing shed does by itself once the fleet file asks for it:
// a rule the owner's prose need not state when it is on.
type Setting struct {
	// Name is where it is set in the fleet file.
	Name string
	// Does says what shed does with it, as the model reads it.
	Does string
	// On reports whether this fleet file has it on.
	On func(*config.Config) bool
	// Repo, for a setting of a repository, reports whether it is on for
	// one: the setting says nothing about the others.
	Repo func(config.Repo) bool
}

// Repos names the repositories a repository's setting is on for.
func (s Setting) Repos(c *config.Config) []string {
	if s.Repo == nil {
		return nil
	}
	var names []string
	for _, r := range c.Repos {
		if s.Repo(r) {
			names = append(names, r.Name)
		}
	}
	return names
}

func anyRepo(f func(config.Repo) bool) func(*config.Config) bool {
	return func(c *config.Config) bool { return slices.ContainsFunc(c.Repos, f) }
}

func anyWorker(f func(config.Worker) bool) func(*config.Config) bool {
	return func(c *config.Config) bool {
		return slices.ContainsFunc(c.Machines, func(m config.Machine) bool { return slices.ContainsFunc(m.Workers, f) })
	}
}

func always(*config.Config) bool { return true }

// Catalog is what shed does by itself, setting by setting.
var Catalog = []Setting{
	{Name: "signals (always on)", Does: "the hand-back format: a worker reports in a comment headed Hand-back, asks the owner after Decisions needed:, asks for a look after Needs eyes:, says Not ready: when an issue is not ready; the owner answers Owner ruling, Eyes checked, Ready now", On: always},
	{Name: "the brief shed writes (always on)", Does: "one issue at a time on a new branch; the plan in the issue with a Paths line; Depends on #N; check for an existing pull request first; hand back again after rework; write progress in the issue; lessons in the hand-back; do not split an issue that is too big but hand it back as Not ready: too big", On: always},
	{Name: "repos[].issue_quota", Does: "limits how many issues workers may open a day in the repository; issues past it are held for the owner", On: anyRepo(func(r config.Repo) bool { return r.IssueQuota > 0 }), Repo: func(r config.Repo) bool { return r.IssueQuota > 0 }},
	{Name: "repos[].verify", Does: "a verifier reviews each handed-back pull request at its risk tier before the owner does", On: anyRepo(func(r config.Repo) bool { return r.Verify != nil }), Repo: func(r config.Repo) bool { return r.Verify != nil }},
	{Name: "repos[].verify.merge_low", Does: "a pull request the verifier passed at low risk is merged by shed once its checks pass", On: anyRepo(func(r config.Repo) bool { return r.Verify != nil && r.Verify.MergeLow }), Repo: func(r config.Repo) bool { return r.Verify != nil && r.Verify.MergeLow }},
	{Name: "repos[].verify.crew_low", Does: "a crew-mate's review that passes at low risk counts as the verification", On: anyRepo(func(r config.Repo) bool { return r.Verify != nil && r.Verify.CrewLow }), Repo: func(r config.Repo) bool { return r.Verify != nil && r.Verify.CrewLow }},
	{Name: "repos[].verify.merge_train", Does: "verified pull requests are merged in order, each brought up to date with its base first", On: anyRepo(func(r config.Repo) bool { return r.Verify != nil && r.Verify.MergeTrain != "" }), Repo: func(r config.Repo) bool { return r.Verify != nil && r.Verify.MergeTrain != "" }},
	{Name: "repos[].close_finished", Does: "shed closes an issue whose last pull request merged and that lists nothing still to do", On: anyRepo(func(r config.Repo) bool { return r.CloseFinished }), Repo: func(r config.Repo) bool { return r.CloseFinished }},
	{Name: "repos[].circuit_breaker", Does: "new work waits while the default branch is red, and a health worker fixes it", On: anyRepo(func(r config.Repo) bool { return r.CircuitBreaker }), Repo: func(r config.Repo) bool { return r.CircuitBreaker }},
	{Name: "repos[].max_open_prs", Does: "limits the repository's open pull requests; new work is opened as a parked draft until there is room", On: anyRepo(func(r config.Repo) bool { return r.MaxOpenPRs > 0 }), Repo: func(r config.Repo) bool { return r.MaxOpenPRs > 0 }},
	{Name: "repos[].gates", Does: "shed runs the repository's fast checks (format, lint, tests, build) on each pull request's head and posts the result", On: anyRepo(func(r config.Repo) bool { return r.Gates.Command != "" }), Repo: func(r config.Repo) bool { return r.Gates.Command != "" }},
	{Name: "workers[].eye_checks", Does: "the worker looks at merged work on staging and posts Eyes checked or Eyes failed", On: anyWorker(func(w config.Worker) bool { return w.EyeChecks })},
	{Name: "workers[].fresh_per_issue", Does: "the worker's context is cleared before each new issue", On: anyWorker(func(w config.Worker) bool { return w.FreshPerIssue })},
	{Name: "workers[].peers", Does: "workers message their peers' sessions directly", On: anyWorker(func(w config.Worker) bool { return len(w.Peers) > 0 })},
	{Name: "workers[].crew", Does: "crew-mates on a goal message before changing what another builds on, review each other's pull requests, and build to the goal's brief", On: anyWorker(func(w config.Worker) bool { return w.Crew })},
	{Name: "workers[].health", Does: "the worker fixes a red default branch", On: anyWorker(func(w config.Worker) bool { return w.Health })},
	{Name: "machines[].capacity", Does: "workers run at a lower priority so CI keeps the cores (nice), new work waits while the machine is loaded (max_load, busy_when), and unstarted work moves to a machine with room (release_after). Heavy commands taking turns is the shed lock command, not this setting", On: func(c *config.Config) bool {
		return slices.ContainsFunc(c.Machines, func(m config.Machine) bool {
			return m.Capacity.MaxLoad > 0 || m.Capacity.BusyWhen != "" || m.Capacity.Nice != 0 || m.Capacity.ReleaseAfter != ""
		})
	}},
	{Name: "supervisor.overlap_labels", Does: "issues of the same area are held back while a worker has one of them in hand", On: func(c *config.Config) bool { return c.Supervisor.OverlapLabels != "" }},
	{Name: "supervisor.goal_labels", Does: "goals: the queue hands out the goal nearest done first, and the owner can focus on or table a goal", On: func(c *config.Config) bool { return c.Supervisor.GoalLabels != "" }},
	{Name: "supervisor.affinity_ttl", Does: "rework and follow-on work wait for the worker that has the code open", On: func(c *config.Config) bool { return c.Supervisor.AffinityTTL != "" }},
	{Name: "supervisor.models", Does: "the model is chosen for each issue from its labels", On: func(c *config.Config) bool {
		return c.Supervisor.Models.Default != "" || len(c.Supervisor.Models.Labels) > 0
	}},
	{Name: "supervisor.no_work_after", Does: "the owner is notified when a worker has had no work for a while", On: func(c *config.Config) bool { return c.Supervisor.NoWorkAfter != "" }},
	{Name: "supervisor.recommend_after", Does: "an idle worker recommends an answer to a decision or a ready ask that has waited", On: func(c *config.Config) bool { return c.Supervisor.RecommendAfter != "" }},
	{Name: "supervisor.crew_release_after", Does: "a goal's waiting slice goes to its crew on another machine while the builders here are busy", On: func(c *config.Config) bool { return c.Supervisor.CrewReleaseAfter != "" }},
}
