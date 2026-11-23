package backlog

import (
	"regexp"
	"strings"
)

var sessionRE = regexp.MustCompile(`(?im)^\s*(?:\*\*)?session:(?:\*\*)?\s*(\S+)\s*$`)

// PlanSession returns the session a worker signed the issue's plan with: the
// last "Session:" line of its comments, as a worker with peers writes it
// beside its paths. "" when no plan is signed.
func PlanSession(i Issue) string {
	session := ""
	for _, c := range i.Comments {
		if all := sessionRE.FindAllStringSubmatch(c.Body, -1); len(all) > 0 {
			session = strings.Trim(all[len(all)-1][1], "`*")
		}
	}
	return session
}
