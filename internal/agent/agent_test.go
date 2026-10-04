package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

var (
	start = time.Date(2026, 10, 4, 3, 0, 0, 0, time.UTC)
	next  = start.Add(24 * time.Hour)
)

// runAndRead runs one task and returns the records it wrote.
func runAndRead(t *testing.T, m config.Machine, task config.Task) []runlog.Record {
	t.Helper()
	dir := t.TempDir()
	if _, err := RunTask(context.Background(), dir, m, task, start, next); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, runlog.RunsFile))
	if err != nil {
		t.Fatal(err)
	}
	var records []runlog.Record
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		rec, err := runlog.ParseLine(line)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, rec)
	}
	return records
}

func TestRunTaskWritesStartThenEnd(t *testing.T) {
	records := runAndRead(t, config.Machine{}, config.Task{Name: "hello", Run: "echo hi"})
	if len(records) != 2 || !records[0].Running() || records[1].Running() {
		t.Fatalf("records = %+v, want a start record then an end record", records)
	}
	end := records[1]
	if end.ExitCode != 0 || end.Output != "hi\n" || !end.Next.Equal(next) {
		t.Fatalf("end record = %+v", end)
	}
}

func TestRunTaskRecordsFailure(t *testing.T) {
	records := runAndRead(t, config.Machine{}, config.Task{Name: "bad", Run: "echo oops >&2; exit 3"})
	if end := records[1]; end.ExitCode != 3 || end.Output != "oops\n" {
		t.Fatalf("end record = %+v", end)
	}
}

func TestRunTaskUsesMachineInit(t *testing.T) {
	m := config.Machine{Init: "export SHED_TEST_VALUE=7"}
	records := runAndRead(t, m, config.Task{Name: "env", Run: `test "$SHED_TEST_VALUE" = 7`})
	if records[1].ExitCode != 0 {
		t.Fatalf("end record = %+v", records[1])
	}
}

func TestRunTaskStopsAtTimeout(t *testing.T) {
	began := time.Now()
	records := runAndRead(t, config.Machine{}, config.Task{Name: "slow", Run: "sleep 30 & sleep 30", Timeout: "200ms"})
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("task ran %s, the timeout did not stop it", took)
	}
	if end := records[1]; end.ExitCode != exitNotRun || !strings.Contains(end.Output, "timeout") {
		t.Fatalf("end record = %+v", end)
	}
}

func TestOutputKeepsOnlyTheTail(t *testing.T) {
	records := runAndRead(t, config.Machine{}, config.Task{Name: "loud", Run: "yes line | head -n 5000; echo the-end"})
	out := records[1].Output
	if len(out) != outputTail || !strings.HasSuffix(out, "the-end\n") {
		t.Fatalf("output is %d bytes, ends %q", len(out), out[len(out)-10:])
	}
}

func TestAgentRejectsUnknownMachine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shed.yaml")
	if err := os.WriteFile(path, []byte("machines:\n  - {name: box, host: local}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := Agent{ConfigPath: path, Machine: "other", StateDir: t.TempDir(), Logf: t.Logf}
	if err := a.Run(context.Background()); err == nil || !strings.Contains(err.Error(), `machine "other"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestAgentWritesHeartbeatAndStopsOnCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shed.yaml")
	if err := os.WriteFile(path, []byte("machines:\n  - {name: box, host: local}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Agent{ConfigPath: path, Machine: "box", StateDir: state, Logf: t.Logf}.Run(ctx) }()

	heartbeat := filepath.Join(state, runlog.HeartbeatFile)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(heartbeat); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat file after 5s")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
}

func TestStopWaitsForATickInProgress(t *testing.T) {
	typing, release := make(chan struct{}), make(chan struct{})
	stop := every(context.Background(), time.Hour, func(context.Context) {
		close(typing) // the supervisor is in the middle of a message
		<-release
	})
	<-typing

	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned while a tick was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return after the tick finished")
	}
}

func TestStopCancelsTheTicksContext(t *testing.T) {
	seen := make(chan error, 1)
	started := make(chan struct{})
	stop := every(context.Background(), time.Hour, func(ctx context.Context) {
		close(started)
		<-ctx.Done() // a model call that ends when the agent stops
		seen <- ctx.Err()
	})
	<-started
	stop()
	if err := <-seen; err == nil {
		t.Fatal("the tick's context was not cancelled")
	}
}

func TestNotifyGivesTheCommandTheMessageAndWhoItIsAbout(t *testing.T) {
	out := filepath.Join(t.TempDir(), "note")
	err := Notify(`printf '%s|%s|%s' "$SHED_MACHINE" "$SHED_WORKER" "$SHED_MESSAGE" > `+out, "box", "app", "box: worker app needs you: a prompt is open")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); string(got) != "box|app|box: worker app needs you: a prompt is open" {
		t.Fatalf("the command saw %q", got)
	}
}

func TestNotifyReportsACommandThatFails(t *testing.T) {
	if err := Notify("echo no route >&2; exit 7", "box", "", "m"); err == nil || !strings.Contains(err.Error(), "no route") {
		t.Fatalf("error = %v", err)
	}
}
