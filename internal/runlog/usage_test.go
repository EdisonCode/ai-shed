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
	u, ok := usageFromLine([]byte(line), "")
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
