package supervisor

import (
	"strings"
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
)

func TestAnUnattendedClaudeWorkerStartsWithBackgroundRequestsOff(t *testing.T) {
	got := sessionCommand(config.Worker{Command: "claude --permission-mode auto"})
	want := "claude --permission-mode auto --settings '" + quietSettings + "'"
	if got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
}

func TestTheDefaultCommandIsQuietedToo(t *testing.T) {
	if got := sessionCommand(config.Worker{}); !strings.HasPrefix(got, "claude --settings ") {
		t.Fatalf("command = %q", got)
	}
}

func TestACommandThatIsNotClaudeIsLeftAlone(t *testing.T) {
	if got := sessionCommand(config.Worker{Command: "codex --full-auto"}); got != "codex --full-auto" {
		t.Fatalf("command = %q", got)
	}
}

func TestACommandWithItsOwnSettingsIsLeftAlone(t *testing.T) {
	command := "claude --settings ~/worker.json"
	if got := sessionCommand(config.Worker{Command: command}); got != command {
		t.Fatalf("command = %q", got)
	}
}

func TestAWorkerThatTalksWithTheOwnerIsLeftAlone(t *testing.T) {
	command := "claude --remote-control box-ask"
	if got := sessionCommand(config.Worker{Command: command, Ask: true}); got != command {
		t.Fatalf("command = %q", got)
	}
}
