package status

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/looks"
)

type fakeGoalCounts map[string][2]int

func (f fakeGoalCounts) GoalCounts(_ context.Context, _, label string) (int, int, error) {
	return f[label][0], f[label][1], nil
}

func withLabels(i backlog.Issue, labels ...string) backlog.Issue {
	for _, l := range labels {
		i.Labels = append(i.Labels, backlog.Label{Name: l})
	}
	return i
}

func goalsConfig() *config.Config {
	cfg := oneMachine()
	cfg.Supervisor.GoalLabels = "goal:"
	cfg.Supervisor.OverlapLabels = "area:"
	return cfg
}

func TestTheDigestShowsEachGoalClosedOfTotalNetAndWhatHoldsItsNextSlice(t *testing.T) {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	held := withLabels(issue(14), "goal:export")
	held.Deps = []backlog.Dep{{Repo: "org/app", Number: 12, Open: true}}
	workable := withLabels(issue(21), "goal:refunds")
	flow := fakeFlow{issues: map[string][]looks.FlowIssue{"org/app": {
		{Number: 15, CreatedAt: ago(time.Hour), Labels: []string{"goal:export"}},
		{Number: 9, CreatedAt: ago(9 * 24 * time.Hour), ClosedAt: ago(2 * time.Hour), Labels: []string{"goal:export"}},
		{Number: 8, CreatedAt: ago(9 * 24 * time.Hour), ClosedAt: ago(3 * time.Hour), Labels: []string{"goal:export"}},
	}}}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{issues: []backlog.Issue{held, workable}}, Flow: flow,
		Goals: fakeGoalCounts{"goal:export": {7, 10}, "goal:refunds": {0, 4}}}

	d := c.Digest(context.Background(), goalsConfig(), &fakeMerged{}, ago(24*time.Hour), now)

	if len(d.Goals) != 2 || d.Goals[0].Label != "goal:export" || d.Goals[0].Closed != 7 || d.Goals[0].Total != 10 || d.Goals[0].Net != -1 {
		t.Fatalf("goals = %+v", d.Goals)
	}
	var out bytes.Buffer
	RenderDigest(&out, d)
	wantLines(t, out.String(),
		"Goals: 2",
		"org/app  goal:export   7 of 10 closed  net -1  next #14 is held: waits for #12",
		"org/app  goal:refunds  0 of 4 closed   net +0  next #21 is workable",
	)
}

func TestDecisionsWithoutTheOwnerAreGroupedByGoalThenAreaThenRepository(t *testing.T) {
	decided := func(number int, choice string, labels ...string) backlog.Issue {
		return withLabels(issue(number, "## Hand-back\n**PR:** none\n**Decided without you:**\n- "+choice+"\n**Decisions needed:** none"), labels...)
	}
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{issues: []backlog.Issue{
		decided(5, "Rounded half up.", "area:billing"),
		decided(6, "Kept the old column.", "goal:export", "area:billing"),
		decided(7, "Logged at info."),
	}}}

	d := c.Digest(context.Background(), goalsConfig(), &fakeMerged{}, now.Add(-24*time.Hour), now)

	var out bytes.Buffer
	RenderDigest(&out, d)
	wantLines(t, out.String(), "Decided without you: 3\n"+
		"  goal:export\n    org/app#6  Kept the old column.\n"+
		"  area:billing\n    org/app#5  Rounded half up.\n"+
		"  org/app\n    org/app#7  Logged at info.\n")
}

func TestWithoutGoalLabelsTheDigestHasNoGoalsAndDecisionsAreNotGrouped(t *testing.T) {
	i := withLabels(issue(6, "## Hand-back\n**PR:** none\n**Decided without you:**\n- Kept the old column.\n**Decisions needed:** none"), "goal:export")
	c := Collector{Runner: fakeRunner{}, Lister: fakeLister{issues: []backlog.Issue{i}}, Goals: fakeGoalCounts{"goal:export": {7, 10}}}

	d := c.Digest(context.Background(), oneMachine(), &fakeMerged{}, now.Add(-24*time.Hour), now)

	var out bytes.Buffer
	RenderDigest(&out, d)
	if d.Goals != nil || strings.Contains(out.String(), "Goals:") {
		t.Fatalf("goals without goal_labels:\n%s", out.String())
	}
	wantLines(t, out.String(), "Decided without you: 1\n  org/app#6  Kept the old column.")
}
