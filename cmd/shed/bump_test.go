package main

import (
	"testing"

	"github.com/edisoncode/ai-shed/internal/backlog"
)

func TestARefusedBumpSaysWhatLabelsTheIssueHasNow(t *testing.T) {
	seen := backlog.Issue{Repo: "org/app", Number: 41, Labels: []backlog.Label{{Name: "other-box"}, {Name: "p1"}}}
	if got, want := notInQueue("box", 41, seen), "org/app#41 is open, but no queue of machine box selects it; its labels now: other-box, p1"; got != want {
		t.Errorf("open elsewhere: %q, want %q", got, want)
	}
	if got, want := notInQueue("box", 41, backlog.Issue{}), "#41 is not open in any queue of machine box"; got != want {
		t.Errorf("not open: %q, want %q", got, want)
	}
}

func TestABumpNamesTheQueueLabelThatKeepsAReserveIssueElsewhere(t *testing.T) {
	i := backlog.Issue{Repo: "org/app", Number: 41, Labels: []backlog.Label{{Name: "p1"}, {Name: "machine:other"}}}
	if got := queueLabel([]string{"machine:box", "machine:other"}, i); got != "machine:other" {
		t.Errorf("queue label = %q, want machine:other", got)
	}
	if got := queueLabel([]string{"machine:box"}, i); got != "" {
		t.Errorf("queue label = %q, want none", got)
	}
}
