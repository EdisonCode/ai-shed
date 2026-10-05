package status

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/edisoncode/ai-shed/internal/probe"
)

// workerSession is the tmux session that holds supervised workers.
const workerSession = "shed"

// Render writes the report for a terminal.
func Render(w io.Writer, reports []MachineReport) {
	total := 0
	for _, r := range reports {
		renderMachine(w, r)
		total += len(r.Attention)
		fmt.Fprintln(w)
	}
	if total == 0 {
		fmt.Fprintln(w, "All machines are on track.")
		return
	}
	fmt.Fprintf(w, "%d item(s) need you.\n", total)
	if line := waitingTotals(reports); line != "" {
		fmt.Fprintln(w, line)
	}
}

// waitingTotals counts what waits on the owner by kind, with the age of the
// oldest: the owner's own backlog. An issue in the queue of two machines
// waits once.
func waitingTotals(reports []MachineReport) string {
	type total struct {
		count  int
		oldest time.Duration
	}
	var kinds []string
	totals := map[string]*total{}
	counted := map[string]bool{}
	for _, r := range reports {
		for _, i := range r.Issues {
			for _, a := range i.Asks {
				key := fmt.Sprintf("%s#%d %s", i.Repo, i.Number, a.Name)
				if counted[key] {
					continue
				}
				counted[key] = true
				t := totals[a.Name]
				if t == nil {
					t = &total{}
					totals[a.Name] = t
					kinds = append(kinds, a.Name)
				}
				t.count++
				if !a.Since.IsZero() {
					t.oldest = max(t.oldest, r.Now.Sub(a.Since))
				}
			}
		}
	}
	if len(kinds) == 0 {
		return ""
	}
	words := make([]string, len(kinds))
	for n, kind := range kinds {
		words[n] = fmt.Sprintf("%d %s", totals[kind].count, kind)
		if totals[kind].oldest > 0 {
			words[n] += fmt.Sprintf(" (oldest %s)", Short(totals[kind].oldest))
		}
	}
	return "Waiting on you: " + strings.Join(words, ", ") + "."
}

func renderMachine(w io.Writer, r MachineReport) {
	if r.Probe == nil {
		fmt.Fprintf(w, "%s  %s  UNREACHABLE\n", r.Name, r.Host)
	} else {
		p := r.Probe
		fmt.Fprintf(w, "%s  %s  load %.2f/%d  disk %d%%  agent %s\n", r.Name, r.Host, p.Load1, p.Cores, p.DiskUsedPct, agentWord(r))
		if len(p.Checks) > 0 {
			words := make([]string, len(p.Checks))
			for i, c := range p.Checks {
				words[i] = "ok " + c.Name
				if !c.OK {
					words[i] = "FAIL " + c.Name
				}
			}
			fmt.Fprintf(w, "  checks   %s\n", strings.Join(words, ", "))
		}
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if r.Probe != nil && len(r.Probe.Windows) > 0 {
		fmt.Fprintln(w, "  windows")
		for _, win := range r.Probe.Windows {
			fmt.Fprintf(tw, "    %s:%s\t%s\tactive %s ago\n", win.Session, win.Name, windowCommand(r, win), Short(r.Probe.Now.Sub(win.LastActivity)))
		}
		tw.Flush()
	}
	if len(r.Workers) > 0 && r.Probe != nil {
		fmt.Fprintln(w, "  supervised")
		for _, wk := range r.Workers {
			if wk.Last == nil {
				fmt.Fprintf(tw, "    %s\tno check-in yet\n", wk.Name)
				continue
			}
			reason := wk.Last.Reason
			if wk.RecyclePending {
				reason = "[fresh session pending] " + reason
			}
			fmt.Fprintf(tw, "    %s\t%s\t%s\t%s ago\t%s\n", wk.Name, inHand(wk), wk.Last.Verdict, Short(r.Probe.Now.Sub(wk.Last.Time)), reason)
		}
		tw.Flush()
	}
	if len(r.Issues) > 0 {
		fmt.Fprintln(w, "  issues")
		for _, i := range r.Issues {
			state := i.State
			if len(i.Waiting) > 0 {
				state = strings.Join(i.Waiting, "+")
			}
			title := i.Title
			if i.Rework != "" {
				title += " (" + i.Rework + ")"
			}
			fmt.Fprintf(tw, "    %s#%d\t%s\t%s\t%s\n", i.Repo, i.Number, state, i.Worker, title)
		}
		tw.Flush()
	}
	if len(r.Tasks) > 0 {
		fmt.Fprintln(w, "  tasks")
		for _, t := range r.Tasks {
			fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\n", t.Name, t.Schedule, taskWord(t), lastRun(r, t))
		}
		tw.Flush()
	}
	for _, a := range r.Attention {
		fmt.Fprintf(w, "  ! %s\n", a)
	}
}

func agentWord(r MachineReport) string {
	switch age := r.Probe.Now.Sub(r.Probe.Heartbeat); {
	case r.Probe.Heartbeat.IsZero():
		return "none"
	case age > heartbeatStale:
		return "DOWN"
	default:
		return "ok"
	}
}

func taskWord(t TaskStatus) string {
	word := t.State
	if t.State == TaskFailed {
		word = "FAILED"
	}
	if t.Missed {
		word += ", MISSED"
	}
	return word
}

func lastRun(r MachineReport, t TaskStatus) string {
	if t.Last == nil || r.Probe == nil {
		return ""
	}
	if t.Last.Running() {
		return "started " + Short(r.Probe.Now.Sub(t.Last.Start)) + " ago"
	}
	return fmt.Sprintf("%s ago, took %s", Short(r.Probe.Now.Sub(t.Last.End)), Short(t.Last.End.Sub(t.Last.Start)))
}

func inHand(w WorkerStatus) string {
	if w.Issue == 0 {
		return "no issue"
	}
	return fmt.Sprintf("#%d", w.Issue)
}

// shells are the programs a window shows when no tool runs in it.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true}

// windowCommand names what runs in a window. tmux reports the process name,
// and some tools set theirs to a version number; for a supervised worker's
// window the fleet file knows the tool's real name.
func windowCommand(r MachineReport, win probe.Window) string {
	if win.Session != workerSession || shells[win.Command] {
		return win.Command
	}
	for _, w := range r.Workers {
		if w.Name == win.Name && w.Tool != "" {
			return w.Tool
		}
	}
	return win.Command
}
