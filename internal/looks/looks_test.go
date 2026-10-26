package looks

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

var now = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

const (
	stagingCommit    = "aaaaaaa"
	productionCommit = "bbbbbbb"
)

// fakeGitHub has pull requests merged at known times. Staging serves the
// merge commits listed in onStaging.
type fakeGitHub struct {
	closed    []backlog.Issue
	merged    map[int]string
	deployed  time.Time
	onStaging map[string]bool
	// early are the pull requests merged before production's deploy, and
	// inProduction the merge commits production serves.
	early        map[int]bool
	inProduction map[string]bool
	closedSince  time.Time
	containCalls int
	containErr   error
}

func (f *fakeGitHub) ClosedSince(_ context.Context, _ config.IssueSource, since time.Time) ([]backlog.Issue, error) {
	f.closedSince = since
	return f.closed, nil
}
func (f *fakeGitHub) MergedSince(context.Context, string, time.Time) (map[int]Pull, error) {
	pulls := map[int]Pull{}
	// Merged in number order, as merges into one branch are.
	for number, commit := range f.merged {
		at := now.Add(-time.Duration(1000-number) * time.Second)
		if f.early[number] {
			at = f.deployed.Add(-time.Hour).Add(time.Duration(number) * time.Second)
		}
		pulls[number] = Pull{Number: number, Commit: commit, MergedAt: at}
	}
	return pulls, nil
}
func (f *fakeGitHub) CommitTime(context.Context, string, string) (time.Time, error) {
	return f.deployed, nil
}
func (f *fakeGitHub) Contains(_ context.Context, _, head, commit string) (bool, error) {
	f.containCalls++
	if f.containErr != nil {
		return false, f.containErr
	}
	return (head == stagingCommit && f.onStaging[commit]) || (head == productionCommit && f.inProduction[commit]), nil
}

func handedBack(number, pr int, more ...string) backlog.Issue {
	i := backlog.Issue{Repo: "org/app", Number: number, Title: "Fix the thing"}
	bodies := append([]string{"## Hand-back\n**PR:** #" + strconv.Itoa(pr) + " (ready)\n**Needs eyes:** open /orders and check the total"}, more...)
	for n, body := range bodies {
		i.Comments = append(i.Comments, backlog.Comment{Body: body, CreatedAt: now.Add(time.Duration(n-number) * time.Hour)})
	}
	return i
}

var (
	repo    = config.Repo{Name: "org/app", Staging: config.Environment{Commit: "staging"}, Production: config.Environment{Commit: "production"}}
	sources = []config.IssueSource{{Repo: "org/app", Label: "machine:box"}, {Repo: "org/other", Label: "machine:box"}}
)

func reader(gh *fakeGitHub) *Reader {
	return &Reader{GitHub: gh, Now: func() time.Time { return now }, Run: func(_ context.Context, command string) (string, error) {
		return map[string]string{"staging": stagingCommit + "\n", "production": productionCommit + "\n"}[command], nil
	}}
}

func states(r Report) map[int]string {
	got := map[int]string{}
	for _, l := range r.Looks {
		got[l.Number] = l.State
	}
	return got
}

func TestMergedWorkOwesALookUntilSomeoneLooked(t *testing.T) {
	gh := &fakeGitHub{deployed: now.Add(-48 * time.Hour),
		merged:    map[int]string{41: "c41", 42: "c42", 43: "c43", 44: "c44", 45: "c45"},
		onStaging: map[string]bool{"c41": true, "c42": true, "c43": true},
		closed: []backlog.Issue{
			handedBack(1, 41), // on staging, nobody looked
			handedBack(2, 44), // merged after the last staging deploy
			handedBack(3, 42, "Eyes checked: the total is right."),
			handedBack(4, 43, "## Hand-back\n**Eyes failed:** the total is blank\n**Decisions needed:** 1. fix now?"),
			handedBack(6, 46), // merged before production's commit: already shipped
		}}
	open := []backlog.Issue{handedBack(5, 45), {Repo: "org/other", Number: 9}}

	report, err := reader(gh).Read(context.Background(), repo, sources, open, config.DefaultSignals())
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{1: Due, 2: Awaiting, 4: Failed, 5: Awaiting}
	if got := states(report); len(got) != len(want) || got[1] != Due || got[2] != Awaiting || got[4] != Failed || got[5] != Awaiting {
		t.Fatalf("looks = %v, want %v", got, want)
	}
	if report.Merged != 5 || report.Staging != stagingCommit || report.Production != productionCommit {
		t.Fatalf("report = %+v", report)
	}
	if len(report.Passed) != 1 || report.Passed[0].Number != 3 || report.Passed[0].State != Passed {
		t.Fatalf("passed = %+v, want the look of #3", report.Passed)
	}
	if report.Looks[0].Number != 5 {
		t.Fatalf("first look is #%d, want the one asked for longest ago", report.Looks[0].Number)
	}
}

func TestWindowStartsAtProductionsCommitOrTheLookBackLimit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deployed time.Duration
		want     time.Duration
	}{
		{"production was deployed two days ago", 48 * time.Hour, 48 * time.Hour},
		{"production is a month old: the limit applies", 30 * 24 * time.Hour, config.DefaultLookBack},
	} {
		gh := &fakeGitHub{deployed: now.Add(-tc.deployed)}
		report, err := reader(gh).Read(context.Background(), repo, sources, nil, config.DefaultSignals())
		if err != nil {
			t.Fatal(err)
		}
		if want := now.Add(-tc.want); !report.Since.Equal(want) || !gh.closedSince.Equal(now.Add(-config.DefaultLookBack)) || report.SinceProduction != (tc.want == tc.deployed) {
			t.Errorf("%s: window starts %s (closed issues asked since %s), want %s, and closed issues from the look-back limit", tc.name, report.Since, gh.closedSince, want)
		}
	}
}

func TestWithoutAProductionCommandTheWindowIsTheLookBack(t *testing.T) {
	stagingOnly := repo
	stagingOnly.Production = config.Environment{}
	stagingOnly.LookBack = "72h"
	report, err := reader(&fakeGitHub{}).Read(context.Background(), stagingOnly, sources, nil, config.DefaultSignals())
	if err != nil {
		t.Fatal(err)
	}
	if report.Production != "" || !report.Since.Equal(now.Add(-72*time.Hour)) {
		t.Fatalf("report = %+v", report)
	}
}

func TestCommitCommandThatDoesNotPrintACommitIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		err       error
		want      string
	}{
		{"a page, not a hash", `{"status":"ok"}`, nil, "not a commit hash"},
		{"the command failed", "", errors.New("exit status 7"), "read staging's commit: exit status 7"},
	} {
		r := reader(&fakeGitHub{})
		r.Run = func(context.Context, string) (string, error) { return tc.out, tc.err }
		if _, err := r.Read(context.Background(), repo, sources, nil, config.DefaultSignals()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestWhatStagingContainsIsAskedOnce(t *testing.T) {
	gh := &fakeGitHub{deployed: now.Add(-48 * time.Hour), merged: map[int]string{41: "c41"}, closed: []backlog.Issue{handedBack(1, 41)}}
	r := reader(gh)
	for range 3 {
		if _, err := r.Read(context.Background(), repo, sources, nil, config.DefaultSignals()); err != nil {
			t.Fatal(err)
		}
	}
	if gh.containCalls != 1 {
		t.Fatalf("asked %d times whether staging has the commit, want 1", gh.containCalls)
	}
}

func TestStagingThatCannotBeAskedLeavesTheRestOfTheReport(t *testing.T) {
	gh := &fakeGitHub{deployed: now.Add(-48 * time.Hour), merged: map[int]string{41: "c41", 44: "c44"},
		containErr: errors.New("gh api compare: timed out at shed's deadline after 20s"),
		closed: []backlog.Issue{
			handedBack(1, 41),
			handedBack(4, 44, "## Hand-back\n**Eyes failed:** the total is blank"),
		}}
	report, err := reader(gh).Read(context.Background(), repo, sources, nil, config.DefaultSignals())
	if err != nil {
		t.Fatalf("err = %v, want the report without what staging serves", err)
	}
	if got := states(report); report.Merged != 2 || got[1] != Awaiting || got[4] != Failed {
		t.Fatalf("merged %d, looks %v: want 2 merged, #1 awaiting and #4 failed", report.Merged, got)
	}
	if !strings.Contains(report.Unchecked, "timed out") {
		t.Fatalf("unchecked = %q, want the reason staging could not be asked", report.Unchecked)
	}
}

func TestACallKilledAtTheDeadlineSaysSo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	err := ghError(ctx, []string{"api", "repos/org/app/compare/a...b"}, errors.New("signal: killed"), "", 3200*time.Millisecond)
	if want := "timed out at shed's deadline after 3.2s"; !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to say %q", err, want)
	}
}

func TestParseMergedKeepsWhatWasMergedInTheWindow(t *testing.T) {
	data := `[{"number":41,"title":"Retry the export","mergedAt":"2026-10-04T10:00:00Z","mergeCommit":{"oid":"c41"}},
	          {"number":40,"mergedAt":"2026-10-04T01:00:00Z","mergeCommit":{"oid":"c40"}}]`
	merged, err := parseMerged([]byte(data), time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	if err != nil || len(merged) != 1 || merged[41].Commit != "c41" || merged[41].Title != "Retry the export" {
		t.Fatalf("merged = %v, %v", merged, err)
	}
}

func TestLookThatCouldNotBeDoneIsBlockedUntilTheOwnerClearsIt(t *testing.T) {
	gh := &fakeGitHub{deployed: now.Add(-48 * time.Hour),
		merged:    map[int]string{41: "c41", 42: "c42"},
		onStaging: map[string]bool{"c41": true, "c42": true},
		closed: []backlog.Issue{
			handedBack(1, 41, "Eyes blocked: staging asks for a sign-in I may not type."),
			handedBack(2, 42, "Eyes blocked: staging was down.", "Eyes unblocked: it is up again."),
		}}
	report, err := reader(gh).Read(context.Background(), repo, sources, nil, config.DefaultSignals())
	if err != nil {
		t.Fatal(err)
	}
	if got := states(report); len(got) != 2 || got[1] != Blocked || got[2] != Due {
		t.Fatalf("looks = %v, want #1 blocked and #2 due again", got)
	}
	for _, l := range report.Looks {
		if l.Number == 1 && l.Note != "staging asks for a sign-in I may not type." {
			t.Fatalf("note = %q", l.Note)
		}
	}
	if len(report.Passed) != 0 {
		t.Fatalf("passed = %+v; a blocked look has not passed", report.Passed)
	}
}

func TestParsePullTimesReadsEachStepOfEachPullRequest(t *testing.T) {
	data := `{"data":{"repository":{
		"p64":{"createdAt":"2026-10-13T04:42:54Z","mergedAt":"2026-10-13T04:44:18Z",
			"first":{"nodes":[{"commit":{"authoredDate":"2026-10-13T04:36:24Z"}}]},
			"head":{"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"nodes":[
				{"__typename":"CheckRun","startedAt":"2026-10-13T04:43:05Z","completedAt":"2026-10-13T04:43:50Z"},
				{"__typename":"StatusContext","createdAt":"2026-10-13T04:44:03Z"},
				{"__typename":"CheckRun","startedAt":"2026-10-13T04:43:01Z","completedAt":null}]}}}}]},
			"timelineItems":{"nodes":[]}},
		"p65":{"createdAt":"2026-10-13T05:00:00Z","mergedAt":"2026-10-13T07:00:00Z",
			"first":{"nodes":[{"commit":{"authoredDate":"2026-10-13T04:50:00Z"}}]},
			"head":{"nodes":[{"commit":{"statusCheckRollup":null}}]},
			"timelineItems":{"nodes":[{"createdAt":"2026-10-13T06:00:00Z"}]}}}}}`
	times, err := parsePullTimes([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	want := map[int]PullTimes{
		64: {FirstCommit: at("2026-10-13T04:36:24Z"), Ready: at("2026-10-13T04:42:54Z"),
			ChecksStarted: at("2026-10-13T04:43:01Z"), ChecksDone: at("2026-10-13T04:44:03Z"), Merged: at("2026-10-13T04:44:18Z")},
		65: {FirstCommit: at("2026-10-13T04:50:00Z"), Ready: at("2026-10-13T06:00:00Z"), Merged: at("2026-10-13T07:00:00Z")},
	}
	if len(times) != len(want) {
		t.Fatalf("times = %+v, want %+v", times, want)
	}
	for n, w := range want {
		if times[n] != w {
			t.Errorf("#%d = %+v, want %+v", n, times[n], w)
		}
	}
}

func TestALookStillAskedOfWorkProductionServesIsOwedAfterTheFact(t *testing.T) {
	gh := &fakeGitHub{deployed: now.Add(-2 * time.Hour),
		merged:       map[int]string{41: "c41", 42: "c42", 43: "c43"},
		early:        map[int]bool{41: true, 42: true, 43: true},
		onStaging:    map[string]bool{"c41": true, "c42": true, "c43": true},
		inProduction: map[string]bool{"c41": true, "c42": true},
	}
	open := []backlog.Issue{
		handedBack(1, 41), // shipped, nobody looked
		handedBack(2, 42, "Eyes checked: the total is right."), // shipped and looked at
		handedBack(3, 43), // merged before the deploy, not in it
	}
	report, err := reader(gh).Read(context.Background(), repo, sources, open, config.DefaultSignals())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Looks) != 1 || report.Looks[0].Number != 1 || report.Looks[0].State != Due || !report.Looks[0].Production {
		t.Fatalf("looks = %+v, want only #1, due after the fact", report.Looks)
	}
	if report.Merged != 0 || len(report.Passed) != 0 {
		t.Fatalf("merged = %d, passed = %+v: work before the window is not counted in it", report.Merged, report.Passed)
	}
}

// ancestry answers whether head has a commit from a fixed set, and counts
// and can fail its calls.
type ancestry struct {
	has   map[string]bool
	calls int
	fail  string // a commit whose comparison fails
}

func (a *ancestry) contains(_ context.Context, _, _, commit string) (bool, error) {
	a.calls++
	if commit == a.fail {
		return false, errors.New("gh api compare: timed out at shed's deadline after 45s")
	}
	return a.has[commit], nil
}

func mergedInOrder(base string, from, to int) map[int]Pull {
	pulls := map[int]Pull{}
	for n := from; n <= to; n++ {
		pulls[n] = Pull{Number: n, Commit: base + strconv.Itoa(n), Base: base, MergedAt: now.Add(time.Duration(n) * time.Minute)}
	}
	return pulls
}

func TestHistoryOfHundredsOfMergesTakesAFewComparisons(t *testing.T) {
	merged := mergedInOrder("main", 1, 300)
	a := &ancestry{has: map[string]bool{}}
	for n := 1; n <= 210; n++ {
		a.has["main"+strconv.Itoa(n)] = true
	}
	has, err := History(context.Background(), a.contains, "org/app", stagingCommit, merged)
	if err != nil {
		t.Fatal(err)
	}
	if a.calls > 10 {
		t.Fatalf("%d comparisons for 300 merges, want a binary search's few", a.calls)
	}
	if len(has) != 300 || !has["main1"] || !has["main210"] || has["main211"] || has["main300"] {
		t.Fatalf("history = %d merges, main210 %v, main211 %v", len(has), has["main210"], has["main211"])
	}
}

func TestAComparisonThatFailsLeavesOnlyTheMergesItWouldHaveSettledUnknown(t *testing.T) {
	merged := mergedInOrder("main", 1, 8)
	for n, p := range mergedInOrder("release", 11, 12) {
		merged[n] = p
	}
	// The search of main asks about main5 first.
	a := &ancestry{has: map[string]bool{"main1": true, "release11": true}, fail: "main5"}
	has, err := History(context.Background(), a.contains, "org/app", stagingCommit, merged)
	if err == nil || !strings.Contains(err.Error(), "8 of 10 merge(s)") {
		t.Fatalf("err = %v, want it to say how many merges are unknown", err)
	}
	if _, known := has["main3"]; known {
		t.Fatalf("main3 is known after the comparison that would settle it failed")
	}
	if !has["release11"] || has["release12"] {
		t.Fatalf("history = %v: another branch's merges are still settled", has)
	}
}
