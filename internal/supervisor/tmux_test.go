package supervisor

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Runs against a real tmux on a private server, so it never touches the
// user's own sessions.
func TestTmuxOpenSendObserveCapture(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	tm := Tmux{Socket: "shed-test-" + strings.ReplaceAll(t.Name(), "/", "-")}
	t.Cleanup(func() { tm.run("kill-server") })

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
	if strings.Contains(other, "42") {
		t.Fatalf("text sent to window one reached one-more:\n%s", other)
	}
	if obs, _ := tm.Observe("on"); obs.Exists {
		t.Fatal("a window name prefix must not match")
	}
}
