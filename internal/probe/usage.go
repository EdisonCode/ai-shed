package probe

import (
	"path/filepath"
	"strconv"
	"strings"
)

// WorkerSession is the tmux session the supervised workers run in.
const WorkerSession = "shed"

// usageScript lists the pane of each worker's window and every process on
// the machine, so the CPU and memory of each worker's process tree can be
// added up. ps runs in the C locale: elsewhere it may print "1,5" for 1.5.
const usageScript = `if tmux has-session -t ` + WorkerSession + ` 2>/dev/null; then
tmux list-panes -s -t ` + WorkerSession + ` -F '@@pane #{pane_pid} #{window_name}' 2>/dev/null
LC_ALL=C ps -A -o pid=,ppid=,pcpu=,rss=,command= 2>/dev/null | cut -c1-200 | sed 's/^/@@ps /'
fi
`

// Usage is what a worker's process tree uses now: the tool and everything
// it started, such as a test run.
type Usage struct {
	// CPU is the percent of one core, summed over the tree: 250 is two and
	// a half cores.
	CPU float64 `json:"cpu_pct"`
	// MemKB is the resident memory of the tree.
	MemKB int `json:"mem_kb"`
	// Top is the process of the tree that holds the most memory.
	Top Process `json:"top"`
}

// Process is one process of a worker's tree.
type Process struct {
	PID     int     `json:"pid"`
	CPU     float64 `json:"cpu_pct"`
	MemKB   int     `json:"mem_kb"`
	Command string  `json:"command"`
}

type psLine struct {
	Process
	ppid int
}

// parsePS reads one line of `ps -o pid=,ppid=,pcpu=,rss=,command=`.
func parsePS(line string) (psLine, bool) {
	f := strings.Fields(line)
	if len(f) < 5 {
		return psLine{}, false
	}
	pid, err1 := strconv.Atoi(f[0])
	ppid, err2 := strconv.Atoi(f[1])
	cpu, err3 := strconv.ParseFloat(f[2], 64)
	rss, err4 := strconv.Atoi(f[3])
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return psLine{}, false
	}
	return psLine{Process: Process{PID: pid, CPU: cpu, MemKB: rss, Command: strings.Join(f[4:], " ")}, ppid: ppid}, true
}

// treeUsage adds up the usage of each window's pane and everything under
// it, by window name. A window whose pane is gone is left out.
func treeUsage(panes map[string]int, procs []psLine) map[string]Usage {
	if len(panes) == 0 || len(procs) == 0 {
		return nil
	}
	byPID := map[int]psLine{}
	children := map[int][]int{}
	for _, p := range procs {
		byPID[p.PID] = p
		children[p.ppid] = append(children[p.ppid], p.PID)
	}
	out := map[string]Usage{}
	for window, pane := range panes {
		if _, ok := byPID[pane]; !ok {
			continue
		}
		var u Usage
		seen := map[int]bool{}
		for stack := []int{pane}; len(stack) > 0; {
			pid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[pid] {
				continue
			}
			seen[pid] = true
			p := byPID[pid]
			u.CPU += p.CPU
			u.MemKB += p.MemKB
			if p.MemKB > u.Top.MemKB {
				u.Top = p.Process
			}
			stack = append(stack, children[pid]...)
		}
		out[window] = u
	}
	return out
}

// ShortCommand names a process by its program and, when it is not an
// option, its first argument, as in "node vitest", without directories.
func ShortCommand(command string) string {
	f := strings.Fields(command)
	if len(f) == 0 {
		return ""
	}
	name := filepath.Base(f[0])
	if len(f) > 1 && !strings.HasPrefix(f[1], "-") {
		name += " " + filepath.Base(f[1])
	}
	return name
}
