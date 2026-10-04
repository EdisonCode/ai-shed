package status

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

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
		if len(p.Windows) > 0 {
			words := make([]string, len(p.Windows))
			for i, win := range p.Windows {
				words[i] = fmt.Sprintf("%s:%s (%s, active %s ago)", win.Session, win.Name, win.Command, Short(p.Now.Sub(win.LastActivity)))
			}
			fmt.Fprintf(w, "  workers  %s\n", strings.Join(words, ", "))
		}
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(r.Issues) > 0 {
		fmt.Fprintln(w, "  issues")
		for _, i := range r.Issues {
			state := i.State
			if len(i.Waiting) > 0 {
				state = strings.Join(i.Waiting, "+")
			}
			fmt.Fprintf(tw, "    %s#%d\t%s\t%s\t%s\n", i.Repo, i.Number, state, i.Worker, i.Title)
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
