package supervisor

import (
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

var waitsOnItsOwnCommand = Verdict{Verdict: runlog.VerdictOnTrack, Reason: "it waits on a background command it started itself"}

type fakeShells struct{ shells []Shell }

func (f *fakeShells) Shells(string) ([]Shell, error) { return f.shells, nil }

func TestAWorkerThatWaitsOnItsOwnWorkTooLongIsToldToCheckIt(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h", OwnWaitMax: "45m"}, assign(12), waitsOnItsOwnCommand)
	f.tick(2 * time.Minute) // it takes #12
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute) // silent; the reviewer finds it waiting on its own command
	sent := len(f.term.sent)

	f.tick(40 * time.Minute)
	if len(f.term.sent) != sent {
		t.Fatalf("sent %q inside the bound", f.term.sent[sent:])
	}
	f.tick(5 * time.Minute)
	c := f.lastCheckin(t)
	if c.Kind != KindWait || !c.Sent || len(f.term.sent) != sent+1 || !strings.Contains(c.Message, "printed nothing for 47m") {
		t.Fatalf("check-in = %+v, sent %q; want the worker told to check its own work", c, f.term.sent)
	}
}

func TestAWorkerThatWaitsOnItsOwnWorkASecondTimeInOneIssueIsStuck(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h", OwnWaitMax: "45m"}, assign(12), waitsOnItsOwnCommand)
	f.tick(2 * time.Minute)
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute)
	f.tick(45 * time.Minute) // told once
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute) // it answered, and is found waiting on its own work again
	sent := len(f.term.sent)

	f.tick(45 * time.Minute)
	if c := f.lastCheckin(t); c.Verdict != runlog.VerdictStuck || len(f.term.sent) != sent {
		t.Fatalf("check-in = %+v, sent %q; want stuck and nothing typed", c, f.term.sent[sent:])
	}
	f.tick(45 * time.Minute)
	if n := len(f.checkins(t)); f.checkins(t)[n-2].Verdict == runlog.VerdictStuck {
		t.Fatalf("stuck was recorded twice for one silence")
	}
}

func TestANewIssueStartsTheOwnWaitCountAgain(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h", OwnWaitMax: "45m"}, assign(12), waitsOnItsOwnCommand, assign(13), waitsOnItsOwnCommand)
	f.lister.issues = append(f.lister.issues, labeled(13))
	f.tick(2 * time.Minute)
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute)
	f.tick(45 * time.Minute) // told once on #12
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute) // handed #13
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute) // waits on its own work on #13

	f.tick(45 * time.Minute)
	if c := f.lastCheckin(t); c.Kind != KindWait || !c.Sent {
		t.Fatalf("check-in = %+v; want a first message on the new issue, not stuck", c)
	}
}

func TestTheOwnWaitBoundCanBeTurnedOff(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h", OwnWaitMax: "0"}, assign(12), waitsOnItsOwnCommand)
	f.tick(2 * time.Minute)
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute)
	sent := len(f.term.sent)
	f.tick(3 * time.Hour)
	if len(f.term.sent) != sent {
		t.Fatalf("sent %q with the bound off", f.term.sent[sent:])
	}
}

func TestTheReviewerAndTheWaitMessageSeeTheShellsTheToolStarted(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h", OwnWaitMax: "45m"}, assign(12), waitsOnItsOwnCommand)
	shells := &fakeShells{shells: []Shell{{PID: 4242, Age: 3 * time.Hour, CPU: 2 * time.Second, Command: `zsh -c sed -n 1p /dev/stdin`}}}
	f.Shells = shells
	f.tick(2 * time.Minute)
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute)
	prompt := f.reviewer.prompts[len(f.reviewer.prompts)-1]
	if !strings.Contains(prompt, "<shells>") || !strings.Contains(prompt, "pid 4242, running 3h, CPU 2s") || !strings.Contains(prompt, "zsh -c sed -n 1p /dev/stdin") {
		t.Fatalf("prompt lacks the shells:\n%s", prompt)
	}

	shells.shells[0].Age += 45 * time.Minute
	f.tick(45 * time.Minute)
	c := f.lastCheckin(t)
	if !strings.Contains(c.Message, "pid 4242, running 3h, CPU 2s, unchanged since 48m ago") {
		t.Fatalf("message = %q; want the shell with its CPU time unchanged", c.Message)
	}
}

func TestParseShellsKeepsTheShellsTheToolStarted(t *testing.T) {
	ps := `    1     0 10-02:00:00 00:01:00 /sbin/init
  100     1    05:00:00 00:00:01 tmux new-session
  200   100    05:00:00 00:00:00 -zsh
  300   200    04:00:00 00:12:00 claude --permission-mode auto
  400   300    04:00:00 00:00:30 node /opt/mcp/server.js
  500   300    03:00:00  0:00.02 /bin/zsh -c -l source /tmp/snap && eval 'sed -n 1p /dev/stdin'
  501   500    03:00:00  0:01.50 sed -n 1p /dev/stdin
  600   300       01:30 00:00:00 bash -c until ! pgrep -f x; do sleep 5; done
`
	got := parseShells(ps, 200, time.Now())
	if len(got) != 2 {
		t.Fatalf("shells = %+v, want the two shells claude started", got)
	}
	if got[0].PID != 500 || got[0].Age != 3*time.Hour || got[0].CPU != 1520*time.Millisecond {
		t.Fatalf("first shell = %+v, want pid 500, 3h, the CPU of it and its children", got[0])
	}
	if got[1].PID != 600 || got[1].Age != 90*time.Second {
		t.Fatalf("second shell = %+v", got[1])
	}
}

func TestBriefSaysHowToRunALongCommand(t *testing.T) {
	f := newFixture(t, config.Supervisor{})
	brief := f.briefText(f.Machine.Workers[0])
	if !strings.Contains(brief, "writes an exit marker") || !strings.Contains(brief, "`pgrep -f`") {
		t.Fatalf("brief lacks the rule for long commands:\n%s", brief)
	}
}
