package deploy

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/probe"
)

func TestPlatform(t *testing.T) {
	cases := map[string]string{
		"Linux x86_64\n":  "linux-amd64",
		"Linux aarch64\n": "linux-arm64",
		"Darwin arm64\n":  "darwin-arm64",
		"Darwin x86_64\n": "darwin-amd64",
	}
	for uname, want := range cases {
		if got, err := Platform(uname); err != nil || got != want {
			t.Errorf("Platform(%q) = %q, %v; want %q", uname, got, err, want)
		}
	}
}

func TestPlatformRejectsUnknown(t *testing.T) {
	if _, err := Platform("FreeBSD amd64\n"); err == nil {
		t.Fatal("an unsupported platform must be an error")
	}
}

// Deploys to this machine with HOME pointed at a temporary directory.
func TestMachineInstallsBinaryConfigAndName(t *testing.T) {
	home, dist := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	for _, p := range platforms {
		if err := os.WriteFile(filepath.Join(dist, "shed-"+p), []byte("binary for "+p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dist, "shed.yaml")
	if err := os.WriteFile(cfgPath, []byte("machines: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := config.Machine{Name: "box", Host: config.LocalHost}
	if err := Machine(context.Background(), probe.ShellRunner{}, m, dist, cfgPath); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(home, RemoteBinary))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("binary: %v, mode %v", err, info)
	}
	if got, _ := os.ReadFile(filepath.Join(home, RemoteConfig)); string(got) != "machines: []\n" {
		t.Fatalf("config = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(home, RemoteMachine)); string(got) != "box\n" {
		t.Fatalf("machine name = %q", got)
	}
}

func TestMachineNeedsTheDistBinary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := config.Machine{Name: "box", Host: config.LocalHost}
	if err := Machine(context.Background(), probe.ShellRunner{}, m, t.TempDir(), "unused"); err == nil {
		t.Fatal("a missing dist binary must be an error")
	}
}

var box = config.Machine{Name: "box", Host: "me@box"}

func TestHookIsToldWhichMachine(t *testing.T) {
	var out bytes.Buffer
	if err := Hook(context.Background(), `echo "$SHED_MACHINE at $SHED_HOST"`, box, &out, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "box at me@box\n" {
		t.Fatalf("hook output = %q", out.String())
	}
}

func TestFailingHookIsAnError(t *testing.T) {
	var out bytes.Buffer
	err := Hook(context.Background(), "echo sync failed >&2; exit 4", box, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "deploy hook") || !strings.Contains(out.String(), "sync failed") {
		t.Fatalf("error = %v, output = %q", err, out.String())
	}
}

func TestNoHookDoesNothing(t *testing.T) {
	if err := Hook(context.Background(), "", box, nil, nil); err != nil {
		t.Fatal(err)
	}
}
