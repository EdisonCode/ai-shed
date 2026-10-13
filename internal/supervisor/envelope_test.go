package supervisor

import (
	"context"
	"strings"
	"testing"

	"github.com/edisoncode/ai-shed/internal/runlog"
)

const envelope = `{"type":"result","subtype":"success","is_error":false,"result":"{\"verdict\":\"on_track\",\"reason\":\"fine\"}","session_id":"s1","total_cost_usd":0.004,"modelUsage":{"claude-sonnet-5-5":{"inputTokens":1200,"outputTokens":80,"cacheReadInputTokens":30000,"cacheCreationInputTokens":500,"costUSD":0.004}}}`

func TestReviewerJSONEnvelopeIsUnwrappedAndItsUsageRecorded(t *testing.T) {
	dir := t.TempDir()
	r := CommandReviewer{Command: "cat >/dev/null; printf '%s' '" + envelope + "'", Dir: dir, UsageDir: dir}
	v, err := r.Review(context.Background(), "prompt")
	if err != nil || v.Verdict != "on_track" {
		t.Fatalf("verdict = %+v, %v", v, err)
	}
	got, err := runlog.ReadUsage(dir, runlog.ReviewerLedger)
	if err != nil || len(got) != 1 {
		t.Fatalf("reviewer usage = %+v, %v", got, err)
	}
	if u := got[0]; u.Model != "claude-sonnet-5-5" || u.Input != 1200 || u.Output != 80 || u.CacheRead != 30000 || u.CacheWrite != 500 || u.ID != "s1/claude-sonnet-5-5" {
		t.Fatalf("usage = %+v", u)
	}
}

func TestReviewerPlainTextIsUsedAsItIs(t *testing.T) {
	dir := t.TempDir()
	r := CommandReviewer{Command: `cat >/dev/null; echo '{"verdict":"done","reason":"r"}'`, Dir: dir, UsageDir: dir}
	if v, err := r.Review(context.Background(), "p"); err != nil || v.Verdict != "done" {
		t.Fatalf("verdict = %+v, %v", v, err)
	}
	if got, _ := runlog.ReadUsage(dir, runlog.ReviewerLedger); len(got) != 0 {
		t.Fatalf("usage recorded from plain text: %+v", got)
	}
}

func TestReviewerEnvelopeErrorNamesItsResult(t *testing.T) {
	failed := `{"type":"result","subtype":"success","is_error":true,"result":"Claude usage limit reached. Your limit will reset at 3pm.","session_id":"s2","modelUsage":{}}`
	r := CommandReviewer{Command: "cat >/dev/null; printf '%s' '" + failed + "'; exit 1", Dir: t.TempDir()}
	_, err := r.Ask(context.Background(), "p")
	if err == nil || !strings.Contains(err.Error(), "usage limit reached") || !isLimit(err) {
		t.Fatalf("err = %v, want the result text, read as a limit", err)
	}
}
