package deploy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

var here = config.Machine{Name: "box", Host: config.LocalHost}

// distFiles makes a binary for every platform and a fleet file, and returns
// the lookup for the binaries and the fleet file's path.
func distFiles(t *testing.T) (func(string) (string, error), string) {
	t.Helper()
	dist := t.TempDir()
	for _, p := range platforms {
		if err := os.WriteFile(filepath.Join(dist, "shed-"+p), []byte("binary for "+p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dist, "shed.yaml")
	if err := os.WriteFile(cfgPath, []byte("machines: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return func(p string) (string, error) { return filepath.Join(dist, "shed-"+p), nil }, cfgPath
}

// Deploys to this machine with HOME pointed at a temporary directory.
func TestMachineInstallsBinaryConfigAndName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binaryFor, cfgPath := distFiles(t)

	res, err := Machine(context.Background(), probe.ShellRunner{}, here, "v1.2.3", binaryFor, cfgPath)
	if err != nil || !res.BinaryInstalled {
		t.Fatalf("result = %+v, %v", res, err)
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

// installed puts a stand-in shed that reports the given version on the machine.
func installed(t *testing.T, home, version string) string {
	t.Helper()
	path := filepath.Join(home, RemoteBinary)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho shed "+version+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func neverFetched(t *testing.T) func(string) (string, error) {
	return func(string) (string, error) {
		t.Fatal("the binary was fetched for a machine that already runs this version")
		return "", nil
	}
}

func TestMachineThatRunsThisVersionKeepsItsBinaryAndGetsTheConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	installed(t, home, "v1.2.3")
	_, cfgPath := distFiles(t)

	res, err := Machine(context.Background(), probe.ShellRunner{}, here, "v1.2.3", neverFetched(t), cfgPath)
	if err != nil || res.BinaryInstalled {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if got, _ := os.ReadFile(filepath.Join(home, RemoteConfig)); string(got) != "machines: []\n" {
		t.Fatalf("config = %q; the fleet file must still be sent", got)
	}
}

func TestMachineOnAnOlderVersionGetsTheNewBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := installed(t, home, "v1.2.2")
	binaryFor, cfgPath := distFiles(t)

	res, err := Machine(context.Background(), probe.ShellRunner{}, here, "v1.2.3", binaryFor, cfgPath)
	if err != nil || !res.BinaryInstalled {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if got, _ := os.ReadFile(path); !strings.HasPrefix(string(got), "binary for ") {
		t.Fatalf("binary = %q", got)
	}
}

func TestUnreleasedBuildIsAlwaysSent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	installed(t, home, "6f9c728")
	binaryFor, cfgPath := distFiles(t)

	res, err := Machine(context.Background(), probe.ShellRunner{}, here, "6f9c728", binaryFor, cfgPath)
	if err != nil || !res.BinaryInstalled {
		t.Fatalf("result = %+v, %v; two working-tree builds can differ under one version", res, err)
	}
}

func TestMachineReportsAMissingBinary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, cfgPath := distFiles(t)
	missing := func(string) (string, error) { return "", errors.New("release v9.9.9 not found") }
	if _, err := Machine(context.Background(), probe.ShellRunner{}, here, "v9.9.9", missing, cfgPath); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %v", err)
	}
}

// scriptedRunner answers every command with fixed output and records it.
type scriptedRunner struct {
	out      string
	err      error
	commands []string
}

func (s *scriptedRunner) Run(_ context.Context, _ config.Machine, command string, _ io.Reader) ([]byte, error) {
	s.commands = append(s.commands, command)
	return []byte(s.out), s.err
}

func TestRestartAgentUsesThePlatformsServiceManager(t *testing.T) {
	for platform, want := range map[string]string{"linux-amd64": "systemctl --user restart shed-agent", "darwin-arm64": "launchctl kill SIGTERM"} {
		r := &scriptedRunner{out: "restarted\n"}
		restarted, err := RestartAgent(context.Background(), r, box, platform)
		if err != nil || !restarted || !strings.Contains(r.commands[0], want) {
			t.Errorf("%s: restarted = %v, %v, command %q", platform, restarted, err, r.commands)
		}
	}
}

func TestRestartAgentReportsAServiceThatIsNotInstalled(t *testing.T) {
	restarted, err := RestartAgent(context.Background(), &scriptedRunner{out: "absent\n"}, box, "linux-amd64")
	if err != nil || restarted {
		t.Fatalf("restarted = %v, %v", restarted, err)
	}
}

func TestRestartAgentReportsAFailedRestart(t *testing.T) {
	r := &scriptedRunner{err: errors.New("Failed to connect to bus")}
	if _, err := RestartAgent(context.Background(), r, box, "linux-amd64"); err == nil || !strings.Contains(err.Error(), "restart agent") {
		t.Fatalf("error = %v", err)
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

func TestAgentInstalled(t *testing.T) {
	for out, want := range map[string]bool{"present\n": true, "absent\n": false} {
		got, err := AgentInstalled(context.Background(), &scriptedRunner{out: out}, box, "darwin-arm64")
		if err != nil || got != want {
			t.Errorf("output %q: installed = %v, %v; want %v", out, got, err, want)
		}
	}
}

func TestInstallAgentWritesTheServiceFileAndStartsIt(t *testing.T) {
	cases := map[string][2]string{
		"linux-amd64":  {".config/systemd/user/shed-agent.service", "systemctl --user enable --now shed-agent"},
		"darwin-arm64": {"Library/LaunchAgents/com.edisoncode.shed-agent.plist", "launchctl bootstrap"},
	}
	for platform, want := range cases {
		r := &scriptedRunner{}
		if _, err := InstallAgent(context.Background(), r, box, platform); err != nil {
			t.Fatal(err)
		}
		if len(r.commands) != 2 || !strings.Contains(r.commands[0], want[0]) || !strings.Contains(r.commands[1], want[1]) {
			t.Errorf("%s: commands = %q", platform, r.commands)
		}
	}
}

func TestInstallAgentPassesOnWhatTheOwnerMustStillDo(t *testing.T) {
	r := &scriptedRunner{out: "The agent will stop when you log out: run 'loginctl enable-linger' on the machine.\n"}
	note, err := InstallAgent(context.Background(), r, box, "linux-amd64")
	if err != nil || !strings.Contains(note, "enable-linger") {
		t.Fatalf("note = %q, %v", note, err)
	}
}

func TestInstallAgentReportsAServiceThatDidNotStart(t *testing.T) {
	r := &scriptedRunner{err: errors.New("Bootstrap failed: 5: Input/output error")}
	if _, err := InstallAgent(context.Background(), r, box, "darwin-arm64"); err == nil {
		t.Fatal("a failed start must be an error")
	}
}

func TestRecycleLeavesARequestForTheAgent(t *testing.T) {
	for now, want := range map[bool]string{false: "", true: "now"} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if err := Recycle(context.Background(), probe.ShellRunner{}, here, "app", now); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(home, ".local/state/shed/recycle/app"))
		if err != nil || string(got) != want {
			t.Errorf("now=%v: request = %q, %v; want %q", now, got, err, want)
		}
	}
}

func TestCancelRemovesOnlyATaskThatStillWaits(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx, r := context.Background(), probe.ShellRunner{}
	tasks := filepath.Join(home, ".local/state/shed/tasks")
	if err := PushTask(ctx, r, here, "", "t1", []byte("for anyone")); err != nil {
		t.Fatal(err)
	}
	if err := PushTask(ctx, r, here, "app", "t2", []byte("for app")); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(tasks, "_taken"), 0o755)
	if err := os.Rename(filepath.Join(tasks, "app/t2.md"), filepath.Join(tasks, "_taken/t2.md")); err != nil {
		t.Fatal(err)
	}

	for id, want := range map[string]string{"t1": TaskCancelled, "t2": TaskTaken, "t3": TaskAbsent} {
		if got, err := CancelTask(ctx, r, here, id); err != nil || got != want {
			t.Errorf("cancel %s = %q, %v; want %q", id, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(tasks, "_any/t1.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the cancelled task is still on the machine: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasks, "_taken/t2.md")); err != nil {
		t.Errorf("a task in a worker's hands was removed: %v", err)
	}
}

func TestSecondDeployOfAMachineIsRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, r := context.Background(), probe.ShellRunner{}
	unlock, err := Lock(ctx, r, here)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(ctx, r, here); !errors.Is(err, ErrBusy) {
		t.Fatalf("second lock = %v, want a refusal", err)
	}
	unlock()
	again, err := Lock(ctx, r, here)
	if err != nil {
		t.Fatalf("lock after the first deploy finished = %v", err)
	}
	again()
}

func TestLockLeftByADeadDeployIsTakenOver(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stale := filepath.Join(home, lockDir)
	os.MkdirAll(stale, 0o755)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	unlock, err := Lock(context.Background(), probe.ShellRunner{}, here)
	if err != nil {
		t.Fatalf("lock = %v; an hour-old lock is from a deploy that died", err)
	}
	// It is this deploy's lock now: a third deploy must be refused.
	if _, err := Lock(context.Background(), probe.ShellRunner{}, here); !errors.Is(err, ErrBusy) {
		t.Fatalf("lock after the takeover = %v, want a refusal", err)
	}
	unlock()
}
