package backlog

import (
	"slices"
	"testing"
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
