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

// PullLister lists a repository's open pull requests.
type PullLister interface {
	OpenPulls(ctx context.Context, repo string) ([]backlog.OpenPull, error)
}

// staleDraftAfter is how long a draft goes without a commit before it is
// listed: a hold that has quietly ended gets seen.
const staleDraftAfter = 48 * time.Hour

// OpenPulls are the open pull requests of the fleet's repositories that
// nothing else in the report shows: the ones no queue covers, and the
// drafts that have gone quiet.
type OpenPulls struct {
	// Uncovered are the open pull requests whose closing issues are in no
	// machine's queue, or that close no issue. No worker is sent to them.
	Uncovered []LoosePull `json:"uncovered"`
	// StaleDrafts are the drafts with no commit for 48 hours.
	StaleDrafts []LoosePull `json:"stale_drafts"`
	// Problems lists the repositories whose pull requests could not be read.
	Problems []string `json:"problems,omitempty"`
}

// LoosePull is one open pull request the owner should see.
type LoosePull struct {
	Repo   string    `json:"repo"`
	Number int       `json:"number"`
	Title  string    `json:"title"`
	Opened time.Time `json:"opened"`
	CI     string    `json:"ci"`
	Draft  bool      `json:"draft,omitempty"`
	// LastCommit is when its latest commit was made, or when it was opened
	// if GitHub listed no commit.
	LastCommit time.Time `json:"last_commit"`
	// Why says why no queue covers it.
	Why string `json:"why,omitempty"`
	// Hold is what a draft waits for, as its issue's hand-back or its own
	// body says; empty when neither says.
	Hold string `json:"hold,omitempty"`
}

// collectPulls reads the open pull requests of every repository a machine
// takes issues from, and keeps the ones no queue covers and the quiet
// drafts. A repository whose queue could not be read has no pull request
// called uncovered: its issues may well be queued.
func (c Collector) collectPulls(ctx context.Context, cfg *config.Config, machines []MachineReport, now time.Time) *OpenPulls {
	if c.Pulls == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	queued := map[string]bool{}
	unread := map[string]bool{}
	holds := map[string]string{}
	for _, m := range machines {
		for _, repo := range m.unlisted {
			unread[repo] = true
		}
		for _, i := range m.Issues {
			queued[issueKey(i.Repo, i.Number)] = true
			for _, d := range i.Drafts {
				holds[issueKey(i.Repo, d.PR)] = d.WaitsFor
			}
		}
	}
	out := &OpenPulls{Uncovered: []LoosePull{}, StaleDrafts: []LoosePull{}}
	for _, repo := range fleetRepos(cfg) {
		pulls, err := c.Pulls.OpenPulls(ctx, repo)
		if err != nil {
			out.Problems = append(out.Problems, err.Error())
			continue
		}
		for _, p := range pulls {
			if p.Batch {
				continue
			}
			lp := LoosePull{Repo: p.Repo, Number: p.Number, Title: p.Title, Opened: p.CreatedAt, CI: p.CI, Draft: p.Draft, LastCommit: quietSince(p)}
			if why := uncovered(p, queued); why != "" && !unread[repo] {
				lp.Why = why
				out.Uncovered = append(out.Uncovered, lp)
			}
			if p.Draft && lp.LastCommit.Before(now.Add(-staleDraftAfter)) {
				lp.Why = ""
				lp.Hold = holds[issueKey(p.Repo, p.Number)]
				if lp.Hold == "" {
					lp.Hold = backlog.HoldReason(p.Body)
				}
				out.StaleDrafts = append(out.StaleDrafts, lp)
			}
		}
	}
	slices.SortStableFunc(out.Uncovered, func(a, b LoosePull) int { return a.Opened.Compare(b.Opened) })
	slices.SortStableFunc(out.StaleDrafts, func(a, b LoosePull) int { return a.LastCommit.Compare(b.LastCommit) })
	return out
}

// fleetRepos lists every repository a machine takes issues from, in the
// order of the fleet file.
func fleetRepos(cfg *config.Config) []string {
	var repos []string
	for _, m := range cfg.Machines {
		for _, src := range m.AllSources() {
			if !slices.Contains(repos, src.Repo) {
				repos = append(repos, src.Repo)
			}
		}
	}
	return repos
}

// uncovered says why no queue covers the pull request, or "" when one does.
func uncovered(p backlog.OpenPull, queued map[string]bool) string {
	if len(p.Closes) == 0 {
		return "closes no issue"
	}
	var words []string
	for _, d := range p.Closes {
		if queued[issueKey(d.Repo, d.Number)] {
			return ""
		}
		words = append(words, backlog.DepWords([]backlog.Dep{d}, p.Repo))
	}
	if len(words) == 1 {
		return words[0] + " is in no queue"
	}
	return strings.Join(words, ", ") + " are in no queue"
}

// quietSince is when the pull request last changed: its latest commit, or
// its opening when it has none.
func quietSince(p backlog.OpenPull) time.Time {
	if p.LastCommit.IsZero() {
		return p.CreatedAt
	}
	return p.LastCommit
}

// RenderPulls lists the pull requests no queue covers and the quiet drafts,
// then a blank line. Nothing is written when there are none.
func RenderPulls(w io.Writer, p *OpenPulls, now time.Time) {
	if p.empty() {
		return
	}
	renderPulls(w, p, now)
	fmt.Fprintln(w)
}

func (p *OpenPulls) empty() bool {
	return p == nil || len(p.Uncovered)+len(p.StaleDrafts)+len(p.Problems) == 0
}

func renderPulls(w io.Writer, p *OpenPulls, now time.Time) {
	if len(p.Uncovered) > 0 {
		fmt.Fprintf(w, "PRs no queue covers: %d\n", len(p.Uncovered))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, lp := range p.Uncovered {
			ci := "CI " + lp.CI
			if lp.Draft {
				ci = "draft, " + ci
			}
			fmt.Fprintf(tw, "  %s\t%s#%d\t%s\t%s\t%s\n", Short(now.Sub(lp.Opened)), lp.Repo, lp.Number, ci, lp.Why, lp.Title)
		}
		tw.Flush()
	}
	if len(p.StaleDrafts) > 0 {
		fmt.Fprintf(w, "Drafts with no commit for 48h: %d\n", len(p.StaleDrafts))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, lp := range p.StaleDrafts {
			hold := lp.Hold
			if hold == "" {
				hold = "no hold given"
			}
			fmt.Fprintf(tw, "  %s\t%s#%d\t%s\t%s\n", Short(now.Sub(lp.LastCommit)), lp.Repo, lp.Number, hold, lp.Title)
		}
		tw.Flush()
	}
	for _, problem := range p.Problems {
		fmt.Fprintf(w, "! cannot list the open pull requests: %s\n", problem)
	}
}
