package backlog

import (
	"slices"
	"strings"
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
)

const eyesAsked = "## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** open /orders and check the refund total"

func TestALookBlockedForMissingDataGoesBackToAWorkerAsRework(t *testing.T) {
	signals := config.DefaultSignals()
	issue := mergedIssue(eyesAsked, "**Eyes blocked:** no data: staging has no order with a partial refund, and the seed tool cannot make one")

	got := Rework(issue, signals)

	if !strings.Contains(got, "staging has no order with a partial refund") || !strings.Contains(got, "seed") {
		t.Fatalf("Rework = %q, want the missing state and a seed to add", got)
	}
	if w := Waiting(issue, signals); len(w) != 0 {
		t.Fatalf("Waiting = %v, want nothing on the owner", w)
	}
	if f := Finished(issue, signals); f != 0 {
		t.Fatalf("a look blocked for data left its issue finished: %d", f)
	}
}

func TestALookBlockedForAnotherReasonStillWaitsOnTheOwner(t *testing.T) {
	signals := config.DefaultSignals()
	issue := mergedIssue(eyesAsked, "**Eyes blocked:** staging is down")

	if got := Rework(issue, signals); got != "" {
		t.Fatalf("Rework = %q, want none", got)
	}
	if w := Waiting(issue, signals); !slices.Equal(w, []string{"eyes"}) {
		t.Fatalf("Waiting = %v, want the eye check", w)
	}
}

func TestABuilderThatAddsTheSeedAndHandsBackAgainEndsTheRework(t *testing.T) {
	signals := config.DefaultSignals()
	issue := mergedIssue(eyesAsked, "Eyes blocked: no data: no partial refund", "## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** run `make seed-refund`, then open /orders")

	if got := Rework(issue, signals); got != "" {
		t.Fatalf("Rework = %q, want none once the look is asked again", got)
	}
}

func TestTheMissingDataOfABlockedLookIsReadFromItsNote(t *testing.T) {
	signals := config.DefaultSignals()
	for _, tc := range []struct {
		block, want string
	}{
		{"Eyes blocked: no data: no partial refund", "no partial refund"},
		{"Eyes blocked: No data - no partial refund", "no partial refund"},
		{"Eyes blocked: staging has no data center", ""},
		{"Eyes blocked: staging is down", ""},
	} {
		if got := NoDataLook(mergedIssue(eyesAsked, tc.block), signals); got != tc.want {
			t.Errorf("NoDataLook(%q) = %q, want %q", tc.block, got, tc.want)
		}
	}
}

func TestALookBlockedForMissingDataIsALookThatDidNotPass(t *testing.T) {
	issue := mergedIssue(eyesAsked, "Eyes blocked: no data: no partial refund", eyesAsked, "Eyes checked: the total is right")
	if done, firstPass := LooksDone(issue, config.DefaultSignals()); done != 2 || firstPass {
		t.Fatalf("done %d, first pass %v; want 2, false", done, firstPass)
	}
}
