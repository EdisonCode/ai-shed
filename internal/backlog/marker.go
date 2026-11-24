package backlog

import "regexp"

// FoundWhilePhrase starts the line a worker puts in an issue it opens while
// building another: "Found while building #12". The digest counts the
// issues that carry it, to tell the work the fleet adds from the work it is
// given.
const FoundWhilePhrase = "Found while building"

var foundWhileRE = regexp.MustCompile(regexp.QuoteMeta(FoundWhilePhrase) + `\s+(?:[\w.-]+/[\w.-]+)?#\d+`)

// FoundWhileBuilding reports whether an issue's body carries the marker of
// an issue a worker opened: the phrase, then the issue it was building.
func FoundWhileBuilding(body string) bool {
	return foundWhileRE.MatchString(body)
}
