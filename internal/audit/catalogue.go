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

// Catalogue is what shed does by itself, setting by setting.
var Catalogue = []Setting{
	{"signals (always on)", "the hand-back format: a worker reports in a comment headed Hand-back, asks the owner after Decisions needed:, asks for a look after Needs eyes:, says Not ready: when an issue is not ready; the owner answers Owner ruling, Eyes checked, Ready now", always},
	{"the brief shed writes (always on)", "one issue at a time on a new branch; the plan in the issue with a Paths line; Depends on #N; check for an existing pull request first; hand back again after rework; write progress in the issue; lessons in the hand-back; do not split an issue that is too big but hand it back as Not ready: too big", always},
	{"repos[].issue_quota", "limits how many issues workers may open a day in the repository; issues past it are held for the owner", anyRepo(func(r config.Repo) bool { return r.IssueQuota > 0 })},
	{"repos[].verify", "a verifier reviews each handed-back pull request at its risk tier before the owner does", anyRepo(func(r config.Repo) bool { return r.Verify != nil })},
	{"repos[].verify.merge_low", "a pull request the verifier passed at low risk is merged by shed once its checks pass", anyRepo(func(r config.Repo) bool { return r.Verify != nil && r.Verify.MergeLow })},
	{"repos[].verify.crew_low", "a crew-mate's review that passes at low risk counts as the verification", anyRepo(func(r config.Repo) bool { return r.Verify != nil && r.Verify.CrewLow })},
	{"repos[].verify.merge_train", "verified pull requests are merged in order, each brought up to date with its base first", anyRepo(func(r config.Repo) bool { return r.Verify != nil && r.Verify.MergeTrain != "" })},
	{"repos[].close_finished", "shed closes an issue whose last pull request merged and that lists nothing still to do", anyRepo(func(r config.Repo) bool { return r.CloseFinished })},
	{"repos[].circuit_breaker", "new work waits while the default branch is red, and a health worker fixes it", anyRepo(func(r config.Repo) bool { return r.CircuitBreaker })},
	{"repos[].max_open_prs", "limits the repository's open pull requests; new work is opened as a parked draft until there is room", anyRepo(func(r config.Repo) bool { return r.MaxOpenPRs > 0 })},
	{"repos[].gates", "shed runs the repository's fast checks (format, lint, tests, build) on each pull request's head and posts the result", anyRepo(func(r config.Repo) bool { return r.Gates.Command != "" })},
	{"workers[].eye_checks", "the worker looks at merged work on staging and posts Eyes checked or Eyes failed", anyWorker(func(w config.Worker) bool { return w.EyeChecks })},
	{"workers[].fresh_per_issue", "the worker's context is cleared before each new issue", anyWorker(func(w config.Worker) bool { return w.FreshPerIssue })},
	{"workers[].peers", "workers message their peers' sessions directly", anyWorker(func(w config.Worker) bool { return len(w.Peers) > 0 })},
	{"workers[].crew", "crew-mates on a goal message before changing what another builds on, review each other's pull requests, and build to the goal's brief", anyWorker(func(w config.Worker) bool { return w.Crew })},
	{"workers[].health", "the worker fixes a red default branch", anyWorker(func(w config.Worker) bool { return w.Health })},
	{"machines[].capacity", "workers yield a loaded machine to CI, heavy commands take turns, and unstarted work moves to a machine with room", func(c *config.Config) bool {
		return slices.ContainsFunc(c.Machines, func(m config.Machine) bool {
			return m.Capacity.MaxLoad > 0 || m.Capacity.BusyWhen != "" || m.Capacity.Nice != 0 || m.Capacity.ReleaseAfter != ""
		})
	}},
	{"supervisor.overlap_labels", "issues of the same area are held back while a worker has one of them in hand", func(c *config.Config) bool { return c.Supervisor.OverlapLabels != "" }},
	{"supervisor.goal_labels", "goals: the queue hands out the goal nearest done first, and the owner can focus on or table a goal", func(c *config.Config) bool { return c.Supervisor.GoalLabels != "" }},
	{"supervisor.affinity_ttl", "rework and follow-on work wait for the worker that has the code open", func(c *config.Config) bool { return c.Supervisor.AffinityTTL != "" }},
	{"supervisor.models", "the model is chosen for each issue from its labels", func(c *config.Config) bool {
		return c.Supervisor.Models.Default != "" || len(c.Supervisor.Models.Labels) > 0
	}},
	{"supervisor.no_work_after", "the owner is notified when a worker has had no work for a while", func(c *config.Config) bool { return c.Supervisor.NoWorkAfter != "" }},
	{"supervisor.recommend_after", "an idle worker recommends an answer to a decision or a ready ask that has waited", func(c *config.Config) bool { return c.Supervisor.RecommendAfter != "" }},
	{"supervisor.crew_release_after", "a goal's waiting slice goes to its crew on another machine while the builders here are busy", func(c *config.Config) bool { return c.Supervisor.CrewReleaseAfter != "" }},
}
