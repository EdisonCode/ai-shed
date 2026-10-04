// Package agent runs on a worker machine. It runs the machine's scheduled
// tasks, supervises its workers, and records both for the watcher to read.
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
	"github.com/edisoncode/ai-shed/internal/supervisor"
)

const (
	heartbeatEvery = 30 * time.Second
	superviseEvery = 30 * time.Second
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

// Run schedules the machine's tasks and supervises its workers until ctx is
// cancelled. A config file that changes on disk is loaded again; a broken
// edit keeps the old schedule.
func (a Agent) Run(ctx context.Context) error {
	current, modified, err := a.load(ctx)
	if err != nil {
		return err
	}
	defer func() { <-current.stop().Done() }()

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
		current.stop()
		current = next
	}
}

// loaded is what one version of the config started.
type loaded struct {
	scheduler      *cron.Cron
	stopSupervisor context.CancelFunc
}

func (l loaded) stop() context.Context {
	l.stopSupervisor()
	return l.scheduler.Stop()
}

// load reads the config, starts a scheduler for this machine's tasks and a
// supervisor for its workers.
func (a Agent) load(ctx context.Context) (loaded, time.Time, error) {
	info, err := os.Stat(a.ConfigPath)
	if err != nil {
		return loaded{}, time.Time{}, fmt.Errorf("read config: %w", err)
	}
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return loaded{}, time.Time{}, err
	}
	m, ok := cfg.Machine(a.Machine)
	if !ok {
		return loaded{}, time.Time{}, fmt.Errorf("machine %q is not in %s", a.Machine, a.ConfigPath)
	}
	adoptPath(m)

	// A task that is still running when its next time comes is skipped.
	scheduler := cron.New(cron.WithChain(cron.SkipIfStillRunning(cron.DiscardLogger)))
	for _, t := range m.Tasks {
		schedule, err := config.ParseSchedule(t.Schedule)
		if err != nil {
			return loaded{}, time.Time{}, fmt.Errorf("task %q: %w", t.Name, err)
		}
		scheduler.Schedule(schedule, cron.FuncJob(func() {
			start := time.Now()
			if err := RunTask(ctx, a.StateDir, m, t, start, schedule.Next(start)); err != nil {
				a.Logf("task %s: %v", t.Name, err)
			}
		}))
	}
	scheduler.Start()

	supCtx, stopSupervisor := context.WithCancel(ctx)
	if len(m.Workers) > 0 {
		go a.supervise(supCtx, cfg, m)
	}
	a.Logf("machine %s: %d task(s) scheduled, %d worker(s) supervised", m.Name, len(m.Tasks), len(m.Workers))
	return loaded{scheduler: scheduler, stopSupervisor: stopSupervisor}, info.ModTime(), nil
}

// supervise checks in on the machine's workers until ctx is cancelled. It
// has its own loop so a slow review never delays the heartbeat.
func (a Agent) supervise(ctx context.Context, cfg *config.Config, m config.Machine) {
	sup := &supervisor.Supervisor{
		Machine:        m,
		Settings:       cfg.Supervisor,
		Signals:        cfg.Signals,
		StandingOrders: cfg.Defaults.StandingOrders,
		Terminal:       supervisor.Tmux{},
		Reviewer:       supervisor.CommandReviewer{Command: m.Command(cfg.Supervisor.CommandOrDefault()), Dir: a.StateDir},
		Lister:         backlog.GH{},
		StateDir:       a.StateDir,
		Now:            time.Now,
		Sleep:          time.Sleep,
		Logf:           a.Logf,
	}
	ticker := time.NewTicker(superviseEvery)
	defer ticker.Stop()
	for {
		sup.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// adoptPath gives the agent the PATH that the machine's init line sets, so
// tmux, gh and the reviewer resolve as they do in checks and tasks. A service
// manager starts the agent with a much shorter PATH.
func adoptPath(m config.Machine) {
	if m.Init == "" {
		return
	}
	out, err := exec.Command("sh", "-c", m.Init+"\nprintf '\\n%s' \"$PATH\"").Output()
	if err != nil {
		return
	}
	lines := strings.Split(string(out), "\n")
	if path := lines[len(lines)-1]; path != "" {
		os.Setenv("PATH", path)
	}
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
