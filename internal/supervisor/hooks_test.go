package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
)

const (
	stateHooks = `{"hooks": {
		"UserPromptSubmit": [{"hooks": [{"type": "command", "command": "$HOME/.local/bin/shed mark working"}]}],
		"Stop": [{"hooks": [{"type": "command", "command": "$HOME/.local/bin/shed mark idle"}]}]
	}}`
	usageHook = `{"hooks": {
		"SubagentStop": [{"hooks": [{"type": "command", "command": "$HOME/.local/bin/shed mark usage"}]}]
	}}`
)

func writeSettings(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude", name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStateHooksWithoutTheSubagentHookLeaveSubAgentsOutOfTheCost(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	writeSettings(t, home, "settings.json", stateHooks)
	got := HookWarnings(home, config.Worker{Name: "app", Dir: dir})
	if len(got) != 1 || !strings.Contains(got[0], "cost comments leave out sub-agents: add the SubagentStop hook") {
		t.Fatalf("warnings = %q", got)
	}
}

func TestSubagentHookInTheWorkersOwnSettingsIsFound(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	writeSettings(t, home, "settings.json", stateHooks)
	writeSettings(t, dir, "settings.local.json", usageHook)
	if got := HookWarnings(home, config.Worker{Name: "app", Dir: dir}); len(got) != 0 {
		t.Fatalf("warnings = %q, want none", got)
	}
}

func TestWithoutStateHooksThereIsNoCostToWarnAbout(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	if got := HookWarnings(home, config.Worker{Name: "app", Dir: dir}); len(got) != 0 {
		t.Fatalf("warnings = %q, want none", got)
	}
}

func TestAnotherToolsSettingsAreNotRead(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	writeSettings(t, home, "settings.json", stateHooks)
	if got := HookWarnings(home, config.Worker{Name: "app", Dir: dir, Command: "codex"}); len(got) != 0 {
		t.Fatalf("warnings = %q, want none for a tool that is not claude", got)
	}
}

func TestSettingsThatCannotBeParsedAreNamed(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	writeSettings(t, dir, "settings.json", "{not json")
	got := HookWarnings(home, config.Worker{Name: "app", Dir: dir})
	if len(got) != 1 || !strings.Contains(got[0], "cannot read the hooks in "+filepath.Join(dir, ".claude", "settings.json")) {
		t.Fatalf("warnings = %q", got)
	}
}

func TestClassifyShowsWarningsBesideAReadyWorker(t *testing.T) {
	r := PreflightResult{Worker: "app", State: PreflightRunning, Warnings: []string{"cost comments leave out sub-agents"}}
	line, ready := Classify(context.Background(), &fakeAsker{}, r)
	if !ready || !strings.Contains(line, "app: already running\n    note: cost comments leave out sub-agents") {
		t.Fatalf("ready = %v, line = %q", ready, line)
	}
}
