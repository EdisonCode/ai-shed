package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/pricing"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

const costNote = "From the session transcript. A few calls the tool keeps only in memory are missing."

// postCost comments on the pull request of the issue in hand with the tokens
// the worker's sessions spent on it. It posts nothing when the hand-back
// names no pull request, when no calls have been recorded, or when the
// comment would say what it already says. A failure is logged and does not
// stop the check-in: the work does not wait on the comment.
func (s *Supervisor) postCost(ctx context.Context, w config.Worker, st *workerState, queue []QueueItem) {
	if s.Cost == nil || st.issue == 0 {
		return
	}
	item, ok := find(queue, st.issue)
	if !ok || item.PR == 0 {
		return
	}
	if err := runlog.RefreshUsage(s.StateDir, w.Name); err != nil {
		s.Logf("worker %s: usage: %v", w.Name, err)
	}
	usage, err := runlog.ReadUsage(s.StateDir, w.Name)
	if err != nil {
		s.Logf("worker %s: usage: %v", w.Name, err)
		return
	}
	checkins, err := runlog.ReadCheckins(s.StateDir)
	if err != nil {
		s.Logf("worker %s: usage: %v", w.Name, err)
		return
	}
	body, ok := formatCost(st.issue, checkins, w.Name, usage, s.Prices)
	if !ok {
		return
	}
	key := item.Repo + "#" + strconv.Itoa(item.PR)
	if s.costPosted(w.Name, key) == body {
		return
	}
	if err := s.Cost.PostCost(ctx, item.Repo, item.PR, body); err != nil {
		s.Logf("worker %s: cost of %s: %v", w.Name, key, err)
		return
	}
	if err := s.rememberCost(w.Name, key, body); err != nil {
		s.Logf("worker %s: cost of %s: %v", w.Name, key, err)
	}
	totals, _ := issueCost(st.issue, checkins, w.Name, usage, s.Prices)
	record := runlog.PRCost{Repo: item.Repo, PR: item.PR, Issue: st.issue, Worker: w.Name, Updated: s.Now(), Models: totals}
	record.Dollars, record.Unpriced = priced(totals)
	if err := runlog.RecordPRCost(s.StateDir, record); err != nil {
		s.Logf("worker %s: cost of %s: %v", w.Name, key, err)
	}
}

// issueCost totals the tokens the worker spent on issue, by model, and by
// pass: the last pass is this one, earlier ones are earlier passes such as
// rework. With prices, each call is priced on its own, as the API bills it.
func issueCost(issue int, checkins []runlog.Checkin, worker string, usage []runlog.Usage, prices pricing.Table) (totals map[string]runlog.ModelTokens, passes []map[string]runlog.ModelTokens) {
	totals = map[string]runlog.ModelTokens{}
	for _, sp := range issueSpans(checkins, worker) {
		if sp.issue != issue {
			continue
		}
		one := map[string]runlog.ModelTokens{}
		for _, u := range usage {
			if !inSpan(u.Time, sp) {
				continue
			}
			model := runlog.ShortModel(u.Model)
			dollars, isPriced := prices.Cost(u.Model, u.Tokens())
			for _, byModel := range []map[string]runlog.ModelTokens{one, totals} {
				t := byModel[model]
				t.Add(u)
				t.Dollars += dollars
				t.Priced = isPriced
				byModel[model] = t
			}
		}
		passes = append(passes, one)
	}
	return totals, passes
}

// agentCost is what one kind of agent spent on an issue: the worker's own
// session (Agent ""), or one type of sub-agent.
type agentCost struct {
	agent   string
	dollars float64
	models  []string
}

// agentSplit totals the issue's calls by agent, the worker session first
// and then the costliest, and counts the sub-agents the worker started and
// the calls recorded from sub-agents.
func agentSplit(issue int, checkins []runlog.Checkin, worker string, usage []runlog.Usage, prices pricing.Table) (agents []agentCost, dispatches, subCalls int) {
	byAgent := map[string]*agentCost{}
	for _, sp := range issueSpans(checkins, worker) {
		if sp.issue != issue {
			continue
		}
		for _, u := range usage {
			if !inSpan(u.Time, sp) {
				continue
			}
			dispatches += u.Dispatches
			if u.Agent != "" {
				subCalls++
			}
			a := byAgent[u.Agent]
			if a == nil {
				a = &agentCost{agent: u.Agent}
				byAgent[u.Agent] = a
			}
			dollars, _ := prices.Cost(u.Model, u.Tokens())
			a.dollars += dollars
			if model := runlog.ShortModel(u.Model); !slices.Contains(a.models, model) {
				a.models = append(a.models, model)
			}
		}
	}
	for _, a := range byAgent {
		sort.Strings(a.models)
		agents = append(agents, *a)
	}
	sort.SliceStable(agents, func(i, j int) bool {
		if (agents[i].agent == "") != (agents[j].agent == "") {
			return agents[i].agent == ""
		}
		if agents[i].dollars != agents[j].dollars {
			return agents[i].dollars > agents[j].dollars
		}
		return agents[i].agent < agents[j].agent
	})
	return agents, dispatches, subCalls
}

// issueModel is the model the issue was last handed over with; "" for none.
func issueModel(issue int, checkins []runlog.Checkin, worker string) string {
	model := ""
	for _, c := range checkins {
		if c.Worker == worker && c.Sent && c.Issue == issue && c.Model != "" {
			model = c.Model
		}
	}
	return model
}

// offModel names the sub-agents that ran on a model of another family than
// the issue's, as "implementer (opus-5-5)". The worker's own session is left
// out: one that is told the model orchestrates on its own.
func offModel(agents []agentCost, model string) []string {
	want := family(model)
	var off []string
	for _, a := range agents {
		if a.agent == "" {
			continue
		}
		var other []string
		for _, m := range a.models {
			if family(m) != want {
				other = append(other, m)
			}
		}
		if len(other) > 0 {
			off = append(off, fmt.Sprintf("%s (%s)", a.agent, strings.Join(other, ", ")))
		}
	}
	return off
}

// family is a model's line, as in sonnet for claude-sonnet-5-5 or sonnet.
func family(model string) string {
	short := runlog.ShortModel(model)
	f, _, _ := strings.Cut(short, "-")
	return f
}

// agentWords is the comment's line that splits the cost by agent, as in
// "By agent: worker session $2.00 (opus-5-5); Explore $0.50 (haiku-5-5)".
// "" when only the worker's session made calls.
func agentWords(agents []agentCost, withPrices bool) string {
	if len(agents) < 2 {
		return ""
	}
	parts := make([]string, len(agents))
	for i, a := range agents {
		name := a.agent
		if name == "" {
			name = "worker session"
		}
		if withPrices {
			name += " " + spellDollars(a.dollars)
		}
		parts[i] = fmt.Sprintf("%s (%s)", name, strings.Join(a.models, ", "))
	}
	return "By agent: " + strings.Join(parts, "; ")
}

// priced sums the dollars of the models that have a price, and names the
// ones that have none.
func priced(byModel map[string]runlog.ModelTokens) (dollars float64, unpriced []string) {
	for model, t := range byModel {
		if t.Priced {
			dollars += t.Dollars
		} else if t.Billed() > 0 {
			unpriced = append(unpriced, model)
		}
	}
	sort.Strings(unpriced)
	return dollars, unpriced
}

// formatCost is the comment for the tokens spent on issue. ok is false when
// nothing was recorded. Without prices, it names tokens only.
func formatCost(issue int, checkins []runlog.Checkin, worker string, usage []runlog.Usage, prices pricing.Table) (string, bool) {
	totals, passes := issueCost(issue, checkins, worker, usage, prices)
	if len(passes) == 0 || len(tokenLines("", totals, false)) == 0 {
		return "", false
	}
	withPrices := prices != nil
	last := passes[len(passes)-1]
	lines := tokenLines("", totals, withPrices)
	if len(passes) > 1 && len(tokenLines("", last, false)) > 0 {
		lines = append(tokenLines("This pass", last, withPrices), tokenLines("Total", totals, withPrices)...)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n## Cost\n%s\n", backlog.CostMarker, strings.Join(lines, "\n"))
	if withPrices {
		dollars, unpriced := priced(totals)
		fmt.Fprintf(&b, "\nAt API rates: %s", spellDollars(dollars))
		if len(unpriced) > 0 {
			fmt.Fprintf(&b, ", leaving out %s", strings.Join(unpriced, ", "))
		}
		b.WriteString(". A subscription does not pay this price; it weighs the kinds of token as the published rates do.\n")
	}
	read, output := 0, 0
	for _, t := range totals {
		read, output = read+t.CacheRead, output+t.Output
	}
	if output > 0 && read > 0 {
		// Each turn re-reads the context: a high ratio is a long context
		// carried through many turns.
		fmt.Fprintf(&b, "\nCache read to output: %d to 1.\n", (read+output/2)/output)
	}
	agents, dispatches, subCalls := agentSplit(issue, checkins, worker, usage, prices)
	if words := agentWords(agents, withPrices); words != "" {
		fmt.Fprintf(&b, "\n%s\n", words)
	}
	if model := issueModel(issue, checkins, worker); model != "" {
		if off := offModel(agents, model); len(off) > 0 {
			fmt.Fprintf(&b, "\nRan on another model than the issue's (%s): %s.\n", model, strings.Join(off, "; "))
		}
	}
	if dispatches > 0 && subCalls == 0 {
		fmt.Fprintf(&b, "\n%d sub-agent(s) ran and none of their tokens were recorded: this count is partial. The worker's tool needs the SubagentStop hook that runs `shed mark usage`.\n", dispatches)
	}
	fmt.Fprintf(&b, "\n%s\n", costNote)
	return b.String(), true
}

// spellDollars rounds to the cent, or to a tenth of a cent below one.
func spellDollars(d float64) string {
	if d > 0 && d < 0.01 {
		return fmt.Sprintf("$%.3f", d)
	}
	return fmt.Sprintf("$%.2f", d)
}

type span struct {
	issue int
	start time.Time
	end   time.Time // exclusive; zero while the pass is still open
}

// issueSpans splits a worker's check-ins into the passes it spent on each
// issue. A pass starts when an issue is handed over, and again when the
// worker is sent back after it had finished.
func issueSpans(checkins []runlog.Checkin, worker string) []span {
	var spans []span
	cur := 0
	resting := false
	for _, c := range checkins {
		if c.Worker != worker {
			continue
		}
		if c.Verdict == runlog.VerdictDone {
			resting = true
		}
		if !c.Sent || c.Issue == 0 {
			continue
		}
		if c.Issue == cur && !resting {
			continue
		}
		if len(spans) > 0 && spans[len(spans)-1].end.IsZero() {
			spans[len(spans)-1].end = c.Time
		}
		spans = append(spans, span{issue: c.Issue, start: c.Time})
		cur = c.Issue
		resting = false
	}
	return spans
}

func inSpan(t time.Time, sp span) bool {
	if t.Before(sp.start) {
		return false
	}
	return sp.end.IsZero() || t.Before(sp.end)
}

func tokenLines(prefix string, byModel map[string]runlog.ModelTokens, withPrices bool) []string {
	models := make([]string, 0, len(byModel))
	for model, t := range byModel {
		if t.Billed() > 0 {
			models = append(models, model)
		}
	}
	sort.Strings(models)
	var lines []string
	for _, model := range models {
		line := tokenLine(prefix, model, byModel[model])
		switch {
		case !withPrices:
		case byModel[model].Priced:
			line += " — " + spellDollars(byModel[model].Dollars)
		default:
			line += " — no published rate"
		}
		lines = append(lines, line)
	}
	return lines
}

func tokenLine(prefix, model string, t runlog.ModelTokens) string {
	counts := fmt.Sprintf("uncached input %s, cache write %s, cache read %s, output %s",
		spellCount(t.Input), spellCount(t.CacheWrite), spellCount(t.CacheRead), spellCount(t.Output))
	switch {
	case prefix != "" && model != "":
		return prefix + " — " + model + " — " + counts
	case model != "":
		return model + " — " + counts
	case prefix != "":
		return prefix + " — " + counts
	default:
		return counts
	}
}

// spellCount renders a token count the way a person scans it: 180000 is 180k.
func spellCount(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	scale, suffix := 1000.0, "k"
	if n >= 1_000_000 {
		scale, suffix = 1_000_000, "M"
	}
	return strings.TrimSuffix(strconv.FormatFloat(float64(n)/scale, 'f', 1, 64), ".0") + suffix
}

func costState(dir, worker string) string {
	return filepath.Join(dir, "cost", worker+".json")
}

func (s *Supervisor) costPosted(worker, key string) string {
	bodies, err := readCostBodies(s.StateDir, worker)
	if err != nil {
		s.Logf("worker %s: cost: %v", worker, err)
		return ""
	}
	return bodies[key]
}

func (s *Supervisor) rememberCost(worker, key, body string) error {
	bodies, err := readCostBodies(s.StateDir, worker)
	if err != nil {
		return err
	}
	if bodies == nil {
		bodies = map[string]string{}
	}
	bodies[key] = body
	return writeCostBodies(s.StateDir, worker, bodies)
}

func readCostBodies(dir, worker string) (map[string]string, error) {
	data, err := os.ReadFile(costState(dir, worker))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cost comment: %w", err)
	}
	var bodies map[string]string
	if err := json.Unmarshal(data, &bodies); err != nil {
		return nil, fmt.Errorf("read cost comment: %w", err)
	}
	return bodies, nil
}

func writeCostBodies(dir, worker string, bodies map[string]string) error {
	data, err := json.Marshal(bodies)
	if err != nil {
		return fmt.Errorf("write cost comment: %w", err)
	}
	path := costState(dir, worker)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("write cost comment: %w", err)
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// taskCostHeading starts the cost a task's report is given.
const taskCostHeading = "## Cost (from shed)"

// postItemCost gives the work the worker just finished its cost, where no
// pull request carries it: on the issue, for an eye check, a verification
// or a pass with no pull request, and in the report, for a one-off task.
// The comment on an issue carries the worker's own marker, so a builder's
// cost and a looker's on one issue stand side by side.
func (s *Supervisor) postItemCost(ctx context.Context, w config.Worker, st *workerState, queue []QueueItem) {
	if s.Cost == nil || (st.issue == 0 && st.task == "") {
		return
	}
	if item, ok := find(queue, st.issue); st.task == "" && ok && item.PR != 0 && !st.look && !st.verify {
		return // its pull request has the cost comment
	}
	if err := runlog.RefreshUsage(s.StateDir, w.Name); err != nil {
		s.Logf("worker %s: usage: %v", w.Name, err)
	}
	usage, err := runlog.ReadUsage(s.StateDir, w.Name)
	if err != nil {
		s.Logf("worker %s: usage: %v", w.Name, err)
		return
	}
	checkins, err := runlog.ReadCheckins(s.StateDir)
	if err != nil {
		s.Logf("worker %s: usage: %v", w.Name, err)
		return
	}
	if st.task != "" {
		s.writeTaskCost(w, st.task, checkins, usage)
		return
	}
	if st.repo == "" {
		return
	}
	body, ok := formatCost(st.issue, checkins, w.Name, usage, s.Prices)
	if !ok {
		return
	}
	marker := fmt.Sprintf("<!-- shed-cost %s -->", w.Name)
	body = strings.Replace(body, backlog.CostMarker, marker, 1)
	key := fmt.Sprintf("%s#%d issue", st.repo, st.issue)
	if s.costPosted(w.Name, key) == body {
		return
	}
	if err := s.Cost.PostMarked(ctx, st.repo, st.issue, marker, body); err != nil {
		s.Logf("worker %s: cost of %s: %v", w.Name, key, err)
		return
	}
	if err := s.rememberCost(w.Name, key, body); err != nil {
		s.Logf("worker %s: cost of %s: %v", w.Name, key, err)
	}
}

// writeTaskCost adds the tokens the worker spent on a one-off task to the
// end of its report, once.
func (s *Supervisor) writeTaskCost(w config.Worker, task string, checkins []runlog.Checkin, usage []runlog.Usage) {
	path := filepath.Join(expandHome(w.Dir), TaskDir, task+taskReport)
	report, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(report), taskCostHeading) {
		return
	}
	var start time.Time
	for _, c := range checkins {
		if c.Worker == w.Name && c.Sent && c.Task == task {
			start = c.Time
		}
	}
	if start.IsZero() {
		return
	}
	byModel := map[string]runlog.ModelTokens{}
	for _, u := range usage {
		if u.Time.Before(start) {
			continue
		}
		model := runlog.ShortModel(u.Model)
		t := byModel[model]
		t.Add(u)
		dollars, ok := s.Prices.Cost(u.Model, u.Tokens())
		t.Dollars, t.Priced = t.Dollars+dollars, ok
		byModel[model] = t
	}
	lines := tokenLines("", byModel, s.Prices != nil)
	if len(lines) == 0 {
		return
	}
	block := fmt.Sprintf("\n%s\n\n%s\n", taskCostHeading, strings.Join(lines, "\n"))
	if s.Prices != nil {
		dollars, _ := priced(byModel)
		block += fmt.Sprintf("\nAt API rates: %s.\n", spellDollars(dollars))
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		s.Logf("worker %s: cost of task %s: %v", w.Name, task, err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(block); err != nil {
		s.Logf("worker %s: cost of task %s: %v", w.Name, task, err)
	}
}
