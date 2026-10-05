// Package probe asks a machine for its state in one round trip: a generated
// shell script goes in, tagged lines come back.
package probe

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// Result is the state of one machine at Now (the machine's own clock).
type Result struct {
	Now         time.Time     `json:"now"`
	Cores       int           `json:"cores"`
	Load1       float64       `json:"load1"`
	DiskUsedPct int           `json:"disk_used_pct"`
	Checks      []CheckResult `json:"checks"`
	Windows     []Window      `json:"windows"`
	// Heartbeat is zero when no agent has ever run on the machine.
	Heartbeat time.Time       `json:"heartbeat,omitzero"`
	Runs      []runlog.Record `json:"-"`
	// Checkins are the supervisor's recent check-ins, oldest first.
	Checkins []runlog.Checkin `json:"-"`
	// Recycle names the workers whose session the owner asked to replace.
	Recycle []string `json:"-"`
	// Marks is what each worker's tool last reported about itself, by worker.
	Marks map[string]runlog.Mark `json:"-"`
	// Tasks are the one-off tasks on the machine, waiting or handed over.
	Tasks []Task `json:"-"`
}

// Task is a one-off task's file on the machine. Where is the worker it waits
// for, runlog.TaskAny, or runlog.TaskTaken once a worker was handed it.
type Task struct {
	Where string
	ID    string
}

type CheckResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Window is a tmux window. Workers run in tmux, one window each.
type Window struct {
	Session      string    `json:"session"`
	Name         string    `json:"name"`
	Command      string    `json:"command"`
	LastActivity time.Time `json:"last_activity"`
}

const header = `echo "@@now $(date +%s)"
echo "@@cores $(getconf _NPROCESSORS_ONLN 2>/dev/null)"
echo "@@load $(uptime)"
echo "@@disk $(df -Pk "$HOME" | tail -n 1)"
`

// The window line uses a printable separator: a tmux client with no UTF-8
// locale prints "_" in place of a tab.
const footer = `tmux list-windows -a -F '@@win|#{window_activity}|#{session_name}|#{pane_current_command}|#{window_name}' 2>/dev/null
s="$HOME/` + runlog.StateDir + `"
[ -f "$s/` + runlog.HeartbeatFile + `" ] && echo "@@heartbeat $(cat "$s/` + runlog.HeartbeatFile + `")"
[ -f "$s/` + runlog.RunsFile + `" ] && tail -n 200 "$s/` + runlog.RunsFile + `" | sed 's/^/@@run /'
[ -f "$s/` + runlog.CheckinsFile + `" ] && tail -n 100 "$s/` + runlog.CheckinsFile + `" | sed 's/^/@@checkin /'
[ -d "$s/` + runlog.RecycleDir + `" ] && ls "$s/` + runlog.RecycleDir + `" | sed 's/^/@@recycle /'
for f in "$s/` + runlog.TasksDir + `"/*/*.md; do [ -f "$f" ] && echo "@@task $(basename "$(dirname "$f")") $(basename "$f" .md)"; done
for f in "$s/` + runlog.MarksDir + `"/*; do [ -f "$f" ] && echo "@@mark $(basename "$f") $(cat "$f")"; done
echo "@@end"
`

// Script builds the shell script that reports a machine's state. Each check
// runs in its own shell, so a broken check cannot stop the report.
func Script(m config.Machine, checks []config.Check) string {
	var b strings.Builder
	if m.Init != "" {
		b.WriteString(m.Init + "\n")
	}
	b.WriteString(header)
	for i, c := range checks {
		fmt.Fprintf(&b, "out=$(sh -c %s 2>&1 </dev/null); echo \"@@check %d $?\"\n", quote(m.Command(c.Run)), i)
		fmt.Fprintf(&b, "printf '%%s\\n' \"$out\" | head -n 1 | sed 's/^/@@detail %d /'\n", i)
	}
	b.WriteString(footer)
	return b.String()
}

func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Linux prints "load average: 0.52, 0.40, 0.31"; macOS prints
// "load averages: 1.83 1.92 2.01".
var loadRE = regexp.MustCompile(`load averages?: ([0-9]+\.[0-9]+)`)

// Parse reads the output of Script. Output without the end marker was cut
// short and is an error.
func Parse(out string, checks []config.Check) (Result, error) {
	res := Result{Checks: make([]CheckResult, len(checks))}
	for i, c := range checks {
		res.Checks[i].Name = c.Name
	}
	complete := false
	for _, line := range strings.Split(out, "\n") {
		tag, rest, _ := strings.Cut(line, " ")
		switch tag {
		case "@@now":
			res.Now = epoch(rest)
		case "@@cores":
			res.Cores, _ = strconv.Atoi(strings.TrimSpace(rest))
		case "@@load":
			if m := loadRE.FindStringSubmatch(rest); m != nil {
				res.Load1, _ = strconv.ParseFloat(m[1], 64)
			}
		case "@@disk":
			// POSIX df: filesystem, blocks, used, available, capacity, mount.
			if f := strings.Fields(rest); len(f) >= 5 {
				res.DiskUsedPct, _ = strconv.Atoi(strings.TrimSuffix(f[4], "%"))
			}
		case "@@check", "@@detail":
			idx, value, _ := strings.Cut(rest, " ")
			i, err := strconv.Atoi(idx)
			if err != nil || i < 0 || i >= len(checks) {
				return Result{}, fmt.Errorf("probe output names unknown check %q", idx)
			}
			if tag == "@@check" {
				res.Checks[i].OK = strings.TrimSpace(value) == "0"
			} else {
				res.Checks[i].Detail = strings.TrimSpace(value)
			}
		case "@@heartbeat":
			res.Heartbeat = epoch(rest)
		case "@@run":
			// A line the agent was still writing is skipped, not fatal.
			if r, err := runlog.ParseLine(rest); err == nil {
				res.Runs = append(res.Runs, r)
			}
		case "@@checkin":
			if c, err := runlog.ParseCheckin(rest); err == nil {
				res.Checkins = append(res.Checkins, c)
			}
		case "@@recycle":
			res.Recycle = append(res.Recycle, strings.TrimSpace(rest))
		case "@@task":
			if where, id, ok := strings.Cut(strings.TrimSpace(rest), " "); ok {
				res.Tasks = append(res.Tasks, Task{Where: where, ID: id})
			}
		case "@@mark":
			// A mark the hook was still writing is skipped, not fatal.
			worker, line, _ := strings.Cut(rest, " ")
			if m, err := runlog.ParseMark(line); err == nil {
				if res.Marks == nil {
					res.Marks = map[string]runlog.Mark{}
				}
				res.Marks[worker] = m
			}
		case "@@end":
			complete = true
		default:
			if w, ok := parseWindow(line); ok {
				res.Windows = append(res.Windows, w)
			}
		}
	}
	if !complete || res.Now.IsZero() {
		return Result{}, fmt.Errorf("probe output is incomplete (%d bytes)", len(out))
	}
	return res, nil
}

func parseWindow(line string) (Window, bool) {
	f := strings.SplitN(line, "|", 5)
	if len(f) != 5 || f[0] != "@@win" {
		return Window{}, false
	}
	return Window{LastActivity: epoch(f[1]), Session: f[2], Command: f[3], Name: f[4]}, true
}

func epoch(s string) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// Run probes one machine.
func Run(ctx context.Context, r Runner, m config.Machine, checks []config.Check) (Result, error) {
	out, err := r.Run(ctx, m, "sh -s", strings.NewReader(Script(m, checks)))
	if err != nil {
		return Result{}, err
	}
	return Parse(string(out), checks)
}
