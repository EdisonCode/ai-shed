package backlog

import (
	"slices"
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
)

const (
	handBackAsking = "## Hand-back\n**PR:** #41 (draft)\n**Needs eyes:** nothing\n**Decisions needed:**\n1. Keep the old column? Recommend yes."
	handBackClean  = "## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** nothing\n**Decisions needed:** none"
	ruling         = "## Owner ruling, 2026-10-04\n1. Keep it."
)

func TestWaiting(t *testing.T) {
	cases := []struct {
		name     string
		comments []string
		want     []string
	}{
		{"no comments", nil, nil},
		{"plain discussion", []string{"I looked at this, the cause is the cache."}, nil},
		{"hand-back asks for a decision", []string{handBackAsking}, []string{"decision", "review"}},
		{"hand-back with nothing to decide", []string{handBackClean}, []string{"review"}},
		{"ruling answers the decision", []string{handBackAsking, ruling}, []string{"review"}},
		{"a later hand-back asks again", []string{handBackAsking, ruling, handBackAsking}, []string{"decision", "review"}},
		{"ask that cites an earlier ruling stays open", []string{"Per the owner ruling above.\nDecisions needed: 1. A new one."}, []string{"decision"}},
		{"needs eyes with steps", []string{"**Needs eyes:** open /orders and check the total"}, []string{"eyes"}},
		{"owner's check answers needs eyes", []string{"**Needs eyes:** open /orders and check the total", "Eyes checked: the total is right."}, nil},
		{"needs eyes cleared in quotes", []string{`**Needs eyes:** "nothing"`}, nil},
		{"phrase match ignores case", []string{"decisions NEEDED: which one?"}, []string{"decision"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var issue Issue
			for _, body := range tc.comments {
				issue.Comments = append(issue.Comments, Comment{Body: body})
			}
			got := Waiting(issue, config.DefaultSignals())
			if !slices.Equal(got, tc.want) {
				t.Fatalf("waiting = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReviewSignalFollowsItsPullRequest(t *testing.T) {
	laterHandBack := "## Hand-back\n**PR:** #58 (ready)\n**Decisions needed:** none"
	cases := []struct {
		name     string
		comments []string
		openPRs  map[int]PR
		want     []string
	}{
		{"pull request still open", []string{handBackClean}, map[int]PR{41: {}}, []string{"review"}},
		{"pull request merged: the issue is free again", []string{handBackClean}, map[int]PR{}, nil},
		{"second pull request of the same issue is open", []string{handBackClean, laterHandBack}, map[int]PR{58: {}}, []string{"review"}},
		{"first is still open but the latest hand-back's is merged", []string{handBackClean, laterHandBack}, map[int]PR{41: {}}, nil},
		{"open pull requests unknown: stay open", []string{handBackClean}, nil, []string{"review"}},
		{"no pull request number: stay open", []string{"**PR:** not opened yet, branch pushed"}, map[int]PR{}, []string{"review"}},
		{"a merged pull request does not answer a decision", []string{handBackAsking}, map[int]PR{}, []string{"decision"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := Issue{OpenPRs: tc.openPRs}
			for _, body := range tc.comments {
				issue.Comments = append(issue.Comments, Comment{Body: body})
			}
			if got := Waiting(issue, config.DefaultSignals()); !slices.Equal(got, tc.want) {
				t.Fatalf("waiting = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPullRequestThatCannotMergeNeedsAWorkerNotTheOwner(t *testing.T) {
	cases := []struct {
		name        string
		comments    []string
		pr          PR
		wantWaiting []string
		wantRework  string
	}{
		{"healthy pull request waits for review", []string{handBackClean}, PR{}, []string{"review"}, ""},
		{"conflicting pull request goes back to a worker", []string{handBackClean}, PR{Problem: "conflicts with the base branch"}, nil, "pull request #41 conflicts with the base branch"},
		{"failed check goes back to a worker", []string{handBackClean}, PR{Problem: "has a failed check (integration)"}, nil, "pull request #41 has a failed check (integration)"},
		{"an open decision still waits on the owner", []string{handBackAsking}, PR{Problem: "conflicts with the base branch"}, []string{"decision"}, "pull request #41 conflicts with the base branch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := Issue{OpenPRs: map[int]PR{41: tc.pr}}
			for _, body := range tc.comments {
				issue.Comments = append(issue.Comments, Comment{Body: body})
			}
			if got := Waiting(issue, config.DefaultSignals()); !slices.Equal(got, tc.wantWaiting) {
				t.Errorf("waiting = %v, want %v", got, tc.wantWaiting)
			}
			if got := Rework(issue, config.DefaultSignals()); got != tc.wantRework {
				t.Errorf("rework = %q, want %q", got, tc.wantRework)
			}
		})
	}
}

func TestIssueWithNoPullRequestNeedsNoRework(t *testing.T) {
	issue := Issue{OpenPRs: map[int]PR{41: {Problem: "conflicts with the base branch"}}, Comments: []Comment{{Body: "Just a discussion of #41."}}}
	if got := Rework(issue, config.DefaultSignals()); got != "" {
		t.Fatalf("rework = %q", got)
	}
}

func TestParsePRs(t *testing.T) {
	prs, err := parsePRs([]byte(`[
		{"number": 1, "mergeable": "MERGEABLE", "statusCheckRollup": [{"name": "build", "conclusion": "SUCCESS"}, {"name": "optional", "conclusion": "SKIPPED"}]},
		{"number": 2, "mergeable": "CONFLICTING", "statusCheckRollup": [{"name": "build", "conclusion": "FAILURE"}]},
		{"number": 3, "mergeable": "MERGEABLE", "statusCheckRollup": [{"name": "lint", "conclusion": "SUCCESS"}, {"name": "integration", "conclusion": "FAILURE"}]},
		{"number": 4, "mergeable": "UNKNOWN", "statusCheckRollup": [{"context": "ci/legacy", "state": "ERROR"}]},
		{"number": 5, "mergeable": "MERGEABLE", "statusCheckRollup": [{"name": "build", "status": "IN_PROGRESS", "conclusion": ""}]}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{1: "", 2: "conflicts with the base branch", 3: "has a failed check (integration)", 4: "has a failed check (ci/legacy)", 5: ""}
	for number, problem := range want {
		if got, ok := prs[number]; !ok || got.Problem != problem {
			t.Errorf("pull request %d: problem = %q (present %v), want %q", number, got.Problem, ok, problem)
		}
	}
}
