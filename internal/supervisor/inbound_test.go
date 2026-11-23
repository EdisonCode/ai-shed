package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
)

func TestAnUnattendedWorkerRefusesPeerMessages(t *testing.T) {
	if !strings.Contains(sessionCommand(config.Worker{}), `"crossSessionInbound":"refuse"`) {
		t.Fatalf("command = %q", sessionCommand(config.Worker{}))
	}
}

func TestAWorkerStartedWithShedsSettingsHasNoInboundWarning(t *testing.T) {
	if got := InboundWarnings(t.TempDir(), config.Worker{Command: "claude --permission-mode auto"}); len(got) != 0 {
		t.Fatalf("warnings = %q", got)
	}
}

func TestOwnSettingsFileWithoutTheKeyIsNamed(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "worker.json"), []byte(`{"model":"sonnet"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := InboundWarnings(home, config.Worker{Command: "claude --settings ~/worker.json"})
	if len(got) != 1 || !strings.Contains(got[0], "worker.json does not set crossSessionInbound") {
		t.Fatalf("warnings = %q", got)
	}
}

func TestOwnSettingsFileThatRefusesHasNoWarning(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "worker.json"), []byte(`{"crossSessionInbound":"refuse"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := InboundWarnings(home, config.Worker{Command: "claude --settings ~/worker.json"}); len(got) != 0 {
		t.Fatalf("warnings = %q", got)
	}
}

func TestOwnSettingsThatHoldMessagesAreWarnedAbout(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "worker.json"), []byte(`{"crossSessionInbound":"hold"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := InboundWarnings(home, config.Worker{Command: "claude --settings ~/worker.json"})
	if len(got) != 1 || !strings.Contains(got[0], "holds peer messages") {
		t.Fatalf("warnings = %q", got)
	}
}

func TestInlineSettingsWithoutTheKeyAreWarnedAbout(t *testing.T) {
	got := InboundWarnings(t.TempDir(), config.Worker{Command: `claude --settings '{"model":"sonnet"}'`})
	if len(got) != 1 || !strings.Contains(got[0], "does not set crossSessionInbound") {
		t.Fatalf("warnings = %q", got)
	}
}

func TestASettingsFileThatCannotBeReadIsNamed(t *testing.T) {
	got := InboundWarnings(t.TempDir(), config.Worker{Command: "claude --settings ~/missing.json"})
	if len(got) != 1 || !strings.Contains(got[0], "missing.json") {
		t.Fatalf("warnings = %q", got)
	}
}

func TestAWorkerThatTalksWithTheOwnerHasNoInboundWarning(t *testing.T) {
	if got := InboundWarnings(t.TempDir(), config.Worker{Command: "claude --settings ~/missing.json", Ask: true}); len(got) != 0 {
		t.Fatalf("warnings = %q", got)
	}
}
