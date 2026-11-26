package backlog

import (
	"slices"
	"testing"
)

const briefBody = `The checkout runs on the new pricing service.

**Slices:**
1. #41 the pricing API
2. org/web#12 the checkout page uses it

**Contracts:**
- GET /prices returns cents per currency — provided by #41
- the price-changed event, provided by org/api#7

## Done when
- a customer pays the new price on staging, end to end
`

func TestAGoalBriefIsReadIntoItsSlicesContractsAndDoneWhen(t *testing.T) {
	b := ParseGoalBrief(Issue{Repo: "org/api", Number: 40, Title: "Goal brief: new pricing", Body: briefBody})
	if !slices.Equal(b.Slices, []string{"#41 the pricing API", "org/web#12 the checkout page uses it"}) {
		t.Fatalf("slices = %q", b.Slices)
	}
	want := []Contract{{Text: "GET /prices returns cents per currency — provided by #41", Provider: "#41"}, {Text: "the price-changed event, provided by org/api#7", Provider: "org/api#7"}}
	if !slices.Equal(b.Contracts, want) {
		t.Fatalf("contracts = %+v", b.Contracts)
	}
	if !slices.Equal(b.DoneWhen, []string{"a customer pays the new price on staging, end to end"}) {
		t.Fatalf("done when = %q", b.DoneWhen)
	}
}

func TestTheBriefOfAGoalIsTheIssueTitledSoWithTheLowestNumber(t *testing.T) {
	issues := []Issue{{Number: 41, Title: "The pricing API"}, {Number: 52, Title: "Goal brief: again"}, {Number: 40, Title: "goal brief: new pricing"}}
	if b, ok := BriefOf(issues); !ok || b.Number != 40 {
		t.Fatalf("brief = %+v, %v", b, ok)
	}
	if _, ok := BriefOf(issues[:1]); ok {
		t.Fatal("a goal with no brief has one")
	}
}
