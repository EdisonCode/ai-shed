package status

import (
	"context"
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
	// Batches are the open batches of a batch train: one, unless two
	// machines each opened one at once.
	Batches []TrainBatch `json:"batches,omitempty"`
}

// TrainBatch is a batch of a batch train: a draft pull request that tests
// several pull requests together.
type TrainBatch struct {
	PR    int        `json:"pr"`
	Since time.Time  `json:"since"`
	Cars  []TrainCar `json:"cars"`
	// Checks says where the batch's checks stand: running, passed, or what
	// failed.
	Checks string `json:"checks"`
}

// BatchReader reads the open batches of a batch train, and where the checks
// of each stand.
type BatchReader interface {
	Batches(ctx context.Context, repo string) ([]backlog.Batch, error)
	ReadHead(ctx context.Context, repo string, pr int) (backlog.Head, error)
}

// readBatches reads the repository's open batches into the train.
func readBatches(ctx context.Context, r BatchReader, repo string, t *Train) error {
	batches, err := r.Batches(ctx, repo)
	if err != nil {
		return err
	}
	for _, b := range batches {
		h, err := r.ReadHead(ctx, repo, b.PR)
		if err != nil {
			return err
		}
		tb := TrainBatch{PR: b.PR, Since: b.Created, Checks: checkWords(h)}
		for _, c := range b.Cars {
			tb.Cars = append(tb.Cars, TrainCar{Issue: c.Issue, PR: c.PR, Since: b.Created})
		}
		t.Batches = append(t.Batches, tb)
	}
	return nil
}

func checkWords(h backlog.Head) string {
	switch {
	case h.Problem != "":
		return h.Problem
	case h.Green():
		return "checks passed"
	case h.AllSkipped():
		return "every check was skipped: its CI did not run on it"
	default:
		return "checks running"
	}
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
// worker does it after the merge. With mergeHigh, a high-risk pass boards
// as a low-risk one does.
func readTrain(open []backlog.Issue, signals []config.Signal, now time.Time, lookAfter, mergeHigh bool) *Train {
	t := &Train{}
	seen := map[int]bool{}
	for _, i := range open {
		if seen[i.Number] {
			continue
		}
		seen[i.Number] = true
		i.Verify, i.MergeHigh = true, mergeHigh
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
	if len(t.InHand) == 0 && len(t.Next) == 0 && len(t.Batches) == 0 {
		fmt.Fprintln(w, "  merge train: empty")
		return
	}
	var parts []string
	batched := map[int]bool{}
	for _, b := range t.Batches {
		var cars []string
		for _, c := range b.Cars {
			batched[c.PR] = true
			cars = append(cars, fmt.Sprintf("PR #%d (#%d)", c.PR, c.Issue))
		}
		parts = append(parts, fmt.Sprintf("batch PR #%d of %s %s, %s", b.PR, strings.Join(cars, ", "), Short(now.Sub(b.Since)), b.Checks))
	}
	for _, c := range t.InHand {
		if batched[c.PR] {
			continue
		}
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
