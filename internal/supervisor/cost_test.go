package supervisor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/pricing"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

func TestCostCommentNamesTheTokensAndSplitsAReworkPass(t *testing.T) {
	first := t0
	again := t0.Add(time.Hour)
	checkins := []runlog.Checkin{
		{Time: first, Worker: "app", Sent: true, Issue: 12, Verdict: runlog.VerdictNudge},
		{Time: first.Add(30 * time.Minute), Worker: "app", Verdict: runlog.VerdictDone},
		{Time: again, Worker: "app", Sent: true, Issue: 12, Verdict: runlog.VerdictNudge},
		{Time: first.Add(time.Minute), Worker: "docs", Sent: true, Issue: 12, Verdict: runlog.VerdictNudge},
	}
	usage := []runlog.Usage{
		{ID: "a", Time: first.Add(time.Minute), Model: "claude-sonnet-4-5-20250929", Input: 180000, Output: 85000, CacheWrite: 40000, CacheRead: 980000},
		{ID: "b", Time: again.Add(time.Minute), Model: "claude-sonnet-4-5-20250929", Input: 2000, Output: 500, CacheWrite: 1000, CacheRead: 3000},
		// Before #12 was handed over, and after it was handed on: neither is this issue.
		{ID: "before", Time: first.Add(-time.Hour), Model: "claude-opus-4-5", Input: 99999, Output: 99999},
	}
	checkins = append(checkins, runlog.Checkin{Time: again.Add(30 * time.Minute), Worker: "app", Sent: true, Issue: 13, Verdict: runlog.VerdictNudge})
	usage = append(usage, runlog.Usage{ID: "c", Time: again.Add(31 * time.Minute), Model: "claude-sonnet-4-5", Input: 50, Output: 50})

	body, ok := formatCost(12, checkins, "app", usage, nil)
	if !ok {
		t.Fatal("no comment")
	}
	for _, want := range []string{
		"<!-- shed-cost -->",
		"## Cost",
		"This pass — sonnet-4-5 — uncached input 2k, cache write 1k, cache read 3k, output 500",
		"Total — sonnet-4-5 — uncached input 182k, cache write 41k, cache read 983k, output 85.5k",
		"From the session transcript.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "opus") || strings.Contains(body, "uncached input 50") {
		t.Fatalf("another issue's tokens were included:\n%s", body)
	}

	one, ok := formatCost(12, checkins[:1], "app", usage[:1], nil)
	if !ok || strings.Contains(one, "This pass") || strings.Contains(one, "Total") {
		t.Fatalf("a single pass should be one line:\n%s", one)
	}
	if !strings.Contains(one, "sonnet-4-5 — uncached input 180k, cache write 40k, cache read 980k, output 85k") {
		t.Fatalf("body:\n%s", one)
	}
	if _, ok := formatCost(12, nil, "app", usage, nil); ok {
		t.Fatal("an issue that was never handed over has a cost")
	}
}

func TestCostCommentPricesEachModelAtTheAPIRates(t *testing.T) {
	checkins := []runlog.Checkin{{Time: t0, Worker: "app", Sent: true, Issue: 12, Verdict: runlog.VerdictNudge}}
	usage := []runlog.Usage{
		{ID: "a", Time: t0.Add(time.Minute), Model: "claude-sonnet-4-5-20250929", Input: 180000, Output: 85000, CacheWrite: 40000, CacheRead: 980000},
		{ID: "b", Time: t0.Add(2 * time.Minute), Model: "some-local-model", Output: 10},
	}
	body, ok := formatCost(12, checkins, "app", usage, pricing.Default())
	if !ok {
		t.Fatal("no comment")
	}
	for _, want := range []string{
		"sonnet-4-5 — uncached input 180k, cache write 40k, cache read 980k, output 85k — $2.26",
		"some-local-model — uncached input 0, cache write 0, cache read 0, output 10 — no published rate",
		"At API rates: $2.26, leaving out some-local-model. A subscription does not pay this price; it weighs the kinds of token as the published rates do.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
}

func TestCostCommentSplitsTheWorkerSessionFromItsSubAgents(t *testing.T) {
	checkins := []runlog.Checkin{{Time: t0, Worker: "app", Sent: true, Issue: 12, Verdict: runlog.VerdictNudge}}
	usage := []runlog.Usage{
		{ID: "a", Time: t0.Add(time.Minute), Model: "claude-opus-5-5", Output: 100_000, Dispatches: 2},
		{ID: "b", Time: t0.Add(2 * time.Minute), Model: "claude-sonnet-5-5", Output: 100_000, Agent: "implementer"},
		{ID: "c", Time: t0.Add(3 * time.Minute), Model: "claude-opus-5-5", Output: 50_000, Agent: "implementer"},
		{ID: "d", Time: t0.Add(4 * time.Minute), Model: "claude-haiku-5-5", Output: 1_000_000, Agent: "Explore"},
	}
	body, _ := formatCost(12, checkins, "app", usage, pricing.Default())
	if want := "By agent: worker session $2.00 (opus-5-5); implementer $2.00 (opus-5-5, sonnet-5-5); Explore $0.50 (haiku-5-5)"; !strings.Contains(body, want) {
		t.Fatalf("body lacks %q:\n%s", want, body)
	}
	if strings.Contains(body, "partial") {
		t.Fatalf("sub-agent tokens were recorded, yet the count is called partial:\n%s", body)
	}
}

func TestCostCommentSaysWhenSubAgentsRanAndWereNotCounted(t *testing.T) {
	checkins := []runlog.Checkin{{Time: t0, Worker: "app", Sent: true, Issue: 12, Verdict: runlog.VerdictNudge}}
	usage := []runlog.Usage{{ID: "a", Time: t0.Add(time.Minute), Model: "claude-opus-5-5", Output: 1000, Dispatches: 3}}
	body, _ := formatCost(12, checkins, "app", usage, nil)
	if want := "3 sub-agent(s) ran and none of their tokens were recorded: this count is partial."; !strings.Contains(body, want) {
		t.Fatalf("body lacks %q:\n%s", want, body)
	}
}

func TestCostCommentFlagsSubAgentsThatRanOffTheIssuesModel(t *testing.T) {
	checkins := []runlog.Checkin{{Time: t0, Worker: "app", Sent: true, Issue: 12, Model: "sonnet", Verdict: runlog.VerdictNudge}}
	usage := []runlog.Usage{
		{ID: "a", Time: t0.Add(time.Minute), Model: "claude-opus-5-5", Output: 1000},
		{ID: "b", Time: t0.Add(2 * time.Minute), Model: "claude-opus-5-5", Output: 1000, Agent: "implementer"},
		{ID: "c", Time: t0.Add(3 * time.Minute), Model: "claude-sonnet-5-5", Output: 1000, Agent: "general-purpose"},
	}
	body, _ := formatCost(12, checkins, "app", usage, nil)
	if want := "Ran on another model than the issue's (sonnet): implementer (opus-5-5)."; !strings.Contains(body, want) {
		t.Fatalf("body lacks %q:\n%s", want, body)
	}
	usage[1].Model = "claude-sonnet-5-5"
	if body, _ := formatCost(12, checkins, "app", usage, nil); strings.Contains(body, "another model") {
		t.Fatalf("every sub-agent ran on the issue's model, yet:\n%s", body)
	}
}

func TestCostCommentGivesTheRatioOfCacheReadsToOutput(t *testing.T) {
	checkins := []runlog.Checkin{{Time: t0, Worker: "app", Sent: true, Issue: 12, Verdict: runlog.VerdictNudge}}
	usage := []runlog.Usage{
		{ID: "a", Time: t0.Add(time.Minute), Model: "claude-opus-5-5", CacheRead: 15_700_000, Output: 52_000},
		{ID: "b", Time: t0.Add(2 * time.Minute), Model: "claude-sonnet-5-5", CacheRead: 300_000, Output: 8_000},
	}
	body, _ := formatCost(12, checkins, "app", usage, nil)
	if want := "Cache read to output: 267 to 1."; !strings.Contains(body, want) {
		t.Fatalf("body lacks %q:\n%s", want, body)
	}
}

type fakeCost struct {
	repos  []string
	prs    []int
	bodies []string
}

func (f *fakeCost) PostCost(_ context.Context, repo string, pr int, body string) error {
	f.repos = append(f.repos, repo)
	f.prs = append(f.prs, pr)
	f.bodies = append(f.bodies, body)
	return nil
}

func TestHandBackPostsTheTokenCostOnce(t *testing.T) {
	cost := &fakeCost{}
	f := started(t, config.Supervisor{}, assign(12), onTrack, onTrack)
	f.Cost = cost
	f.lister.issues[0].Comments = []backlog.Comment{{Body: "## Hand-back\n**PR:** #41 (ready)\n**Decisions needed:** none"}}

	f.tick(2 * time.Minute) // #12 is handed over; no calls recorded yet
	if len(cost.bodies) != 0 {
		t.Fatalf("posted before any usage: %q", cost.bodies)
	}

	if err := runlog.RecordTranscript(f.StateDir, "app", writeUsageTranscript(t, f.now), "sess"); err != nil {
		t.Fatal(err)
	}
	f.term.running(f.now)
	f.tick(2 * time.Minute) // on track, the hand-back names #41
	if len(cost.bodies) != 1 || cost.repos[0] != "org/app" || cost.prs[0] != 41 {
		t.Fatalf("posted %v %v\n%s", cost.repos, cost.prs, cost.bodies)
	}
	if !strings.Contains(cost.bodies[0], "sonnet-4-5 — uncached input 180k, cache write 40k, cache read 980k, output 85k") {
		t.Fatalf("body:\n%s", cost.bodies[0])
	}

	costs, err := runlog.ReadPRCosts(f.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) != 1 || costs[0].Repo != "org/app" || costs[0].PR != 41 || costs[0].Issue != 12 || costs[0].Worker != "app" || costs[0].Models["sonnet-4-5"].Output != 85000 {
		t.Fatalf("recorded = %+v, want the cost of #41 for the report", costs)
	}

	f.term.running(f.now)
	f.tick(2 * time.Minute) // the same calls: the comment is left as it is
	if len(cost.bodies) != 1 {
		t.Fatalf("posted %d times with nothing new", len(cost.bodies))
	}
}

func writeUsageTranscript(t *testing.T, at time.Time) string {
	t.Helper()
	path := t.TempDir() + "/session.jsonl"
	line := `{"type":"assistant","uuid":"a1","timestamp":"` + at.Add(time.Second).UTC().Format(time.RFC3339) + `","message":{"id":"m1","model":"claude-sonnet-4-5-20250929","usage":{"input_tokens":180000,"output_tokens":85000,"cache_creation_input_tokens":40000,"cache_read_input_tokens":980000}}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
