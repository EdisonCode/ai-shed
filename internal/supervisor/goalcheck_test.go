package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// goalCheckFleet is a machine whose crew worker box-app comes first of its
// crew, with the brief of goal:pricing and the goal's open slices given.
func goalCheckFleet(t *testing.T, brief backlog.Issue, open ...backlog.Issue) *Supervisor {
	t.Helper()
	l := &countingLister{byQuery: map[string][]backlog.Issue{
		"org/app \"Goal brief:\" in:title": {brief},
		"org/app label:\"goal:pricing\"":   append([]backlog.Issue{brief}, open...),
	}}
	return &Supervisor{
		Machine: config.Machine{Name: "box", Workers: []config.Worker{{Name: "app", Crew: true, Session: "box-app",
			PeerList: []config.Peer{{Machine: "mini", Worker: "web", Session: "mini-web", Remote: true}}}}},
		Settings: config.Supervisor{GoalLabels: "goal:"}, Signals: config.DefaultSignals(), Lister: l, CrewRepos: []string{"org/app"},
		StateDir: t.TempDir(), Logf: t.Logf, Now: func() time.Time { return t0 },
	}
}

func pricingBriefIssue() backlog.Issue {
	b := labeled(40, "goal:pricing")
	b.Title, b.Body = "Goal brief: pricing", "**Done when:**\n- a customer pays the new price on staging"
	return b
}

func goalCheckTasks(t *testing.T, s *Supervisor) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(s.StateDir, runlog.TasksDir, "app", "*-goalcheck-*.md"))
	return m
}

func TestWhenAGoalsLastSliceClosesItsFirstCrewWorkerIsAskedToCheckIt(t *testing.T) {
	s := goalCheckFleet(t, pricingBriefIssue())
	s.offerGoalChecks(t.Context(), t0)
	s.lastGoalCheck = time.Time{}
	s.offerGoalChecks(t.Context(), t0.Add(time.Hour))
	tasks := goalCheckTasks(t, s)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %q, want one", tasks)
	}
	brief, _ := os.ReadFile(tasks[0])
	for _, want := range []string{"a customer pays the new price on staging", "\"Goal checked:\"", "\"Goal check failed:\"", "Change no code"} {
		if !strings.Contains(string(brief), want) {
			t.Fatalf("task lacks %q:\n%s", want, brief)
		}
	}
}

func TestNoGoalCheckWhileSlicesAreOpenOrOnceItPassed(t *testing.T) {
	s := goalCheckFleet(t, pricingBriefIssue(), labeled(41, "goal:pricing"))
	s.offerGoalChecks(t.Context(), t0)
	if tasks := goalCheckTasks(t, s); len(tasks) != 0 {
		t.Fatalf("tasks = %q with a slice open", tasks)
	}
	passed := pricingBriefIssue()
	passed.Comments = []backlog.Comment{{Body: "Goal checked: it holds"}}
	s = goalCheckFleet(t, passed)
	s.offerGoalChecks(t.Context(), t0)
	if tasks := goalCheckTasks(t, s); len(tasks) != 0 {
		t.Fatalf("tasks = %q after a pass", tasks)
	}
}

func TestOnlyTheFirstCrewWorkerOfTheGoalOffersTheCheck(t *testing.T) {
	s := goalCheckFleet(t, pricingBriefIssue())
	s.Machine.Workers[0].Session = "zz-app" // mini-web comes first
	s.offerGoalChecks(t.Context(), t0)
	if tasks := goalCheckTasks(t, s); len(tasks) != 0 {
		t.Fatalf("tasks = %q on a machine whose crew worker is not first", tasks)
	}
}
