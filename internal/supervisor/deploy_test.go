package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/looks"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// withDeploys lets the fixture's machine deploy staging, and records the
// deploy commands it runs.
func withDeploys(f *fixture, waiting int, orAfter string) (*fakeLooks, *[]string) {
	reader := withStaging(f)
	reader.staging = "aaa1111"
	f.Repos[0].Staging.Deploy = "gh workflow run deploy.yml -f environment=staging"
	f.Repos[0].Staging.DeployFrom = "box"
	f.Repos[0].Staging.DeployWhen = config.DeployWhen{WaitingLooks: waiting, OrAfter: orAfter}
	var ran []string
	f.RunDeploy = func(_ context.Context, command string) (string, int) {
		ran = append(ran, command)
		return "queued", 0
	}
	return reader, &ran
}

// noOwnWait keeps the bound on a wait for the worker's own work out of tests
// whose worker rests on track for hours: the deploy is what they test.
var noOwnWait = config.Supervisor{OwnWaitMax: "0"}

func awaiting(n int, since time.Time) []looks.Look {
	var out []looks.Look
	for i := range n {
		l := look(20+i, 60+i, looks.Awaiting)
		l.Since = since
		out = append(out, l)
	}
	return out
}

func readDeploys(t *testing.T, f *fixture) []runlog.Deploy {
	t.Helper()
	d, err := runlog.ReadDeploys(f.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestStagingIsDeployedWhenEnoughLooksWaitForIt(t *testing.T) {
	f := started(t, noOwnWait, onTrack)
	reader, ran := withDeploys(f, 3, "4h")
	reader.looks = awaiting(2, f.now)
	f.tick(2 * time.Minute)
	if len(*ran) != 0 {
		t.Fatalf("deployed with 2 looks waiting, want 3")
	}
	reader.looks = awaiting(3, f.now)
	f.tick(5 * time.Minute) // a resting worker's queue is read again
	if len(*ran) != 1 {
		t.Fatalf("ran %q with 3 looks waiting, want one deploy", *ran)
	}
	if d := readDeploys(t, f); len(d) != 1 || d[0].Before != "aaa1111" || d[0].Result != runlog.DeployStarted {
		t.Fatalf("records = %+v", d)
	}
}

func TestStagingIsDeployedWhenTheOldestLookHasWaitedLongEnough(t *testing.T) {
	f := started(t, noOwnWait, onTrack)
	reader, ran := withDeploys(f, 5, "4h")
	reader.looks = awaiting(1, f.now.Add(-3*time.Hour))
	f.tick(2 * time.Minute)
	if len(*ran) != 0 {
		t.Fatalf("deployed after 3h, want 4h")
	}
	f.tick(time.Hour)
	if len(*ran) != 1 {
		t.Fatalf("ran %q after 4h, want one deploy", *ran)
	}
}

func TestNoDeployWhileALookIsInProgressAndNoLookIsHandedOutDuringOne(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(7), onTrack, assign(12))
	reader, ran := withDeploys(f, 1, "4h")
	reader.looks = []looks.Look{look(7, 41, looks.Due)}
	f.tick(2 * time.Minute) // #7's look is handed over
	reader.looks = append(awaiting(2, f.now), look(7, 41, looks.Due))
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute) // on track with the look in hand
	if len(*ran) != 0 {
		t.Fatalf("deployed while a worker looks at staging")
	}

	// The look is done; staging is deployed, and the next look waits for it.
	reader.looks = append(awaiting(2, f.now), look(8, 42, looks.Due))
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute)
	if len(*ran) != 1 {
		t.Fatalf("ran %q once the look was done, want one deploy", *ran)
	}
	if c := f.lastCheckin(t); c.Issue == 8 {
		t.Fatalf("check-in = %+v: a look was handed out while staging deploys", c)
	}

	// Staging serves a new commit: the deploy is over and looks go out again.
	reader.staging = "bbb2222"
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute)
	d := readDeploys(t, f)
	if last := d[len(d)-1]; last.Result != runlog.DeployServed || last.After != "bbb2222" {
		t.Fatalf("records = %+v, want the deploy served", d)
	}
}

func TestAFailedDeployIsToldOnceAndNotRetriedInALoop(t *testing.T) {
	f := started(t, noOwnWait, onTrack)
	reader, ran := withDeploys(f, 1, "4h")
	var notes []string
	f.Notify = func(_, message string) { notes = append(notes, message) }
	f.RunDeploy = func(_ context.Context, command string) (string, int) {
		*ran = append(*ran, command)
		return "HTTP 403: Resource not accessible", 1
	}
	reader.looks = awaiting(2, f.now)
	f.tick(2 * time.Minute)
	f.tick(10 * time.Minute)
	f.tick(time.Hour)
	if len(*ran) != 1 || len(notes) != 1 || !strings.Contains(notes[0], "staging deploy of org/app failed") {
		t.Fatalf("ran %q, notes %q; want one try and one note", *ran, notes)
	}
	f.tick(4 * time.Hour)
	if len(*ran) != 2 {
		t.Fatalf("ran %d times after or_after, want a second try", len(*ran))
	}
}

func TestADeployThatIsNeverServedEndsAndIsTold(t *testing.T) {
	f := started(t, noOwnWait, onTrack)
	reader, _ := withDeploys(f, 1, "4h")
	var notes []string
	f.Notify = func(_, message string) { notes = append(notes, message) }
	reader.looks = awaiting(2, f.now)
	f.tick(2 * time.Minute)
	f.tick(61 * time.Minute)
	d := readDeploys(t, f)
	if last := d[len(d)-1]; last.Result != runlog.DeployNotServed || len(notes) != 1 {
		t.Fatalf("records = %+v, notes = %q", d, notes)
	}
}

func TestOnlyTheNamedMachineDeploys(t *testing.T) {
	f := started(t, noOwnWait, onTrack)
	reader, ran := withDeploys(f, 1, "4h")
	f.Repos[0].Staging.DeployFrom = "other-box"
	reader.looks = awaiting(3, f.now)
	f.tick(2 * time.Minute)
	if len(*ran) != 0 {
		t.Fatalf("a machine that is not deploy_from deployed")
	}
}

func TestAnAgentRestartedDuringADeployGoesOnWaitingForIt(t *testing.T) {
	f := started(t, noOwnWait, onTrack)
	reader, ran := withDeploys(f, 1, "4h")
	if err := runlog.AppendDeploy(f.StateDir, runlog.Deploy{Repo: "org/app", Start: f.now.Add(-10 * time.Minute), Before: "aaa1111", Result: runlog.DeployStarted}); err != nil {
		t.Fatal(err)
	}
	reader.looks = append(awaiting(3, f.now), look(8, 42, looks.Due))
	f.tick(2 * time.Minute)
	if len(*ran) != 0 {
		t.Fatalf("deployed again while the last deploy is not served")
	}
	if c := f.lastCheckin(t); c.Issue == 8 {
		t.Fatalf("a look was handed out while staging deploys")
	}
	reader.staging = "bbb2222"
	f.term.running(f.now.Add(time.Minute))
	f.tick(3 * time.Minute)
	if d := readDeploys(t, f); d[len(d)-1].Result != runlog.DeployServed {
		t.Fatalf("records = %+v", d)
	}
}
