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

func peered() config.Worker {
	return config.Worker{
		Name:     "app",
		Command:  "claude --permission-mode auto",
		Session:  "box-app",
		PeerList: []config.Peer{{Machine: "box", Worker: "api", Session: "box-api"}},
	}
}

func TestAWorkerWithPeersAcceptsTheirMessagesAndIsNamed(t *testing.T) {
	got := sessionCommand(peered())
	if !strings.Contains(got, `"crossSessionInbound":"accept"`) || !strings.Contains(got, "--name 'box-app'") {
		t.Fatalf("command = %q", got)
	}
}

func TestAWorkerWithPeersAndItsOwnSettingsIsStillNamed(t *testing.T) {
	w := peered()
	w.Command = "claude --settings ~/worker.json"
	if got := sessionCommand(w); got != "claude --settings ~/worker.json --name 'box-app'" {
		t.Fatalf("command = %q", got)
	}
}

func TestOwnSettingsThatRefuseAreWarnedAboutWhenTheWorkerHasPeers(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "worker.json"), []byte(`{"crossSessionInbound":"refuse"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	w := peered()
	w.Command = "claude --settings ~/worker.json"
	got := InboundWarnings(home, w)
	if len(got) != 1 || !strings.Contains(got[0], `refuses messages, but the worker has peers`) {
		t.Fatalf("warnings = %q", got)
	}
}

func TestAProjectFileThatRefusesIsWarnedAboutWhenTheWorkerHasPeers(t *testing.T) {
	dir := t.TempDir()
	writeSettings(t, dir, "settings.json", `{"crossSessionInbound":"refuse"}`)
	w := peered()
	w.Dir = dir
	got := InboundWarnings(t.TempDir(), w)
	if len(got) != 1 || !strings.Contains(got[0], "settings.json refuses messages from other sessions") {
		t.Fatalf("warnings = %q", got)
	}
}

func TestABareWorkerWithPeersCannotReceive(t *testing.T) {
	w := peered()
	w.Command = "claude --bare"
	got := InboundWarnings(t.TempDir(), w)
	if len(got) != 1 || !strings.Contains(got[0], "--bare") {
		t.Fatalf("warnings = %q", got)
	}
}

func TestTheBriefNamesEachPeer(t *testing.T) {
	var b strings.Builder
	w := peered()
	w.PeerList = append(w.PeerList, config.Peer{Machine: "mini", Worker: "web", Session: "mini-web", Remote: true})
	peersBrief(&b, w)
	for _, want := range []string{"## Peers", "`box-app`", "`box-api`", "`mini-web` (on mini)", "Never ask a peer to make an edit"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("brief lacks %q:\n%s", want, b.String())
		}
	}
}

func TestAWorkerWithoutPeersGetsNoPeersSection(t *testing.T) {
	var b strings.Builder
	peersBrief(&b, config.Worker{Name: "app"})
	if b.Len() != 0 {
		t.Fatalf("brief = %q", b.String())
	}
}
