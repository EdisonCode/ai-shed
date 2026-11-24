package backlog

import (
	"regexp"
	"strings"

	"github.com/edisoncode/ai-shed/internal/config"
)

// The phrases a crew-mate starts its review comment with, as in
// "Crew review passed (low): ..." or "Crew review: changes (high): ...".
const (
	CrewPassedPhrase  = "Crew review passed"
	CrewChangesPhrase = "Crew review: changes"
)

// Crew review states.
const (
	CrewDue     = "due"     // handed back, its checks passed, and no crew-mate has reviewed it
	CrewPassed  = "passed"  // a crew-mate passed it
	CrewChanges = "changes" // a crew-mate asked for changes
)

var crewRE = regexp.MustCompile(`(?im)^[\s*_>#]*crew review(?:\s+(passed)|\s*:\s*(changes))\b[\s*_]*(?:\((low|high)\))?[\s*_]*:[\s*_]*(.*)$`)

// CrewState is what a crew-mate said about the pull request of an issue's
// last hand-back.
type CrewState struct {
	// State is empty when nothing is to be reviewed: nothing was handed
	// back, or its pull request is a draft, cannot merge as it stands, or
	// its checks still run.
	State string `json:"state,omitempty"`
	PR    int    `json:"pr,omitempty"`
	// Tier and Note are from the review: the tier it was reviewed at, and
	// the rest of its first line.
	Tier string `json:"tier,omitempty"`
	Note string `json:"note,omitempty"`
}

// CrewReview reads where the crew review of the issue's last hand-back
// stands. Only the comments after that hand-back count: a new hand-back,
// after changes, needs a new review.
func CrewReview(issue Issue, signals []config.Signal) CrewState {
	index, pr := lastHandBack(issue, signals)
	open, isOpen := issue.OpenPRs[pr]
	if index < 0 || !isOpen {
		return CrewState{}
	}
	c := CrewState{PR: pr}
	for _, comment := range issue.Comments[index+1:] {
		m := crewRE.FindStringSubmatch(comment.Body)
		if m == nil {
			continue
		}
		c.State = CrewPassed
		if m[2] != "" {
			c.State = CrewChanges
		}
		c.Tier, c.Note = strings.ToLower(m[3]), strings.TrimSpace(strings.Trim(m[4], "*_ "))
	}
	if c.State == "" {
		if open.Draft || open.Problem != "" || open.Pending || open.NoCI {
			return CrewState{}
		}
		c.State = CrewDue
	}
	return c
}
