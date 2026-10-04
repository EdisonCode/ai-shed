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
		{"ask that cites an earlier ruling stays open", []string{"## Hand-back\nPer the owner ruling above.\nDecisions needed: 1. A new one."}, []string{"decision"}},
		{"needs eyes with steps", []string{"## Hand-back\n**Needs eyes:** open /orders and check the total"}, []string{"eyes"}},
		{"owner's check answers needs eyes", []string{"## Hand-back\n**Needs eyes:** open /orders and check the total", "Eyes checked: the total is right."}, nil},
		{"needs eyes cleared in quotes", []string{"## Hand-back\n**Needs eyes:** \"nothing\""}, nil},
		{"phrase match ignores case", []string{"hand-BACK\ndecisions NEEDED: which one?"}, []string{"decision"}},
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
		{"no pull request number: stay open", []string{"## Hand-back\n**PR:** not opened yet, branch pushed"}, map[int]PR{}, []string{"review"}},
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

func TestOwnersAnswerAfterAHandBackSendsTheIssueBack(t *testing.T) {
	const sentBack = "the owner answered in the issue (Owner ruling) after pull request #41 was handed back; read the answer and apply it"
	laterHandBack := "## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** nothing\n**Decisions needed:** none\nApplied Owner ruling 1."
	cases := []struct {
		name        string
		comments    []string
		openPRs     map[int]PR
		wantWaiting []string
		wantRework  string
	}{
		{"asked, not yet answered", []string{handBackAsking}, map[int]PR{41: {}}, []string{"decision", "review"}, ""},
		{"answered: back to a worker, and not waiting for review", []string{handBackAsking, ruling}, map[int]PR{41: {}}, nil, sentBack},
		{"the worker applied the answer and handed back again", []string{handBackAsking, ruling, laterHandBack}, map[int]PR{41: {}}, []string{"review"}, ""},
		{"a second answer after that sends it back again", []string{handBackAsking, ruling, laterHandBack, ruling}, map[int]PR{41: {}}, nil, sentBack},
		{"an answer given before any hand-back is planning, not rework", []string{ruling, handBackClean}, map[int]PR{41: {}}, []string{"review"}, ""},
		{"an answer with no pull request open is not rework", []string{handBackAsking, ruling}, map[int]PR{}, nil, ""},
		{"an eyes check that passed does not send it back", []string{handBackClean, "Eyes checked: looks right."}, map[int]PR{41: {}}, []string{"review"}, ""},
		{"answered and conflicting: both reasons", []string{handBackAsking, ruling}, map[int]PR{41: {Problem: "conflicts with the base branch"}}, nil, sentBack + "; pull request #41 conflicts with the base branch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := Issue{OpenPRs: tc.openPRs}
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

func TestWhileAWorkerIsDueBackOnlyAQuestionWaitsOnTheOwner(t *testing.T) {
	needsEyes := "## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** open /orders and check the total\n**Decisions needed:**\n1. Keep the column?"
	issue := Issue{OpenPRs: map[int]PR{41: {Problem: "conflicts with the base branch"}}, Comments: []Comment{{Body: needsEyes}}}
	if got := Waiting(issue, config.DefaultSignals()); !slices.Equal(got, []string{"decision"}) {
		t.Fatalf("waiting = %v; the look and the review can wait for the rebased work, the question cannot", got)
	}
}

func TestAskCountsOnlyInAHandBack(t *testing.T) {
	scoping := "Scope for the worker. When you stop, post a hand-off with:\n**PR:** none yet\n**Needs eyes:** the page to check\n**Decisions needed:** anything I must rule on"
	cases := []struct {
		name     string
		comments []string
		want     []string
	}{
		{"the phrases in instructions ask the owner nothing", []string{scoping}, nil},
		{"the phrases in a discussion ask the owner nothing", []string{"Should this go under Decisions needed: or is it obvious?"}, nil},
		{"a real hand-back after the instructions counts", []string{scoping, handBackAsking}, []string{"decision", "review"}},
		{"instructions after a hand-back do not reopen or clear it", []string{handBackClean, scoping}, []string{"review"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := Issue{OpenPRs: map[int]PR{41: {}}}
			for _, body := range tc.comments {
				issue.Comments = append(issue.Comments, Comment{Body: body})
			}
			if got := Waiting(issue, config.DefaultSignals()); !slices.Equal(got, tc.want) {
				t.Fatalf("waiting = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWithNoHandBackPhraseAnAskCountsAnywhere(t *testing.T) {
	signals := config.DefaultSignals()
	for i := range signals {
		signals[i].HandBack = ""
	}
	issue := Issue{Comments: []Comment{{Body: "Decisions needed: which one?"}}}
	if got := Waiting(issue, signals); !slices.Equal(got, []string{"decision"}) {
		t.Fatalf("waiting = %v", got)
	}
}
