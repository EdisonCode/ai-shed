package backlog

import (
	"fmt"
	"regexp"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

// noChangeRE finds a hand-back's word that nothing needed to change.
var noChangeRE = regexp.MustCompile(`(?i)\b(nothing (needed to |had to )?change[ds]?|no change[s]? (was |were )?(needed|required|made)|no changes?\b)`)

// Unpushed says why a hand-back after rework does not show the work: the
// issue's pull request is still at head, the commit it was sent back with
// at sent, and the hand-back does not say that nothing needed to change.
// A report that a fix was pushed is a claim; the head is the check. "" when
// the head moved, the hand-back says no change, no hand-back came since, or
// no head was recorded.
func Unpushed(issue Issue, signals []config.Signal, head string, sent time.Time) string {
	if head == "" {
		return ""
	}
	index, pr := lastHandBack(issue, signals)
	if index < 0 || !issue.Comments[index].CreatedAt.After(sent) {
		return ""
	}
	open, isOpen := issue.OpenPRs[pr]
	if !isOpen || open.Head != head || noChangeRE.MatchString(issue.Comments[index].Body) {
		return ""
	}
	return fmt.Sprintf("the hand-back after rework left pull request #%d at %s, the head it was sent back with, and does not say that nothing needed to change: push the fix, or hand back saying nothing changed", pr, head[:min(7, len(head))])
}
