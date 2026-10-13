package backlog

import (
	"regexp"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

// Where the verification of a handed-back pull request stands.
const (
	VerifyPending = "pending" // its checks still run: the deterministic checks come first
	VerifyDue     = "due"     // its checks passed and no verifier has looked
	VerifyPassed  = "passed"  // a verifier looked and it does what the issue asks
	VerifyFailed  = "failed"  // a verifier looked and sent it back
)

// The phrases a verifier starts its comment with, as in
// "Verified (high): ..." or "Verification failed (low): ...".
const (
	VerifiedPhrase           = "Verified"
	VerificationFailedPhrase = "Verification failed"
)

var verdictRE = regexp.MustCompile(`(?im)^[\s*_>#]*(verification failed|verified)\b[\s*_]*(?:\((low|high)\))?[\s*_]*:[\s*_]*(.*)$`)

// VerificationState is what a verifier said about the pull request of an
// issue's last hand-back.
type VerificationState struct {
	// State is empty when nothing is to be verified: the repository does
	// not verify, nothing was handed back, the pull request is a draft or
	// cannot merge as it stands.
	State string `json:"state,omitempty"`
	PR    int    `json:"pr,omitempty"`
	// Tier and Note are from the verifier's comment: the tier it verified
	// at, and the rest of its first line.
	Tier string    `json:"tier,omitempty"`
	Note string    `json:"note,omitempty"`
	At   time.Time `json:"at,omitzero"`
}

// Verification reads where the verification of the issue's last hand-back
// stands. Only the comments after that hand-back count: a new hand-back
// needs a new verification.
func Verification(issue Issue, signals []config.Signal) VerificationState {
	if !issue.Verify {
		return VerificationState{}
	}
	index, pr := lastHandBack(issue, signals)
	open, isOpen := issue.OpenPRs[pr]
	if index < 0 || !isOpen || open.Draft {
		return VerificationState{}
	}
	v := VerificationState{PR: pr}
	for _, c := range issue.Comments[index+1:] {
		m := verdictRE.FindStringSubmatch(c.Body)
		if m == nil {
			continue
		}
		v.State = VerifyPassed
		if strings.EqualFold(m[1], VerificationFailedPhrase) {
			v.State = VerifyFailed
		}
		v.Tier, v.Note, v.At = strings.ToLower(m[2]), strings.TrimSpace(strings.Trim(m[3], "*_ ")), c.CreatedAt
	}
	switch {
	case v.State != "":
	case open.Problem != "":
		// A failed check is rework; no verifier looks at it.
		return VerificationState{}
	case open.Pending:
		v.State = VerifyPending
	default:
		v.State = VerifyDue
	}
	return v
}

// unverified reports whether the issue's handed-back pull request has not
// yet passed a verification it needs.
func unverified(issue Issue, signals []config.Signal) bool {
	switch Verification(issue, signals).State {
	case VerifyPending, VerifyDue:
		return true
	}
	return false
}
