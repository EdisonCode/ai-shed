package tidy

import (
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/probe"
)

func TestSessionsIdleLongerThanTheLimitAreClosed(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	windows := []probe.Window{
		{Session: "shed", Name: "app", LastActivity: now},
		{Session: "work", Name: "notes", LastActivity: now.Add(-8 * 24 * time.Hour)},
		{Session: "work", Name: "today", LastActivity: now.Add(-2 * time.Hour)},
	}
	got := Sessions(windows, []string{"work:notes", "work:today"}, now, 3*24*time.Hour)
	if len(got) != 2 || got[0].Name != "work:notes" || !got[0].Close || got[0].Idle != 8*24*time.Hour || got[1].Close {
		t.Fatalf("sessions = %+v, want work:notes closed and work:today kept", got)
	}
}

func TestCloseScriptQuotesNamesAndTargetsExactly(t *testing.T) {
	script := CloseSessionsScript([]Session{{Session: "work", Window: "it's mine", Close: true}, {Session: "work", Window: "kept"}})
	if !strings.Contains(script, `tmux kill-window -t '=work:=it'\''s mine'`) || strings.Contains(script, "kept") {
		t.Fatalf("script:\n%s", script)
	}
}
