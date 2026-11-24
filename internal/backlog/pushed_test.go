package backlog

import (
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

func TestAHandBackAfterReworkThatMovedNothingIsCaught(t *testing.T) {
	sent := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	handBack := func(body string) Issue {
		i := Issue{Comments: []Comment{
			{Body: "## Hand-back\n**PR:** #41 (ready)", CreatedAt: sent.Add(-time.Hour)},
			{Body: body, CreatedAt: sent.Add(time.Hour)},
		}}
		i.OpenPRs = map[int]PR{41: {Head: "abc1234"}}
		return i
	}
	signals := config.DefaultSignals()
	if got := Unpushed(handBack("## Hand-back\n**PR:** #41 (ready)\nPushed the fix."), signals, "abc1234", sent); got != "the hand-back after rework left pull request #41 at abc1234, the head it was sent back with, and does not say that nothing needed to change: push the fix, or hand back saying nothing changed" {
		t.Fatalf("unpushed = %q", got)
	}
	for name, i := range map[string]Issue{
		"the head moved": func() Issue {
			i := handBack("## Hand-back\n**PR:** #41 (ready)")
			i.OpenPRs[41] = PR{Head: "def5678"}
			return i
		}(),
		"it says no change":    handBack("## Hand-back\n**PR:** #41 (ready)\nNothing changed: the conflict was already resolved."),
		"no hand-back since":   func() Issue { i := handBack(""); i.Comments = i.Comments[:1]; return i }(),
		"no head was recorded": handBack("## Hand-back\n**PR:** #41 (ready)"),
	} {
		head := "abc1234"
		if name == "no head was recorded" {
			head = ""
		}
		if got := Unpushed(i, signals, head, sent); got != "" {
			t.Errorf("%s: unpushed = %q, want none", name, got)
		}
	}
}
