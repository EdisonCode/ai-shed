package main

import (
	"strings"
	"testing"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

func asking(bodies ...string) backlog.Issue {
	i := backlog.Issue{Repo: "org/app", Number: 7, OpenPRs: map[int]backlog.PR{41: {}}}
	for _, b := range bodies {
		i.Comments = append(i.Comments, backlog.Comment{Body: b})
	}
	return i
}

func TestAnAnswerIsPostedUnderThePhraseOfTheOpenSignal(t *testing.T) {
	got, err := answerComment(asking("## Hand-back\n**PR:** none\n**Decisions needed:** keep the column?"), config.DefaultSignals(), "", false, false, "Keep it.")
	if err != nil || got != "Owner ruling: Keep it." {
		t.Fatalf("comment = %q, %v", got, err)
	}
}

func TestAnAcceptanceUsesTheSignalsAcceptWord(t *testing.T) {
	got, err := answerComment(asking("## Hand-back\n**PR:** none\n**Decisions needed:** keep the column?"), config.DefaultSignals(), "", true, false, "")
	if err != nil || got != "Owner ruling: accepted" {
		t.Fatalf("comment = %q, %v", got, err)
	}
}

func TestABlockedLookIsUnblocked(t *testing.T) {
	issue := asking("## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** open /orders", "Eyes blocked: staging was down")
	got, err := answerComment(issue, config.DefaultSignals(), "", false, true, "staging is back")
	if err != nil || got != "Eyes unblocked: staging is back" {
		t.Fatalf("comment = %q, %v", got, err)
	}
}

func TestAnAnswerIsRefusedWhenTwoSignalsAreOpen(t *testing.T) {
	issue := asking("## Hand-back\n**PR:** #41 (ready)\n**Decisions needed:** keep it?\n**Needs eyes:** open /orders")
	_, err := answerComment(issue, config.DefaultSignals(), "", false, false, "Keep it.")
	if err == nil || !strings.Contains(err.Error(), "decision, eyes") || !strings.Contains(err.Error(), "-signal") {
		t.Fatalf("err = %v, want both signals named", err)
	}
	if got, err := answerComment(issue, config.DefaultSignals(), "decision", false, false, "Keep it."); err != nil || got != "Owner ruling: Keep it." {
		t.Fatalf("with -signal: comment = %q, %v", got, err)
	}
}

func TestAnAnswerIsRefusedWhenOnlyAnActClearsTheIssue(t *testing.T) {
	_, err := answerComment(asking("## Hand-back\n**PR:** #41 (ready)"), config.DefaultSignals(), "", false, false, "Looks fine.")
	if err == nil || !strings.Contains(err.Error(), "merge or close pull request #41") {
		t.Fatalf("err = %v, want the act that clears the review", err)
	}
}

func TestAnAnswerIsRefusedWhenNothingWaits(t *testing.T) {
	if _, err := answerComment(asking(), config.DefaultSignals(), "", false, false, "Keep it."); err == nil {
		t.Fatal("an answer to an issue that asks nothing was accepted")
	}
}
