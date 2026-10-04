package supervisor

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// privateTmux is a real tmux on a private server, so a test never touches
// the user's own sessions.
func privateTmux(t *testing.T) Tmux {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	tm := Tmux{Socket: "shed-test-" + strings.ReplaceAll(t.Name(), "/", "-")}
	t.Cleanup(func() { tm.run("kill-server") })
	return tm
}

// withoutLocale gives the test the environment a service manager gives the
// agent: no locale. tmux then prints "_" in place of any control character.
func withoutLocale(t *testing.T) {
	t.Helper()
	for _, name := range []string{"LANG", "LC_ALL", "LC_CTYPE"} {
		t.Setenv(name, "")
	}
}

func TestTmuxOpenSendObserveCapture(t *testing.T) {
	tm := privateTmux(t)

	if obs, err := tm.Observe("one"); err != nil || obs.Exists {
		t.Fatalf("before any session: %+v, %v", obs, err)
	}
	for _, window := range []string{"one", "one-more"} {
		if err := tm.Open(window, t.TempDir()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tm.Send("one", "echo answer $((6*7)); sleep 30"); err != nil {
		t.Fatal(err)
	}

	var screen string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		screen, _ = tm.Capture("one", 20)
		if strings.Contains(screen, "answer 42") {
			break
		}
	}
	if !strings.Contains(screen, "answer 42") {
		t.Fatalf("screen never showed the command's output:\n%s", screen)
	}

	obs, err := tm.Observe("one")
	if err != nil || !obs.Exists || obs.Command != "sleep" || time.Since(obs.LastActivity) > time.Minute {
		t.Fatalf("observation = %+v, %v; want the running sleep", obs, err)
	}
	// The other window must be untouched: exact names, not prefixes.
	other, _ := tm.Capture("one-more", 20)
	if strings.Contains(other, "answer") {
		t.Fatalf("text sent to window one reached one-more:\n%s", other)
	}
	if obs, _ := tm.Observe("on"); obs.Exists {
		t.Fatal("a window name prefix must not match")
	}
}

// The first real run failed here: under launchd the agent has no locale, the
// window list came back with "_" for every tab, and a running worker read as
// missing.
func TestTmuxObservesAWindowWithoutALocale(t *testing.T) {
	tm := privateTmux(t)
	withoutLocale(t)
	if err := tm.Open("portal", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	obs, err := tm.Observe("portal")
	if err != nil || !obs.Exists || obs.Command == "" || time.Since(obs.LastActivity) > time.Minute {
		t.Fatalf("observation = %+v, %v; want the window that was just opened", obs, err)
	}
}

func TestTmuxOpenRefusesANameThatIsTaken(t *testing.T) {
	tm := privateTmux(t)
	if err := tm.Open("portal", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := tm.Open("portal", t.TempDir()); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("second Open = %v, want a refusal", err)
	}
	out, _ := tm.run("list-windows", "-t", "="+Session, "-F", "#{window_name}")
	if strings.Count(out, "portal") != 1 {
		t.Fatalf("windows:\n%s\nwant exactly one named portal", out)
	}
}

func TestTmuxReportsTwoWindowsWithOneName(t *testing.T) {
	tm := privateTmux(t)
	if err := tm.Open("portal", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	// A person, or an older shed, made a second one.
	if _, err := tm.run("new-window", "-d", "-n", "portal", "-t", "="+Session+":"); err != nil {
		t.Fatal(err)
	}
	if _, err := tm.Observe("portal"); err == nil || !strings.Contains(err.Error(), "2 windows named portal") {
		t.Fatalf("Observe = %v, want it to report the two windows", err)
	}
}

// A nudge may hold a dash or a quote mark, and a worker's screen is full of
// symbols. Both must survive an agent that has no locale.
func TestTmuxKeepsUnicodeWithoutALocale(t *testing.T) {
	tm := privateTmux(t)
	withoutLocale(t)
	if err := tm.Open("portal", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := tm.Send("portal", "cat"); err != nil {
		t.Fatal(err)
	}
	if err := tm.Send("portal", "Stop — that is outside the brief ✓"); err != nil {
		t.Fatal(err)
	}
	var screen string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		screen, _ = tm.Capture("portal", 20)
		if strings.Count(screen, "Stop — that is outside the brief ✓") == 2 {
			return // typed once, echoed once by cat
		}
	}
	t.Fatalf("screen never showed the text as it was sent:\n%s", screen)
}

func TestTmuxCloseEndsTheWindow(t *testing.T) {
	tm := privateTmux(t)
	for _, window := range []string{"keep", "preflight-app"} {
		if err := tm.Open(window, t.TempDir()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tm.Close("preflight-app"); err != nil {
		t.Fatal(err)
	}
	gone, _ := tm.Observe("preflight-app")
	kept, _ := tm.Observe("keep")
	if gone.Exists || !kept.Exists {
		t.Fatalf("closed window exists = %v, other window exists = %v", gone.Exists, kept.Exists)
	}
}

func TestTmuxCaptureMarksFaintText(t *testing.T) {
	tm := privateTmux(t)
	if err := tm.Open("portal", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	// The program prints a prompt mark, then a suggestion drawn faint.
	if err := tm.Send("portal", `clear; printf '> \033[2mTake issue 12\033[0m\n'; sleep 30`); err != nil {
		t.Fatal(err)
	}
	var screen string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		screen, _ = tm.Capture("portal", 20)
		if strings.Contains(screen, "> [greyed out: Take issue 12]") {
			return
		}
	}
	t.Fatalf("the faint text was not marked:\n%s", screen)
}
