package status

import (
	"testing"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

func TestALookBlockedForMissingDataIsReworkAndWaitsNotOnTheOwner(t *testing.T) {
	blocked := issue(7, "## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** open /orders", "**Eyes blocked:** no data: no order with a partial refund")
	blocked.OpenPRs, blocked.MergedPRs = map[int]backlog.PR{}, map[int]bool{41: true}
	m := config.Machine{Name: "box", Workers: []config.Worker{{Name: "app"}}}

	r := Assess(m, signals, now, healthy(), []backlog.Issue{blocked})

	got := r.Issues[0]
	if got.State != Rework || len(got.Waiting) != 0 {
		t.Fatalf("issue = %+v, want rework that waits on nobody", got)
	}
}
