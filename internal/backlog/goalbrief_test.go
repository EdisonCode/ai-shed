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
	if len(b.Contracts) != 2 || b.Contracts[0].Text != "GET /prices returns cents per currency — provided by #41" || b.Contracts[0].Provider != "#41" ||
		b.Contracts[1].Provider != "org/api#7" || len(b.Contracts[0].Users) != 0 {
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

func TestAGoalCheckIsDueUntilACrewWorkerPassesOrFailsItAndDueAgainWhenAsked(t *testing.T) {
	brief := Issue{Title: "Goal brief: pricing"}
	if s, asked := GoalCheck(brief); s != GoalCheckDue || asked != -1 {
		t.Fatalf("state = %q, asked = %d", s, asked)
	}
	brief.Comments = append(brief.Comments, Comment{Body: "Goal check failed: the checkout shows dollars"})
	if s, _ := GoalCheck(brief); s != GoalCheckFailed {
		t.Fatalf("state = %q, want failed", s)
	}
	brief.Comments = append(brief.Comments, Comment{Body: "Goal check again: the fix merged"})
	if s, asked := GoalCheck(brief); s != GoalCheckDue || asked != 1 {
		t.Fatalf("state = %q, asked = %d, want due from comment 1", s, asked)
	}
	brief.Comments = append(brief.Comments, Comment{Body: "Goal checked:\n- a customer pays the new price: yes"})
	if s, _ := GoalCheck(brief); s != GoalCheckPassed {
		t.Fatalf("state = %q, want passed", s)
	}
}
