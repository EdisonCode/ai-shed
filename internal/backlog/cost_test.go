package backlog

import "testing"

func TestCostCommentIDIsTheLastMarkedComment(t *testing.T) {
	body := `[
		{"id": 1, "body": "a review note"},
		{"id": 7, "body": "<!-- shed-cost -->\n## Cost\nfirst"},
		{"id": 8, "body": "<!-- shed-cost -->\n## Cost\nsecond"}
	]`
	id, ok := markedCommentID([]byte(body), CostMarker)
	if !ok || id != 8 {
		t.Fatalf("id = %d, %v", id, ok)
	}
	if _, ok := markedCommentID([]byte(`[{"id": 1, "body": "no marker"}]`), CostMarker); ok {
		t.Fatal("a comment with no marker was taken for the cost")
	}
	if _, ok := markedCommentID([]byte(`not json`), CostMarker); ok {
		t.Fatal("a body that is not a comment list was taken for the cost")
	}
}
