package status

import (
	"context"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/edisoncode/ai-shed/internal/looks"
)

// PullTimer reads when each step of merged pull requests happened.
type PullTimer interface {
	PullTimes(ctx context.Context, repo string, numbers []int) (map[int]looks.PullTimes, error)
}

// Stages is where a merged pull request's time went. A zero stage is one
// that GitHub has no record of, or whose span ran backwards.
type Stages struct {
	// Build is from the first commit to ready for review.
	Build time.Duration `json:"build,omitempty"`
	// Checks is the merged head's checks, from the first started to the
	// last finished.
	Checks time.Duration `json:"checks,omitempty"`
	// Waits is from the later of checks finished and ready to the merge: time
	// in the owner's hands or in a merge queue.
	Waits time.Duration `json:"waits,omitempty"`
}

// PullStages is one merged pull request and where its time went.
type PullStages struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Stages
}

func stagesOf(t looks.PullTimes) Stages {
	span := func(from, to time.Time) time.Duration {
		if from.IsZero() || to.IsZero() || !to.After(from) {
			return 0
		}
		return to.Sub(from)
	}
	waitFrom := t.Ready
	if t.ChecksDone.After(waitFrom) {
		waitFrom = t.ChecksDone
	}
	return Stages{
		Build:  span(t.FirstCommit, t.Ready),
		Checks: span(t.ChecksStarted, t.ChecksDone),
		Waits:  span(waitFrom, t.Merged),
	}
}

// collectStages reads where the time of each repository's merged pull
// requests went. What cannot be read is a problem of the digest.
func (c Collector) collectStages(ctx context.Context, d *Digest) {
	if c.Times == nil {
		return
	}
	for _, rm := range d.Merged {
		if len(rm.Pulls) == 0 {
			continue
		}
		numbers := make([]int, len(rm.Pulls))
		for i, p := range rm.Pulls {
			numbers[i] = p.Number
		}
		times, err := c.Times.PullTimes(ctx, rm.Repo, numbers)
		if err != nil {
			d.Problems = append(d.Problems, err.Error())
			continue
		}
		for _, n := range numbers {
			if t, ok := times[n]; ok {
				d.Stages = append(d.Stages, PullStages{Repo: rm.Repo, Number: n, Stages: stagesOf(t)})
			}
		}
	}
}

var stageRows = []struct {
	name, meaning string
	of            func(Stages) time.Duration
}{
	{"build", "first commit to ready for review", func(s Stages) time.Duration { return s.Build }},
	{"checks", "the merged head's checks, first started to last finished", func(s Stages) time.Duration { return s.Checks }},
	{"waits", "checks finished or ready, whichever is later, to merged", func(s Stages) time.Duration { return s.Waits }},
}

// renderStages writes the median and worst of each stage, and names the
// pull request that was worst.
func renderStages(w io.Writer, stages []PullStages) {
	if len(stages) == 0 {
		return
	}
	fmt.Fprintf(w, "\nWhere the time went: %d merged pull request(s), median and worst\n", len(stages))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, row := range stageRows {
		var values []time.Duration
		var worst PullStages
		for _, s := range stages {
			if v := row.of(s.Stages); v > 0 {
				values = append(values, v)
				if v > row.of(worst.Stages) {
					worst = s
				}
			}
		}
		if len(values) == 0 {
			fmt.Fprintf(tw, "  %s\t-\t-\t-\t%s\n", row.name, row.meaning)
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s#%d\t%s\n", row.name, Short(medianDuration(values)), Short(row.of(worst.Stages)), worst.Repo, worst.Number, row.meaning)
	}
	tw.Flush()
}

func medianDuration(values []time.Duration) time.Duration {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
