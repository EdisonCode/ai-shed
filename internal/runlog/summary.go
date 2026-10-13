package runlog

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/pricing"
)

// UsageSummary is what a machine's workers spent in a window, and what each
// pull request they handed back cost.
type UsageSummary struct {
	Since   time.Time     `json:"since"`
	Now     time.Time     `json:"now"`
	Workers []WorkerUsage `json:"workers"`
	// PRs are every pull request a cost was posted on, whenever: work merged
	// in a window may have been handed back before it.
	PRs []PRCost `json:"prs"`
	// Kinds splits the window's spend by the kind of item the worker had in
	// hand at each call, so the work no pull request carries is seen.
	Kinds []KindSpend `json:"kinds,omitempty"`
}

// Kinds of work that spend is split into.
const (
	KindPR     = "pull request" // an issue whose work a cost comment was posted for
	KindPlan   = "issue, no pull request"
	KindLook   = "eye check"
	KindVerify = "verification"
	KindTask   = "one-off task"
	KindNoItem = "no item" // nothing in hand: before the first item, at rest, after a hand-back
	KindReview = "reviewer"
)

// KindOrder is the order the kinds are reported in.
var KindOrder = []string{KindPR, KindPlan, KindVerify, KindLook, KindTask, KindNoItem, KindReview}

// KindSpend is the spend of one kind of work at API rates: one value per
// item, or a single value for spend that has no item.
type KindSpend struct {
	Kind  string    `json:"kind"`
	Items []float64 `json:"items"`
}

// WorkerUsage is one worker's tokens in the window, by model.
type WorkerUsage struct {
	Worker   string                 `json:"worker"`
	Models   map[string]ModelTokens `json:"models"`
	Dollars  float64                `json:"dollars"`
	Unpriced []string               `json:"unpriced,omitempty"`
}

// Summarize totals the ledgers in dir for calls in [since, now), priced call
// by call. Every worker with a ledger is counted, also one that is no longer
// in the fleet file.
func Summarize(dir string, since, now time.Time, prices pricing.Table) (UsageSummary, error) {
	s := UsageSummary{Since: since, Now: now, Workers: []WorkerUsage{}}
	ledgers, err := filepath.Glob(filepath.Join(dir, UsageDir, "*.jsonl"))
	if err != nil {
		return s, fmt.Errorf("list usage: %w", err)
	}
	checkins, err := ReadCheckins(dir)
	if err != nil {
		return s, err
	}
	if s.PRs, err = ReadPRCosts(dir); err != nil {
		return s, err
	}
	if s.PRs == nil {
		s.PRs = []PRCost{}
	}
	// An issue a cost comment was posted for was built into a pull request.
	built := map[string]bool{}
	for _, c := range s.PRs {
		built[fmt.Sprintf("%s#%d", c.Worker, c.Issue)] = true
	}
	byItem := map[string]map[string]float64{}
	sort.Strings(ledgers)
	for _, path := range ledgers {
		worker := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		calls, err := ReadUsage(dir, worker)
		if err != nil {
			return s, err
		}
		wu := WorkerUsage{Worker: worker, Models: map[string]ModelTokens{}}
		spans := itemSpans(checkins, worker)
		for _, u := range calls {
			if u.Time.Before(since) || !u.Time.Before(now) {
				continue
			}
			model := ShortModel(u.Model)
			t := wu.Models[model]
			t.Add(u)
			dollars, ok := prices.Cost(u.Model, u.Tokens())
			t.Dollars, t.Priced = t.Dollars+dollars, ok
			wu.Models[model] = t
			if !ok {
				continue
			}
			kind, item := KindReview, ""
			if worker != ReviewerLedger {
				kind, item = spanAt(spans, u.Time, built)
			}
			if byItem[kind] == nil {
				byItem[kind] = map[string]float64{}
			}
			byItem[kind][item] += dollars
		}
		if len(wu.Models) == 0 {
			continue
		}
		for model, t := range wu.Models {
			if t.Priced {
				wu.Dollars += t.Dollars
			} else {
				wu.Unpriced = append(wu.Unpriced, model)
			}
		}
		sort.Strings(wu.Unpriced)
		s.Workers = append(s.Workers, wu)
	}
	for _, kind := range KindOrder {
		items := byItem[kind]
		if len(items) == 0 {
			continue
		}
		keys := make([]string, 0, len(items))
		for k := range items {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ks := KindSpend{Kind: kind}
		for _, k := range keys {
			ks.Items = append(ks.Items, items[k])
		}
		s.Kinds = append(s.Kinds, ks)
	}
	return s, nil
}

// itemSpan is a stretch of a worker's time with one item in hand. An item
// key names the worker and the issue or task.
type itemSpan struct {
	kind, item string
	issue      int
	start, end time.Time
}

// itemSpans splits a worker's check-ins into what it had in hand: an item
// starts when it is handed over and ends at the next hand-over, when the
// worker is found done, or when its session starts again.
func itemSpans(checkins []Checkin, worker string) []itemSpan {
	var spans []itemSpan
	end := func(t time.Time) {
		if n := len(spans); n > 0 && spans[n-1].end.IsZero() {
			spans[n-1].end = t
		}
	}
	open := func() *itemSpan {
		if n := len(spans); n > 0 && spans[n-1].end.IsZero() {
			return &spans[n-1]
		}
		return nil
	}
	for _, c := range checkins {
		if c.Worker != worker {
			continue
		}
		switch {
		case c.Verdict == VerdictStarted || c.Verdict == VerdictRecycled || c.Verdict == VerdictDone || c.Verdict == VerdictReleased:
			end(c.Time)
		case c.Sent && c.Task != "":
			end(c.Time)
			spans = append(spans, itemSpan{kind: KindTask, item: worker + " task " + c.Task, start: c.Time})
		case c.Sent && c.Issue != 0:
			kind := KindPlan
			switch {
			case c.Look:
				kind = KindLook
			case c.Verify:
				kind = KindVerify
			}
			if cur := open(); cur != nil && cur.issue == c.Issue && cur.kind == kind {
				continue
			}
			end(c.Time)
			spans = append(spans, itemSpan{kind: kind, item: fmt.Sprintf("%s#%d", worker, c.Issue), issue: c.Issue, start: c.Time})
		}
	}
	return spans
}

// spanAt names the kind and the item of the call at t. An issue whose work
// was built into a pull request is of that kind, all its passes together.
func spanAt(spans []itemSpan, t time.Time, built map[string]bool) (kind, item string) {
	for _, sp := range spans {
		if t.Before(sp.start) || (!sp.end.IsZero() && !t.Before(sp.end)) {
			continue
		}
		if sp.kind == KindPlan && built[sp.item] {
			return KindPR, sp.item
		}
		return sp.kind, sp.item
	}
	return KindNoItem, ""
}

// ShortModel drops the vendor prefix, a trailing date and a context suffix,
// so claude-sonnet-4-5-20250929 is sonnet-4-5.
func ShortModel(model string) string {
	return strings.TrimPrefix(pricing.Normalize(model), "claude-")
}
