package backlog

import (
	"slices"
	"testing"
	"time"
)

func TestOpenPullsAreReadWithTheirClosingIssuesChecksAndLastCommit(t *testing.T) {
	data := `[
{"number":41,"title":"Add the export button","body":"Closes #12","headRefName":"export","createdAt":"2026-10-01T10:00:00Z","isDraft":false,
 "closingIssuesReferences":[{"number":12,"repository":{"name":"app","owner":{"login":"org"}}},{"number":7,"repository":{"name":"api","owner":{"login":"org"}}}],
 "statusCheckRollup":[{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"FAILURE"}],
 "commits":[{"committedDate":"2026-10-01T09:00:00Z"},{"committedDate":"2026-10-02T09:00:00Z"}]},
{"number":44,"title":"Try the new chart","body":"","headRefName":"chart","createdAt":"2026-10-03T10:00:00Z","isDraft":true,
 "closingIssuesReferences":[],"statusCheckRollup":[],"commits":[]}
]`
	pulls, err := parseOpenPulls([]byte(data), "org/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(pulls) != 2 {
		t.Fatalf("pulls = %+v", pulls)
	}
	got := pulls[0]
	if got.Repo != "org/app" || got.Number != 41 || got.Draft || got.CI != CIFailed ||
		!got.LastCommit.Equal(time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)) ||
		!slices.Equal(got.Closes, []Dep{{Repo: "org/app", Number: 12}, {Repo: "org/api", Number: 7}}) {
		t.Fatalf("pull 41 = %+v", got)
	}
	if pulls[1].CI != CINone || !pulls[1].Draft || len(pulls[1].Closes) != 0 {
		t.Fatalf("pull 44 = %+v", pulls[1])
	}
}

func TestShedsOwnBatchPullRequestsAreMarked(t *testing.T) {
	data := `[{"number":101,"title":"Merge train batch: #41","body":"` + BatchPhrase + ` x","headRefName":"shed-train/box/5","createdAt":"2026-10-01T10:00:00Z","isDraft":true}]`
	pulls, err := parseOpenPulls([]byte(data), "org/app")
	if err != nil {
		t.Fatal(err)
	}
	if !pulls[0].Batch {
		t.Fatalf("pull = %+v, want a batch", pulls[0])
	}
}

func TestTheCIStateOfAPullRequestIsItsWorstCheck(t *testing.T) {
	run := func(status, conclusion string) check {
		return check{Name: "test", Status: status, Conclusion: conclusion}
	}
	for _, tc := range []struct {
		name   string
		checks []check
		want   string
	}{
		{"no checks", nil, CINone},
		{"all passed", []check{run("COMPLETED", "SUCCESS"), {Context: "ci", State: "SUCCESS"}}, CIPassed},
		{"one still runs", []check{run("COMPLETED", "SUCCESS"), run("IN_PROGRESS", "")}, CIPending},
		{"one failed while another runs", []check{run("IN_PROGRESS", ""), {Context: "ci", State: "ERROR"}}, CIFailed},
		{"every check skipped", []check{run("COMPLETED", "SKIPPED")}, CISkipped},
	} {
		if got := ciState(tc.checks); got != tc.want {
			t.Errorf("%s: state = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTheHoldOfADraftIsTheLineOfItsBodyThatSaysWhatItWaitsFor(t *testing.T) {
	body := "Adds the chart.\n\nDraft: waits for #12 to merge.\n\nMore words."
	if got := HoldReason(body); got != "Draft: waits for #12 to merge." {
		t.Fatalf("hold = %q", got)
	}
	if got := HoldReason("Adds the chart."); got != "" {
		t.Fatalf("hold = %q, want none", got)
	}
}
