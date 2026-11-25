package runlog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGoalModesReadsEachGoalByItsLabel(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, GoalsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, GoalsDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(GoalFile("goal: route planner"), "focus\n")
	write(GoalFile("goal: invoices"), "tabled")
	write(GoalFile("goal: odd"), "sideways")
	write(GoalFile("goal: half")+".new", "focus")

	modes, err := GoalModes(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"goal: route planner": GoalFocus, "goal: invoices": GoalTabled}
	if len(modes) != len(want) {
		t.Fatalf("modes = %v, want %v", modes, want)
	}
	for label, mode := range want {
		if modes[label] != mode {
			t.Errorf("mode of %q = %q, want %q", label, modes[label], mode)
		}
	}
}

func TestGoalModesWithNoDirectoryIsNone(t *testing.T) {
	modes, err := GoalModes(t.TempDir())
	if err != nil || len(modes) != 0 {
		t.Fatalf("GoalModes = %v, %v; want none", modes, err)
	}
}
