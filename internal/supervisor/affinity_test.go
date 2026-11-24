package supervisor

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// withDocs adds a second worker, docs, that built issue n and has since
// moved on to issue 30.
func withDocs(t *testing.T, f *fixture, n int, verdict string) {
	t.Helper()
	f.Machine.Workers = append(f.Machine.Workers, config.Worker{Name: "docs", Dir: t.TempDir(), Brief: "b"})
	appendCheckin(t, f, runlog.Checkin{Time: f.now.Add(-2 * time.Hour), Worker: "docs", Kind: KindIdle, Verdict: runlog.VerdictNudge, Sent: true, Issue: n})
	appendCheckin(t, f, runlog.Checkin{Time: f.now.Add(-time.Hour), Worker: "docs", Kind: KindIdle, Verdict: runlog.VerdictNudge, Sent: true, Issue: 30})
	if verdict != "" {
		appendCheckin(t, f, runlog.Checkin{Time: f.now, Worker: "docs", Kind: KindIdle, Verdict: verdict})
	}
}

func TestReworkWaitsForTheWorkerThatBuiltIt(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	withDocs(t, f, 12, "")
	f.lister.issues = []backlog.Issue{handedBackWith(12, "conflicts with the base branch")}
	f.tick(2 * time.Minute)
	if prompt := f.reviewer.prompts[0]; !strings.Contains(prompt, "[held: rework for docs, which built it and has its context]") {
		t.Fatalf("the rework is not left to its builder:\n%s", prompt)
	}
}

func TestReworkGoesToAnyWorkerWhenItsBuilderIsStuckOrLimited(t *testing.T) {
	for _, verdict := range []string{runlog.VerdictStuck, runlog.VerdictLimited} {
		f := started(t, config.Supervisor{}, done)
		withDocs(t, f, 12, verdict)
		f.lister.issues = []backlog.Issue{handedBackWith(12, "conflicts with the base branch")}
		f.tick(2 * time.Minute)
		if prompt := f.reviewer.prompts[0]; strings.Contains(prompt, "rework for docs") {
			t.Errorf("%s builder: the rework still waits for it:\n%s", verdict, prompt)
		}
	}
}

func TestReworkGoesToAnyWorkerOnceTheReservationLapses(t *testing.T) {
	f := started(t, config.Supervisor{AffinityTTL: "1h"}, done, done)
	withDocs(t, f, 12, "")
	f.lister.issues = []backlog.Issue{handedBackWith(12, "conflicts with the base branch")}
	f.tick(2 * time.Minute)
	f.tick(61 * time.Minute)
	if prompt := f.reviewer.prompts[len(f.reviewer.prompts)-1]; strings.Contains(prompt, "rework for docs") {
		t.Fatalf("the reservation did not lapse:\n%s", prompt)
	}
}

func TestAnIssueThatOverlapsWorkInHandStaysReservedForThatWorker(t *testing.T) {
	f := started(t, config.Supervisor{OverlapLabels: "area:"}, done, done)
	f.Machine.Workers = append(f.Machine.Workers, config.Worker{Name: "docs", Dir: t.TempDir(), Brief: "b"})
	appendCheckin(t, f, runlog.Checkin{Time: f.now, Worker: "docs", Kind: KindIdle, Verdict: runlog.VerdictNudge, Sent: true, Issue: 12})
	f.lister.issues = []backlog.Issue{labeled(11, "area:billing"), labeled(12, "area:billing"), labeled(20)}
	f.tick(2 * time.Minute)
	// docs handed #12 back and took #20: #11 is still the follow-on of the
	// code it has open.
	appendCheckin(t, f, runlog.Checkin{Time: f.now, Worker: "docs", Kind: KindIdle, Verdict: runlog.VerdictNudge, Sent: true, Issue: 20})
	f.tick(10 * time.Minute)
	if prompt := f.reviewer.prompts[len(f.reviewer.prompts)-1]; !strings.Contains(prompt, "org/app#11 Issue [held: reserved for docs: it follows #12, which docs built]") {
		t.Fatalf("#11 is not reserved for docs:\n%s", prompt)
	}
}

func TestAffinityTTLZeroTurnsRoutingOff(t *testing.T) {
	f := started(t, config.Supervisor{AffinityTTL: "0s"}, done)
	withDocs(t, f, 12, "")
	f.lister.issues = []backlog.Issue{handedBackWith(12, "conflicts with the base branch")}
	f.tick(2 * time.Minute)
	if prompt := f.reviewer.prompts[0]; strings.Contains(prompt, "rework for docs") {
		t.Fatalf("routing is off, yet:\n%s", prompt)
	}
}

func TestReworkOfItsOwnIssueKeepsAFreshWorkersContext(t *testing.T) {
	f := freshWorker(t, config.Supervisor{CacheTTL: "1h"}, assign(13), assign(12))
	built(t, f, 12)
	f.lister.issues = []backlog.Issue{handedBackWith(12, "conflicts with the base branch"), labeled(13)}
	f.tick(2 * time.Minute)
	sent := f.handOver()
	if slices.Contains(sent, "/clear") {
		t.Fatalf("sent = %q, want no clear: the worker built #12 and its context helps", sent)
	}
}
