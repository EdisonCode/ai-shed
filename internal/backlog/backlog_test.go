package backlog

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

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
		{"no pull request at all: nothing to review", []string{"## Hand-back\n**PR:** none (plan-only pass)\n**Decisions needed:** none"}, map[int]PR{}, nil},
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
	const sentBack = "the owner answered in the issue (Owner ruling) after pull request #41 was handed back; read the answer and apply it; if it asks for no change, hand back again and say so"
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
		{"an answer that accepts the work does not send it back", []string{handBackAsking, "Owner ruling: accepted"}, map[int]PR{41: {}}, []string{"review"}, ""},
		{"accepting in a heading with emphasis", []string{handBackAsking, "**Owner ruling:** Accepted as shipped."}, map[int]PR{41: {}}, []string{"review"}, ""},
		{"a change asked for after an acceptance sends it back", []string{handBackAsking, "Owner ruling: accepted", ruling}, map[int]PR{41: {}}, nil, sentBack},
		{"an acceptance after a change was asked for takes it back", []string{handBackAsking, ruling, "Owner ruling: accepted"}, map[int]PR{41: {}}, []string{"review"}, ""},
		{"accepted but conflicting: only the conflict", []string{handBackAsking, "Owner ruling: accepted"}, map[int]PR{41: {Problem: "conflicts with the base branch"}}, nil, "pull request #41 conflicts with the base branch"},
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

func TestAsksSayHowLongTheOwnerHasBeenAskedAndForWhichPullRequest(t *testing.T) {
	first, second := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 4, 11, 0, 0, 0, time.UTC)
	issue := Issue{OpenPRs: map[int]PR{41: {}}, Comments: []Comment{
		{Body: "## Hand-back\n**PR:** #41 (draft)\n**Decisions needed:**\n1. Keep the column? See #3.", CreatedAt: first},
		{Body: "## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** open /orders", CreatedAt: second},
	}}
	want := []Ask{{Name: "decision", Since: first}, {Name: "eyes", Since: second}, {Name: "review", Since: second, PR: 41}}
	if got := Asks(issue, config.DefaultSignals()); !slices.Equal(got, want) {
		t.Fatalf("asks = %+v, want %+v", got, want)
	}
}

func TestRulingAfterAPlanOnlyHandBackFreesTheIssue(t *testing.T) {
	issue := Issue{OpenPRs: map[int]PR{}, Comments: []Comment{
		{Body: "## Hand-back\n**PR:** none (plan-only pass)\n**Decisions needed:**\n1. Build it this way?"},
		{Body: "Owner ruling: go for the build."},
	}}
	if got := Waiting(issue, config.DefaultSignals()); len(got) != 0 {
		t.Fatalf("waiting = %v; the ruling was the only thing asked for", got)
	}
}

func TestAskOfAReviewWithNoPullRequestSaysSo(t *testing.T) {
	issue := Issue{OpenPRs: map[int]PR{}, Comments: []Comment{{Body: "## Hand-back\n**PR:** not opened yet, branch pushed"}}}
	asks := Asks(issue, config.DefaultSignals())
	if len(asks) != 1 || asks[0].What() != "review (no pull request named)" {
		t.Fatalf("asks = %+v; a review that follows no pull request must be visible", asks)
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

func TestEyeCheckOfHandedBackWork(t *testing.T) {
	asks := "## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** open /orders and check the total"
	failed := "## Hand-back\n**Eyes failed:** the total is blank on staging\n**Decisions needed:**\n1. Fix now or ship without?"
	fixed := "## Hand-back\n**PR:** #58 (ready)\n**Needs eyes:** open /orders again"
	cases := []struct {
		name        string
		comments    []string
		wantState   string
		wantPR      int
		wantWaiting []string
	}{
		{"asked", []string{asks}, EyesAsked, 41, []string{"eyes"}},
		{"nothing to look at", []string{handBackClean}, "", 41, nil},
		{"looked at and passed", []string{asks, "Eyes checked: the total is right."}, EyesPassed, 41, nil},
		{"an answer with nothing asked is no pass", []string{handBackClean, "Eyes checked anyway."}, "", 41, nil},
		{"looked at and failed: the look is done, the decision is open", []string{asks, failed}, EyesFailed, 41, []string{"decision"}},
		{"failed, ruled on: free for a worker", []string{asks, failed, ruling}, EyesFailed, 41, nil},
		{"the fix asks for a new look", []string{asks, failed, ruling, fixed}, EyesAsked, 58, []string{"eyes"}},
		{"a worker could not look", []string{asks, "Eyes blocked: staging asks for a sign-in I may not type."}, EyesBlocked, 41, []string{"eyes"}},
		{"the owner cleared the way", []string{asks, "Eyes blocked: signed out.", "Eyes unblocked: signed in again."}, EyesAsked, 41, []string{"eyes"}},
		{"the owner looked instead", []string{asks, "Eyes blocked: signed out.", "Eyes checked: the total is right."}, EyesPassed, 41, nil},
		{"blocked again after the way was cleared", []string{asks, "Eyes blocked: signed out.", "Eyes unblocked", "**Eyes blocked:**\nstaging is down"}, EyesBlocked, 41, []string{"eyes"}},
		{"a look that passed cannot be blocked", []string{asks, "Eyes checked: fine.", "Eyes blocked: too late."}, EyesPassed, 41, nil},
		{"an ask with no pull request owes no look on staging", []string{"## Hand-back\n**Needs eyes:** open /orders"}, "", 0, []string{"eyes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := Issue{OpenPRs: map[int]PR{}} // every pull request is merged
			for _, body := range tc.comments {
				issue.Comments = append(issue.Comments, Comment{Body: body})
			}
			state, pr, _, _ := EyeCheck(issue, config.DefaultSignals())
			if state != tc.wantState || pr != tc.wantPR {
				t.Errorf("eye check = %q of #%d, want %q of #%d", state, pr, tc.wantState, tc.wantPR)
			}
			if got := Waiting(issue, config.DefaultSignals()); !slices.Equal(got, tc.wantWaiting) {
				t.Errorf("waiting = %v, want %v", got, tc.wantWaiting)
			}
		})
	}
}

func TestBlockedEyeCheckSaysWhatStoppedIt(t *testing.T) {
	issue := Issue{OpenPRs: map[int]PR{}, Comments: []Comment{
		{Body: "## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** open /orders"},
		{Body: "**Eyes blocked:** Staging asks for a Customer sign-in.\nThe owner must sign in."},
	}}
	if state, _, _, note := EyeCheck(issue, config.DefaultSignals()); state != EyesBlocked || note != "Staging asks for a Customer sign-in." {
		t.Fatalf("eye check = %q, note %q", state, note)
	}
}

type listByLabel map[string][]Issue

func (l listByLabel) List(_ context.Context, src config.IssueSource) ([]Issue, error) {
	if src.Label == "broken" {
		return nil, errors.New("gh: not logged in")
	}
	return l[src.Label], nil
}

func TestFindLooksInEverySourceForAnOpenIssue(t *testing.T) {
	lister := listByLabel{"box": {{Number: 12, Title: "Paginate"}}, "docs": {{Number: 50, Title: "Glossary"}}}
	sources := []config.IssueSource{{Repo: "org/app", Label: "box"}, {Repo: "org/app", Label: "docs"}}

	if i, found, err := Find(context.Background(), lister, sources, 50); err != nil || !found || i.Title != "Glossary" {
		t.Fatalf("#50 = %+v, %v, %v", i, found, err)
	}
	if _, found, err := Find(context.Background(), lister, sources, 99); err != nil || found {
		t.Fatalf("#99 found = %v, %v; it is open in no source", found, err)
	}
	if _, _, err := Find(context.Background(), lister, []config.IssueSource{{Label: "broken"}}, 12); err == nil {
		t.Fatal("a source that cannot be listed must be an error, not a missing issue")
	}
}

func TestRuled(t *testing.T) {
	plan := "## Hand-back\n**PR:** none (plan-only pass)\n**Decisions needed:**\n1. Build it this way?"
	cases := []struct {
		name     string
		comments []string
		want     bool
	}{
		{"a question nobody answered", []string{plan}, false},
		{"the owner answered and no worker has been back", []string{plan, ruling}, true},
		{"a worker handed back after the answer", []string{plan, ruling, handBackClean}, false},
		{"a ruling with no question before it", []string{handBackClean, ruling}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var issue Issue
			for _, body := range tc.comments {
				issue.Comments = append(issue.Comments, Comment{Body: body})
			}
			if got := Ruled(issue, config.DefaultSignals()); got != tc.want {
				t.Fatalf("ruled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDecided(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"a list", "## Hand-back\n**PR:** #41 (ready)\n**Decided without you:**\n- Kept the old column; the alternative is to drop it; revert commit abc123.\n- Page size 50, not 100.\n**Decisions needed:** none", []string{"Kept the old column; the alternative is to drop it; revert commit abc123.", "Page size 50, not 100."}},
		{"one line", "## Hand-back\n**Decided without you:** kept the old column\n\nNotes follow.", []string{"kept the old column"}},
		{"none", "## Hand-back\n**Decided without you:** none\n**Decisions needed:** none", nil},
		{"no such part", handBackClean, nil},
		{"outside a hand-back", "Plan: list them under Decided without you: later", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decided(Issue{Comments: []Comment{{Body: tc.body}}}, config.DefaultSignals())
			if !slices.Equal(got, tc.want) {
				t.Fatalf("decided = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestChoicesAWorkerMadeDoNotHoldAnIssue(t *testing.T) {
	body := "## Hand-back\n**PR:** none\n**Decided without you:**\n- Kept the old column.\n**Decisions needed:** none"
	if got := Waiting(Issue{Comments: []Comment{{Body: body}}}, config.DefaultSignals()); len(got) != 0 {
		t.Fatalf("waiting = %v, want nothing", got)
	}
}
