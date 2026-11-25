package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordedMessagesAreReadBackInOrder(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	for i, to := range []string{"box-api", "mini-web"} {
		if err := RecordMessage(dir, "app", Message{Time: t0.Add(time.Duration(i) * time.Minute), To: to, Chars: 40}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Messages(dir, "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].To != "box-api" || got[1].To != "mini-web" || got[1].Chars != 40 {
		t.Fatalf("messages = %+v", got)
	}
}

func TestAWorkerThatSentNothingHasNoMessages(t *testing.T) {
	got, err := Messages(t.TempDir(), "app")
	if err != nil || len(got) != 0 {
		t.Fatalf("messages = %+v, err = %v", got, err)
	}
}

func TestALineThatCannotBeReadIsSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := RecordMessage(dir, "app", Message{Time: time.Now(), To: "box-api"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, MessagesDir, "app"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{half\n")
	f.Close()
	got, err := Messages(dir, "app")
	if err != nil || len(got) != 1 {
		t.Fatalf("messages = %+v, err = %v", got, err)
	}
}

func TestOldMessagesAreDroppedOnceTheLogGrows(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := 0; i < maxMessageLines+10; i++ {
		if err := RecordMessage(dir, "app", Message{Time: now.Add(-48 * time.Hour), To: "old"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordMessage(dir, "app", Message{Time: now, To: "new"}); err != nil {
		t.Fatal(err)
	}
	got, _ := Messages(dir, "app")
	if len(got) > maxMessageLines || got[len(got)-1].To != "new" {
		t.Fatalf("kept %d messages, last %+v", len(got), got[len(got)-1])
	}
}

func TestCountsAreByPeerSinceATime(t *testing.T) {
	now := time.Now()
	ms := []Message{
		{Time: now.Add(-2 * time.Hour), To: "box-api"},
		{Time: now.Add(-time.Minute), To: "box-api"},
		{Time: now.Add(-time.Minute), To: "box-api"},
		{Time: now.Add(-time.Minute), To: "mini-web"},
	}
	got := CountSince(ms, now.Add(-time.Hour))
	if got["box-api"] != 2 || got["mini-web"] != 1 || len(got) != 2 {
		t.Fatalf("counts = %v", got)
	}
}

func TestAHookAboutSendMessageNamesTheTargetAndLength(t *testing.T) {
	h := ParseHook(strings.NewReader(`{"session_id":"s","tool_name":"SendMessage","tool_input":{"to":"box-api","message":"Schema migration finished"}}`))
	if h.ToolName != "SendMessage" || h.SentTo != "box-api" || h.SentChars != len("Schema migration finished") {
		t.Fatalf("hook = %+v", h)
	}
}

func TestAHookAboutAnotherToolSendsNothing(t *testing.T) {
	h := ParseHook(strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`))
	if h.SentTo != "" {
		t.Fatalf("hook = %+v", h)
	}
}

func TestAFailedSendNamesItsError(t *testing.T) {
	failure := ParseHook(strings.NewReader(`{"hook_event_name":"PostToolUseFailure","tool_name":"SendMessage","tool_input":{"to":"pc-api","message":"Route moved"},"error":"no agent named pc-api is reachable"}`))
	if failure.SentTo != "pc-api" || failure.SendFailed != "no agent named pc-api is reachable" {
		t.Fatalf("failure hook = %+v", failure)
	}
	result := ParseHook(strings.NewReader(`{"hook_event_name":"PostToolUse","tool_name":"SendMessage","tool_input":{"to":"pc-api","message":"Route moved"},"tool_response":"Error: no agent named pc-api is reachable"}`))
	if result.SendFailed != "Error: no agent named pc-api is reachable" {
		t.Fatalf("result hook = %+v", result)
	}
	sent := ParseHook(strings.NewReader(`{"hook_event_name":"PostToolUse","tool_name":"SendMessage","tool_input":{"to":"pc-api","message":"Route moved"},"tool_response":{"delivered":true}}`))
	if sent.SendFailed != "" {
		t.Fatalf("sent hook = %+v", sent)
	}
}

func TestAFailedSendIsNotCounted(t *testing.T) {
	now := time.Now()
	got := CountSince([]Message{{Time: now, To: "pc-api", Failed: "unreachable"}, {Time: now, To: "pc-api"}}, now.Add(-time.Hour))
	if got["pc-api"] != 1 {
		t.Fatalf("counts = %v", got)
	}
}
