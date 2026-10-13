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
	sort.Strings(ledgers)
	for _, path := range ledgers {
		worker := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		calls, err := ReadUsage(dir, worker)
		if err != nil {
			return s, err
		}
		wu := WorkerUsage{Worker: worker, Models: map[string]ModelTokens{}}
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
	if s.PRs, err = ReadPRCosts(dir); err != nil {
		return s, err
	}
	if s.PRs == nil {
		s.PRs = []PRCost{}
	}
	return s, nil
}

// ShortModel drops the vendor prefix, a trailing date and a context suffix,
// so claude-sonnet-4-5-20250929 is sonnet-4-5.
func ShortModel(model string) string {
	return strings.TrimPrefix(pricing.Normalize(model), "claude-")
}
