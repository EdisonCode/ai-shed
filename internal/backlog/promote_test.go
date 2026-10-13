package backlog

import (
	"slices"
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
)

func TestAReviewQueuedForMergeIsNotTheOwners(t *testing.T) {
	i := Issue{Repo: "org/app", Number: 12, Verify: true, OpenPRs: map[int]PR{41: {AutoMerge: true}},
		Comments: []Comment{{Body: "## Hand-back\n**PR:** #41 (ready)"}, {Body: "Verified (low): ran the tests"}}}
	if w := Waiting(i, config.DefaultSignals()); slices.Contains(w, "review") {
		t.Fatalf("waiting = %v: the pull request is on its way to the merge", w)
	}
}

func TestThePullRequestsAnIssueHandedBackAreRead(t *testing.T) {
	i := Issue{Comments: []Comment{
		{Body: "## Hand-back\n**PR:** #41 (backend, merged) and #42 (frontend, ready)"},
		{Body: "## Hand-back\n**PR:** #42 (ready)"},
		{Body: "## Hand-back\n**PR:** none"},
	}}
	if got := HandedPRs(i, config.DefaultSignals()); !slices.Equal(got, []int{41, 42}) {
		t.Fatalf("pull requests = %v", got)
	}
}

func TestShedsOwnQueuedMergeIsRead(t *testing.T) {
	i := Issue{Comments: []Comment{{Body: QueuedPhrase + " pull request #41, verified at low risk with green checks."}}}
	if !QueuedByShed(i, 41) || QueuedByShed(i, 42) {
		t.Fatal("the queued merge is not read by its pull request")
	}
}
