package backlog

import "testing"

func TestCostCommentIDIsTheLastMarkedComment(t *testing.T) {
	body := `[
		{"id": 1, "body": "a review note"},
		{"id": 7, "body": "<!-- shed-cost -->\n## Cost\nfirst"},
		{"id": 8, "body": "<!-- shed-cost -->\n## Cost\nsecond"}
	]`
	id, ok := costCommentID([]byte(body))
	if !ok || id != 8 {
		t.Fatalf("id = %d, %v", id, ok)
	}
	if _, ok := costCommentID([]byte(`[{"id": 1, "body": "no marker"}]`)); ok {
		t.Fatal("a comment with no marker was taken for the cost")
	}
	if _, ok := costCommentID([]byte(`not json`)); ok {
		t.Fatal("a body that is not a comment list was taken for the cost")
	}
}
