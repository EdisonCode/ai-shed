package runlog

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/pricing"
)

func writeLedger(t *testing.T, dir, worker string, calls ...Usage) {
	t.Helper()
	found := map[string]Usage{}
	for _, u := range calls {
		found[u.ID] = u
	}
	if err := appendUsage(dir, worker, found); err != nil {
		t.Fatal(err)
	}
}

func TestSummaryTotalsTheWindowByWorkerAndModel(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	since := now.Add(-7 * 24 * time.Hour)
	writeLedger(t, dir, "app",
		Usage{ID: "old", Time: since.Add(-time.Minute), Model: "claude-opus-5-5", Output: 1_000_000},
		Usage{ID: "a", Time: since.Add(time.Hour), Model: "claude-opus-5-5", Output: 1_000_000},
		Usage{ID: "b", Time: now.Add(-time.Hour), Model: "claude-sonnet-5-5-20260901", Input: 1_000_000},
		Usage{ID: "c", Time: now.Add(-time.Hour), Model: "local", Output: 10},
	)
	writeLedger(t, dir, "docs", Usage{ID: "d", Time: now.Add(-time.Hour), Model: "claude-haiku-5-5", Output: 1_000_000})
	if err := RecordPRCost(dir, PRCost{Repo: "org/app", PR: 41, Issue: 12, Worker: "app", Dollars: 3}); err != nil {
		t.Fatal(err)
	}

	s, err := Summarize(dir, since, now, pricing.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Workers) != 2 || s.Workers[0].Worker != "app" || s.Workers[1].Worker != "docs" {
		t.Fatalf("workers = %+v", s.Workers)
	}
	app := s.Workers[0]
	if app.Models["opus-5-5"].Output != 1_000_000 || app.Models["sonnet-5-5"].Input != 1_000_000 || app.Models["local"].Output != 10 {
		t.Fatalf("app = %+v, want the window's calls only, by short model name", app.Models)
	}
	if math.Abs(app.Dollars-22) > 1e-9 || strings.Join(app.Unpriced, ",") != "local" {
		t.Fatalf("app dollars = %v, unpriced %v; want 20 + 2, local unpriced", app.Dollars, app.Unpriced)
	}
	if len(s.PRs) != 1 || s.PRs[0].PR != 41 {
		t.Fatalf("pull requests = %+v", s.PRs)
	}
}

func TestSummaryOfAMachineWithNoLedgerIsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, UsageDir), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Summarize(dir, time.Time{}, time.Now(), pricing.Default())
	if err != nil || len(s.Workers) != 0 || len(s.PRs) != 0 {
		t.Fatalf("summary = %+v, err = %v", s, err)
	}
}

func TestShortModelDropsTheVendorTheDateAndTheContextSuffix(t *testing.T) {
	for in, want := range map[string]string{"claude-sonnet-4-5-20250929": "sonnet-4-5", "claude-opus-5-5[1m]": "opus-5-5", "local": "local"} {
		if got := ShortModel(in); got != want {
			t.Errorf("ShortModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSummarySplitsTheSpendByTheKindOfItemInHand(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	for _, c := range []Checkin{
		{Time: at(0), Worker: "app", Verdict: VerdictStarted, Sent: true},
		{Time: at(10), Worker: "app", Verdict: VerdictNudge, Sent: true, Issue: 12},            // built, PR #41
		{Time: at(20), Worker: "app", Verdict: VerdictNudge, Sent: true, Issue: 13},            // plan only
		{Time: at(30), Worker: "app", Verdict: VerdictNudge, Sent: true, Issue: 7, Look: true}, // eye check
		{Time: at(40), Worker: "app", Verdict: VerdictNudge, Sent: true, Task: "t-1"},          // one-off task
		{Time: at(50), Worker: "app", Verdict: VerdictDone},                                    // rests
		{Time: at(60), Worker: "app", Verdict: VerdictNudge, Sent: true, Issue: 14},            // built, PR #42
	} {
		if err := AppendCheckin(dir, c); err != nil {
			t.Fatal(err)
		}
	}
	million := func(id string, m int) Usage {
		return Usage{ID: id, Time: at(m), Model: "claude-haiku-5-5", Output: 1_000_000} // $5 each at haiku's output rate
	}
	writeLedger(t, dir, "app", million("before", 5), million("pr", 12), million("pr2", 15), million("plan", 25),
		million("look", 35), million("task", 45), million("resting", 55), million("pr3", 65))
	writeLedger(t, dir, ReviewerLedger, million("rev", 30))
	for _, c := range []PRCost{{Repo: "org/app", PR: 41, Issue: 12, Worker: "app"}, {Repo: "org/app", PR: 42, Issue: 14, Worker: "app"}} {
		if err := RecordPRCost(dir, c); err != nil {
			t.Fatal(err)
		}
	}

	s, err := Summarize(dir, t0, at(120), pricing.Default())
	if err != nil {
		t.Fatal(err)
	}
	var per float64
	for _, w := range s.Workers {
		if w.Worker == "app" {
			per = w.Models["haiku-5-5"].Dollars / 8 // the app ledger has eight calls of one price
		}
	}
	want := map[string][]float64{
		KindPR:     {2 * per, per},
		KindPlan:   {per},
		KindLook:   {per},
		KindTask:   {per},
		KindNoItem: {2 * per},
		KindReview: {per},
	}
	if len(s.Kinds) != len(want) {
		t.Fatalf("kinds = %+v, want %d kinds", s.Kinds, len(want))
	}
	for _, k := range s.Kinds {
		if w := want[k.Kind]; !sameDollars(k.Items, w) {
			t.Errorf("%s = %v, want %v", k.Kind, k.Items, w)
		}
	}
}

func sameDollars(got, want []float64) bool {
	if len(got) != len(want) {
		return false
	}
	var a, b float64
	for i := range got {
		a, b = a+got[i], b+want[i]
	}
	return math.Abs(a-b) < 1e-9
}
