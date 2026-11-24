package backlog

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

// noChangeRE finds a hand-back's word that nothing needed to change.
var noChangeRE = regexp.MustCompile(`(?i)\b(nothing (needed to |had to )?change[ds]?|no change[s]? (was |were )?(needed|required|made)|no changes?\b)`)

// Unpushed says why a hand-back after rework does not show the work: the
// issue's pull request is still at head, the commit it was sent back with
// at sent for the reason why, and the hand-back does not say that nothing
// needed to change. A report that a fix was pushed is a claim; the head is
// the check. "" when the head moved, the hand-back says no change, no
// hand-back came since, or no head was recorded; and when the rework was
// only a failed check or a draft, and the pull request is now neither: a
// rerun of its CI, or marking it ready, fixes that without a commit. A
// conflict does not clear without one.
func Unpushed(issue Issue, signals []config.Signal, head, why string, sent time.Time) string {
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
	if fixedWithoutCommit(why, pr) && open.Problem == "" && !open.Draft {
		return ""
	}
	return fmt.Sprintf("the hand-back after rework left pull request #%d at %s, the head it was sent back with, and does not say that nothing needed to change: push the fix, or hand back saying nothing changed", pr, head[:min(7, len(head))])
}

// fixedWithoutCommit reports whether a rework reason names nothing but a
// failed check on pull request pr, or that it is a draft, as Rework words
// them.
func fixedWithoutCommit(why string, pr int) bool {
	if strings.Contains(why, "; ") {
		return false
	}
	prefix := fmt.Sprintf("pull request #%d ", pr)
	return strings.HasPrefix(why, prefix+failedCheckProblem) || strings.HasPrefix(why, prefix+stillDraft)
}
