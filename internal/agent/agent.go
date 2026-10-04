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
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	notifyTimeout  = 30 * time.Second
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
// edit keeps the old one in force.
func (a Agent) Run(ctx context.Context) error {
	cfg, m, modified, err := a.read()
	if err != nil {
		return err
	}
	current := a.start(ctx, cfg, m)
	defer func() { current.stop() }()

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
		cfg, m, _, err := a.read()
		if err != nil {
			a.Logf("config changed but was not loaded, the old one stays in force: %v", err)
			continue
		}
		// The old supervisor finishes what it is typing before the new one
		// starts, so two never act on one worker.
		current.stop()
		current = a.start(ctx, cfg, m)
	}
}

// loaded is what one version of the config started.
type loaded struct {
	scheduler *cron.Cron
	// stopSupervisor returns when the supervisor has finished its tick.
	stopSupervisor func()
}

// stop ends the supervisor and the schedule. Task runs in progress finish
// under the old scheduler.
func (l loaded) stop() {
	l.stopSupervisor()
	l.scheduler.Stop()
}

// read loads the config and finds this machine in it.
func (a Agent) read() (*config.Config, config.Machine, time.Time, error) {
	info, err := os.Stat(a.ConfigPath)
	if err != nil {
		return nil, config.Machine{}, time.Time{}, fmt.Errorf("read config: %w", err)
	}
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return nil, config.Machine{}, time.Time{}, err
	}
	m, ok := cfg.Machine(a.Machine)
	if !ok {
		return nil, config.Machine{}, time.Time{}, fmt.Errorf("machine %q is not in %s", a.Machine, a.ConfigPath)
	}
	return cfg, m, info.ModTime(), nil
}

// start runs a scheduler for the machine's tasks and a supervisor for its
// workers.
func (a Agent) start(ctx context.Context, cfg *config.Config, m config.Machine) loaded {
	adoptPath(m)

	// A task that is still running when its next time comes is skipped.
	scheduler := cron.New(cron.WithChain(cron.SkipIfStillRunning(cron.DiscardLogger)))
	notify := a.notifier(cfg, m)
	// failing holds the tasks whose last run failed, so the owner hears of a
	// failure once and not at every run.
	var mu sync.Mutex
	failing := map[string]bool{}
	for _, t := range m.Tasks {
		// The config was validated when it was read.
		schedule, _ := config.ParseSchedule(t.Schedule)
		scheduler.Schedule(schedule, cron.FuncJob(func() {
			start := time.Now()
			rec, err := RunTask(ctx, a.StateDir, m, t, start, schedule.Next(start))
			if err != nil {
				a.Logf("task %s: %v", t.Name, err)
			}
			mu.Lock()
			wasFailing := failing[t.Name]
			failing[t.Name] = rec.ExitCode != 0
			mu.Unlock()
			if rec.ExitCode != 0 && !wasFailing && notify != nil {
				notify("", fmt.Sprintf("%s: task %s failed (exit %d)", m.Name, t.Name, rec.ExitCode))
			}
		}))
	}
	scheduler.Start()

	stopSupervisor := func() {}
	if len(m.Workers) > 0 {
		sup := a.supervisor(cfg, m)
		sup.Notify = notify
		if m.Capacity.Set() {
			sup.Busy = supervisor.SystemBusy(m, runtime.NumCPU())
		}
		stopSupervisor = every(ctx, superviseEvery, sup.Tick)
	}
	a.Logf("machine %s: %d task(s) scheduled, %d worker(s) supervised", m.Name, len(m.Tasks), len(m.Workers))
	return loaded{scheduler: scheduler, stopSupervisor: stopSupervisor}
}

// every calls tick now and then at each interval, on its own goroutine, so a
// slow tick never delays the heartbeat. The returned stop cancels the tick's
// context and waits for a tick in progress to return: a tick is cut short
// only where it can be (a model call), never between the keystrokes of a
// message to a worker.
func every(ctx context.Context, interval time.Duration, tick func(context.Context)) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			tick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func (a Agent) supervisor(cfg *config.Config, m config.Machine) *supervisor.Supervisor {
	return &supervisor.Supervisor{
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
}

// notifier returns the function that tells the owner something, or nil when
// the fleet file has no notify command.
func (a Agent) notifier(cfg *config.Config, m config.Machine) func(worker, message string) {
	if cfg.Defaults.Notify == "" {
		return nil
	}
	command := m.Command(cfg.Defaults.Notify)
	return func(worker, message string) {
		if err := Notify(command, m.Name, worker, message); err != nil {
			a.Logf("notify: %v", err)
		}
	}
}

// Notify runs the owner's notify command. Where the message goes is the
// command's business; shed passes it in SHED_MESSAGE, with SHED_MACHINE and
// SHED_WORKER (empty when the message is not about a worker).
func Notify(command, machine, worker, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = append(os.Environ(), "SHED_MESSAGE="+message, "SHED_MACHINE="+machine, "SHED_WORKER="+worker)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
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

// RunTask runs one task, appends its start and end records to the run log and
// returns the end record. The returned error is about the log; a failing task
// is not an error here.
func RunTask(ctx context.Context, stateDir string, m config.Machine, t config.Task, start, next time.Time) (runlog.Record, error) {
	rec := runlog.Record{Task: t.Name, Start: start, Next: next}
	if err := runlog.Append(stateDir, rec); err != nil {
		return rec, err
	}
	output, exit := execute(ctx, m.Command(t.Run), t.TimeoutOrDefault())
	rec.End, rec.ExitCode, rec.Output = time.Now(), exit, tail(output, outputTail)
	return rec, runlog.Append(stateDir, rec)
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
