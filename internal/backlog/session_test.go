package backlog

import "testing"

func TestASignedPlanNamesItsSession(t *testing.T) {
	i := Issue{Comments: []Comment{{Body: "## Plan\n**Paths:** internal/billing/\n**Session:** mini-web"}}}
	if got := PlanSession(i); got != "mini-web" {
		t.Fatalf("session = %q", got)
	}
}

func TestTheLatestSignatureCounts(t *testing.T) {
	i := Issue{Comments: []Comment{{Body: "**Session:** `box-app`"}, {Body: "Session: mini-web"}}}
	if got := PlanSession(i); got != "mini-web" {
		t.Fatalf("session = %q", got)
	}
}

func TestAnUnsignedPlanHasNoSession(t *testing.T) {
	i := Issue{Comments: []Comment{{Body: "## Plan\n**Paths:** internal/billing/"}}}
	if got := PlanSession(i); got != "" {
		t.Fatalf("session = %q", got)
	}
}
