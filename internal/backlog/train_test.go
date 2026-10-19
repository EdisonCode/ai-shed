package backlog

import (
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

func boarding(comments ...string) Issue {
	i := Issue{OpenPRs: map[int]PR{41: {}}, Comments: []Comment{{Body: "## Hand-back\n**PR:** #41 (ready)"}}}
	for _, c := range comments {
		i.Comments = append(i.Comments, Comment{Body: c})
	}
	return i
}

func TestAPullRequestQueuedAfterItsHandBackIsOnTheTrain(t *testing.T) {
	i := boarding("Verified (low): fine", QueuedPhrase+" pull request #41, verified at low risk with green checks.")
	if got, _ := Boarded(i, config.DefaultSignals()); got != 41 {
		t.Fatalf("boarded = %d, want 41", got)
	}
}

func TestAPullRequestTakenOffTheTrainIsNotOnIt(t *testing.T) {
	i := boarding(QueuedPhrase+" pull request #41, verified.", OffTrainPhrase+" pull request #41, its check test failed.")
	if got, _ := Boarded(i, config.DefaultSignals()); got != 0 {
		t.Fatalf("boarded = %d after it was taken off", got)
	}
	if !TakenOff(i, config.DefaultSignals()) {
		t.Fatal("not read as taken off")
	}
}

func TestANewHandBackLeavesTheTrain(t *testing.T) {
	i := boarding(QueuedPhrase+" pull request #41, verified.", "## Hand-back\n**PR:** #41 (ready)")
	if got, _ := Boarded(i, config.DefaultSignals()); got != 0 {
		t.Fatalf("boarded = %d: the new hand-back has not been verified", got)
	}
	i.Comments = append(i.Comments[:1], Comment{Body: OffTrainPhrase + " pull request #41, its check test failed."}, Comment{Body: "## Hand-back\n**PR:** #41 (ready)"})
	if TakenOff(i, config.DefaultSignals()) {
		t.Fatal("a new hand-back after it was taken off still reads as taken off")
	}
}

func TestAHeadIsGreenOnlyWhenItsChecksRanAndPassed(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		want bool
	}{
		{"passed", `{"state":"OPEN","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","statusCheckRollup":[{"name":"test","status":"COMPLETED","conclusion":"SUCCESS"}]}`, true},
		{"no checks yet", `{"state":"OPEN","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","statusCheckRollup":[]}`, false},
		{"running", `{"state":"OPEN","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","statusCheckRollup":[{"name":"test","status":"IN_PROGRESS"}]}`, false},
		{"failed", `{"state":"OPEN","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","statusCheckRollup":[{"name":"test","status":"COMPLETED","conclusion":"FAILURE"}]}`, false},
		{"every check skipped", `{"state":"OPEN","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","statusCheckRollup":[{"name":"test","status":"COMPLETED","conclusion":"SKIPPED"},{"name":"summary","status":"COMPLETED","conclusion":"SKIPPED"}]}`, false},
		{"passed beside a skipped one", `{"state":"OPEN","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","statusCheckRollup":[{"name":"test","status":"COMPLETED","conclusion":"SUCCESS"},{"name":"docs","status":"COMPLETED","conclusion":"SKIPPED"}]}`, true},
	} {
		h, base, err := parseHead([]byte(tc.json))
		if err != nil || base != "main" || h.SHA != "abc" || !h.Open {
			t.Fatalf("%s: head = %+v, base = %q, err = %v", tc.name, h, base, err)
		}
		if h.Green() != tc.want {
			t.Errorf("%s: green = %v, want %v", tc.name, h.Green(), tc.want)
		}
	}
}

const offForCommits = OffTrainPhrase + " pull request #41, at c0ffee4, had new commits (f1x, was c0ffee4) while batch pull request #101 tested it. " + ReverifyPhrase + " at its new head."

func TestAPullRequestTakenOffForNewCommitsIsDueAVerificationAgain(t *testing.T) {
	i := verifying(handedBack, "Verified (low): fine", QueuedPhrase+" pull request #41, verified.", offForCommits)
	v := Verification(i, config.DefaultSignals())
	if v.State != VerifyDue || v.Since != "c0ffee4" {
		t.Fatalf("verification = %+v, want due again since c0ffee4", v)
	}
	if reason := Rework(i, config.DefaultSignals()); reason != "" {
		t.Fatalf("rework = %q: new commits need a verifier, not a builder", reason)
	}
}

func TestAPullRequestReverifiedAfterNewCommitsBoardsAgain(t *testing.T) {
	i := verifying(handedBack, "Verified (low): fine", QueuedPhrase+" pull request #41, verified.", offForCommits, "Verified (low): the new commit only fixes a test")
	if TakenOff(i, config.DefaultSignals()) {
		t.Fatal("still read as taken off after a verifier passed the new head")
	}
	if v, ready := MergeReady(i, config.DefaultSignals(), time.Now(), false); !ready || v.Since != "" {
		t.Fatalf("merge ready = %v (%+v), want it ready to board again", ready, v)
	}
}

func TestAPullRequestTakenOffForAFailureIsReworkWithTheReason(t *testing.T) {
	i := verifying(handedBack, "Verified (low): fine", QueuedPhrase+" pull request #41, verified.",
		OffTrainPhrase+" pull request #41, at c0ffee4, tested by itself on the base branch in batch pull request #103, has a failed check (test). "+ReworkPhrase+", and boards again once its next hand-back is verified.")
	reason := Rework(i, config.DefaultSignals())
	if !strings.Contains(reason, "has a failed check (test); fix what it found") || strings.Contains(reason, ReworkPhrase) {
		t.Fatalf("rework = %q, want the failure named and a fix asked for", reason)
	}
	if v := Verification(i, config.DefaultSignals()); v.State != VerifyPassed {
		t.Fatalf("verification = %+v: a failure on the train is a builder's, not a verifier's", v)
	}
}
