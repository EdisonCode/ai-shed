package backlog

import "testing"

func TestAnIssueAWorkerFoundWhileBuildingAnotherCarriesTheMarker(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{"The export drops the header row.\n\nFound while building #12.", true},
		{"Found while building org/api#7", true},
		{"Found while building the importer, by hand.", false},
		{"The export drops the header row.", false},
	} {
		if got := FoundWhileBuilding(tc.body); got != tc.want {
			t.Errorf("FoundWhileBuilding(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}
