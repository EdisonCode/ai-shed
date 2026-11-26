package backlog

import (
	"regexp"
	"slices"
	"strings"
)

// GoalBriefPrefix starts the title of a goal's brief: the one issue of a
// goal that holds its shape, for the crew that builds it. A brief is not a
// slice of the goal: no worker is handed it, and it is not counted.
const GoalBriefPrefix = "Goal brief:"

// The headings of a goal brief's parts, each a bold label or a heading.
const (
	briefSlices    = "Slices"
	briefContracts = "Contracts"
	briefDoneWhen  = "Done when"
)

// IsGoalBrief reports whether an issue is a goal's brief.
func IsGoalBrief(title string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(title)), strings.ToLower(GoalBriefPrefix))
}

// GoalBrief is the shape of a goal as its brief gives it.
type GoalBrief struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	// Slices are the goal's slices in their order, one per entry.
	Slices []string `json:"slices,omitempty"`
	// Contracts are what one part provides and others use.
	Contracts []Contract `json:"contracts,omitempty"`
	// DoneWhen is what makes the whole goal done, one per entry.
	DoneWhen []string `json:"done_when,omitempty"`
}

// Contract is something one slice provides and others use: an endpoint, an
// event, a schema, a shared type.
type Contract struct {
	// Text is the contract as the brief writes it.
	Text string `json:"text"`
	// Provider is the issue that provides it, as "#41" or "org/api#41";
	// empty when the brief names none.
	Provider string `json:"provider,omitempty"`
}

var (
	briefIssueRE = regexp.MustCompile(`([\w.-]+/[\w.-]+)?#(\d+)`)
	briefHeadRE  = regexp.MustCompile(`(?im)^[\s#*_]*(slices|contracts|done when)[\s*_]*:?[\s*_]*$|^[\s*_]*(slices|contracts|done when)[\s*_]*:[\s*_]*`)
)

// ParseGoalBrief reads a goal brief's parts from its body.
func ParseGoalBrief(i Issue) GoalBrief {
	b := GoalBrief{Repo: i.Repo, Number: i.Number, URL: i.URL}
	b.Slices = briefPart(i.Body, briefSlices)
	b.DoneWhen = briefPart(i.Body, briefDoneWhen)
	for _, line := range briefPart(i.Body, briefContracts) {
		c := Contract{Text: line}
		if m := briefIssueRE.FindStringSubmatch(line); m != nil {
			c.Provider = m[0]
		}
		b.Contracts = append(b.Contracts, c)
	}
	return b
}

// briefPart returns the entries of one part of a brief: the lines after its
// heading, up to the next heading or the end, list markers cut.
func briefPart(body, name string) []string {
	var entries []string
	in := false
	for _, line := range strings.Split(body, "\n") {
		if m := briefHeadRE.FindStringSubmatch(line); m != nil {
			head := strings.ToLower(m[1] + m[2])
			in = head == strings.ToLower(name)
			if rest := strings.TrimSpace(line[len(m[0]):]); in && rest != "" {
				entries = append(entries, rest)
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") && !briefIssueRE.MatchString(strings.Fields(trimmed)[0]) {
			in = false
			continue
		}
		if in && trimmed != "" {
			entries = append(entries, strings.TrimSpace(entryRE.ReplaceAllString(trimmed, "")))
		}
	}
	return entries
}

// BriefOf returns the brief among a goal's issues: the one with the lowest
// number, when more than one claims to be it.
func BriefOf(issues []Issue) (Issue, bool) {
	var briefs []Issue
	for _, i := range issues {
		if IsGoalBrief(i.Title) {
			briefs = append(briefs, i)
		}
	}
	if len(briefs) == 0 {
		return Issue{}, false
	}
	slices.SortFunc(briefs, func(a, b Issue) int { return a.Number - b.Number })
	return briefs[0], true
}
