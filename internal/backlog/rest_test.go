package backlog

import (
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

const noStepHandBack = "## Hand-back\n**PR:** none\n**Needs eyes:** nothing\n**Decisions needed:** none\n**Owner act:** run the backfill on production"

func restedIssue(comments ...string) Issue {
	at := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	i := Issue{Repo: "org/app", Number: 7, OpenPRs: map[int]PR{}, MergedPRs: map[int]bool{}, UpdatedAt: at}
	for n, body := range comments {
		i.Comments = append(i.Comments, Comment{Body: body, CreatedAt: at.Add(time.Duration(n-len(comments)+1) * time.Minute)})
	}
	return i
}

func noStepAsk(t *testing.T, i Issue) (Ask, bool) {
	t.Helper()
	for _, a := range Asks(i, config.DefaultSignals()) {
		if a.Name == NoStepAsk {
			return a, true
		}
	}
	return Ask{}, false
}

func TestAHandBackWithNoPullRequestAndNoAskWaitsOnTheOwner(t *testing.T) {
	i := restedIssue("## Plan\nadd the column", noStepHandBack)
	a, ok := noStepAsk(t, i)
	if !ok {
		t.Fatalf("asks = %+v, want a %q ask", Asks(i, config.DefaultSignals()), NoStepAsk)
	}
	if a.Left != "run the backfill on production" || a.What() != "no worker step: run the backfill on production" {
		t.Fatalf("ask = %+v (%q), want the owner act named", a, a.What())
	}
	if !a.Since.Equal(i.Comments[1].CreatedAt) {
		t.Fatalf("since = %v, want the hand-back's time", a.Since)
	}
	if got := Effects(i, config.DefaultSignals(), time.Now())[0].Effect; got != EffectReturns {
		t.Fatalf("effect = %q, want %q: an answer gives the issue back to a worker", got, EffectReturns)
	}
}

func TestACommentAfterTheRestingHandBackMakesTheIssueWorkable(t *testing.T) {
	i := restedIssue(noStepHandBack, "The backfill ran. Now drop the old column too.")
	if a, ok := noStepAsk(t, i); ok {
		t.Fatalf("ask = %+v, want none after the owner wrote", a)
	}
}

func TestAnEditOrLabelAfterTheRestingHandBackMakesTheIssueWorkable(t *testing.T) {
	i := restedIssue(noStepHandBack)
	i.UpdatedAt = i.Comments[0].CreatedAt.Add(time.Hour)
	if a, ok := noStepAsk(t, i); ok {
		t.Fatalf("ask = %+v, want none after the issue changed", a)
	}
}

func TestAHandBackThatLeavesWorkIsNotRested(t *testing.T) {
	cases := map[string]Issue{
		"an open pull request": func() Issue {
			i := restedIssue("## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** nothing\n**Decisions needed:** none")
			i.OpenPRs[41] = PR{}
			return i
		}(),
		"still to do":  restedIssue("## Hand-back\n**PR:** none\n**Still to do:**\n- the API\n\n**Decisions needed:** none"),
		"a question":   restedIssue("## Hand-back\n**PR:** none\n**Decisions needed:** keep the column?"),
		"a dependency": restedIssue("## Hand-back\n**PR:** none\n**Decisions needed:** none\nDepends on #12"),
		"no hand-back": restedIssue("## Plan\nadd the column"),
	}
	for name, i := range cases {
		if a, ok := noStepAsk(t, i); ok {
			t.Errorf("%s: ask = %+v, want none", name, a)
		}
	}
}
