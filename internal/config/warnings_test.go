package config

import (
	"strings"
	"testing"
)

const withWorker = minimal + "    workers:\n      - {name: w, dir: /tmp, brief: Fix the issues.}\nsupervisor: {cache_ttl: 1h}\n"

func warnings(t *testing.T, yaml string) []string {
	t.Helper()
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Warnings()
}

func TestAFleetWithWorkersAndNoNotifyIsWarned(t *testing.T) {
	got := warnings(t, withWorker)
	if len(got) != 1 || !strings.Contains(got[0], "defaults.notify is not set") {
		t.Fatalf("warnings = %q", got)
	}
}

func TestAFleetWithWorkersAndNoCacheTTLIsWarned(t *testing.T) {
	yaml := minimal + "    workers:\n      - {name: w, dir: /tmp, brief: b}\ndefaults: {notify: 'true'}\n"
	got := warnings(t, yaml)
	if len(got) != 1 || !strings.Contains(got[0], "cache_ttl: 1h") {
		t.Fatalf("warnings = %q", got)
	}
}

func TestAnExplicitFiveMinuteCacheTTLIsNotWarned(t *testing.T) {
	yaml := minimal + "    workers:\n      - {name: w, dir: /tmp, brief: b}\ndefaults: {notify: 'true'}\nsupervisor: {cache_ttl: 5m}\n"
	if got := warnings(t, yaml); len(got) != 0 {
		t.Fatalf("warnings = %q", got)
	}
}

func TestAFleetWithNoWorkersNeedsNoNotify(t *testing.T) {
	if got := warnings(t, minimal); len(got) != 0 {
		t.Fatalf("warnings = %q", got)
	}
}

func TestAFleetThatNotifiesAndAsksNothingHasNoWarnings(t *testing.T) {
	if got := warnings(t, withWorker+"defaults: {notify: 'true'}\n"); len(got) != 0 {
		t.Fatalf("warnings = %q", got)
	}
}

func TestStandingOrdersThatChangeDirectoryBeforeGitAreWarned(t *testing.T) {
	got := warnings(t, withWorker+"defaults: {notify: 'true', standing_orders: 'Before you push, run cd ~/src/app && git fetch.'}\n")
	if len(got) != 1 || !strings.Contains(got[0], "standing_orders") || !strings.Contains(got[0], "git -C") {
		t.Fatalf("warnings = %q", got)
	}
}

func TestABriefThatChangesDirectoryBeforeGitIsWarned(t *testing.T) {
	yaml := minimal + "    workers:\n      - {name: w, dir: /tmp, brief: 'Run cd ../shared; git pull first.'}\ndefaults: {notify: 'true'}\nsupervisor: {cache_ttl: 1h}\n"
	got := warnings(t, yaml)
	if len(got) != 1 || !strings.Contains(got[0], `worker "w"`) {
		t.Fatalf("warnings = %q", got)
	}
}

func TestARuleAgainstChangingDirectoryBeforeGitIsNotWarned(t *testing.T) {
	if got := warnings(t, withWorker+"defaults: {notify: 'true', standing_orders: 'NO cd BEFORE GIT. Never run `cd <dir> && git ...`; use git -C.'}\n"); len(got) != 0 {
		t.Fatalf("warnings = %q", got)
	}
}

func TestGitAloneIsNotWarned(t *testing.T) {
	if got := warnings(t, withWorker+"defaults: {notify: 'true', standing_orders: 'Use git -C ~/src/app status; cd into the worktree first.'}\n"); len(got) != 0 {
		t.Fatalf("warnings = %q", got)
	}
}
