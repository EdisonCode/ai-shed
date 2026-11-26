package backlog

import (
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
)

func TestTheEyeChecksDoneOnAnIssueAreCountedInOrder(t *testing.T) {
	signals := config.DefaultSignals()
	comments := func(bodies ...string) Issue {
		i := Issue{Repo: "org/app", Number: 5}
		for _, b := range bodies {
			i.Comments = append(i.Comments, Comment{Body: b})
		}
		return i
	}
	ask := "## Hand-back\n**PR:** #41 (ready)\nNeeds eyes: the export button"
	for _, tc := range []struct {
		name      string
		issue     Issue
		done      int
		firstPass bool
	}{
		{"passed on the first look", comments(ask, "Eyes checked: fine"), 1, true},
		{"failed, then passed", comments(ask, "Eyes failed: the button is gray", ask, "Eyes checked"), 2, false},
		{"asked and not looked at", comments(ask), 0, false},
		{"an answer with no ask", comments("Eyes checked"), 0, false},
	} {
		done, firstPass := LooksDone(tc.issue, signals)
		if done != tc.done || firstPass != tc.firstPass {
			t.Errorf("%s: done %d, first pass %v; want %d, %v", tc.name, done, firstPass, tc.done, tc.firstPass)
		}
	}
}
