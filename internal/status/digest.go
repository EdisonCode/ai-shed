package status

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/looks"
)

// MergedLister lists a repository's pull requests merged after a time.
type MergedLister interface {
	MergedSince(ctx context.Context, repo string, since time.Time) (map[int]looks.Pull, error)
}

// Digest is what happened in a window and what it left for the owner: what
// merged, how the eye checks went, and what waits on them now.
type Digest struct {
	Since time.Time `json:"since"`
	Now   time.Time `json:"now"`
	// Merged lists each repository's pull requests merged in the window.
	Merged []RepoMerged `json:"merged"`
	// Report is the state now: machines, and each repository's release state.
	Report Report `json:"report"`
	// Problems lists what could not be read.
	Problems []string `json:"problems,omitempty"`
}

// RepoMerged is one repository's pull requests merged in the window, oldest
// first.
type RepoMerged struct {
	Repo  string       `json:"repo"`
	Pulls []looks.Pull `json:"pulls"`
}

// Digest collects the report and the pull requests merged since a time, for
// every repository a machine takes issues from.
func (c Collector) Digest(ctx context.Context, cfg *config.Config, merged MergedLister, since, now time.Time) Digest {
	d := Digest{Since: since, Now: now, Report: c.CollectAll(ctx, cfg), Merged: []RepoMerged{}}
	var repos []string
	for _, m := range cfg.Machines {
		for _, src := range m.AllSources() {
			if !slices.Contains(repos, src.Repo) {
				repos = append(repos, src.Repo)
			}
		}
	}
	for _, repo := range repos {
		pulls, err := merged.MergedSince(ctx, repo, since)
		if err != nil {
			d.Problems = append(d.Problems, err.Error())
			continue
		}
		rm := RepoMerged{Repo: repo, Pulls: []looks.Pull{}}
		for _, p := range pulls {
			rm.Pulls = append(rm.Pulls, p)
		}
		slices.SortFunc(rm.Pulls, func(a, b looks.Pull) int { return a.MergedAt.Compare(b.MergedAt) })
		d.Merged = append(d.Merged, rm)
	}
	return d
}

// waitingItem is one thing an issue asks of the owner.
type waitingItem struct {
	issue IssueStatus
	what  string
	since time.Time
}

// RenderDigest writes the digest for a terminal or a message.
func RenderDigest(w io.Writer, d Digest) {
	fmt.Fprintf(w, "shed digest: the last %s, since %s\n", Short(d.Now.Sub(d.Since)), d.Since.Local().Format("Mon 15:04"))

	total := 0
	for _, rm := range d.Merged {
		total += len(rm.Pulls)
	}
	fmt.Fprintf(w, "\nMerged: %d pull request(s)\n", total)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, rm := range d.Merged {
		for _, p := range rm.Pulls {
			fmt.Fprintf(tw, "  %s#%d\t%s ago\t%s\n", rm.Repo, p.Number, Short(d.Now.Sub(p.MergedAt)), p.Title)
		}
	}
	tw.Flush()

	for _, r := range d.Report.Repos {
		fmt.Fprintf(w, "\nEye checks: %s\n", r.Repo)
		if r.Error != "" {
			fmt.Fprintf(w, "  ! cannot read the release state: %s\n", r.Error)
			continue
		}
		passed := 0
		for _, l := range r.Passed {
			if l.Since.After(d.Since) {
				passed++
			}
		}
		fmt.Fprintf(w, "  %d passed in this window; now %s, %d failed\n", passed, r.notDone(), r.count(looks.Failed))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, l := range r.Passed {
			if l.Since.After(d.Since) {
				fmt.Fprintf(tw, "    #%d\tPR #%d\tpassed\t%s ago\t%s\n", l.Number, l.PR, Short(d.Now.Sub(l.Since)), l.Title)
			}
		}
		for _, l := range r.Looks {
			if l.State == looks.Failed || l.State == looks.Blocked {
				fmt.Fprintf(tw, "    #%d\tPR #%d\t%s\t%s ago\t%s\n", l.Number, l.PR, lookWords[l.State], Short(d.Now.Sub(l.Since)), l.Title)
			}
		}
		tw.Flush()
	}

	// What waits on the owner, the oldest first. An issue in the queue of
	// two machines waits once.
	var waiting []waitingItem
	seen := map[string]bool{}
	for _, m := range d.Report.Machines {
		for _, i := range m.Issues {
			for _, a := range i.Asks {
				key := issueKey(i.Repo, i.Number) + " " + a.Name
				if seen[key] {
					continue
				}
				seen[key] = true
				waiting = append(waiting, waitingItem{issue: i, what: a.What(), since: a.Since})
			}
		}
	}
	slices.SortStableFunc(waiting, func(a, b waitingItem) int { return a.since.Compare(b.since) })
	fmt.Fprintf(w, "\nWaiting on you: %d\n", len(waiting))
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, item := range waiting {
		age := "-"
		if !item.since.IsZero() {
			age = Short(d.Now.Sub(item.since))
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s#%d\t%s\n", age, item.what, item.issue.Repo, item.issue.Number, item.issue.Title)
	}
	tw.Flush()

	// What the workers chose by themselves. It asks nothing of the owner,
	// who may overrule a choice with a ruling on the issue.
	var decided []string
	seen = map[string]bool{}
	for _, m := range d.Report.Machines {
		for _, i := range m.Issues {
			if key := issueKey(i.Repo, i.Number); !seen[key] {
				seen[key] = true
				for _, choice := range i.Decided {
					decided = append(decided, fmt.Sprintf("%s  %s", key, choice))
				}
			}
		}
	}
	if len(decided) > 0 {
		fmt.Fprintf(w, "\nDecided without you: %d\n", len(decided))
		for _, line := range decided {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}

	// Everything else that needs the owner: a machine, a worker, a task.
	var other []string
	for _, m := range d.Report.Machines {
		for _, a := range m.Attention {
			if !strings.Contains(a, " waits on you (") {
				other = append(other, m.Name+": "+a)
			}
		}
	}
	other = append(other, d.Problems...)
	if len(other) > 0 {
		fmt.Fprintf(w, "\nAlso needs you: %d\n", len(other))
		for _, a := range other {
			fmt.Fprintf(w, "  ! %s\n", a)
		}
	}
}
