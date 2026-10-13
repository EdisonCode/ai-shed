package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTranscriptUsageIsRecordedOnceAndSidechainsAreSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	body := strings.Join([]string{
		`{"type":"user","uuid":"u1","message":{"role":"user"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-10-12T15:00:00Z","isSidechain":true,"message":{"id":"side","model":"claude-haiku-4-5","usage":{"input_tokens":9,"output_tokens":9}}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-10-12T15:01:00Z","message":{"id":"m1","model":"claude-sonnet-4-5","usage":{"input_tokens":180,"output_tokens":85,"cache_creation_input_tokens":40,"cache_read_input_tokens":980}}}`,
		`{"type":"assistant","uuid":"a2b","timestamp":"2026-10-12T15:01:01Z","message":{"id":"m1","model":"claude-sonnet-4-5","usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"assistant","uuid":"a3","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":0,"output_tokens":0}}}`,
		`not json`,
	}, "\n")
	if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RecordTranscript(dir, "app", path, "sess"); err != nil {
		t.Fatal(err)
	}
	if err := RecordTranscript(dir, "app", path, "sess"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadUsage(dir, "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("usage = %+v, want the one billed call", got)
	}
	u := got[0]
	if u.ID != "m1" || u.Session != "sess" || u.Model != "claude-sonnet-4-5" || u.Input != 180 || u.Output != 85 || u.CacheWrite != 40 || u.CacheRead != 980 {
		t.Fatalf("usage = %+v", u)
	}
	if !u.Time.Equal(time.Date(2026, 10, 12, 15, 1, 0, 0, time.UTC)) {
		t.Fatalf("time = %s", u.Time)
	}

	// The turn that was still being written when the hook first ran.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"assistant","uuid":"a4","timestamp":"2026-10-12T15:02:00Z","message":{"id":"m2","model":"claude-sonnet-4-5","usage":{"output_tokens":7}}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := RefreshUsage(dir, "app"); err != nil {
		t.Fatal(err)
	}
	got, err = ReadUsage(dir, "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].ID != "m2" || got[1].Output != 7 {
		t.Fatalf("after refresh = %+v", got)
	}
}

func TestCacheWritesForAnHourAreKeptApart(t *testing.T) {
	line := `{"type":"assistant","uuid":"a","timestamp":"2026-10-12T15:00:00Z","message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":5,"output_tokens":7,"cache_creation_input_tokens":300,"cache_read_input_tokens":900,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200}}}}`
	u, _, ok := usageFromLine([]byte(line), "", "")
	if !ok || u.CacheWrite != 300 || u.CacheWrite1h != 200 {
		t.Fatalf("usage = %+v, want 300 written, 200 of it for an hour", u)
	}
	if got := u.Tokens(); got.CacheWrite != 300 || got.CacheWrite1h != 200 || got.Input != 5 || got.Output != 7 || got.CacheRead != 900 {
		t.Fatalf("tokens = %+v", got)
	}
}

func TestOverlongTranscriptLinesAreSkipped(t *testing.T) {
	var got []string
	input := "short\n" + strings.Repeat("x", 100) + "\n" + strings.Repeat("y", 70*1024) + "\nafter\n"
	if err := scanLines(strings.NewReader(input), 20, func(line []byte) {
		got = append(got, string(line))
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "short" || got[1] != "after" {
		t.Fatalf("lines = %q", got)
	}
}

func TestParseHookReadsTheTranscriptPath(t *testing.T) {
	got := ParseHook(strings.NewReader(`{"session_id":"sess","transcript_path":"/tmp/s.jsonl","message":"allow?"}`))
	if got.SessionID != "sess" || got.TranscriptPath != "/tmp/s.jsonl" || got.Message != "allow?" {
		t.Fatalf("hook = %+v", got)
	}
	if got := ParseHook(strings.NewReader("not json")); got != (Hook{}) {
		t.Fatalf("hook = %+v, want none", got)
	}
}

func TestParseHookReadsTheSubAgentsOwnTranscript(t *testing.T) {
	got := ParseHook(strings.NewReader(`{"session_id":"s","transcript_path":"/p.jsonl","agent_type":"Explore","agent_transcript_path":"/s/subagents/agent-1.jsonl"}`))
	if got.AgentTranscriptPath != "/s/subagents/agent-1.jsonl" || got.AgentType != "Explore" || got.TranscriptPath != "/p.jsonl" {
		t.Fatalf("hook = %+v", got)
	}
}

func TestSubAgentTranscriptIsCountedAndTaggedWithItsType(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-1.jsonl")
	// Every row of a sub-agent's own transcript is a sidechain.
	line := `{"type":"assistant","isSidechain":true,"timestamp":"2026-10-12T15:00:00Z","message":{"id":"sub1","model":"claude-haiku-5-5","usage":{"input_tokens":2,"output_tokens":3,"cache_creation_input_tokens":36388}}}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RecordAgentTranscript(dir, "app", path, "s", "Explore"); err != nil {
		t.Fatal(err)
	}
	if err := RefreshUsage(dir, "app"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadUsage(dir, "app")
	if err != nil || len(got) != 1 || got[0].Agent != "Explore" || got[0].CacheWrite != 36388 {
		t.Fatalf("usage = %+v, %v; want the sub-agent's call, tagged Explore", got, err)
	}
}

func TestSubAgentTypeFallsBackToItsMetaFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-2.jsonl")
	line := `{"type":"assistant","isSidechain":true,"timestamp":"2026-10-12T15:00:00Z","message":{"id":"sub2","model":"claude-sonnet-5-5","usage":{"output_tokens":3}}}`
	os.WriteFile(path, []byte(line+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "agent-2.meta.json"), []byte(`{"agentType":"implementer","description":"x"}`), 0o644)
	if err := RecordAgentTranscript(dir, "app", path, "s", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadUsage(dir, "app"); len(got) != 1 || got[0].Agent != "implementer" {
		t.Fatalf("usage = %+v", got)
	}
}

func TestParentCallsCountTheSubAgentsTheyDispatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	body := strings.Join([]string{
		`{"type":"assistant","timestamp":"2026-10-12T15:00:00Z","message":{"id":"p1","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t1","name":"Agent","input":{"subagent_type":"Explore"}}],"usage":{"output_tokens":5}}}`,
		`{"type":"assistant","timestamp":"2026-10-12T15:00:00Z","message":{"id":"p1","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t2","name":"Task","input":{"subagent_type":"general-purpose"}}],"usage":{"output_tokens":5}}}`,
		`{"type":"assistant","timestamp":"2026-10-12T15:01:00Z","message":{"id":"p2","model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t3","name":"Bash","input":{}}],"usage":{"output_tokens":5}}}`,
	}, "\n")
	os.WriteFile(path, []byte(body+"\n"), 0o644)
	if err := RecordTranscript(dir, "app", path, "s"); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadUsage(dir, "app")
	if len(got) != 2 || got[0].Dispatches != 2 || got[1].Dispatches != 0 || got[0].Agent != "" {
		t.Fatalf("usage = %+v, want two dispatches on p1, none on p2", got)
	}
}
