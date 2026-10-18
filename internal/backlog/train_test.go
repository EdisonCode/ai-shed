package backlog

import (
	"testing"

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
