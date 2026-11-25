package supervisor

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

const remoteLost = "Remote Control disconnected — signed-in claude.ai account or organization changed on this machine — run /remote-control to start a session for the current account, or /login to switch back, then /remote-control"

func TestTheScreenSaysRemoteControlIsLostUntilItIsTypedAgain(t *testing.T) {
	for name, tc := range map[string]struct {
		screen string
		lost   bool
	}{
		"connected":                {"⏺ Tests pass.\n\n> \n  /rc active", false},
		"dropped":                  {"⏺ Tests pass.\n" + remoteLost + "\n\n> ", true},
		"reconnect failed":         {"> /remote-control\nCouldn't reconnect to your Remote Control session\n> ", true},
		"typed again since":        {remoteLost + "\n\n> /remote-control\n  Remote Control active\n> ", false},
		"scrolled out of the rows": {remoteLost + strings.Repeat("\n⏺ more work", remoteRows) + "\n> ", false},
	} {
		if got := RemoteControlLost(tc.screen); got != tc.lost {
			t.Errorf("%s: lost = %v, want %v", name, got, tc.lost)
		}
	}
}

// remoteWorker is a started worker that runs with Remote Control, resting.
func remoteWorker(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, config.Supervisor{}, done, done, done, done, done, done)
	f.Machine.Workers[0].Command = "claude --permission-mode auto --remote-control"
	f.tick(0)
	f.term.sent = nil
	f.term.running(f.now)
	f.mark(t, runlog.MarkIdle, "", f.now)
	return f
}

func (f *fixture) typedRemote() int {
	n := 0
	for _, s := range f.term.sent {
		if s == reconnectRemote {
			n++
		}
	}
	return n
}

func TestAWorkerAtRestThatLostRemoteControlIsReconnected(t *testing.T) {
	f := remoteWorker(t)
	f.term.screen = "⏺ Done.\n" + remoteLost + "\n\n> "
	f.tick(3 * time.Minute)
	if f.typedRemote() != 1 {
		t.Fatalf("sent = %q, want /remote-control once", f.term.sent)
	}
	r, ok := runlog.ReadRemote(f.StateDir, "app")
	if !ok || !r.Down() || r.Typed != 1 {
		t.Fatalf("recorded %+v, %v", r, ok)
	}
	f.term.screen = remoteLost + "\n\n> /remote-control\n  Remote Control active\n> "
	f.tick(3 * time.Minute)
	if r, _ := runlog.ReadRemote(f.StateDir, "app"); r.Down() {
		t.Fatalf("Remote Control is still recorded down after it came back: %+v", r)
	}
}

func TestABusyWorkerIsReconnectedOnlyOnceItRests(t *testing.T) {
	f := remoteWorker(t)
	f.mark(t, runlog.MarkWorking, "", f.now)
	f.term.screen = remoteLost + "\n⏺ Running the tests…"
	f.tick(3 * time.Minute)
	if f.typedRemote() != 0 {
		t.Fatalf("typed into a turn in progress: %q", f.term.sent)
	}
	if r, ok := runlog.ReadRemote(f.StateDir, "app"); !ok || !r.Down() {
		t.Fatalf("the lost Remote Control was not recorded: %+v, %v", r, ok)
	}
	f.mark(t, runlog.MarkIdle, "", f.now)
	f.tick(3 * time.Minute)
	if f.typedRemote() != 1 {
		t.Fatalf("sent = %q, want /remote-control once it rests", f.term.sent)
	}
}

func TestTheOwnerIsToldWhenTypingItDoesNotBringRemoteControlBack(t *testing.T) {
	f := remoteWorker(t)
	var told []string
	f.Notify = func(_, message string) { told = append(told, message) }
	f.term.screen = "> /remote-control\nCouldn't reconnect to your Remote Control session\n> "
	for range 2 * (remoteTries + 2) {
		f.tick(3 * time.Minute)
	}
	if f.typedRemote() != remoteTries {
		t.Fatalf("typed /remote-control %d times, want %d", f.typedRemote(), remoteTries)
	}
	if len(told) != 1 || !strings.Contains(told[0], "shed recycle box app") {
		t.Fatalf("told = %q", told)
	}
	if r, _ := runlog.ReadRemote(f.StateDir, "app"); !r.GaveUp {
		t.Fatalf("recorded %+v, want gave up", r)
	}
}

func TestAWorkerWithoutRemoteControlIsNotWatched(t *testing.T) {
	f := started(t, config.Supervisor{}, done, done)
	f.mark(t, runlog.MarkIdle, "", f.now)
	f.term.screen = remoteLost + "\n> "
	f.tick(3 * time.Minute)
	if slices.Contains(f.term.sent, reconnectRemote) {
		t.Fatalf("sent = %q", f.term.sent)
	}
}
