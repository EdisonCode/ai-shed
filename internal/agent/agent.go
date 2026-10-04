// Package agent runs on a worker machine. It runs the machine's scheduled
// tasks and records each run for the watcher to read.
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

const (
	heartbeatEvery = 30 * time.Second
	// outputTail is how much of a task's output a run record keeps.
	outputTail = 2000
	// exitNotRun is the exit code of a run that timed out or could not start.
	exitNotRun = -1
)

type Agent struct {
	ConfigPath string
	Machine    string
	StateDir   string
	Logf       func(format string, args ...any)
}

// Run schedules the machine's tasks until ctx is cancelled. A config file
// that changes on disk is loaded again; a broken edit keeps the old schedule.
func (a Agent) Run(ctx context.Context) error {
	scheduler, modified, err := a.load(ctx)
	if err != nil {
		return err
	}
	defer func() { <-scheduler.Stop().Done() }()

	ticker := time.NewTicker(heartbeatEvery)
	defer ticker.Stop()
	for {
		if err := a.beat(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		info, err := os.Stat(a.ConfigPath)
		if err != nil || info.ModTime().Equal(modified) {
			continue
		}
		modified = info.ModTime()
		next, _, err := a.load(ctx)
		if err != nil {
			a.Logf("config changed but was not loaded, the old schedule stays: %v", err)
			continue
		}
		// Runs in progress finish under the old scheduler.
		scheduler.Stop()
		scheduler = next
	}
}

// load reads the config and starts a scheduler for this machine's tasks.
func (a Agent) load(ctx context.Context) (*cron.Cron, time.Time, error) {
	info, err := os.Stat(a.ConfigPath)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read config: %w", err)
	}
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return nil, time.Time{}, err
	}
	m, ok := cfg.Machine(a.Machine)
	if !ok {
		return nil, time.Time{}, fmt.Errorf("machine %q is not in %s", a.Machine, a.ConfigPath)
	}
	// A task that is still running when its next time comes is skipped.
	scheduler := cron.New(cron.WithChain(cron.SkipIfStillRunning(cron.DiscardLogger)))
	for _, t := range m.Tasks {
		schedule, err := config.ParseSchedule(t.Schedule)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("task %q: %w", t.Name, err)
		}
		scheduler.Schedule(schedule, cron.FuncJob(func() {
			start := time.Now()
			if err := RunTask(ctx, a.StateDir, m, t, start, schedule.Next(start)); err != nil {
				a.Logf("task %s: %v", t.Name, err)
			}
		}))
	}
	scheduler.Start()
	a.Logf("machine %s: %d task(s) scheduled", m.Name, len(m.Tasks))
	return scheduler, info.ModTime(), nil
}

func (a Agent) beat() error {
	if err := os.MkdirAll(a.StateDir, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	if err := os.WriteFile(filepath.Join(a.StateDir, runlog.HeartbeatFile), []byte(now+"\n"), 0o644); err != nil {
		return fmt.Errorf("write heartbeat: %w", err)
	}
	return nil
}

// RunTask runs one task and appends its start and end records to the run log.
// The returned error is about the log; a failing task is not an error here.
func RunTask(ctx context.Context, stateDir string, m config.Machine, t config.Task, start, next time.Time) error {
	rec := runlog.Record{Task: t.Name, Start: start, Next: next}
	if err := runlog.Append(stateDir, rec); err != nil {
		return err
	}
	output, exit := execute(ctx, m.Command(t.Run), t.TimeoutOrDefault())
	rec.End, rec.ExitCode, rec.Output = time.Now(), exit, tail(output, outputTail)
	return runlog.Append(stateDir, rec)
}

// execute runs a shell script and returns its combined output and exit code.
// The script gets its own process group so a timeout stops its children too.
func execute(ctx context.Context, script string, timeout time.Duration) (string, int) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second

	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return string(out) + fmt.Sprintf("\nshed: stopped after the %s timeout", timeout), exitNotRun
	case ctx.Err() != nil:
		return string(out) + "\nshed: stopped because the agent shut down", exitNotRun
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode()
	default:
		return string(out) + "\nshed: " + err.Error(), exitNotRun
	}
}

func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}
