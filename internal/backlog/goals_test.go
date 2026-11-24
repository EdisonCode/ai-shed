package backlog

import "testing"

func TestTheGoalOfAnIssueIsItsLabelWithTheGoalPrefix(t *testing.T) {
	for _, tc := range []struct {
		labels []string
		prefix string
		want   string
	}{
		{[]string{"machine:box", "goal:export"}, "goal:", "goal:export"},
		{[]string{"goal:refunds", "goal:export"}, "goal:", "goal:export"},
		{[]string{"machine:box"}, "goal:", ""},
		{[]string{"goal:export"}, "", ""},
	} {
		if got := GoalLabel(tc.labels, tc.prefix); got != tc.want {
			t.Errorf("GoalLabel(%v, %q) = %q, want %q", tc.labels, tc.prefix, got, tc.want)
		}
	}
}

func TestAGoalIsCountedClosedOfTotal(t *testing.T) {
	data := `[{"state":"CLOSED"},{"state":"OPEN"},{"state":"CLOSED"}]`
	closed, total, err := parseGoalCounts([]byte(data))
	if err != nil || closed != 2 || total != 3 {
		t.Fatalf("closed %d of %d, %v; want 2 of 3", closed, total, err)
	}
}
