package backlog

import (
	"strings"

	"github.com/edisoncode/ai-shed/internal/config"
)

// NoDataNote starts the note of a look blocked because staging lacks the
// state it needs and nothing can make it: "Eyes blocked: no data: no order
// with a partial refund". Such a look is the builder's to unblock, with a
// seed, not the owner's.
const NoDataNote = "no data"

// NoDataLook returns the state a blocked look says staging lacks, when its
// note starts with NoDataNote; "" for a look that is not blocked so.
func NoDataLook(issue Issue, signals []config.Signal) string {
	state, _, _, note := EyeCheck(issue, signals)
	if state != EyesBlocked {
		return ""
	}
	missing, _ := noData(note)
	return missing
}

// noData reads a blocked look's note: what state is missing, and whether
// the note starts with NoDataNote.
func noData(note string) (string, bool) {
	if len(note) < len(NoDataNote) || !strings.EqualFold(note[:len(NoDataNote)], NoDataNote) {
		return "", false
	}
	rest := note[len(NoDataNote):]
	if rest != "" && !strings.ContainsAny(rest[:1], " :-–—") {
		// "no database": another word.
		return "", false
	}
	missing := strings.TrimSpace(strings.TrimLeft(rest, " :-–—"))
	if missing == "" {
		missing = "the note names none"
	}
	return missing, true
}
