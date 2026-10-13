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
