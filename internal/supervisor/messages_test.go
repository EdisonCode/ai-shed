package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

func (f *fixture) sendMessages(t *testing.T, to string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := runlog.RecordMessage(f.StateDir, "app", runlog.Message{Time: f.now, To: to, Chars: 30}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAWorkerThatMessagesOneSessionPastTheLimitIsToldToStop(t *testing.T) {
	f := started(t, config.Supervisor{MessageLimit: 5})
	f.sendMessages(t, "box-api", 5)
	f.tick(time.Minute)
	c := f.lastCheckin(t)
	if c.Kind != KindMessages || c.Verdict != runlog.VerdictNudge || !strings.Contains(c.Message, "Stop messaging box-api") {
		t.Fatalf("check-in = %+v", c)
	}
	if len(f.term.sent) == 0 || !strings.Contains(f.term.sent[len(f.term.sent)-1], "Stop messaging box-api") {
		t.Fatalf("sent = %q", f.term.sent)
	}
}

func TestAWorkerUnderTheLimitIsLeftAlone(t *testing.T) {
	f := started(t, config.Supervisor{MessageLimit: 5})
	f.sendMessages(t, "box-api", 4)
	f.tick(time.Minute)
	for _, c := range f.checkins(t) {
		if c.Kind == KindMessages {
			t.Fatalf("check-in = %+v", c)
		}
	}
}

func TestAWorkerThatGoesOnMessagingAfterItWasToldIsStuck(t *testing.T) {
	f := started(t, config.Supervisor{MessageLimit: 5}, done, done, done)
	f.sendMessages(t, "box-api", 5)
	f.tick(time.Minute)
	f.now = f.now.Add(10 * time.Second)
	f.sendMessages(t, "box-api", loopAfterTold)
	f.tick(time.Minute)
	c := f.lastCheckin(t)
	if c.Kind != KindMessages || c.Verdict != runlog.VerdictStuck || !strings.Contains(c.Reason, "box-api") {
		t.Fatalf("check-in = %+v", c)
	}
	f.tick(time.Minute)
	stuck := 0
	for _, c := range f.checkins(t) {
		if c.Kind == KindMessages && c.Verdict == runlog.VerdictStuck {
			stuck++
		}
	}
	if stuck != 1 {
		t.Fatalf("the loop was reported %d times, want once", stuck)
	}
}

func TestANegativeLimitTurnsTheCheckOff(t *testing.T) {
	f := started(t, config.Supervisor{MessageLimit: -1})
	f.sendMessages(t, "box-api", 100)
	f.tick(time.Minute)
	for _, c := range f.checkins(t) {
		if c.Kind == KindMessages {
			t.Fatalf("check-in = %+v", c)
		}
	}
}
