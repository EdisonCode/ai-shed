package backlog

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

func TestDependenciesAreReadFromTheBodyAndTheComments(t *testing.T) {
	i := Issue{Repo: "org/app", Number: 20, Body: "Show the totals.\n\nDepends on #12 and #14.",
		Comments: []Comment{{Body: "Also blocked by: org/api#7"}, {Body: "This does not depend on anything else."}}}
	got := DependencyRefs(i)
	want := []Dep{{Repo: "org/app", Number: 12}, {Repo: "org/app", Number: 14}, {Repo: "org/api", Number: 7}}
	if !slices.Equal(got, want) {
		t.Fatalf("refs = %+v, want %+v", got, want)
	}
}

func TestAnIssueIsHeldByTheDependenciesThatAreStillOpen(t *testing.T) {
	i := Issue{Deps: []Dep{{Repo: "org/app", Number: 12, Open: true}, {Repo: "org/app", Number: 14}}}
	if got := OpenDeps(i); len(got) != 1 || got[0].Number != 12 {
		t.Fatalf("open = %+v, want #12 only", got)
	}
	if got := DepWords(OpenDeps(i), "org/app"); got != "#12" {
		t.Fatalf("words = %q", got)
	}
}

func TestPathsAreTheLastPathsLineOfTheIssue(t *testing.T) {
	i := Issue{Body: "**Paths:** `internal/billing/`", Comments: []Comment{{Body: "## Plan\n1. Add the column.\n**Paths:** internal/billing/, ./migrations/0042.sql docs/billing.md\n"}}}
	want := []string{"internal/billing/", "migrations/0042.sql", "docs/billing.md"}
	if got := Paths(i); !slices.Equal(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestOverlapByAnAreaLabelOrBySharedPaths(t *testing.T) {
	billing := Issue{Labels: []Label{{Name: "area:billing"}}, Body: "**Paths:** internal/billing/invoice.go"}
	for _, tc := range []struct {
		name  string
		other Issue
		want  string
	}{
		{"same area", Issue{Labels: []Label{{Name: "area:billing"}}}, "both are area:billing"},
		{"a directory holds the file", Issue{Body: "Paths: internal/billing"}, "both touch internal/billing"},
		{"a sibling with a shared prefix", Issue{Body: "Paths: internal/billingx/a.go"}, ""},
		{"another area", Issue{Labels: []Label{{Name: "area:auth"}}, Body: "Paths: internal/auth/"}, ""},
	} {
		if got := Overlap(billing, tc.other, "area:"); got != tc.want {
			t.Errorf("%s: overlap = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := Overlap(billing, Issue{Labels: []Label{{Name: "area:billing"}}}, ""); got != "" {
		t.Errorf("labels overlap with no prefix set: %q", got)
	}
}

func TestParseDepsReadsTheBlockedByRelationAndTheStateOfEachReference(t *testing.T) {
	data := `{"data":{
		"r0":{"i20":{"blockedBy":{"nodes":[{"number":9,"state":"OPEN","repository":{"nameWithOwner":"org/app"}}]}},
		      "d12":{"state":"CLOSED"}},
		"r1":{"d7":{"state":"OPEN"}}}}`
	native, state, err := parseDeps([]byte(data), []string{"org/app", "org/api"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []Dep{{Repo: "org/app", Number: 9, Open: true}}; !slices.Equal(native[20], want) {
		t.Fatalf("native = %+v", native)
	}
	if state["org/app#12"] || !state["org/api#7"] {
		t.Fatalf("state = %v", state)
	}
}

func TestADependencyOnAPullRequestOrAMissingNumberHoldsOnlyTheIssueThatNamesIt(t *testing.T) {
	// GitHub answers with the fields it found, and an error for the number
	// that names nothing.
	data := `{"data":{"r0":{
		"i20":{"blockedBy":{"nodes":[]}},"i21":{"blockedBy":{"nodes":[]}},"i22":{"blockedBy":{"nodes":[]}},
		"d60":{"state":"MERGED"},"d6300":null}},
		"errors":[{"type":"NOT_FOUND","path":["r0","d6300"],"message":"Could not resolve to an issue or pull request with the number of 6300."}]}`
	native, state, err := parseDeps([]byte(data), []string{"org/app"})
	if err != nil {
		t.Fatalf("one number that names nothing failed the whole read: %v", err)
	}
	issues := []Issue{
		{Repo: "org/app", Number: 20, Body: "Depends on #60"},
		{Repo: "org/app", Number: 21, Body: "Depends on #6300"},
		{Repo: "org/app", Number: 22, Body: "Nothing to wait for."},
	}
	applyDeps(issues, native, state)
	want := map[int]string{20: "", 21: "depends on #6300, which is not an issue or pull request", 22: ""}
	for _, i := range issues {
		if got := DepHold(i); got != want[i.Number] {
			t.Errorf("#%d hold = %q, want %q", i.Number, got, want[i.Number])
		}
	}
}

func TestADependencyOnAnOpenPullRequestHoldsTheIssue(t *testing.T) {
	native, state, err := parseDeps([]byte(`{"data":{"r0":{"i20":{"blockedBy":{"nodes":[]}},"d60":{"state":"OPEN"}}}}`), []string{"org/app"})
	if err != nil {
		t.Fatal(err)
	}
	issues := []Issue{{Repo: "org/app", Number: 20, Body: "Depends on #60"}}
	applyDeps(issues, native, state)
	if got := DepHold(issues[0]); got != "waits for #60 to close" {
		t.Fatalf("hold = %q", got)
	}
}

func TestAnErrorOtherThanNotFoundFailsTheDependencyRead(t *testing.T) {
	data := `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`
	if _, _, err := parseDeps([]byte(data), []string{"org/app"}); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("err = %v, want the rate limit", err)
	}
}

func TestANewIssueIsHeldWhileItsRepositoryIsAtItsOpenPullRequestLimit(t *testing.T) {
	open := func(n int) Issue {
		i := Issue{Repo: "org/app", Number: 20, OpenPRs: map[int]PR{}}
		for pr := range n {
			i.OpenPRs[100+pr] = PR{}
		}
		return i
	}
	if got, want := PRLimitHold(open(9), 8, false), "org/app has 9 open pull requests, at or over its limit of 8: they are finished first"; got != want {
		t.Errorf("over the limit: hold = %q, want %q", got, want)
	}
	if got := PRLimitHold(open(8), 8, false); !strings.Contains(got, "8 open pull requests") {
		t.Errorf("at the limit: hold = %q, want it held", got)
	}
	if got := PRLimitHold(open(7), 8, false); got != "" {
		t.Errorf("under the limit: hold = %q, want none", got)
	}
	if got := PRLimitHold(open(50), 0, false); got != "" {
		t.Errorf("no limit: hold = %q, want none", got)
	}
	if got := PRLimitHold(Issue{Repo: "org/app", Number: 20}, 8, false); got != "" {
		t.Errorf("pull requests not looked up: hold = %q, want none", got)
	}
}

func TestAParkedDraftIsReadFromItsHandBack(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	i := Issue{Repo: "org/app", Number: 12, Comments: []Comment{{Body: "## Hand-back\n**PR:** #61 (draft, parked)", CreatedAt: at}}, OpenPRs: map[int]PR{61: {Draft: true}, 41: {}}}
	if pr, since := Parked(i, config.DefaultSignals()); pr != 61 || !since.Equal(at) {
		t.Fatalf("parked = #%d at %v, want #61 at %v", pr, since, at)
	}
	i.OpenPRs[61] = PR{}
	if pr, _ := Parked(i, config.DefaultSignals()); pr != 0 {
		t.Fatalf("parked = #%d, want none once the pull request is ready", pr)
	}
	if got := ReadyPRs(i); got != 2 {
		t.Fatalf("ready = %d, want 2", got)
	}
}
