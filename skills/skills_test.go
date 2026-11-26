package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTheShippedSkillIsTheOneInTheRepository(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("shed", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(Shed) != string(want) || len(Shed) == 0 {
		t.Fatal("the embedded shed skill differs from skills/shed/SKILL.md")
	}
}

func TestASkillThatIsNotInstalledIsNotStale(t *testing.T) {
	stale, err := ShedStale(t.TempDir())
	if err != nil || stale {
		t.Fatalf("stale = %v, %v; want false", stale, err)
	}
}

func TestAnInstalledSkillThatDiffersIsStaleUntilItIsInstalledAgain(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(ShedPath(home)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ShedPath(home), []byte("an older skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	if stale, err := ShedStale(home); err != nil || !stale {
		t.Fatalf("stale = %v, %v; want true", stale, err)
	}

	if _, err := InstallShed(home); err != nil {
		t.Fatal(err)
	}

	if stale, err := ShedStale(home); err != nil || stale {
		t.Fatalf("after install: stale = %v, %v; want false", stale, err)
	}
}
