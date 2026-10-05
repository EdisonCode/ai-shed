package runlog

import (
	"os"
	"path/filepath"
	"testing"
)

func bump(t *testing.T, dir, name, worker string) {
	t.Helper()
	path := filepath.Join(dir, BumpsDir, name)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(worker), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMachineThatWasSentNoBumpHasNone(t *testing.T) {
	bumps, err := Bumps(t.TempDir())
	if err != nil || len(bumps) != 0 {
		t.Fatalf("bumps = %v, %v", bumps, err)
	}
}

func TestBumpsAreReadByIssueWithTheWorkerEachGoesTo(t *testing.T) {
	dir := t.TempDir()
	bump(t, dir, "12", "")
	bump(t, dir, "40", "docs\n")
	bump(t, dir, "40.new", "half written")

	bumps, err := Bumps(dir)
	if err != nil {
		t.Fatal(err)
	}
	if to, ok := bumps[12]; !ok || to != "" {
		t.Errorf("#12 = %q, %v; want a bump that names no worker", to, ok)
	}
	if bumps[40] != "docs" || len(bumps) != 2 {
		t.Errorf("bumps = %v", bumps)
	}
}

func TestRemovedBumpIsGone(t *testing.T) {
	dir := t.TempDir()
	bump(t, dir, "12", "")
	if err := RemoveBump(dir, 12); err != nil {
		t.Fatal(err)
	}
	if bumps, _ := Bumps(dir); len(bumps) != 0 {
		t.Fatalf("bumps = %v", bumps)
	}
	if err := RemoveBump(dir, 12); err != nil {
		t.Fatalf("removing a bump that is not there = %v", err)
	}
}
