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
		"Stop": [{"hooks": [{"type": "command", "command": "$HOME/.local/bin/shed mark idle"}]}],
		"PermissionDenied": [{"hooks": [{"type": "command", "command": "$HOME/.local/bin/shed mark denied"}]}]
	}}`
	// stateHooksOnly has the state hooks and nothing else.
	stateHooksOnly = `{"hooks": {
		"Stop": [{"hooks": [{"type": "command", "command": "$HOME/.local/bin/shed mark idle"}]}],
		"SubagentStop": [{"hooks": [{"type": "command", "command": "$HOME/.local/bin/shed mark usage"}]}]
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

func TestStateHooksWithoutThePermissionDeniedHookLeaveRefusalsUnrecorded(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	writeSettings(t, home, "settings.json", stateHooksOnly)
	got := HookWarnings(home, config.Worker{Name: "app", Dir: dir})
	if len(got) != 1 || !strings.Contains(got[0], "add the PermissionDenied hook that runs `shed mark denied`") {
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

func TestAWorkerWithPeersAndNoMessageHookIsWarned(t *testing.T) {
	dir := t.TempDir()
	w := config.Worker{Name: "app", Dir: dir, PeerList: []config.Peer{{Session: "box-api"}}}
	got := HookWarnings(t.TempDir(), w)
	if len(got) != 2 || !strings.Contains(got[0], "no PostToolUse hook") || !strings.Contains(got[1], "no PostToolUseFailure hook") {
		t.Fatalf("warnings = %q", got)
	}
	writeSettings(t, dir, "settings.json", `{"hooks": {"PostToolUse": [{"matcher": "SendMessage", "hooks": [{"type": "command", "command": "shed mark message"}]}]}}`)
	if got := HookWarnings(t.TempDir(), w); len(got) != 1 || !strings.Contains(got[0], "PostToolUseFailure") {
		t.Fatalf("warnings with the message hook only = %q", got)
	}
	writeSettings(t, dir, "settings.json", `{"hooks": {
		"PostToolUse": [{"matcher": "SendMessage", "hooks": [{"type": "command", "command": "shed mark message"}]}],
		"PostToolUseFailure": [{"matcher": "SendMessage", "hooks": [{"type": "command", "command": "shed mark message"}]}]}}`)
	if got := HookWarnings(t.TempDir(), w); len(got) != 0 {
		t.Fatalf("warnings with both hooks = %q", got)
	}
}
