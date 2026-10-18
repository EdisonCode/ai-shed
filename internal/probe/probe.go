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
	"github.com/edisoncode/ai-shed/internal/diraccess"
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
	// Context is, by worker, the estimated tokens its tool loads at session
	// start.
	Context map[string]int `json:"-"`
	// Gates are the agent's recent gate runs, oldest first.
	Gates []runlog.Gate `json:"-"`
	// Deploys are the staging deploys the agent started, oldest first.
	Deploys []runlog.Deploy `json:"-"`
	// Recycle names the workers whose session the owner asked to replace.
	Recycle []string `json:"-"`
	// Marks is what each worker's tool last reported about itself, by worker.
	Marks map[string]runlog.Mark `json:"-"`
	// Tasks are the one-off tasks on the machine, waiting or handed over.
	Tasks []Task `json:"-"`
	// PausedUntil is when the owner's pause of the machine ends, by the
	// machine's clock; zero when it is not paused.
	PausedUntil time.Time `json:"paused_until,omitzero"`
	// Bumps are the issues the owner put first in line, by number, each with
	// the worker it goes to; "" for the workers whose queue it is in.
	Bumps map[int]string `json:"-"`
	// Account is the email Claude Code on the machine is logged in as, from
	// its own state file; "" when it names none. Machines with one account
	// share one usage limit.
	Account string `json:"account,omitempty"`
	// Pace is the agent's last reading of the machine's spend against its
	// share of its account's weekly budget; nil when it is in no account.
	Pace *runlog.Pace `json:"pace,omitempty"`
	// Usage is what each supervised worker's process tree uses now, by
	// worker; nil when the machine has no worker session.
	Usage map[string]Usage `json:"usage,omitempty"`
	// LoadPeaks are the machine's peak load and swap of each recent hour,
	// oldest first.
	LoadPeaks []runlog.LoadPeak `json:"-"`
	// HighLoadSince is when the load reached the core count, by the agent's
	// readings, while it stays there; zero while it is under them.
	HighLoadSince time.Time `json:"high_load_since,omitzero"`
	// DirProblems are the reads of worker directories that failed or
	// blocked, by this shell or by the workers' tmux server. Their Path is
	// empty: the fleet file has it.
	DirProblems []diraccess.Problem `json:"-"`
	DirHost     diraccess.Host      `json:"-"`
	// AgentDirs is the agent's own last read of the worker dirs; zero Time
	// when it has made none.
	AgentDirs runlog.DirCheck `json:"-"`
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
if [ -f "$HOME/.claude.json" ]; then echo "@@account $(grep -o '"emailAddress": *"[^"]*"' "$HOME/.claude.json" | head -n 1 | sed 's/.*"\([^"]*\)"$/\1/')"; fi
echo "@@load $(uptime)"
echo "@@disk $(df -Pk "$HOME" | tail -n 1)"
`

// The window line uses a printable separator: a tmux client with no UTF-8
// locale prints "_" in place of a tab.
const footer = `tmux list-windows -a -F '@@win|#{window_activity}|#{session_name}|#{pane_current_command}|#{window_name}' 2>/dev/null
s="$HOME/` + runlog.StateDir + `"
[ -f "$s/` + runlog.HeartbeatFile + `" ] && echo "@@heartbeat $(cat "$s/` + runlog.HeartbeatFile + `")"
[ -f "$s/` + runlog.PaceFile + `" ] && echo "@@pace $(cat "$s/` + runlog.PaceFile + `")"
[ -f "$s/` + runlog.DirsFile + `" ] && echo "@@agentdirs $(cat "$s/` + runlog.DirsFile + `")"
[ -f "$s/` + runlog.RunsFile + `" ] && tail -n 200 "$s/` + runlog.RunsFile + `" | sed 's/^/@@run /'
[ -f "$s/` + runlog.CheckinsFile + `" ] && tail -n 500 "$s/` + runlog.CheckinsFile + `" | sed 's/^/@@checkin /'
[ -f "$s/` + runlog.GatesFile + `" ] && tail -n 100 "$s/` + runlog.GatesFile + `" | sed 's/^/@@gate /'
[ -f "$s/` + runlog.DeploysFile + `" ] && tail -n 50 "$s/` + runlog.DeploysFile + `" | sed 's/^/@@deploy /'
[ -f "$s/` + runlog.LoadsFile + `" ] && sed 's/^/@@loads /' "$s/` + runlog.LoadsFile + `"
[ -f "$s/` + runlog.HighLoadFile + `" ] && echo "@@highload $(cat "$s/` + runlog.HighLoadFile + `")"
[ -d "$s/` + runlog.RecycleDir + `" ] && ls "$s/` + runlog.RecycleDir + `" | sed 's/^/@@recycle /'
for f in "$s/` + runlog.TasksDir + `"/*/*.md; do [ -f "$f" ] && echo "@@task $(basename "$(dirname "$f")") $(basename "$f" .md)"; done
[ -f "$s/` + runlog.PauseFile + `" ] && echo "@@pause $(cat "$s/` + runlog.PauseFile + `")"
for f in "$s/` + runlog.BumpsDir + `"/*; do [ -f "$f" ] && echo "@@bump $(basename "$f") $(cat "$f")"; done
for f in "$s/` + runlog.MarksDir + `"/*; do [ -f "$f" ] && echo "@@mark $(basename "$f") $(cat "$f")"; done
for f in "$s/` + runlog.ContextDir + `"/*; do [ -f "$f" ] && echo "@@context $(basename "$f") $(cat "$f")"; done
` + usageScript + `echo "@@end"
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
	b.WriteString(diraccess.Script(WorkerDirs(m), diraccess.Options{}))
	b.WriteString(footer)
	return b.String()
}

// WorkerDirs are the directories of a machine's workers.
func WorkerDirs(m config.Machine) []diraccess.Dir {
	var dirs []diraccess.Dir
	for _, w := range m.Workers {
		dirs = append(dirs, diraccess.Dir{Worker: w.Name, Path: w.Dir})
	}
	return dirs
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
	var pausedUntil time.Time
	panes := map[string]int{}
	var procs []psLine
	for _, line := range strings.Split(out, "\n") {
		tag, rest, _ := strings.Cut(line, " ")
		switch tag {
		case "@@now":
			res.Now = epoch(rest)
		case "@@pace":
			if p, ok, err := runlog.ParsePace([]byte(rest)); err == nil && ok {
				res.Pace = &p
			}
		case "@@agentdirs":
			if c, err := runlog.ParseDirCheck([]byte(rest)); err == nil {
				res.AgentDirs = c
			}
		case "@@account":
			res.Account = strings.TrimSpace(rest)
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
		case "@@context":
			if worker, value, ok := strings.Cut(strings.TrimSpace(rest), " "); ok {
				if n, ok := runlog.ParseContext(value); ok {
					if res.Context == nil {
						res.Context = map[string]int{}
					}
					res.Context[worker] = n
				}
			}
		case "@@gate":
			if g, err := runlog.ParseGate(rest); err == nil {
				res.Gates = append(res.Gates, g)
			}
		case "@@deploy":
			if d, err := runlog.ParseDeploy(rest); err == nil {
				res.Deploys = append(res.Deploys, d)
			}
		case "@@loads":
			if p, err := runlog.ParseLoadPeak(rest); err == nil {
				res.LoadPeaks = append(res.LoadPeaks, p)
			}
		case "@@highload":
			if since, err := runlog.ParseHighLoad(rest); err == nil {
				res.HighLoadSince = since
			}
		case "@@recycle":
			res.Recycle = append(res.Recycle, strings.TrimSpace(rest))
		case "@@task":
			if where, id, ok := strings.Cut(strings.TrimSpace(rest), " "); ok {
				res.Tasks = append(res.Tasks, Task{Where: where, ID: id})
			}
		case "@@pause":
			pausedUntil, _ = runlog.ParsePause(rest)
		case "@@bump":
			// A request that was still being written has another name.
			number, worker, _ := strings.Cut(strings.TrimSpace(rest), " ")
			if issue, err := strconv.Atoi(number); err == nil {
				if res.Bumps == nil {
					res.Bumps = map[int]string{}
				}
				res.Bumps[issue] = strings.TrimSpace(worker)
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
		case "@@pane":
			pid, window, _ := strings.Cut(strings.TrimSpace(rest), " ")
			if n, err := strconv.Atoi(pid); err == nil && window != "" {
				panes[window] = n
			}
		case "@@ps":
			if p, ok := parsePS(rest); ok {
				procs = append(procs, p)
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
	// A pause that has run out is no pause.
	if pausedUntil.After(res.Now) {
		res.PausedUntil = pausedUntil
	}
	res.Usage = treeUsage(panes, procs)
	res.DirProblems, res.DirHost = diraccess.Parse(out, nil)
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
