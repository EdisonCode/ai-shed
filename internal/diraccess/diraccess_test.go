package diraccess

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, script string, env ...string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	return string(out)
}

// privateTmux starts a tmux server of its own with the workers' session, so
// a test never touches the user's sessions.
func privateTmux(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socket := "shed-test-" + strings.ReplaceAll(t.Name(), "/", "-")
	if out, err := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", Session).CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
	return "tmux -L " + socket
}

func TestReadableDirsHaveNoProblems(t *testing.T) {
	dirs := []Dir{{Worker: "a", Path: t.TempDir()}, {Worker: "b", Path: filepath.Join(t.TempDir(), "it's #1")}}
	if err := os.Mkdir(dirs[1].Path, 0o755); err != nil {
		t.Fatal(err)
	}
	problems, host := Parse(run(t, Script(dirs, Options{Tmux: privateTmux(t)})), dirs)
	if len(problems) != 0 {
		t.Fatalf("problems: %+v", problems)
	}
	if host.OS == "" || host.Tmux == "" {
		t.Errorf("host not reported: %+v", host)
	}
}

func TestHomeRelativeDirIsReadUnderHome(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirs := []Dir{{Worker: "a", Path: "~/work"}}
	problems, _ := Parse(run(t, Script(dirs, Options{Tmux: "false"}), "HOME="+home), dirs)
	if len(problems) != 0 {
		t.Fatalf("problems: %+v", problems)
	}
}

func TestMissingDirIsReportedByShellAndTmux(t *testing.T) {
	dirs := []Dir{{Worker: "a", Path: filepath.Join(t.TempDir(), "gone")}}
	problems, _ := Parse(run(t, Script(dirs, Options{Tmux: privateTmux(t)})), dirs)
	if len(problems) != 2 || problems[0].Via != ViaShell || problems[1].Via != ViaTmux {
		t.Fatalf("problems: %+v", problems)
	}
	for _, p := range problems {
		if p.Blocked || p.Path != dirs[0].Path || !strings.Contains(p.Err, "No such file") {
			t.Errorf("problem: %+v", p)
		}
	}
}

func TestNoTmuxSessionSkipsTheTmuxRead(t *testing.T) {
	dirs := []Dir{{Worker: "a", Path: filepath.Join(t.TempDir(), "gone")}}
	problems, _ := Parse(run(t, Script(dirs, Options{Tmux: "false"})), dirs)
	if len(problems) != 1 || problems[0].Via != ViaShell {
		t.Fatalf("problems: %+v", problems)
	}
}

func TestReadThatHangsIsBlockedAndStopped(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "still-running")
	fake := "#!/bin/sh\nsleep 3\ntouch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(bin, "ls"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	dirs := []Dir{{Worker: "a", Path: t.TempDir()}}
	started := time.Now()
	out := run(t, Script(dirs, Options{Tmux: "false", Wait: 300 * time.Millisecond}), "PATH="+bin+":"+os.Getenv("PATH"))
	if waited := time.Since(started); waited > 2*time.Second {
		t.Errorf("check waited %s for a read that hangs", waited)
	}
	problems, _ := Parse(out, dirs)
	if len(problems) != 1 || !problems[0].Blocked {
		t.Fatalf("problems: %+v", problems)
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the blocked read was left running")
	}
}

func TestDescribeNamesTheMacOSFix(t *testing.T) {
	mac := Host{OS: "Darwin", Tmux: "/opt/homebrew/Cellar/tmux/3.5a/bin/tmux"}
	for _, tc := range []struct {
		name string
		p    Problem
		want []string
	}{
		{"tmux denied", Problem{Path: "/Volumes/x", Via: ViaTmux, Err: "ls: /Volumes/x: Operation not permitted"},
			[]string{"not readable by tmux (/opt/homebrew/Cellar/tmux/3.5a/bin/tmux)", "grant /opt/homebrew/Cellar/tmux/3.5a/bin/tmux Full Disk Access", "click Allow"}},
		{"tmux blocked", Problem{Path: "/Volumes/x", Via: ViaTmux, Blocked: true},
			[]string{"did not finish in 5s", "privacy prompt", "Full Disk Access"}},
		{"shell denied", Problem{Path: "/Volumes/x", Via: ViaShell, Err: "ls: /Volumes/x: Operation not permitted"},
			[]string{"not readable by shed's shell", "Allow full disk access for remote users"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.p.Describe(mac)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("%q does not say %q", got, w)
				}
			}
		})
	}
}

func TestDescribeOffMacOSNamesNoMacOSFix(t *testing.T) {
	p := Problem{Path: "/srv/x", Via: ViaShell, Err: "ls: cannot open directory '/srv/x': Permission denied"}
	if got := p.Describe(Host{OS: "Linux"}); strings.Contains(got, "System Settings") {
		t.Errorf("Linux told to use System Settings: %q", got)
	}
}

func TestNoDirsNoScript(t *testing.T) {
	if s := Script(nil, Options{}); s != "" {
		t.Errorf("script for no dirs: %q", s)
	}
}
