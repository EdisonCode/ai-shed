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
	closed       []backlog.Issue
	merged       map[int]string
	deployed     time.Time
	onStaging    map[string]bool
	closedSince  time.Time
	containCalls int
}

func (f *fakeGitHub) ClosedSince(_ context.Context, _ config.IssueSource, since time.Time) ([]backlog.Issue, error) {
	f.closedSince = since
	return f.closed, nil
}
func (f *fakeGitHub) MergedSince(context.Context, string, time.Time) (map[int]string, error) {
	return f.merged, nil
}
func (f *fakeGitHub) CommitTime(context.Context, string, string) (time.Time, error) {
	return f.deployed, nil
}
func (f *fakeGitHub) Contains(_ context.Context, _, head, commit string) (bool, error) {
	f.containCalls++
	return head == stagingCommit && f.onStaging[commit], nil
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
		onStaging: map[string]bool{"c41": true, "c43": true, "c44": true},
		closed: []backlog.Issue{
			handedBack(1, 41), // on staging, nobody looked
			handedBack(2, 42), // merged after the last staging deploy
			handedBack(3, 43, "Eyes checked: the total is right."),
			handedBack(4, 44, "## Hand-back\n**Eyes failed:** the total is blank\n**Decisions needed:** 1. fix now?"),
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
		if want := now.Add(-tc.want); !report.Since.Equal(want) || !gh.closedSince.Equal(want) {
			t.Errorf("%s: window starts %s (closed issues asked since %s), want %s", tc.name, report.Since, gh.closedSince, want)
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

func TestParseMergedKeepsWhatWasMergedInTheWindow(t *testing.T) {
	data := `[{"number":41,"mergedAt":"2026-10-04T10:00:00Z","mergeCommit":{"oid":"c41"}},
	          {"number":40,"mergedAt":"2026-10-04T01:00:00Z","mergeCommit":{"oid":"c40"}}]`
	merged, err := parseMerged([]byte(data), time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC))
	if err != nil || len(merged) != 1 || merged[41] != "c41" {
		t.Fatalf("merged = %v, %v", merged, err)
	}
}
