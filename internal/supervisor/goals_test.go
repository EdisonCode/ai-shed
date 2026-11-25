package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// fakeGoals counts each goal's issues by label: closed, of total.
type fakeGoals struct {
	counts map[string][2]int
	asked  int
}

func (f *fakeGoals) GoalCounts(_ context.Context, _, label string) (int, int, error) {
	f.asked++
	c := f.counts[label]
	return c[0], c[1], nil
}

func TestWithinAPriorityTheGoalNearestDoneIsHandedOutFirst(t *testing.T) {
	f := started(t, config.Supervisor{GoalLabels: "goal:"}, onTrack)
	f.Machine.Issues[0].Priority = []string{"p0"}
	f.Goals = &fakeGoals{counts: map[string][2]int{"goal:export": {8, 10}, "goal:refunds": {1, 10}}}
	f.lister.issues = []backlog.Issue{labeled(10, "goal:refunds"), labeled(20), labeled(30, "goal:export"), labeled(40, "goal:refunds", "p0"), labeled(50, "goal:export")}
	f.tick(2 * time.Minute)

	if got, want := queueShown(t, f.reviewer.prompts[0]), []int{40, 30, 50, 10, 20}; !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
}

func TestWithoutGoalLabelsTheQueueOrderIsUnchanged(t *testing.T) {
	f := started(t, config.Supervisor{}, onTrack)
	goals := &fakeGoals{counts: map[string][2]int{"goal:export": {8, 10}}}
	f.Goals = goals
	f.lister.issues = []backlog.Issue{labeled(10), labeled(30, "goal:export")}
	f.tick(2 * time.Minute)

	if got, want := queueShown(t, f.reviewer.prompts[0]), []int{10, 30}; !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
	if goals.asked != 0 {
		t.Fatalf("goals were counted %d times without goal_labels", goals.asked)
	}
}

func TestAGoalIsCountedOnceAnHour(t *testing.T) {
	f := started(t, config.Supervisor{GoalLabels: "goal:"}, onTrack, onTrack, onTrack)
	goals := &fakeGoals{counts: map[string][2]int{"goal:export": {8, 10}}}
	f.Goals = goals
	f.lister.issues = []backlog.Issue{labeled(30, "goal:export")}
	f.tick(2 * time.Minute)
	asked := goals.asked
	f.tick(20 * time.Minute)
	if goals.asked != asked {
		t.Fatalf("the goal was counted again within the hour: %d, then %d", asked, goals.asked)
	}
	f.tick(time.Hour)
	if goals.asked == asked {
		t.Fatal("the goal was not counted again after an hour")
	}
}

// setGoalMode leaves the owner's mode for a goal on the fake's machine, as
// shed goal does.
func setGoalMode(t *testing.T, f *fixture, label, mode string) {
	t.Helper()
	dir := filepath.Join(f.StateDir, runlog.GoalsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, runlog.GoalFile(label)), []byte(mode), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTheGoalInFocusIsHandedOutBeforeThePriorityOfOtherGoals(t *testing.T) {
	f := started(t, config.Supervisor{GoalLabels: "goal:"}, onTrack)
	f.Machine.Issues[0].Priority = []string{"p0"}
	f.Goals = &fakeGoals{counts: map[string][2]int{"goal:export": {9, 10}, "goal:refunds": {1, 10}}}
	setGoalMode(t, f, "goal:refunds", runlog.GoalFocus)
	f.lister.issues = []backlog.Issue{labeled(10, "goal:export", "p0"), labeled(20), labeled(30, "goal:refunds"), labeled(40, "goal:refunds")}
	f.tick(2 * time.Minute)

	if got, want := queueShown(t, f.reviewer.prompts[0]), []int{30, 40, 10, 20}; !slices.Equal(got, want) {
		t.Fatalf("queue = %v, want %v", got, want)
	}
}

func TestATabledGoalHoldsTheIssuesNobodyStarted(t *testing.T) {
	f := started(t, config.Supervisor{GoalLabels: "goal:"}, onTrack)
	setGoalMode(t, f, "goal:refunds", runlog.GoalTabled)
	f.lister.issues = []backlog.Issue{labeled(10, "goal:refunds"), labeled(20)}
	f.tick(2 * time.Minute)

	prompt := f.reviewer.prompts[0]
	if !strings.Contains(prompt, `org/app#10 Issue [held: the owner tabled goal "goal:refunds"`) {
		t.Fatalf("the tabled goal's issue is not held:\n%s", prompt)
	}
	if strings.Contains(prompt, "org/app#20 Issue [held") {
		t.Fatalf("an issue of no goal was held:\n%s", prompt)
	}
}
