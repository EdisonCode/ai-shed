package looks

import (
	"slices"
	"testing"
	"time"
)

func TestTheIssuesOfAWindowAreReadWithWhenTheyOpenedAndClosedAndTheirPullRequests(t *testing.T) {
	data := `[
{"number":12,"body":"Found while building #9","createdAt":"2026-10-04T08:00:00Z","closedAt":null,"closedByPullRequestsReferences":[]},
{"number":9,"body":"","createdAt":"2026-09-01T08:00:00Z","closedAt":"2026-10-04T10:00:00Z","closedByPullRequestsReferences":[{"number":41},{"number":44}]}
]`
	got, err := parseFlowIssues([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].ClosedAt.IsZero() || got[0].Body != "Found while building #9" ||
		!got[1].ClosedAt.Equal(time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)) || !slices.Equal(got[1].Pulls, []int{41, 44}) {
		t.Fatalf("issues = %+v", got)
	}
}

func TestTheIssuesOfTwoSearchesAreJoinedOncePerIssue(t *testing.T) {
	opened := []FlowIssue{{Number: 12}, {Number: 9}}
	closed := []FlowIssue{{Number: 9}, {Number: 3}}
	got := joinFlow(opened, closed)
	var numbers []int
	for _, i := range got {
		numbers = append(numbers, i.Number)
	}
	if !slices.Equal(numbers, []int{12, 9, 3}) {
		t.Fatalf("numbers = %v, want each issue once", numbers)
	}
}
