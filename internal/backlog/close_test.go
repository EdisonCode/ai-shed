package backlog

import (
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

func TestAVerdictAPassedLookOrShedsOwnCommentDoesNotUnfinishAnIssue(t *testing.T) {
	signals := config.DefaultSignals()
	for _, after := range []string{
		"Verified (low): the totals add up; ran go test ./...",
		"Eyes checked: the total shows on the order page.",
		"Queued for merge by shed: pull request #41, verified at low risk with green checks.",
		"<!-- shed-cost app -->\n## Cost\n$1.20",
	} {
		if got := Finished(mergedIssue(handBackClean, after), signals); got != 41 {
			t.Errorf("after %q: Finished = %d, want 41", after, got)
		}
	}
	if got := Finished(mergedIssue(handBackClean, "Verified (low): fine", "Also handle the empty cart."), signals); got != 0 {
		t.Errorf("a comment that asks for more: Finished = %d, want 0", got)
	}
}

func TestCloseWaitNamesWhatAFinishedIssueStillWaitsFor(t *testing.T) {
	signals := config.DefaultSignals()
	asksEyes := "## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** the total on the order page\n**Decisions needed:** none"
	verified := mergedIssue(handBackClean, "Verified (low): fine")
	verified.Verify = true
	unverified := mergedIssue(handBackClean)
	unverified.Verify = true
	withAct := mergedIssue(handBackClean + "\n**Owner act:** run the backfill")
	for _, tc := range []struct {
		name  string
		issue Issue
		pr    int
		wait  string
	}{
		{"merged with nothing left", mergedIssue(handBackClean), 41, ""},
		{"verified where the repository verifies", verified, 41, ""},
		{"not verified where the repository verifies", unverified, 41, "a verifier's pass"},
		{"its eye check is not done", mergedIssue(asksEyes), 41, "its eye check"},
		{"its eye check passed", mergedIssue(asksEyes, "Eyes checked: it shows."), 41, ""},
		{"an owner act is left", withAct, 41, "the owner act: run the backfill"},
		{"not finished", mergedIssue(handBackClean + "\n**Still to do:**\n- the API"), 0, ""},
	} {
		pr, wait, _ := CloseWait(tc.issue, signals)
		if pr != tc.pr || wait != tc.wait {
			t.Errorf("%s: CloseWait = %d %q, want %d %q", tc.name, pr, wait, tc.pr, tc.wait)
		}
	}
}

func TestCloseWaitStartsTheQuietPeriodAtThePassedLook(t *testing.T) {
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	i := mergedIssue("## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** the total\n**Decisions needed:** none", "Eyes checked: it shows.")
	i.Comments[1].CreatedAt = at
	if _, _, since := CloseWait(i, config.DefaultSignals()); !since.Equal(at) {
		t.Fatalf("since = %v, want the look's time %v", since, at)
	}
}

func TestAFinishedIssueWithAnOwnerActWaitsOnTheOwner(t *testing.T) {
	i := mergedIssue(handBackClean + "\n**Owner act:** run the backfill")
	a, ok := noStepAsk(t, i)
	if !ok || a.Left != "run the backfill" {
		t.Fatalf("ask = %+v, %v; want the owner act", a, ok)
	}
	if _, ok := noStepAsk(t, mergedIssue(handBackClean)); ok {
		t.Fatal("a finished issue with no owner act asks nothing")
	}
}

func TestTheMergeIsReadWithTheStateOfTheChecksOnItsCommit(t *testing.T) {
	answer := func(rollup string) string {
		return `{"data":{"repository":{"pullRequest":{"mergedAt":"2026-11-20T09:00:00Z","mergeCommit":{"oid":"abc123","statusCheckRollup":` + rollup + `}}}}}`
	}
	for rollup, want := range map[string]string{`{"state":"SUCCESS"}`: CIPassed, `{"state":"FAILURE"}`: CIFailed, `{"state":"PENDING"}`: CIPending, `null`: CINone} {
		m, err := parseMerge([]byte(answer(rollup)), "org/app", 41)
		if err != nil {
			t.Fatal(err)
		}
		if m.Checks != want || m.Commit != "abc123" || !m.At.Equal(time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)) {
			t.Errorf("rollup %s: merge = %+v, want checks %s", rollup, m, want)
		}
	}
	if _, err := parseMerge([]byte(`{"data":{"repository":{"pullRequest":{"mergedAt":null,"mergeCommit":null}}}}`), "org/app", 41); err == nil {
		t.Error("an unmerged pull request: want an error")
	}
}
