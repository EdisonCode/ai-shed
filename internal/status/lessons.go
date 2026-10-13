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

// PendingLesson is a lesson workers wrote in their hand-backs that the owner
// has not settled, with every issue it was written in.
type PendingLesson struct {
	Text    string    `json:"text"`
	Sources []string  `json:"sources"`
	Since   time.Time `json:"since"`
}

// collectLessons gathers the lessons of the open issues and of the ones
// closed in the window, one entry per lesson however many workers wrote it.
// shed writes no memory: the owner promotes, rejects or folds each one, and
// says so on its issue.
func (c Collector) collectLessons(ctx context.Context, cfg *config.Config, d *Digest) {
	byKey := map[string]*PendingLesson{}
	var order []string
	seen := map[string]bool{}
	add := func(issues []backlog.Issue) {
		for _, i := range issues {
			key := issueKey(i.Repo, i.Number)
			if seen[key] {
				continue
			}
			seen[key] = true
			for _, l := range backlog.Lessons(i, cfg.Signals) {
				k := backlog.LessonKey(l.Text)
				p := byKey[k]
				if p == nil {
					p = &PendingLesson{Text: l.Text, Since: l.At}
					byKey[k] = p
					order = append(order, k)
				}
				if !slices.Contains(p.Sources, key) {
					p.Sources = append(p.Sources, key)
				}
				if l.At.Before(p.Since) {
					p.Since = l.At
				}
			}
		}
	}
	var sources []config.IssueSource
	listed := map[string]bool{}
	for _, m := range cfg.Machines {
		for _, src := range m.AllSources() {
			if key := fmt.Sprint(src.Repo, src.Label, src.Assignee, src.Query); !listed[key] {
				listed[key] = true
				sources = append(sources, src)
			}
		}
	}
	for _, src := range sources {
		if c.Lister != nil {
			if open, err := c.Lister.List(ctx, src); err == nil {
				add(open)
			}
		}
		if c.Closed != nil {
			if closed, err := c.Closed.ClosedSince(ctx, src, d.Since); err == nil {
				add(closed)
			}
		}
	}
	for _, k := range order {
		d.Lessons = append(d.Lessons, *byKey[k])
	}
	slices.SortStableFunc(d.Lessons, func(a, b PendingLesson) int { return a.Since.Compare(b.Since) })
}

// renderLessons lists the lessons that wait for the owner, oldest first.
func renderLessons(w io.Writer, d Digest) {
	if len(d.Lessons) == 0 {
		return
	}
	fmt.Fprintf(w, "\nLessons to promote: %d (promote, reject or fold each, then comment \"Lessons promoted\" or \"Lesson rejected\" on its issue)\n", len(d.Lessons))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, l := range d.Lessons {
		age := "-"
		if !l.Since.IsZero() {
			age = Short(d.Now.Sub(l.Since))
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", age, strings.Join(l.Sources, ", "), l.Text)
	}
	tw.Flush()
}
