package supervisor

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// ownWaitOrder tells a worker that its wait on its own work has lasted too
// long. The terminal of a hung wait looks like the terminal of a healthy one.
const ownWaitOrder = "You have printed nothing for %s, and were last found waiting on work of your own. Check that it still makes progress: its output file, its process. If it is stuck, end it and go on. If your work is handed back, end what still runs and say so."

// Shell is a shell that a worker's tool started and that still runs: a
// background command, a watcher, a leftover.
type Shell struct {
	PID int
	// Age is how long it has run, and CPU the processor time of it and of
	// everything it started.
	Age     time.Duration
	CPU     time.Duration
	Command string
}

// ShellLister lists the shells a worker's tool started.
type ShellLister interface {
	Shells(window string) ([]Shell, error)
}

// shellSeen is a shell's CPU time when it was last listed.
type shellSeen struct {
	cpu time.Duration
	at  time.Time
}

// waitedTooLong reports whether an idle worker that the reviewer found on
// track has printed nothing for longer than the bound since. Such a worker
// says it waits on something of its own, and nothing else would look at it
// again while its queue stays the same.
func (s *Supervisor) waitedTooLong(st *workerState, idle time.Duration) bool {
	bound := s.Settings.OwnWaitMaxOrDefault()
	return bound > 0 && idle >= bound && st.lastVerdict() == runlog.VerdictOnTrack
}

// ownWaitOver tells the worker to check the work it waits on, once in an
// issue. A second wait that outlasts the bound in the same issue is stuck:
// the worker checked once, and a message did not help.
func (s *Supervisor) ownWaitOver(w config.Worker, st *workerState, obs Observation, now time.Time, idle time.Duration) error {
	bound := s.Settings.OwnWaitMaxOrDefault()
	shells := s.listShells(w, st, now)
	c := runlog.Checkin{Time: now, Worker: w.Name, Kind: KindWait, Activity: obs.LastActivity, Queue: st.queueSeen}
	st.lastReview, st.reviewedActivity = now, obs.LastActivity
	if st.waits > 0 {
		c.Verdict = runlog.VerdictStuck
		c.Reason = fmt.Sprintf("it waited on its own work for more than %s a second time in this issue, after it was told to check it", spell(bound))
		if len(shells) > 0 {
			c.Reason += "; still running: " + strings.Join(shells, "; ")
		}
		s.record(st, c)
		return nil
	}
	c.Verdict = runlog.VerdictNudge
	c.Reason = fmt.Sprintf("it was found waiting on its own work and printed nothing for %s", spell(idle.Truncate(time.Minute)))
	c.Message = fmt.Sprintf(ownWaitOrder, spell(idle.Truncate(time.Minute)))
	if len(shells) > 0 {
		c.Message += " Still running: " + strings.Join(shells, "; ") + "."
	}
	if err := s.Terminal.Send(w.Name, c.Message); err != nil {
		return err
	}
	c.Sent = true
	st.waits++
	s.record(st, c)
	return nil
}

// maxShellCommand is how much of a shell's command line is shown.
const maxShellCommand = 120

// listShells describes the shells the worker's tool started, one line each,
// and remembers their CPU time so the next listing shows one that stopped.
func (s *Supervisor) listShells(w config.Worker, st *workerState, now time.Time) []string {
	if s.Shells == nil {
		return nil
	}
	shells, err := s.Shells.Shells(w.Name)
	if err != nil {
		s.Logf("worker %s: list its shells: %v", w.Name, err)
		return nil
	}
	seen := map[int]shellSeen{}
	var lines []string
	for _, sh := range shells {
		line := fmt.Sprintf("pid %d, running %s, CPU %s", sh.PID, shortDuration(sh.Age), shortDuration(sh.CPU))
		prev, ok := st.shellCPU[sh.PID]
		switch {
		case ok && prev.cpu == sh.CPU:
			line += ", unchanged since " + shortDuration(now.Sub(prev.at)) + " ago"
			seen[sh.PID] = prev
		default:
			seen[sh.PID] = shellSeen{cpu: sh.CPU, at: now}
		}
		lines = append(lines, line+": "+clip(strings.Join(strings.Fields(sh.Command), " "), maxShellCommand))
	}
	st.shellCPU = seen
	return lines
}

// shortDuration writes a duration as its largest unit: 3h, 45m, 2s.
func shortDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// parseShells reads `ps -A -o pid=,ppid=,etime=,time=,command=` and returns
// the shells started by the tool that runs in the pane: the pane's shell
// runs the tool, and the tool runs the shells. Their CPU time includes what
// they started.
func parseShells(ps string, panePID int, now time.Time) []Shell {
	type proc struct {
		pid, ppid int
		age, cpu  time.Duration
		command   string
	}
	var procs []proc
	children := map[int][]int{}
	byPID := map[int]proc{}
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		age, err3 := parseClock(f[2])
		cpu, err4 := parseClock(f[3])
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		p := proc{pid: pid, ppid: ppid, age: age, cpu: cpu, command: strings.Join(f[4:], " ")}
		procs = append(procs, p)
		byPID[pid] = p
		children[ppid] = append(children[ppid], pid)
	}
	pane, ok := byPID[panePID]
	if !ok {
		return nil
	}
	// A pane opened with a shell runs the tool as its child; a pane opened
	// with the tool runs it as the pane's own process.
	tools := []int{panePID}
	if isShell(pane.command) {
		tools = children[panePID]
	}
	var subtreeCPU func(pid int) time.Duration
	subtreeCPU = func(pid int) time.Duration {
		total := byPID[pid].cpu
		for _, child := range children[pid] {
			total += subtreeCPU(child)
		}
		return total
	}
	var shells []Shell
	for _, tool := range tools {
		for _, pid := range children[tool] {
			if p := byPID[pid]; isShell(p.command) {
				shells = append(shells, Shell{PID: pid, Age: p.age, CPU: subtreeCPU(pid), Command: p.command})
			}
		}
	}
	return shells
}

// isShell reports whether a command line runs a shell.
func isShell(command string) bool {
	first, _, _ := strings.Cut(command, " ")
	return shells[filepath.Base(first)] || shells[strings.TrimPrefix(filepath.Base(first), "-")]
}

// parseClock reads a ps time: [[dd-]hh:]mm:ss, with an optional fraction
// of a second, as macOS prints CPU time.
func parseClock(s string) (time.Duration, error) {
	var days int
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, fmt.Errorf("ps time %q: %w", s, err)
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("ps time %q: want [[dd-]hh:]mm:ss", s)
	}
	seconds, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil {
		return 0, fmt.Errorf("ps time %q: %w", s, err)
	}
	total := time.Duration(days)*24*time.Hour + time.Duration(seconds*float64(time.Second))
	for i, unit := range []time.Duration{time.Minute, time.Hour} {
		idx := len(parts) - 2 - i
		if idx < 0 {
			break
		}
		n, err := strconv.Atoi(parts[idx])
		if err != nil {
			return 0, fmt.Errorf("ps time %q: %w", s, err)
		}
		total += time.Duration(n) * unit
	}
	return total.Round(time.Millisecond), nil
}
