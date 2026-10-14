package status

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/looks"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// workerSession is the tmux session that holds supervised workers.
const workerSession = "shed"

// Render writes the report for a terminal: each machine, then the release
// state of each repository the fleet file knows the deploys of.
func Render(w io.Writer, reports []MachineReport, repos []RepoReport) {
	markSharedAccounts(reports)
	total := 0
	for _, r := range reports {
		renderMachine(w, r)
		total += len(r.Attention)
		fmt.Fprintln(w)
	}
	renderAccounts(w, reports)
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
	switch {
	case r.Error != "":
		fmt.Fprintf(w, "%s\n", r.Repo)
	case r.Staging == "":
		// No deploys to tell of: only its merge train.
		fmt.Fprintf(w, "%s\n", r.Repo)
		renderTrain(w, r.Train, r.Now)
	default:
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
		if r.Train != nil {
			renderTrain(w, r.Train, r.Now)
		}
	}
	for _, a := range r.Attention() {
		fmt.Fprintf(w, "  ! %s\n", a)
	}
}

// paceWords says what each pace state does to the hand-outs.
var paceWords = map[string]string{
	runlog.PaceOn:       "on pace",
	runlog.PaceAhead:    "ahead, only priority work is handed out",
	runlog.PaceFarAhead: "far ahead, new work is held on all but a few workers",
}

// renderAccounts totals the pace of each account over its machines. Each
// machine is paced to its own share; the account's limit is what they
// spend together.
func renderAccounts(w io.Writer, reports []MachineReport) {
	type total struct {
		budget, spend, line float64
		machines            []string
	}
	var names []string
	totals := map[string]*total{}
	for _, r := range reports {
		if r.Probe == nil || r.Probe.Pace == nil {
			continue
		}
		p := r.Probe.Pace
		t := totals[p.Account]
		if t == nil {
			t = &total{budget: p.Budget}
			totals[p.Account] = t
			names = append(names, p.Account)
		}
		t.spend += p.Spend
		t.line += p.Line
		t.machines = append(t.machines, r.Name)
	}
	for _, name := range names {
		t := totals[name]
		fmt.Fprintf(w, "Account %s: $%.2f of $%.2f this week, pace line $%.2f (%s)\n\n", name, t.spend, t.budget, t.line, strings.Join(t.machines, ", "))
	}
}

// markSharedAccounts notes, on each machine, the other machines logged in
// to the same account: they share one usage limit.
func markSharedAccounts(reports []MachineReport) {
	byAccount := map[string][]string{}
	for _, r := range reports {
		if r.Probe != nil && r.Probe.Account != "" {
			byAccount[r.Probe.Account] = append(byAccount[r.Probe.Account], r.Name)
		}
	}
	for i, r := range reports {
		if r.Probe == nil || len(byAccount[r.Probe.Account]) < 2 {
			continue
		}
		var others []string
		for _, name := range byAccount[r.Probe.Account] {
			if name != r.Name {
				others = append(others, name)
			}
		}
		reports[i].sharedWith = "  (one usage limit with " + strings.Join(others, ", ") + ")"
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
	totals := map[string]*total{}
	counted := map[string]bool{}
	for _, r := range reports {
		for _, i := range r.Issues {
			if len(i.Asks) == 0 {
				continue
			}
			key := fmt.Sprintf("%s#%d", i.Repo, i.Number)
			if counted[key] {
				continue
			}
			counted[key] = true
			effect := effectOf(i.Asks)
			t := totals[effect]
			if t == nil {
				t = &total{}
				totals[effect] = t
			}
			t.count++
			if at := oldestAsk(i.Asks); !at.IsZero() {
				t.oldest = max(t.oldest, r.Now.Sub(at))
			}
		}
	}
	var words []string
	for _, g := range []struct{ effect, word string }{
		{backlog.EffectReturns, "return to a worker"},
		{backlog.EffectRead, "to read"},
		{"", "to read"},
		{backlog.EffectMerge, "to merge"},
		{backlog.EffectGrace, "in its grace period"},
	} {
		t := totals[g.effect]
		if t == nil {
			continue
		}
		if g.effect == "" && totals[backlog.EffectRead] != nil {
			continue
		}
		word := fmt.Sprintf("%d %s", t.count, g.word)
		if t.oldest > 0 {
			word += fmt.Sprintf(" (oldest %s)", Short(t.oldest))
		}
		words = append(words, word)
	}
	if len(words) == 0 {
		return ""
	}
	return "Waiting on you: " + strings.Join(words, ", ") + "."
}

func renderMachine(w io.Writer, r MachineReport) {
	if r.Probe == nil {
		fmt.Fprintf(w, "%s  %s  UNREACHABLE\n", r.Name, r.Host)
	} else {
		p := r.Probe
		fmt.Fprintf(w, "%s  %s  load %.2f/%d  disk %d%%  agent %s\n", r.Name, r.Host, p.Load1, p.Cores, p.DiskUsedPct, agentWord(r))
		if p.Account != "" {
			fmt.Fprintf(w, "  account  %s%s\n", p.Account, r.sharedWith)
		}
		if pc := p.Pace; pc != nil {
			fmt.Fprintf(w, "  pace     $%.2f of a $%.2f share this week, pace line $%.2f: %s\n", pc.Spend, pc.Share, pc.Line, paceWords[pc.State])
		}
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
			mark := ""
			if slices.Contains(r.Unsupervised, win.Session+":"+win.Name) {
				mark = "\tunsupervised"
			}
			fmt.Fprintf(tw, "    %s:%s\t%s\tactive %s ago%s\n", win.Session, win.Name, windowCommand(r, win), Short(r.Probe.Now.Sub(win.LastActivity)), mark)
		}
		tw.Flush()
		if n := len(r.Unsupervised); n > 0 {
			fmt.Fprintf(w, "  %d unsupervised agent session(s) share this machine's account and CPU with its workers\n", n)
		}
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
		var sizes []string
		for _, wk := range r.Workers {
			if n, ok := r.Probe.Context[wk.Name]; ok {
				sizes = append(sizes, fmt.Sprintf("%s %s", wk.Name, count(n)))
			}
		}
		if len(sizes) > 0 {
			fmt.Fprintf(w, "  context  %s tokens before the first item\n", strings.Join(sizes, ", "))
		}
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
			if i.Held != "" {
				title += " (" + i.Held + ")"
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
	renderGates(w, tw, r)
	if r.Probe != nil && len(r.Probe.Deploys) > 0 {
		fmt.Fprintln(w, "  staging deploys")
		for _, d := range LastDeploys(r.Probe.Deploys) {
			fmt.Fprintf(tw, "    %s\t%s\t%s ago\n", d.Repo, deployWords(d), Short(r.Probe.Now.Sub(d.Start)))
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
	if reset := resetWords(w.Last.Resets, r.Probe.Now); reset != "" {
		words = reset + ", " + words
	}
	if w.Last.Limit != "" {
		words = w.Last.Limit + " limit, " + words
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

// resetWords says when a limit resets, on the machine's clock: "resets Wed
// 18:00 (in 46h)". "" when the time is unknown or past.
func resetWords(resets, now time.Time) string {
	if !resets.After(now) {
		return ""
	}
	return fmt.Sprintf("resets %s (in %s)", resets.In(now.Location()).Format("Mon 15:04"), Short(resets.Sub(now)))
}

func inHand(w WorkerStatus) string {
	if w.Ask {
		return "answers questions"
	}
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

// gatesShown is how many pull requests the gates section lists.
const gatesShown = 5

// renderGates lists the last gate run of each pull request, the latest
// first.
func renderGates(w io.Writer, tw *tabwriter.Writer, r MachineReport) {
	if r.Probe == nil || len(r.Probe.Gates) == 0 {
		return
	}
	var latest []runlog.Gate
	seen := map[string]bool{}
	for i := len(r.Probe.Gates) - 1; i >= 0 && len(latest) < gatesShown; i-- {
		g := r.Probe.Gates[i]
		if key := issueKey(g.Repo, g.PR); !seen[key] {
			seen[key] = true
			latest = append(latest, g)
		}
	}
	fmt.Fprintln(w, "  gates")
	for _, g := range latest {
		result := "passed"
		if !g.Passed() {
			result = fmt.Sprintf("FAILED (exit %d)", g.ExitCode)
		}
		fmt.Fprintf(tw, "    %s#%d\t%s\t%s\t%s ago, took %s\n", g.Repo, g.PR, g.Commit[:min(len(g.Commit), 7)], result, Short(r.Probe.Now.Sub(g.End)), Short(g.End.Sub(g.Start)))
	}
	tw.Flush()
}
