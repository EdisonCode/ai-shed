package status

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/edisoncode/ai-shed/internal/looks"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// workerSession is the tmux session that holds supervised workers.
const workerSession = "shed"

// Render writes the report for a terminal: each machine, then the release
// state of each repository the fleet file knows the deploys of.
func Render(w io.Writer, reports []MachineReport, repos []RepoReport) {
	total := 0
	for _, r := range reports {
		renderMachine(w, r)
		total += len(r.Attention)
		fmt.Fprintln(w)
	}
	for _, r := range repos {
		renderRepo(w, r)
		total += len(r.Attention())
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

// lookWords says each look state the way the report prints it.
var lookWords = map[string]string{looks.Due: "on staging", looks.Awaiting: "waits for staging", looks.Failed: "FAILED", looks.Blocked: "BLOCKED"}

// notDone counts the looks nobody has done, as in "2 not done (1 on staging,
// 1 wait for a staging deploy)". Blocked ones are named when there are any.
func (r RepoReport) notDone() string {
	due, awaiting, blocked := r.count(looks.Due), r.count(looks.Awaiting), r.count(looks.Blocked)
	words := fmt.Sprintf("%d not done (%d on staging, %d wait for a staging deploy", due+awaiting+blocked, due, awaiting)
	if blocked > 0 {
		words += fmt.Sprintf(", %d blocked", blocked)
	}
	return words + ")"
}

// renderRepo writes what is merged and not yet in production, and how much
// of it nobody has looked at: the go or no-go for a production deploy.
func renderRepo(w io.Writer, r RepoReport) {
	if r.Error != "" {
		fmt.Fprintf(w, "%s\n", r.Repo)
	} else {
		fmt.Fprintf(w, "%s  staging %s", r.Repo, clip(r.Staging, 7))
		if r.Production != "" {
			fmt.Fprintf(w, "  production %s", clip(r.Production, 7))
		}
		window := "in the last " + Short(r.Now.Sub(r.Since))
		if r.SinceProduction {
			window = "since production's commit, " + Short(r.Now.Sub(r.Since)) + " ago"
		}
		fmt.Fprintf(w, "\n  %d pull request(s) merged %s\n", r.Merged, window)
		switch {
		case len(r.Looks) == 0:
			fmt.Fprintln(w, "  eye checks: none outstanding")
		default:
			fmt.Fprintf(w, "  eye checks: %s, %d failed\n", r.notDone(), r.count(looks.Failed))
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, l := range r.Looks {
			fmt.Fprintf(tw, "    #%d\tPR #%d\t%s\t%s\t%s\n", l.Number, l.PR, lookWords[l.State], Short(r.Now.Sub(l.Since)), l.Title)
		}
		tw.Flush()
	}
	for _, a := range r.Attention() {
		fmt.Fprintf(w, "  ! %s\n", a)
	}
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
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
		if !p.PausedUntil.IsZero() {
			fmt.Fprintf(w, "  paused   for another %s: nothing is handed out and no worker is nudged\n", Short(p.PausedUntil.Sub(p.Now)))
		}
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
		hooked := slices.ContainsFunc(r.Workers, func(wk WorkerStatus) bool { return wk.State != "" })
		for _, wk := range r.Workers {
			if wk.Last == nil {
				fmt.Fprintf(tw, "    %s\tno check-in yet\n", wk.Name)
				continue
			}
			reason := wk.Last.Reason
			if wk.Last.Verdict == runlog.VerdictLimited {
				reason = limitedWords(r, wk)
			}
			if wk.RecyclePending {
				reason = "[fresh session pending] " + reason
			}
			hand := inHand(wk)
			// The tool's own state has a column only on a machine where
			// some worker's tool reports one.
			if hooked {
				hand += "\t" + toolState(r, wk)
			}
			fmt.Fprintf(tw, "    %s\t%s\t%s\t%s ago\t%s\n", wk.Name, hand, wk.Last.Verdict, Short(r.Probe.Now.Sub(wk.Last.Time)), reason)
		}
		tw.Flush()
	}
	if len(r.OneOff) > 0 {
		fmt.Fprintln(w, "  one-off tasks")
		for _, t := range r.OneOff {
			who := t.Worker
			if who == "" {
				who = "any worker"
			}
			fmt.Fprintf(tw, "    %s\t%s\t%s\n", t.ID, t.State, who)
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
			switch {
			case i.BumpedTo != "":
				title += " (bumped to " + i.BumpedTo + ")"
			case i.Bumped:
				title += " (bumped)"
			}
			if i.Rework != "" {
				title += " (" + i.Rework + ")"
			}
			for _, d := range i.Drafts {
				title += " (" + d.What() + ")"
			}
			fmt.Fprintf(tw, "    %s#%d\t%s\t%s\t%s\n", i.Repo, i.Number, state, i.Worker, title)
		}
		tw.Flush()
		// What a worker chose by itself is told, not asked: no `!` line.
		for _, i := range r.Issues {
			for _, d := range i.Decided {
				fmt.Fprintf(w, "    %s#%d decided without you: %s\n", i.Repo, i.Number, d)
			}
		}
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

// toolState says what the worker's own tool reports and for how long; "-"
// for a tool with no hooks.
func toolState(r MachineReport, w WorkerStatus) string {
	switch {
	case w.State == "":
		return "-"
	case w.StateSince.IsZero():
		return w.State
	}
	return w.State + " " + Short(r.Probe.Now.Sub(w.StateSince))
}

// limitedWords says what the owner needs to know of a worker that is held
// at a usage limit: how long the hold lasts at the most, when the worker last
// printed anything, and how to end the hold. The reason was true when the
// limit was seen. Once the worker has printed since, it is out of date and
// is not shown.
func limitedWords(r MachineReport, w WorkerStatus) string {
	words := "due to be looked at again"
	if left := w.Last.Until.Sub(r.Probe.Now); left > 0 {
		words = "held for up to another " + Short(left)
	}
	stale := false
	for _, win := range r.Probe.Windows {
		if win.Session != workerSession || win.Name != w.Name {
			continue
		}
		if stale = win.LastActivity.After(w.Last.Time); stale {
			words += fmt.Sprintf(", but active %s ago, after the limit was seen", Short(r.Probe.Now.Sub(win.LastActivity)))
		} else {
			words += fmt.Sprintf(", last active %s ago", Short(r.Probe.Now.Sub(win.LastActivity)))
		}
	}
	words += fmt.Sprintf("; if the limit is over: shed resume %s %s", r.Name, w.Name)
	if stale {
		return words
	}
	return words + ". " + w.Last.Reason
}

func inHand(w WorkerStatus) string {
	if w.Task != "" {
		return "task " + w.Task
	}
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
