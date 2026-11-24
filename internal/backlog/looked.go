package backlog

import (
	"strings"

	"github.com/edisoncode/ai-shed/internal/config"
)

// LooksDone counts the eye checks done on the issue: each failure, each
// look blocked for missing data, and each pass of a look that was asked for. firstPass is set when the first one
// passed.
func LooksDone(issue Issue, signals []config.Signal) (done int, firstPass bool) {
	for _, s := range signals {
		if s.Name != config.EyesSignal {
			continue
		}
		owed := false
		for _, c := range issue.Comments {
			body := strings.ToLower(c.Body)
			if value, found := asked(body, s); found {
				owed = !isClear(value, s.Clear)
			}
			blocked, _ := noteAfter(c.Body, s.BlockedBy)
			_, missingData := noData(blocked)
			switch {
			case failedIn(body, s), owed && s.BlockedBy != "" && missingData:
				// A look blocked for data it could not make did not pass.
				done++
			case owed && answeredIn(body, s):
				done++
				owed = false
				if done == 1 {
					firstPass = true
				}
			}
		}
	}
	return done, firstPass
}
