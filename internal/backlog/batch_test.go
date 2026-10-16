package backlog

import (
	"slices"
	"testing"
	"time"
)

func TestABatchDescriptionNamesEachPullRequestAtItsHead(t *testing.T) {
	cars := []Car{{Issue: 12, PR: 41, SHA: "c0ffee41"}, {Issue: 13, PR: 42, SHA: "c0ffee42"}}
	if got := ParseBatchBody(BatchBody(cars)); !slices.Equal(got, cars) {
		t.Fatalf("read back %v, want %v", got, cars)
	}
}

func TestAMachineKnowsItsOwnBatchBranchesAndOnlyThose(t *testing.T) {
	at := time.Unix(5, 0)
	if !BatchOf(BatchBranch("box", at), "box") || BatchOf(BatchBranch("box2", at), "box") {
		t.Fatal("BatchOf")
	}
}

func TestOnlyAStrictStatusCheckRuleRequiresUpToDateBranches(t *testing.T) {
	for rules, want := range map[string]bool{
		`[]`: false,
		`[{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":false}}]`:                    false,
		`[{"type":"deletion"},{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":true}}]`: true,
	} {
		if got, err := strictRules([]byte(rules)); err != nil || got != want {
			t.Errorf("%s: strict = %v, %v, want %v", rules, got, err, want)
		}
	}
}

func TestBatchesAreTheOpenPullRequestsOfBatchBranches(t *testing.T) {
	data := `[{"number":101,"headRefName":"shed-train/box/5","body":"` + BatchPhrase + ` x\n\n- #41 at c0ffee41 (issue #12)\n","createdAt":"2026-10-14T10:00:00Z"},
	{"number":7,"headRefName":"feature","body":"quotes ` + BatchPhrase + `","createdAt":"2026-10-14T10:00:00Z"}]`
	got, err := parseBatches([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PR != 101 || !slices.Equal(got[0].Cars, []Car{{Issue: 12, PR: 41, SHA: "c0ffee41"}}) {
		t.Fatalf("batches = %+v", got)
	}
}
