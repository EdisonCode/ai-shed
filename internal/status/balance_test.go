package status

import (
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// twoMachines is a fleet of two machines on one repository: pc, whose
// worker has had nothing to do for 7 hours, and mini, whose worker has #7
// in hand with #8 and #9 queued behind it, at a load of 3.33 per core.
func twoMachines(miniCapacity config.Capacity) (*config.Config, []MachineReport) {
	worker := []config.Worker{{Name: "app", Dir: "/tmp", Brief: "b"}}
	pc := config.Machine{Name: "pc", Issues: []config.IssueSource{{Repo: "org/app", Label: "machine:pc"}}, Workers: worker}
	mini := config.Machine{Name: "mini", Issues: []config.IssueSource{{Repo: "org/app", Label: "machine:mini"}}, Workers: worker, Capacity: miniCapacity}
	cfg := &config.Config{Machines: []config.Machine{pc, mini}}

	idle := healthy()
	idle.Checkins = []runlog.Checkin{
		{Time: now.Add(-8 * time.Hour), Worker: "app", Verdict: runlog.VerdictNudge, Sent: true, Issue: 3},
		{Time: now.Add(-7 * time.Hour), Worker: "app", Verdict: runlog.VerdictDone, Reason: "every queue item waits on the owner"},
		{Time: now.Add(-time.Hour), Worker: "app", Verdict: runlog.VerdictDone, Reason: "every queue item waits on the owner"},
	}
	loaded := healthy()
	loaded.Cores, loaded.Load1 = 12, 40
	loaded.Checkins = []runlog.Checkin{handedOver(7)}
	return cfg, []MachineReport{
		Assess(pc, signals, now, idle, nil),
		Assess(mini, signals, now, loaded, []backlog.Issue{issue(7), issue(8), issue(9)}),
	}
}

func TestIdleWorkersBesideAMachineWithUnstartedWorkAndNoRoomAreNamed(t *testing.T) {
	cfg, machines := twoMachines(config.Capacity{MaxLoad: 1.5})
	got := Balance(cfg, machines)
	want := "pc: 1 worker(s) idle, the longest for 7h, while mini has 2 queued issue(s) nobody has started (load 3.33 per core, above 1.50); capacity.release_after on mini gives such work to the fleet"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("balance = %q\nwant %q", got, want)
	}
}

func TestAMachineThatAlreadyReleasesItsWorkIsNotToldTo(t *testing.T) {
	cfg, machines := twoMachines(config.Capacity{MaxLoad: 1.5, ReleaseAfter: "30m"})
	if got := Balance(cfg, machines); len(got) != 1 || strings.Contains(got[0], "release_after") {
		t.Fatalf("balance = %q", got)
	}
}

func TestMachinesOnDifferentRepositoriesAreNotOutOfBalance(t *testing.T) {
	cfg, machines := twoMachines(config.Capacity{MaxLoad: 1.5})
	cfg.Machines[0].Issues[0].Repo = "org/other"
	if got := Balance(cfg, machines); len(got) != 0 {
		t.Fatalf("balance = %q; pc cannot take org/app work", got)
	}
}

func TestAMachineWithNothingQueuedIsNotOutOfBalance(t *testing.T) {
	cfg, machines := twoMachines(config.Capacity{MaxLoad: 1.5})
	machines[1].Issues = machines[1].Issues[:1] // only #7, in hand
	if got := Balance(cfg, machines); len(got) != 0 {
		t.Fatalf("balance = %q", got)
	}
}

func TestTimeHeldForRoomIsIdleTime(t *testing.T) {
	idle := runlog.IdleByCause([]runlog.Checkin{
		{Time: now.Add(-3 * time.Hour), Worker: "app", Verdict: runlog.VerdictHeld},
		{Time: now.Add(-time.Hour), Worker: "app", Verdict: runlog.VerdictNudge, Sent: true},
	}, now.Add(-24*time.Hour), now)
	if idle[runlog.IdleRoom] != 2*time.Hour {
		t.Fatalf("idle = %v, want 2h held for room", idle)
	}
}

func TestDigestNamesAMachineThatIdledWhileAnotherHeldWorkForRoom(t *testing.T) {
	cfg, _ := twoMachines(config.Capacity{MaxLoad: 1.5})
	idle := map[string]map[string]float64{
		"pc":   {runlog.IdleNoWork: 7},
		"mini": {runlog.IdleRoom: 4},
	}
	peaks := map[string]Peak{"mini": {LoadPerCore: 3.33, SwapKB: 6 << 20}}
	d := Digest{Since: now.Add(-24 * time.Hour), Now: now, MachineIdle: idle, Peaks: peaks, Imbalance: imbalance(cfg, idle, peaks)}
	var out strings.Builder
	RenderDigest(&out, d)
	for _, want := range []string{
		"  mini  held for room 4.0h; peak load 3.33 per core, swap 6.0G\n",
		"  pc  no work 7.0h\n",
		"  pc idled 7.0 worker-hours with no work while mini held work 4.0h for room, peak load 3.33 per core, swap 6.0G\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("digest lacks %q:\n%s", want, out.String())
		}
	}
}

func TestTheDigestPeakIsTheHighestOfTheHoursInTheWindow(t *testing.T) {
	since := now.Add(-24 * time.Hour)
	hours := []runlog.LoadPeak{
		{Hour: since.Add(-2 * time.Hour), Load: 80, Cores: 8, SwapKB: 9 << 20}, // before the window
		{Hour: since.Add(3 * time.Hour), Load: 20, Cores: 8, SwapKB: 1 << 20},
		{Hour: since.Add(9 * time.Hour), Load: 12, Cores: 8, SwapKB: 3 << 20},
	}
	got, ok := peak(hours, since)
	if !ok || got.LoadPerCore != 2.5 || got.SwapKB != 3<<20 {
		t.Fatalf("peak = %+v, %v; want 2.5 per core and 3G of swap", got, ok)
	}
	if _, ok := peak(hours[:1], since); ok {
		t.Fatal("an hour before the window counts")
	}
}

func TestASingleMachineWithPeaksIsShownByMachine(t *testing.T) {
	var out strings.Builder
	RenderDigest(&out, Digest{Since: now.Add(-24 * time.Hour), Now: now, Peaks: map[string]Peak{"box": {LoadPerCore: 0.4}}})
	if want := "  box  never idle; peak load 0.40 per core, swap none\n"; !strings.Contains(out.String(), want) {
		t.Fatalf("digest lacks %q:\n%s", want, out.String())
	}
}
