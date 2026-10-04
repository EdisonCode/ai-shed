package backlog

import (
	"slices"
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
)

const (
	handBackAsking = "## Hand-back\n**PR:** #41 (draft)\n**Needs eyes:** nothing\n**Decisions needed:**\n1. Keep the old column? Recommend yes."
	handBackClean  = "## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** nothing\n**Decisions needed:** none"
	ruling         = "## Owner ruling, 2026-10-04\n1. Keep it."
)

func TestWaiting(t *testing.T) {
	cases := []struct {
		name     string
		comments []string
		want     []string
	}{
		{"no comments", nil, nil},
		{"plain discussion", []string{"I looked at this, the cause is the cache."}, nil},
		{"hand-back asks for a decision", []string{handBackAsking}, []string{"decision", "review"}},
		{"hand-back with nothing to decide", []string{handBackClean}, []string{"review"}},
		{"ruling answers the decision", []string{handBackAsking, ruling}, []string{"review"}},
		{"a later hand-back asks again", []string{handBackAsking, ruling, handBackAsking}, []string{"decision", "review"}},
		{"ask that cites an earlier ruling stays open", []string{"Per the owner ruling above.\nDecisions needed: 1. A new one."}, []string{"decision"}},
		{"needs eyes with steps", []string{"**Needs eyes:** open /orders and check the total"}, []string{"eyes"}},
		{"needs eyes cleared in quotes", []string{`**Needs eyes:** "nothing"`}, nil},
		{"phrase match ignores case", []string{"decisions NEEDED: which one?"}, []string{"decision"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var issue Issue
			for _, body := range tc.comments {
				issue.Comments = append(issue.Comments, Comment{Body: body})
			}
			got := Waiting(issue, config.DefaultSignals())
			if !slices.Equal(got, tc.want) {
				t.Fatalf("waiting = %v, want %v", got, tc.want)
			}
		})
	}
}
