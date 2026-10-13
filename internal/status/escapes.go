package status

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

// ClosedLister lists a source's issues closed after a time.
type ClosedLister interface {
	ClosedSince(ctx context.Context, src config.IssueSource, since time.Time) ([]backlog.Issue, error)
}

// TierOutcome is how the verifications of one risk tier went, over the
// issues closed in a window: how many passed, how many of those a fault got
// through, and how many the verifier sent back.
type TierOutcome struct {
	Tier     string   `json:"tier"`
	Passed   int      `json:"passed"`
	Escaped  int      `json:"escaped"`
	SentBack int      `json:"sent_back"`
	Escapes  []string `json:"escapes,omitempty"`
}

// collectEscapes reads the verifications of the issues closed in the window.
// A tier whose passes do not escape can be trusted with more; one whose do
// needs a deeper reading.
func (c Collector) collectEscapes(ctx context.Context, cfg *config.Config, d *Digest) {
	if c.Closed == nil {
		return
	}
	seen := map[string]bool{}
	byTier := map[string]*TierOutcome{}
	for _, m := range cfg.Machines {
		for _, src := range m.AllSources() {
			if r, ok := cfg.Repo(src.Repo); !ok || r.Verify == nil {
				continue
			}
			issues, err := c.Closed.ClosedSince(ctx, src, d.Since)
			if err != nil {
				d.Problems = append(d.Problems, err.Error())
				continue
			}
			for _, i := range issues {
				key := issueKey(i.Repo, i.Number)
				if seen[key] {
					continue
				}
				seen[key] = true
				for _, o := range backlog.VerifyOutcomes(i, cfg.Signals) {
					t := byTier[o.Tier]
					if t == nil {
						t = &TierOutcome{Tier: o.Tier}
						byTier[o.Tier] = t
					}
					switch {
					case !o.Passed:
						t.SentBack++
					case o.Escaped != "":
						t.Passed++
						t.Escaped++
						t.Escapes = append(t.Escapes, fmt.Sprintf("%s: %s after it was verified", key, o.Escaped))
					default:
						t.Passed++
					}
				}
			}
		}
	}
	for _, tier := range []string{config.TierLow, config.TierHigh, ""} {
		if t := byTier[tier]; t != nil {
			d.Verification = append(d.Verification, *t)
		}
	}
}

// renderEscapes writes how each tier's verifications went.
func renderEscapes(w io.Writer, outcomes []TierOutcome) {
	if len(outcomes) == 0 {
		return
	}
	fmt.Fprintln(w, "\nVerification of the issues closed in this window:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	var escapes []string
	for _, t := range outcomes {
		name := t.Tier
		if name == "" {
			name = "(no tier)"
		}
		rate := "-"
		if t.Passed > 0 {
			rate = fmt.Sprintf("%.0f%%", 100*float64(t.Escaped)/float64(t.Passed))
		}
		fmt.Fprintf(tw, "  %s\t%d passed\t%d escaped (%s)\t%d sent back\n", name, t.Passed, t.Escaped, rate, t.SentBack)
		escapes = append(escapes, t.Escapes...)
	}
	tw.Flush()
	slices.Sort(escapes)
	for _, e := range escapes {
		fmt.Fprintf(w, "  escaped: %s\n", e)
	}
}

// DefaultGroup is how the safe defaults of one area went, over the issues
// closed in a window: an area is the issue's overlap label, or its
// repository when it has none.
type DefaultGroup struct {
	Area      string   `json:"area"`
	Held      int      `json:"held"`
	Overruled int      `json:"overruled"`
	Examples  []string `json:"examples,omitempty"`
}

// collectDefaults counts, by area, the hand-backs whose safe defaults held
// and the ones the owner overruled: a worker that took a choice that was
// the owner's. A high rate in one area is the evidence to name it in risky.
func (c Collector) collectDefaults(ctx context.Context, cfg *config.Config, d *Digest) {
	if c.Closed == nil {
		return
	}
	seen := map[string]bool{}
	byArea := map[string]*DefaultGroup{}
	var order []string
	listed := map[string]bool{}
	for _, m := range cfg.Machines {
		for _, src := range m.AllSources() {
			key := fmt.Sprint(src.Repo, src.Label, src.Assignee, src.Query)
			if listed[key] {
				continue
			}
			listed[key] = true
			issues, err := c.Closed.ClosedSince(ctx, src, d.Since)
			if err != nil {
				continue // collectEscapes reports a failed read
			}
			for _, i := range issues {
				ik := issueKey(i.Repo, i.Number)
				if seen[ik] {
					continue
				}
				seen[ik] = true
				area := i.Repo
				if prefix := cfg.Supervisor.OverlapLabels; prefix != "" {
					for _, l := range i.LabelNames() {
						if strings.HasPrefix(l, prefix) {
							area = l
							break
						}
					}
				}
				for _, o := range backlog.DefaultOutcomes(i, cfg.Signals) {
					g := byArea[area]
					if g == nil {
						g = &DefaultGroup{Area: area}
						byArea[area] = g
						order = append(order, area)
					}
					if o.Overruled {
						g.Overruled++
						g.Examples = append(g.Examples, fmt.Sprintf("%s  %s — %s", ik, o.Entries[0], o.Ruling))
					} else {
						g.Held++
					}
				}
			}
		}
	}
	slices.Sort(order)
	for _, area := range order {
		d.Defaults = append(d.Defaults, *byArea[area])
	}
}

// renderDefaults writes how each area's safe defaults went.
func renderDefaults(w io.Writer, groups []DefaultGroup) {
	if len(groups) == 0 {
		return
	}
	fmt.Fprintln(w, "\nSafe defaults on the issues closed in this window:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	var examples []string
	for _, g := range groups {
		total := g.Held + g.Overruled
		fmt.Fprintf(tw, "  %s\t%d held\t%d overruled (%.0f%%)\n", g.Area, g.Held, g.Overruled, 100*float64(g.Overruled)/float64(total))
		examples = append(examples, g.Examples...)
	}
	tw.Flush()
	for _, e := range examples {
		fmt.Fprintf(w, "  overruled: %s\n", e)
	}
}

// collectReleases counts, on the issues closed in the window, the releases
// to the fleet, by the machine and worker that released: work that another
// machine finished because this one could not.
func (c Collector) collectReleases(ctx context.Context, cfg *config.Config, d *Digest) {
	if c.Closed == nil || !slices.ContainsFunc(cfg.Accounts, func(a config.Account) bool { return a.Overflow != "" }) {
		return
	}
	seen := map[string]bool{}
	listed := map[string]bool{}
	for _, m := range cfg.Machines {
		for _, src := range m.AllSources() {
			key := fmt.Sprint(src.Repo, src.Label, src.Assignee, src.Query)
			if listed[key] {
				continue
			}
			listed[key] = true
			issues, err := c.Closed.ClosedSince(ctx, src, d.Since)
			if err != nil {
				continue
			}
			for _, i := range issues {
				if seen[issueKey(i.Repo, i.Number)] {
					continue
				}
				seen[issueKey(i.Repo, i.Number)] = true
				for _, cm := range i.Comments {
					body := strings.TrimSpace(cm.Body)
					if !strings.HasPrefix(body, backlog.ReleasedPhrase) {
						continue
					}
					who, _, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(body, backlog.ReleasedPhrase)), ",")
					if d.Releases == nil {
						d.Releases = map[string]int{}
					}
					d.Releases[strings.TrimSpace(who)]++
				}
			}
		}
	}
}

// renderReleases writes how much work moved between machines.
func renderReleases(w io.Writer, releases map[string]int) {
	if len(releases) == 0 {
		return
	}
	var words []string
	total := 0
	for who, n := range releases {
		words = append(words, fmt.Sprintf("%s %d", who, n))
		total += n
	}
	slices.Sort(words)
	fmt.Fprintf(w, "\nReleased to the fleet and finished elsewhere: %d (%s)\n", total, strings.Join(words, ", "))
}
