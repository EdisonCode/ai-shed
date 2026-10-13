package status

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// UsageReader reads what a machine's workers spent since a time.
type UsageReader interface {
	ReadUsage(ctx context.Context, m config.Machine, since time.Time) (runlog.UsageSummary, error)
}

// MachineUsage is one machine's usage in a digest's window.
type MachineUsage struct {
	Machine string              `json:"machine"`
	Summary runlog.UsageSummary `json:"summary"`
}

// PRSpend is what the work on one merged pull request cost, summed over
// every worker and pass that worked on it.
type PRSpend struct {
	Repo    string  `json:"repo"`
	PR      int     `json:"pr"`
	Title   string  `json:"title"`
	Dollars float64 `json:"dollars"`
}

// topPRs is how many of the costliest merged pull requests the digest names.
const topPRs = 5

// collectUsage reads the usage of every machine with supervised workers.
func (c Collector) collectUsage(ctx context.Context, cfg *config.Config, d *Digest) {
	if c.Usage == nil {
		return
	}
	for _, m := range cfg.Machines {
		if len(m.Workers) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		s, err := c.Usage.ReadUsage(ctx, m, d.Since)
		cancel()
		if err != nil {
			d.Problems = append(d.Problems, fmt.Sprintf("%s: usage: %v", m.Name, err))
			continue
		}
		d.Usage = append(d.Usage, MachineUsage{Machine: m.Name, Summary: s})
	}
}

// mergedSpend prices the pull requests merged in the window that a cost was
// posted on, costliest first, and counts the merged ones.
func mergedSpend(d Digest) (spend []PRSpend, merged int) {
	byKey := map[string]*PRSpend{}
	for _, mu := range d.Usage {
		for _, c := range mu.Summary.PRs {
			key := fmt.Sprintf("%s#%d", c.Repo, c.PR)
			if byKey[key] == nil {
				byKey[key] = &PRSpend{Repo: c.Repo, PR: c.PR}
			}
			byKey[key].Dollars += c.Dollars
		}
	}
	for _, rm := range d.Merged {
		for _, p := range rm.Pulls {
			merged++
			if s := byKey[fmt.Sprintf("%s#%d", rm.Repo, p.Number)]; s != nil {
				s.Title = p.Title
				spend = append(spend, *s)
			}
		}
	}
	sort.SliceStable(spend, func(i, j int) bool { return spend[i].Dollars > spend[j].Dollars })
	return spend, merged
}

func median(spend []PRSpend) float64 {
	values := make([]float64, len(spend))
	for i, s := range spend {
		values[i] = s.Dollars
	}
	slices.Sort(values)
	n := len(values)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return values[n/2]
	}
	return (values[n/2-1] + values[n/2]) / 2
}

// renderUsage writes what the fleet spent in the window, at API rates, and
// what the merged work cost.
func renderUsage(w io.Writer, d Digest) {
	if len(d.Usage) > 0 {
		total, byModel := 0.0, map[string]float64{}
		var unpriced []string
		for _, mu := range d.Usage {
			for _, wu := range mu.Summary.Workers {
				total += wu.Dollars
				for model, t := range wu.Models {
					if t.Priced {
						byModel[model] += t.Dollars
					} else if !slices.Contains(unpriced, model) {
						unpriced = append(unpriced, model)
					}
				}
			}
		}
		fmt.Fprintf(w, "\nUsage at API rates: %s (a subscription does not pay this price; it weighs the kinds of token as the published rates do)\n", dollars(total))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, mu := range d.Usage {
			for _, wu := range mu.Summary.Workers {
				models := make([]string, 0, len(wu.Models))
				for model := range wu.Models {
					models = append(models, model)
				}
				sort.Strings(models)
				for _, model := range models {
					t := wu.Models[model]
					cost := "no published rate"
					if t.Priced {
						cost = dollars(t.Dollars)
					}
					fmt.Fprintf(tw, "  %s\t%s\t%s\tin %s\twrite %s\tread %s\tout %s\t%s\n", mu.Machine, workerWord(wu.Worker), model,
						count(t.Input), count(t.CacheWrite), count(t.CacheRead), count(t.Output), cost)
				}
			}
		}
		tw.Flush()
		models := make([]string, 0, len(byModel))
		for model := range byModel {
			models = append(models, model)
		}
		sort.Slice(models, func(i, j int) bool { return byModel[models[i]] > byModel[models[j]] })
		words := make([]string, len(models))
		for i, model := range models {
			words[i] = model + " " + dollars(byModel[model])
		}
		if len(words) > 0 {
			fmt.Fprintf(w, "  by model: %s\n", strings.Join(words, ", "))
		}
		if len(unpriced) > 0 {
			sort.Strings(unpriced)
			fmt.Fprintf(w, "  no published rate, left out of the totals: %s\n", strings.Join(unpriced, ", "))
		}

		spend, merged := mergedSpend(d)
		if merged > 0 {
			fmt.Fprintf(w, "\nMerged pull requests with a cost: %d of %d, median %s\n", len(spend), merged, dollars(median(spend)))
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			for _, s := range spend[:min(len(spend), topPRs)] {
				fmt.Fprintf(tw, "  %s#%d\t%s\t%s\n", s.Repo, s.PR, dollars(s.Dollars), s.Title)
			}
			tw.Flush()
		}
		if !reviewerCounted(d) {
			fmt.Fprintln(w, "  The reviewer's own calls are not counted: its command must print JSON (claude -p --output-format json).")
		}
	}

	var limited []string
	for _, m := range d.Report.Machines {
		if hours := d.LimitHours[m.Name]; hours >= 0.05 {
			limited = append(limited, fmt.Sprintf("%s %.1fh", m.Name, hours))
		}
	}
	if len(limited) > 0 {
		fmt.Fprintf(w, "\nIdle at a usage limit: %s\n", strings.Join(limited, ", "))
	}
}

// workerWord names a ledger in the usage table: the reviewer's is not a
// worker's.
func workerWord(ledger string) string {
	if ledger == runlog.ReviewerLedger {
		return "(reviewer)"
	}
	return ledger
}

func reviewerCounted(d Digest) bool {
	for _, mu := range d.Usage {
		for _, wu := range mu.Summary.Workers {
			if wu.Worker == runlog.ReviewerLedger {
				return true
			}
		}
	}
	return false
}

func dollars(d float64) string {
	return fmt.Sprintf("$%.2f", d)
}

// count renders a token count the way a person scans it: 180000 is 180k.
func count(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 1_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
	default:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1_000_000), ".0") + "M"
	}
}
