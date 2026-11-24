package backlog

import (
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

// NoStepAsk is the name of the ask an issue makes when its last hand-back
// left no step for a worker: no open pull request, nothing still to do and
// no question. Handed out again, a worker would find nothing to do, so it
// waits on the owner until something changes on the issue.
const NoStepAsk = "no worker step"

// OwnerActPhrase begins what a hand-back says only the owner can do, such
// as a step on production.
const OwnerActPhrase = "Owner act:"

// rested returns when the issue's last comment, a hand-back, left no step
// for a worker, and what it says is left for the owner. ok is false when
// the issue has a step for a worker, or something changed since: a comment
// that is not a hand-back, an edit of the body, a label.
func rested(issue Issue, signals []config.Signal) (since time.Time, left string, ok bool) {
	if len(issue.Comments) == 0 || issue.OpenPRs == nil {
		return time.Time{}, "", false
	}
	last := issue.Comments[len(issue.Comments)-1]
	if !isHandBack(last.Body, signals) || len(Remaining(issue, signals)) > 0 || len(DependencyRefs(issue)) > 0 {
		return time.Time{}, "", false
	}
	if index, _ := lastHandBack(issue, signals); index == len(issue.Comments)-1 {
		// It follows an open pull request: that is the work's to finish.
		return time.Time{}, "", false
	}
	// GitHub moves the issue's time on any change, a comment included: one
	// well after the hand-back was an edit or a label.
	if !issue.UpdatedAt.IsZero() && !last.CreatedAt.IsZero() && issue.UpdatedAt.Sub(last.CreatedAt) > time.Minute {
		return time.Time{}, "", false
	}
	left, _ = noteAfter(last.Body, OwnerActPhrase)
	return last.CreatedAt, strings.Trim(left, " *_"), true
}
