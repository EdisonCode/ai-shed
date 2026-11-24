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
	if Finished(issue, signals) != 0 {
		// The merged work is done; what is left, if anything, is the owner's.
		index, _ := lastHandBack(issue, signals)
		left, _ = noteAfter(issue.Comments[index].Body, OwnerActPhrase)
		left = strings.Trim(left, " *_")
		return issue.Comments[index].CreatedAt, left, left != ""
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

// CloseWait says whether the issue's work is done and what, if anything,
// it still waits for before shed may close it. pr is the merged pull
// request that finished it, or 0 when it is not finished. wait names the
// first thing missing: an owner act, a verifier's pass where the repository
// verifies, or its eye check. since is when the quiet period after which it
// may close starts, as far as the issue tells: the last hand-back, or the
// passed look if later. The merge time and the checks on the merge commit
// are for the caller to read from GitHub.
func CloseWait(issue Issue, signals []config.Signal) (pr int, wait string, since time.Time) {
	pr = Finished(issue, signals)
	if pr == 0 {
		return 0, "", time.Time{}
	}
	index, _ := lastHandBack(issue, signals)
	since = issue.Comments[index].CreatedAt
	if left, _ := noteAfter(issue.Comments[index].Body, OwnerActPhrase); strings.Trim(left, " *_") != "" {
		return pr, "the owner act: " + strings.Trim(left, " *_"), since
	}
	if issue.Verify && !passedAfter(issue, index) {
		return pr, "a verifier's pass", since
	}
	switch state, _, at, _ := EyeCheck(issue, signals); state {
	case EyesAsked:
		return pr, "its eye check", since
	case EyesBlocked:
		return pr, "its blocked eye check", since
	case EyesFailed:
		return pr, "its failed eye check", since
	case EyesPassed:
		if at.After(since) {
			since = at
		}
	}
	return pr, "", since
}

// passedAfter reports whether the last verdict after the comment at index
// is a pass.
func passedAfter(issue Issue, index int) bool {
	passed := false
	for _, c := range issue.Comments[index+1:] {
		if m := verdictRE.FindStringSubmatch(c.Body); m != nil {
			passed = strings.EqualFold(m[1], VerifiedPhrase)
		}
	}
	return passed
}
