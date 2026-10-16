package status

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// Balance says where the fleet's work and room are out of step: a machine
// whose workers are idle while another has work nobody has started and no
// room for it. Each machine schedules only its own queue, so only the
// watcher sees both.
func Balance(cfg *config.Config, machines []MachineReport) []string {
	type room struct {
		name    string
		idle    int
		longest time.Duration
		repos   []string
	}
	var spare []room
	for _, r := range machines {
		if r.Probe == nil || len(busyWords(cfg, r)) > 0 {
			continue
		}
		rm := room{name: r.Name, repos: machineRepos(cfg, r.Name)}
		for _, wk := range r.Workers {
			if wk.Last != nil && wk.Last.Verdict == runlog.VerdictDone {
				rm.idle++
				rm.longest = max(rm.longest, r.Probe.Now.Sub(idleSince(r.Probe.Checkins, wk.Name)))
			}
		}
		if rm.idle > 0 {
			spare = append(spare, rm)
		}
	}
	var out []string
	for _, r := range machines {
		if r.Probe == nil {
			continue
		}
		short := busyWords(cfg, r)
		if len(short) == 0 && slices.ContainsFunc(r.Workers, func(wk WorkerStatus) bool {
			return wk.Last != nil && wk.Last.Verdict == runlog.VerdictDone
		}) {
			continue // it has room of its own
		}
		if len(short) == 0 {
			short = []string{"every worker has work"}
		}
		for _, sp := range spare {
			if sp.name == r.Name {
				continue
			}
			queued := 0
			for _, st := range r.Issues {
				if st.State == Queued && slices.Contains(sp.repos, st.Repo) {
					queued++
				}
			}
			if queued == 0 {
				continue
			}
			line := fmt.Sprintf("%s: %d worker(s) idle, the longest for %s, while %s has %d queued issue(s) nobody has started (%s)",
				sp.name, sp.idle, Short(sp.longest), r.Name, queued, strings.Join(short, "; "))
			if m, ok := cfg.Machine(r.Name); ok && m.Capacity.ReleaseAfter == "" {
				line += fmt.Sprintf("; capacity.release_after on %s gives such work to the fleet", r.Name)
			}
			out = append(out, line)
		}
	}
	return out
}

// busyWords says why the machine has no room for new work now: its load is
// above its limit, or hand-overs are held for room. Empty when it has room.
func busyWords(cfg *config.Config, r MachineReport) []string {
	var words []string
	if m, ok := cfg.Machine(r.Name); ok && m.Capacity.MaxLoad > 0 && r.Probe.Cores > 0 {
		if perCore := r.Probe.Load1 / float64(r.Probe.Cores); perCore > m.Capacity.MaxLoad {
			words = append(words, fmt.Sprintf("load %.2f per core, above %.2f", perCore, m.Capacity.MaxLoad))
		}
	}
	held := 0
	for _, wk := range r.Workers {
		if wk.Last != nil && wk.Last.Verdict == runlog.VerdictHeld {
			held++
		}
	}
	if held > 0 {
		words = append(words, fmt.Sprintf("%d hand-over(s) held for room", held))
	}
	return words
}

// machineRepos lists the repositories the machine takes work from.
func machineRepos(cfg *config.Config, name string) []string {
	m, ok := cfg.Machine(name)
	if !ok {
		return nil
	}
	var repos []string
	for _, src := range append(m.AllSources(), backfillSources(m)...) {
		if !slices.Contains(repos, src.Repo) {
			repos = append(repos, src.Repo)
		}
	}
	return repos
}

func backfillSources(m config.Machine) []config.IssueSource {
	var out []config.IssueSource
	for _, w := range m.Workers {
		out = append(out, m.BackfillFor(w)...)
	}
	return out
}

// idleSince returns when the worker's last run of "nothing it can act on"
// check-ins began. Check-ins are in log order.
func idleSince(checkins []runlog.Checkin, worker string) time.Time {
	var since time.Time
	for _, c := range checkins {
		if c.Worker != worker {
			continue
		}
		switch {
		case c.Verdict != runlog.VerdictDone:
			since = time.Time{}
		case since.IsZero():
			since = c.Time
		}
	}
	return since
}

// RenderBalance writes the balance lines, if any.
func RenderBalance(w io.Writer, lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(w, "\nBalance:")
	for _, line := range lines {
		fmt.Fprintf(w, "  %s\n", line)
	}
}

// minImbalance is the least idle time, in worker-hours, that the digest
// counts as one side of an imbalance.
const minImbalance = 0.5

// imbalance names each machine whose workers idled with no work in the
// digest's window while another machine on a repository it takes work from
// held work for room.
func imbalance(cfg *config.Config, idle map[string]map[string]float64) []string {
	var out []string
	for _, spare := range cfg.Machines {
		noWork := idle[spare.Name][runlog.IdleNoWork]
		if noWork < minImbalance {
			continue
		}
		repos := machineRepos(cfg, spare.Name)
		for _, short := range cfg.Machines {
			held := idle[short.Name][runlog.IdleRoom]
			if short.Name == spare.Name || held < minImbalance || !slices.ContainsFunc(machineRepos(cfg, short.Name), func(r string) bool { return slices.Contains(repos, r) }) {
				continue
			}
			out = append(out, fmt.Sprintf("%s idled %.1f worker-hours with no work while %s held work %.1fh for room", spare.Name, noWork, short.Name, held))
		}
	}
	return out
}

// renderMachineIdle writes each machine's idle worker-hours by cause, and
// the imbalances, when the fleet has more than one machine.
func renderMachineIdle(w io.Writer, d Digest) {
	if len(d.MachineIdle) < 2 {
		return
	}
	names := make([]string, 0, len(d.MachineIdle))
	for name := range d.MachineIdle {
		names = append(names, name)
	}
	slices.Sort(names)
	fmt.Fprintln(w, "By machine:")
	for _, name := range names {
		var words []string
		for _, cause := range runlog.IdleCauses {
			if hours := d.MachineIdle[name][cause]; hours >= 0.05 {
				words = append(words, fmt.Sprintf("%s %.1fh", cause, hours))
			}
		}
		if len(words) == 0 {
			words = []string{"never idle"}
		}
		fmt.Fprintf(w, "  %s  %s\n", name, strings.Join(words, ", "))
	}
	for _, line := range d.Imbalance {
		fmt.Fprintf(w, "  %s\n", line)
	}
}
