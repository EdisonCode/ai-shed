package backlog

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

func verifying(comments ...string) Issue {
	i := Issue{Repo: "org/app", Number: 12, Verify: true, OpenPRs: map[int]PR{41: {}}}
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	for n, body := range comments {
		i.Comments = append(i.Comments, Comment{Body: body, CreatedAt: at.Add(time.Duration(n) * time.Hour)})
	}
	return i
}

const handedBack = "## Hand-back\n**PR:** #41 (ready)"

func TestAHandedBackPullRequestIsDueAVerification(t *testing.T) {
	v := Verification(verifying(handedBack), config.DefaultSignals())
	if v.State != VerifyDue || v.PR != 41 {
		t.Fatalf("verification = %+v, want due on #41", v)
	}
}

func TestAVerificationWaitsForTheChecks(t *testing.T) {
	i := verifying(handedBack)
	i.OpenPRs[41] = PR{Pending: true}
	if v := Verification(i, config.DefaultSignals()); v.State != VerifyPending {
		t.Fatalf("state = %q, want pending while the checks run", v.State)
	}
	i.OpenPRs[41] = PR{Problem: "has a failed check (shed/gates)"}
	if v := Verification(i, config.DefaultSignals()); v.State != "" {
		t.Fatalf("state = %q, want none: a failed check is rework, not a verifier's", v.State)
	}
	i.OpenPRs[41] = PR{Draft: true}
	if v := Verification(i, config.DefaultSignals()); v.State != "" {
		t.Fatalf("state = %q, want none for a draft", v.State)
	}
}

func TestTheVerifiersCommentPassesOrFailsTheLastHandBack(t *testing.T) {
	passed := Verification(verifying(handedBack, "**Verified (high):** ran the suite, tried a refund of 0"), config.DefaultSignals())
	if passed.State != VerifyPassed || passed.Tier != "high" {
		t.Fatalf("verification = %+v, want passed at high", passed)
	}
	failed := Verification(verifying(handedBack, "Verification failed (low): the export drops the header row"), config.DefaultSignals())
	if failed.State != VerifyFailed || failed.Note != "the export drops the header row" {
		t.Fatalf("verification = %+v", failed)
	}
	// A new hand-back after the failure needs a new verification.
	again := Verification(verifying(handedBack, "Verification failed (low): no header", handedBack), config.DefaultSignals())
	if again.State != VerifyDue {
		t.Fatalf("state after a new hand-back = %q, want due", again.State)
	}
}

func TestReviewIsNotTheOwnersUntilTheVerificationPasses(t *testing.T) {
	signals := config.DefaultSignals()
	if asks := Waiting(verifying(handedBack), signals); slices.Contains(asks, "review") {
		t.Fatalf("waiting = %v while the verification is due", asks)
	}
	if asks := Waiting(verifying(handedBack, "Verified (low): checked"), signals); !slices.Contains(asks, "review") {
		t.Fatalf("waiting = %v after it passed, want the review", asks)
	}
	plain := verifying(handedBack)
	plain.Verify = false
	if asks := Waiting(plain, signals); !slices.Contains(asks, "review") {
		t.Fatalf("waiting = %v for a repo that does not verify", asks)
	}
}

func TestAFailedVerificationSendsThePullRequestBack(t *testing.T) {
	reason := Rework(verifying(handedBack, "Verification failed (high): a refund of 0 is accepted"), config.DefaultSignals())
	if !strings.Contains(reason, "the verifier sent pull request #41 back: a refund of 0 is accepted") {
		t.Fatalf("rework = %q", reason)
	}
}

func TestAPullRequestSentBackStaysReworkWhileItIsADraft(t *testing.T) {
	i := verifying(handedBack, "Verification failed (high): a refund of 0 is accepted")
	i.OpenPRs[41] = PR{Draft: true}
	if reason := Rework(i, config.DefaultSignals()); !strings.Contains(reason, "the verifier sent pull request #41 back") {
		t.Fatalf("rework = %q, want the verifier's finding while the worker reworks a draft", reason)
	}
	if asks := Asks(i, config.DefaultSignals()); len(asks) != 0 {
		t.Fatalf("asks = %+v, want none of the owner while the worker reworks", asks)
	}
}

func TestAVerificationEscapedWhenTheOwnerOrAnEyeCheckFoundAFaultAfterIt(t *testing.T) {
	signals := config.DefaultSignals()
	i := verifying(handedBack, "Verified (low): ran the tests", "Owner ruling: the total is off by one",
		handedBack, "Verification failed (high): still off by one", handedBack, "Verified (high): checked the totals", "Owner ruling: accepted")
	got := VerifyOutcomes(i, signals)
	want := []VerifyOutcome{
		{Tier: "low", Passed: true, Escaped: "the owner sent it back"},
		{Tier: "high", Passed: false},
		{Tier: "high", Passed: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("outcomes = %+v, want %+v", got, want)
	}
	eyes := verifying(handedBack, "Verified (low): fine", "Eyes failed: the button is hidden on mobile")
	if got := VerifyOutcomes(eyes, signals); len(got) != 1 || got[0].Escaped != "an eye check failed" {
		t.Fatalf("outcomes = %+v", got)
	}
}

func TestAVerifierCanLeaveTheMergeToTheOwner(t *testing.T) {
	v := Verification(verifying(handedBack, "**Verified (high, owner merges):** drops the legacy_total column"), config.DefaultSignals())
	if v.State != VerifyPassed || v.Tier != "high" || !v.OwnerMerges || v.Note != "drops the legacy_total column" {
		t.Fatalf("verification = %+v, want passed at high with the merge left to the owner", v)
	}
	if v := Verification(verifying(handedBack, "Verified (high): tried to break it"), config.DefaultSignals()); v.OwnerMerges {
		t.Fatalf("verification = %+v, want the merge not left to the owner", v)
	}
	if tier, _ := LastVerified(verifying(handedBack, "Verified (high, owner merges): drops a column")); tier != "high" {
		t.Fatalf("last verified tier = %q, want high", tier)
	}
}

func TestAHighPassIsReadyToMergeOnlyWithMergeHigh(t *testing.T) {
	i := verifying(handedBack, "Verified (high): tried to break it")
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	if _, ready := MergeReady(i, config.DefaultSignals(), now, false); ready {
		t.Fatal("a high pass is ready to merge without merge_high")
	}
	i.MergeHigh = true
	if v, ready := MergeReady(i, config.DefaultSignals(), now, false); !ready || v.PR != 41 {
		t.Fatalf("verification = %+v, ready = %v; want a high pass ready with merge_high", v, ready)
	}
	left := verifying(handedBack, "Verified (high, owner merges): drops a column")
	left.MergeHigh = true
	if _, ready := MergeReady(left, config.DefaultSignals(), now, false); ready {
		t.Fatal("a high pass the verifier left to the owner is ready to merge")
	}
}
