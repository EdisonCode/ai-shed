package backlog

import (
	"regexp"
	"slices"
	"strconv"
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

// OwnerMergesPhrase follows the tier in a pass whose merge the verifier
// leaves to the owner, as in "Verified (high, owner merges): ...": a change
// that cannot be undone, such as a dropped column.
const OwnerMergesPhrase = "owner merges"

var verdictRE = regexp.MustCompile(`(?im)^[\s*_>#]*(verification failed|verified)\b[\s*_]*(?:\((low|high)(,\s*owner merges)?\))?[\s*_]*:[\s*_]*(.*)$`)

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
	// OwnerMerges is set when the verifier passed the work and left its
	// merge to the owner.
	OwnerMerges bool `json:"owner_merges,omitempty"`
}

// ShedMerges reports whether shed may queue the merge of a pass at this
// verification, as far as its tier goes: a low pass, or a high one where
// the repository has merge_high, and never one the verifier left to the
// owner.
func ShedMerges(issue Issue, v VerificationState) bool {
	if v.State != VerifyPassed || v.OwnerMerges {
		return false
	}
	return v.Tier == config.TierLow || (v.Tier == config.TierHigh && issue.MergeHigh)
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
	if index < 0 || !isOpen {
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
		v.Tier, v.Note, v.At = strings.ToLower(m[2]), strings.TrimSpace(strings.Trim(m[4], "*_ ")), c.CreatedAt
		v.OwnerMerges = m[3] != ""
	}
	switch {
	case v.State != "":
		// A verdict holds while the pull request is a draft: one sent back
		// is made a draft for its rework, and is still the worker's.
	case open.Draft:
		// Not ready: there is nothing to verify yet.
		return VerificationState{}
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

// VerifyOutcome is what came of one verification: it passed the work or sent
// it back, and for a pass, what found a fault after it, if anything did.
type VerifyOutcome struct {
	Tier    string `json:"tier"`
	Passed  bool   `json:"passed"`
	Escaped string `json:"escaped,omitempty"`
}

// VerifyOutcomes reads every verification of the issue, in order. A pass
// escaped when, before the next verification, the owner sent the work back
// (an answer that is not an acceptance) or a look on staging failed: the
// verifier passed something that was not right.
func VerifyOutcomes(issue Issue, signals []config.Signal) []VerifyOutcome {
	var out []VerifyOutcome
	for _, c := range issue.Comments {
		if m := verdictRE.FindStringSubmatch(c.Body); m != nil {
			out = append(out, VerifyOutcome{Tier: strings.ToLower(m[2]), Passed: !strings.EqualFold(m[1], VerificationFailedPhrase)})
			continue
		}
		if len(out) == 0 || !out[len(out)-1].Passed || out[len(out)-1].Escaped != "" {
			continue
		}
		body := strings.ToLower(c.Body)
		for _, s := range signals {
			switch {
			case failedIn(body, s):
				out[len(out)-1].Escaped = "an eye check failed"
			case s.SendsBack && answeredIn(body, s) && !accepts(c.Body, s):
				out[len(out)-1].Escaped = "the owner sent it back"
			}
		}
	}
	return out
}

// QueuedPhrase starts shed's comment on an issue whose pull request it put
// on its way to the merge.
const QueuedPhrase = "Queued for merge by shed:"

// QueuedByShed reports whether shed queued the pull request for its merge.
func QueuedByShed(issue Issue, pr int) bool {
	want := "#" + strconv.Itoa(pr) + ","
	for _, c := range issue.Comments {
		if strings.HasPrefix(strings.TrimSpace(c.Body), QueuedPhrase) && strings.Contains(c.Body, want) {
			return true
		}
	}
	return false
}

// HandedPRs returns every pull request the issue's hand-backs named, in the
// order first named.
func HandedPRs(issue Issue, signals []config.Signal) []int {
	var out []int
	for _, s := range signals {
		if !s.FollowsPR {
			continue
		}
		for _, c := range issue.Comments {
			if value, found := asked(strings.ToLower(c.Body), s); found {
				for _, n := range prNumbers(value) {
					if !slices.Contains(out, n) {
						out = append(out, n)
					}
				}
			}
		}
	}
	return out
}

// LastVerified returns the tier and the words of the issue's last passed
// verification; empty when none passed.
func LastVerified(issue Issue) (tier, note string) {
	for _, c := range issue.Comments {
		if m := verdictRE.FindStringSubmatch(c.Body); m != nil && !strings.EqualFold(m[1], VerificationFailedPhrase) {
			tier, note = strings.ToLower(m[2]), strings.TrimSpace(strings.Trim(m[4], "*_ "))
		}
	}
	return tier, note
}
