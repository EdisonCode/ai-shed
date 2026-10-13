package backlog

import (
	"regexp"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

// LessonsPhrase heads what a worker learned that the next worker should
// know, in its hand-back.
const LessonsPhrase = "Lessons:"

// settledRE matches the owner's word that the lessons before it are dealt
// with: promoted into memory, rejected, or folded into an entry.
var settledRE = regexp.MustCompile(`(?i)\blessons?\s+(promoted|rejected|folded)\b`)

// Lesson is one entry of a hand-back's Lessons part.
type Lesson struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// Lessons returns the lessons of the issue's hand-backs that the owner has
// not settled: the ones after the last comment that says "Lessons
// promoted", "Lesson rejected" or "Lessons folded".
func Lessons(issue Issue, signals []config.Signal) []Lesson {
	handBack := config.DefaultHandBack
	for _, s := range signals {
		if s.HandBack != "" {
			handBack = s.HandBack
		}
	}
	var lessons []Lesson
	for _, c := range issue.Comments {
		if settledRE.MatchString(c.Body) {
			lessons = nil
			continue
		}
		if !strings.Contains(strings.ToLower(c.Body), strings.ToLower(handBack)) {
			continue
		}
		for _, text := range entriesAfter(c.Body, LessonsPhrase, []string{"none", "nothing"}) {
			if text != "" {
				lessons = append(lessons, Lesson{Text: text, At: c.CreatedAt})
			}
		}
	}
	return lessons
}

// LessonKey is a lesson's text as two workers who learned the same thing
// would both write it: lower case, single spaces, no closing stop.
func LessonKey(text string) string {
	return strings.Join(strings.Fields(strings.TrimRight(strings.ToLower(text), ". ")), " ")
}
