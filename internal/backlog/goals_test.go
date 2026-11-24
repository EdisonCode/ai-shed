package backlog

import (
	"testing"
	"time"
)

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

func TestAGoalsIssuesAreReadWithWhenTheyOpenedAndClosed(t *testing.T) {
	data := `[{"state":"CLOSED","createdAt":"2026-11-01T10:00:00Z","closedAt":"2026-11-20T10:00:00Z"},{"state":"OPEN","createdAt":"2026-11-22T10:00:00Z","closedAt":null}]`
	issues, err := parseGoalIssues([]byte(data))
	if err != nil || len(issues) != 2 || !issues[0].Closed || issues[0].ClosedAt.Day() != 20 || issues[1].Closed || !issues[1].ClosedAt.IsZero() {
		t.Fatalf("issues = %+v, %v", issues, err)
	}
}

func TestAGoalsETAComesFromItsNetClosesPerDayOverTheWeek(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	closed := func(ago time.Duration) GoalIssue {
		return GoalIssue{Closed: true, CreatedAt: now.Add(-30 * day), ClosedAt: now.Add(-ago)}
	}
	// 14 closed and none opened in the week: 2 a day, so 6 open take 3 days.
	var issues []GoalIssue
	for range 14 {
		issues = append(issues, closed(2*day))
	}
	issues = append(issues, closed(20*day))
	for range 6 {
		issues = append(issues, GoalIssue{CreatedAt: now.Add(-20 * day)})
	}
	p := GoalPace(issues, now)
	if p.Open != 6 || p.ClosedWeek != 14 || p.PerDay != 2 || !p.ETA.Equal(now.Add(3*day)) {
		t.Fatalf("pace = %+v", p)
	}
	if got := p.Words(); got != "done about Nov 27 at 2.0 a day" {
		t.Fatalf("words = %q", got)
	}
}

func TestAGoalThatOpensIssuesAsFastAsItClosesThemHasNoETA(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	issues := []GoalIssue{
		{Closed: true, CreatedAt: now.Add(-30 * 24 * time.Hour), ClosedAt: now.Add(-time.Hour)},
		{CreatedAt: now.Add(-2 * time.Hour)},
		{CreatedAt: now.Add(-3 * time.Hour)},
	}
	p := GoalPace(issues, now)
	if !p.ETA.IsZero() || p.Words() != "not converging: 2 opened, 1 closed in 7 days" {
		t.Fatalf("pace = %+v, %q", p, p.Words())
	}
	idle := []GoalIssue{{CreatedAt: now.Add(-30 * 24 * time.Hour)}}
	if got := GoalPace(idle, now).Words(); got != "stalled: nothing opened or closed in 7 days" {
		t.Fatalf("a goal nobody touched: %q", got)
	}
	if got := GoalPace([]GoalIssue{{Closed: true, CreatedAt: now, ClosedAt: now}}, now).Words(); got != "done" {
		t.Fatalf("a goal with nothing open: %q", got)
	}
}
