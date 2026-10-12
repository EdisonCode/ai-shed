package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
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
	body, ok := formatCost(st.issue, checkins, w.Name, usage)
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
}

// formatCost is the comment for the tokens spent on issue. The last span is
// this pass; earlier spans are earlier passes, such as rework. ok is false
// when nothing was recorded.
func formatCost(issue int, checkins []runlog.Checkin, worker string, usage []runlog.Usage) (string, bool) {
	var spans []span
	for _, sp := range issueSpans(checkins, worker) {
		if sp.issue == issue {
			spans = append(spans, sp)
		}
	}
	if len(spans) == 0 {
		return "", false
	}
	totals := map[string]tokens{}
	var passes []map[string]tokens
	for _, sp := range spans {
		one := map[string]tokens{}
		for _, u := range usage {
			if !inSpan(u.Time, sp) {
				continue
			}
			model := shortModel(u.Model)
			t := one[model]
			t.add(u)
			one[model] = t
			all := totals[model]
			all.add(u)
			totals[model] = all
		}
		passes = append(passes, one)
	}
	if len(totals) == 0 {
		return "", false
	}
	last := passes[len(passes)-1]
	lines := tokenLines("", totals)
	if len(spans) > 1 && len(tokenLines("", last)) > 0 {
		lines = append(tokenLines("This pass", last), tokenLines("Total", totals)...)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n## Cost\n%s\n\n%s\n", backlog.CostMarker, strings.Join(lines, "\n"), costNote)
	return b.String(), true
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

type tokens struct {
	input, output, cacheWrite, cacheRead int
}

func (t *tokens) add(u runlog.Usage) {
	t.input += u.Input
	t.output += u.Output
	t.cacheWrite += u.CacheWrite
	t.cacheRead += u.CacheRead
}

func (t tokens) empty() bool {
	return t.input+t.output+t.cacheWrite+t.cacheRead == 0
}

func tokenLines(prefix string, byModel map[string]tokens) []string {
	models := make([]string, 0, len(byModel))
	for model, t := range byModel {
		if !t.empty() {
			models = append(models, model)
		}
	}
	sort.Strings(models)
	var lines []string
	for _, model := range models {
		lines = append(lines, tokenLine(prefix, model, byModel[model]))
	}
	return lines
}

func tokenLine(prefix, model string, t tokens) string {
	counts := fmt.Sprintf("uncached input %s, cache write %s, cache read %s, output %s",
		spellCount(t.input), spellCount(t.cacheWrite), spellCount(t.cacheRead), spellCount(t.output))
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

// shortModel drops the vendor prefix and a trailing date, so
// claude-sonnet-4-5-20250929 is sonnet-4-5.
func shortModel(model string) string {
	model = strings.TrimPrefix(model, "claude-")
	if i := strings.LastIndex(model, "-"); i > 0 && len(model)-i == 9 && digits(model[i+1:]) {
		model = model[:i]
	}
	return model
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
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
