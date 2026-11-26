package status

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/runlog"
)

// repeatedRefusals is how many items in a row the same kind of call must be
// refused on before the owner is told: the standing orders ask for
// something the machine does not allow.
const repeatedRefusals = 3

// Refusal is how often one kind of tool call was refused.
type Refusal struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// refusalsSince counts the refusals since a time by kind, the most first.
func refusalsSince(denials []runlog.Denial, since time.Time) []Refusal {
	counts := map[string]int{}
	for _, d := range denials {
		if d.Time.After(since) {
			counts[d.Kind]++
		}
	}
	var out []Refusal
	for kind, n := range counts {
		out = append(out, Refusal{Kind: kind, Count: n})
	}
	slices.SortFunc(out, func(a, b Refusal) int { return cmp.Or(b.Count-a.Count, cmp.Compare(a.Kind, b.Kind)) })
	return out
}

func refusalWords(rs []Refusal) string {
	words := make([]string, len(rs))
	for i, r := range rs {
		words[i] = fmt.Sprintf("%s (%d)", r.Kind, r.Count)
	}
	return strings.Join(words, ", ")
}

// handBackRefused returns a refused comment on the issue the worker has in
// hand, from the last day: its hand-back cannot reach the owner, and nothing
// else can say so.
func handBackRefused(denials []runlog.Denial, checkins []runlog.Checkin, worker string, now time.Time) (runlog.Denial, bool) {
	issue := runlog.IssueInHand(checkins, worker)
	for _, d := range slices.Backward(denials) {
		if d.Time.Before(now.Add(-24 * time.Hour)) {
			break
		}
		if issue != 0 && d.Kind == "gh issue comment" && d.Issue == issue {
			return d, true
		}
	}
	return runlog.Denial{}, false
}

// repeatedRefusal finds a kind of call refused on each of the last items
// the worker had a refusal on, and names those items.
func repeatedRefusal(denials []runlog.Denial, checkins []runlog.Checkin, worker string) (string, []string) {
	kinds := map[string]map[string]bool{}
	var order []string
	for _, d := range denials {
		item := refusedItem(d, checkins, worker)
		if item == "" {
			continue
		}
		if kinds[item] == nil {
			kinds[item] = map[string]bool{}
		}
		kinds[item][d.Kind] = true
		order = slices.DeleteFunc(order, func(i string) bool { return i == item })
		order = append(order, item)
	}
	if len(order) < repeatedRefusals {
		return "", nil
	}
	last := order[len(order)-repeatedRefusals:]
	for _, kind := range slices.Sorted(maps.Keys(kinds[last[0]])) {
		if !slices.ContainsFunc(last, func(item string) bool { return !kinds[item][kind] }) {
			return kind, last
		}
	}
	return "", nil
}

// refusedItem names what the worker had in hand when a call was refused:
// the issue or the one-off task; "" when it had neither.
func refusedItem(d runlog.Denial, checkins []runlog.Checkin, worker string) string {
	before := checkins
	if i := slices.IndexFunc(checkins, func(c runlog.Checkin) bool { return c.Time.After(d.Time) }); i >= 0 {
		before = checkins[:i]
	}
	if task := runlog.TaskInHand(before, worker); task != "" {
		return "task " + task
	}
	if issue := runlog.IssueInHand(before, worker); issue != 0 {
		return fmt.Sprintf("#%d", issue)
	}
	if d.Issue != 0 {
		return fmt.Sprintf("#%d", d.Issue)
	}
	return ""
}

// assessRefusals sets the worker's refusals of the last day and tells the
// owner what only they can fix: a refused hand-back, or a kind of call the
// machine keeps refusing.
func assessRefusals(st *WorkerStatus, denials []runlog.Denial, checkins []runlog.Checkin, now time.Time, attend func(string, ...any)) {
	st.Refused = refusalsSince(denials, now.Add(-24*time.Hour))
	if d, ok := handBackRefused(denials, checkins, st.Name, now); ok {
		attend("worker %s needs you: its comment on its own issue #%d was refused by its tool's permission check (%s), so its hand-back cannot reach you; allow gh issue comment in the tool's settings on this machine", st.Name, d.Issue, cmp.Or(d.Reason, "no reason given"))
	}
	if kind, items := repeatedRefusal(denials, checkins, st.Name); kind != "" {
		attend("worker %s: %s was refused on %d items in a row (%s): the standing orders ask for something this machine's permission rules do not allow", st.Name, kind, len(items), strings.Join(items, ", "))
	}
}

// collectRefusals totals, by machine, the tool calls refused in the window
// by kind.
func collectRefusals(d *Digest) {
	for _, m := range d.Report.Machines {
		if m.Probe == nil {
			continue
		}
		for _, denials := range m.Probe.Denials {
			for _, r := range refusalsSince(denials, d.Since) {
				if d.Refused == nil {
					d.Refused = map[string]map[string]int{}
				}
				if d.Refused[m.Name] == nil {
					d.Refused[m.Name] = map[string]int{}
				}
				d.Refused[m.Name][r.Kind] += r.Count
			}
		}
	}
}

func renderRefusals(w io.Writer, refused map[string]map[string]int) {
	if len(refused) == 0 {
		return
	}
	fmt.Fprintln(w, "\nRefused by the tools' permission checks:")
	for _, machine := range slices.Sorted(maps.Keys(refused)) {
		var rs []Refusal
		for kind, n := range refused[machine] {
			rs = append(rs, Refusal{Kind: kind, Count: n})
		}
		slices.SortFunc(rs, func(a, b Refusal) int { return cmp.Or(b.Count-a.Count, cmp.Compare(a.Kind, b.Kind)) })
		fmt.Fprintf(w, "  %s: %s\n", machine, refusalWords(rs))
	}
}
