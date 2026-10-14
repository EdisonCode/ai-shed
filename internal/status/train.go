package status

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

// Train is where a repository's merge train stands, as its issues say.
type Train struct {
	// InHand are the pull requests on the train: one, unless two machines
	// each put one on at once.
	InHand []TrainCar `json:"in_hand"`
	// Next are the pull requests ready to board, the first verified first.
	Next []TrainCar `json:"next"`
}

// TrainCar is a pull request on a merge train, or ready to board one.
type TrainCar struct {
	Issue int `json:"issue"`
	PR    int `json:"pr"`
	// Since is when it boarded, or when its verification passed.
	Since time.Time `json:"since"`
	// Checks is set while the checks of its head run.
	Checks bool `json:"checks,omitempty"`
}

// readTrain reads the merge train of a repository with merge_train from its
// open issues. A look on staging does not hold a pull request back when a
// worker does it after the merge.
func readTrain(open []backlog.Issue, signals []config.Signal, now time.Time, lookAfter bool) *Train {
	t := &Train{}
	seen := map[int]bool{}
	for _, i := range open {
		if seen[i.Number] {
			continue
		}
		seen[i.Number] = true
		i.Verify = true
		if pr, at := backlog.Boarded(i, signals); pr != 0 {
			t.InHand = append(t.InHand, TrainCar{Issue: i.Number, PR: pr, Since: at, Checks: i.OpenPRs[pr].Pending})
			continue
		}
		if v, ready := backlog.MergeReady(i, signals, now, lookAfter); ready {
			t.Next = append(t.Next, TrainCar{Issue: i.Number, PR: v.PR, Since: v.At})
		}
	}
	sort.SliceStable(t.Next, func(a, b int) bool { return t.Next[a].Since.Before(t.Next[b].Since) })
	return t
}

// renderTrain writes the pull request in hand and the ones next.
func renderTrain(w io.Writer, t *Train, now time.Time) {
	if len(t.InHand) == 0 && len(t.Next) == 0 {
		fmt.Fprintln(w, "  merge train: empty")
		return
	}
	var parts []string
	for _, c := range t.InHand {
		words := fmt.Sprintf("PR #%d (#%d) in hand %s", c.PR, c.Issue, Short(now.Sub(c.Since)))
		if c.Checks {
			words += ", waiting for its checks"
		}
		parts = append(parts, words)
	}
	if len(t.Next) > 0 {
		var next []string
		for _, c := range t.Next {
			next = append(next, fmt.Sprintf("PR #%d (#%d)", c.PR, c.Issue))
		}
		parts = append(parts, "next "+strings.Join(next, ", "))
	}
	fmt.Fprintf(w, "  merge train: %s\n", strings.Join(parts, "; "))
}
