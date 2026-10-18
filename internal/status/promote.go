package status

import (
	"context"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/looks"
)

// PromotionLine is one pull request merged since production.
type PromotionLine struct {
	PR        int       `json:"pr"`
	Title     string    `json:"title"`
	Issue     int       `json:"issue,omitempty"`
	Tier      string    `json:"tier,omitempty"`
	Verified  string    `json:"verified,omitempty"`
	ByShed    bool      `json:"by_shed"`
	OnStaging bool      `json:"on_staging"`
	MergedAt  time.Time `json:"merged_at"`
}

// Promotion is what a production deploy of a repository would carry: what
// shed merged on the verifier's word, what the owner merged, and whether
// the looks on staging are done.
type Promotion struct {
	Repo       string          `json:"repo"`
	Staging    string          `json:"staging"`
	Production string          `json:"production,omitempty"`
	Since      time.Time       `json:"since"`
	ByShed     []PromotionLine `json:"by_shed"`
	ByOwner    []PromotionLine `json:"by_owner"`
	LooksOpen  int             `json:"looks_open"`
	LooksBad   int             `json:"looks_failed_or_blocked"`
	Problems   []string        `json:"problems,omitempty"`
}

// Blocked reports whether something should stop the promotion: work that
// staging does not serve yet, a look not done, failed or blocked.
func (p Promotion) Blocked() bool {
	if p.LooksOpen > 0 || p.LooksBad > 0 || len(p.Problems) > 0 {
		return true
	}
	return slices.ContainsFunc(append(slices.Clone(p.ByShed), p.ByOwner...), func(l PromotionLine) bool { return !l.OnStaging })
}

// StagingChecker says whether staging's commit has a merge commit in its
// history.
type StagingChecker interface {
	Contains(ctx context.Context, repo, head, commit string) (bool, error)
}

// Promote gathers the brief for promoting the repository to production.
func (c Collector) Promote(ctx context.Context, cfg *config.Config, name string, merged MergedLister, staging StagingChecker) (Promotion, error) {
	repo, ok := cfg.Repo(name)
	if !ok || !repo.Deployed() {
		return Promotion{}, fmt.Errorf("%s has no staging.commit under repos: shed cannot tell what staging serves", name)
	}
	var sources []config.IssueSource
	listed := map[string]bool{}
	for _, m := range cfg.Machines {
		for _, src := range m.AllSources() {
			if key := fmt.Sprint(src.Repo, src.Label, src.Assignee, src.Query); src.Repo == name && !listed[key] {
				listed[key] = true
				sources = append(sources, src)
			}
		}
	}
	var open []backlog.Issue
	for _, src := range sources {
		issues, err := c.Lister.List(ctx, src)
		if err != nil {
			return Promotion{}, err
		}
		open = append(open, issues...)
	}
	report, err := c.Looks.Read(ctx, repo, sources, open, cfg.Signals)
	if err != nil {
		return Promotion{}, err
	}
	p := Promotion{Repo: name, Staging: report.Staging, Production: report.Production, Since: report.Since}
	for _, l := range report.Looks {
		switch l.State {
		case looks.Failed, looks.Blocked:
			p.LooksBad++
		default:
			p.LooksOpen++
		}
	}
	pulls, err := merged.MergedSince(ctx, name, report.Since)
	if err != nil {
		return Promotion{}, err
	}
	issues := slices.Clone(open)
	if c.Closed != nil {
		for _, src := range sources {
			closed, err := c.Closed.ClosedSince(ctx, src, report.Since)
			if err != nil {
				p.Problems = append(p.Problems, err.Error())
				continue
			}
			issues = append(issues, closed...)
		}
	}
	byPR := map[int]backlog.Issue{}
	for _, i := range issues {
		for _, n := range backlog.HandedPRs(i, cfg.Signals) {
			byPR[n] = i
		}
	}
	var ordered []looks.Pull
	for _, pull := range pulls {
		ordered = append(ordered, pull)
	}
	slices.SortFunc(ordered, func(a, b looks.Pull) int { return a.MergedAt.Compare(b.MergedAt) })
	for _, pull := range ordered {
		// The window is by time, to the second; production's own merge, or
		// one in the same second, is already in production.
		if p.Production != "" {
			if in, err := staging.Contains(ctx, name, p.Production, pull.Commit); err == nil && in {
				continue
			}
		}
		line := PromotionLine{PR: pull.Number, Title: pull.Title, MergedAt: pull.MergedAt}
		if i, ok := byPR[pull.Number]; ok {
			line.Issue = i.Number
			line.Tier, line.Verified = backlog.LastVerified(i)
			line.ByShed = backlog.QueuedByShed(i, pull.Number)
		}
		on, err := staging.Contains(ctx, name, report.Staging, pull.Commit)
		if err != nil {
			p.Problems = append(p.Problems, fmt.Sprintf("#%d: %v", pull.Number, err))
		}
		line.OnStaging = on
		if line.ByShed {
			p.ByShed = append(p.ByShed, line)
		} else {
			p.ByOwner = append(p.ByOwner, line)
		}
	}
	return p, nil
}

// RenderPromotion writes the brief, go or no-go first.
func RenderPromotion(w io.Writer, p Promotion, now time.Time) {
	total := len(p.ByShed) + len(p.ByOwner)
	fmt.Fprintf(w, "%s: %d pull request(s) merged since production", p.Repo, total)
	if p.Production != "" {
		fmt.Fprintf(w, " (production %s, %s ago; staging %s)", short(p.Production), Short(now.Sub(p.Since)), short(p.Staging))
	}
	fmt.Fprintln(w)
	if p.Blocked() {
		fmt.Fprintln(w, "No go yet: see below.")
	} else {
		fmt.Fprintln(w, "Go: everything merged is on staging and every look passed.")
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	write := func(title string, lines []PromotionLine) {
		if len(lines) == 0 {
			return
		}
		tw.Flush()
		fmt.Fprintf(w, "\n%s: %d\n", title, len(lines))
		for _, l := range lines {
			where := "on staging"
			if !l.OnStaging {
				where = "NOT ON STAGING"
			}
			issue, tier := "-", l.Tier
			if l.Issue != 0 {
				issue = fmt.Sprintf("#%d", l.Issue)
			}
			if tier == "" {
				tier = "unverified"
			}
			fmt.Fprintf(tw, "  #%d\t%s\t%s\t%s\t%s\n", l.PR, issue, where, tier, l.Title)
			if l.ByShed && l.Verified != "" {
				fmt.Fprintf(tw, "  \t\t\t\tverified: %s\n", l.Verified)
			}
		}
	}
	// A high-risk merge shed queued is read first: it is the one the
	// approval at promotion is most for.
	var high, low []PromotionLine
	for _, l := range p.ByShed {
		if l.Tier == config.TierHigh {
			high = append(high, l)
		} else {
			low = append(low, l)
		}
	}
	write("Queued by shed on the verifier's word, high risk", high)
	write("Queued by shed on the verifier's word, low risk", low)
	write("Merged by you", p.ByOwner)
	tw.Flush()
	fmt.Fprintf(w, "\nLooks on staging: %d not done, %d failed or blocked\n", p.LooksOpen, p.LooksBad)
	for _, pr := range p.Problems {
		fmt.Fprintf(w, "! %s\n", pr)
	}
}

func short(commit string) string { return commit[:min(len(commit), 7)] }
