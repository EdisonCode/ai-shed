package backlog

import (
	"slices"
	"testing"
	"time"
)

func TestABatchDescriptionNamesEachPullRequestAtItsHead(t *testing.T) {
	cars := []Car{{Issue: 12, PR: 41, SHA: "c0ffee41"}, {Issue: 13, PR: 42, SHA: "c0ffee42"}}
	if got := ParseBatchBody(BatchBody(cars, Rebuilt{})); !slices.Equal(got, cars) {
		t.Fatalf("read back %v, want %v", got, cars)
	}
}

func TestABatchDescriptionSaysHowOftenTheBatchWasBuiltAgain(t *testing.T) {
	cars := []Car{{Issue: 12, PR: 41, SHA: "c0ffee41"}}
	if got := ParseRebuilt(BatchBody(cars, Rebuilt{})); got != (Rebuilt{}) {
		t.Fatalf("a first batch reads as rebuilt %+v", got)
	}
	want := Rebuilt{Times: 2, Green: 1, Last: "d00d123"}
	body := BatchBody(cars, want)
	if got := ParseRebuilt(body); got != want {
		t.Fatalf("read back %+v, want %+v", got, want)
	}
	if got := ParseBatchBody(body); !slices.Equal(got, cars) {
		t.Fatalf("pull requests read back %v, want %v", got, cars)
	}
}

func TestAComparisonListsRenamedFilesUnderBothNamesAndKnowsWhenItWasCutShort(t *testing.T) {
	c, err := parseComparison([]byte(`{"commits":["aaa","bbb"],"files":["new.go","old.go","main.go"],"listed":2}`))
	if err != nil || !slices.Equal(c.Commits, []string{"aaa", "bbb"}) || !slices.Equal(c.Files, []string{"new.go", "old.go", "main.go"}) || c.Partial {
		t.Fatalf("comparison = %+v, %v", c, err)
	}
	if c, _ := parseComparison([]byte(`{"commits":[],"files":[],"listed":300}`)); !c.Partial {
		t.Fatal("a comparison of 300 files reads as complete: GitHub lists no more")
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
