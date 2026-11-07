package supervisor

import (
	"strings"
	"unicode"
)

// unsentPrefix is how much of a message the input box must start with for
// the supervisor to know the message sits there unsent. The box may cut a
// long message; its opening is enough to tell it from anything else.
const unsentPrefix = 40

// unsentInput reports whether the worker's input box still holds the start
// of the message: the text between the last two rules of the screen. Its
// tool echoes a sent message above the box, not in it, so a message there
// was typed and not sent. A screen without such a box tells nothing.
func unsentInput(screen, message string) bool {
	typed, want := typedAndWanted(screen, message)
	return want != "" && strings.HasPrefix(typed, want)
}

// stackedInput reports whether the worker's input box holds the message
// behind other text, such as a command that was typed before it and did not
// run. Enter would send both as one message.
func stackedInput(screen, message string) bool {
	typed, want := typedAndWanted(screen, message)
	return want != "" && strings.Contains(typed, want) && !strings.HasPrefix(typed, want)
}

// typedAndWanted returns the text typed in the screen's input box and the
// opening of the message to look for in it, both without spaces. typed is
// empty when the screen has no box or the box shows the tool's placeholder.
func typedAndWanted(screen, message string) (typed, want string) {
	want = squeeze(message)
	if len(want) > unsentPrefix {
		want = want[:unsentPrefix]
	}
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	var rules []int
	for n, line := range lines {
		if isRule(line) {
			rules = append(rules, n)
		}
	}
	if len(rules) < 2 {
		return "", want
	}
	box := lines[rules[len(rules)-2]+1 : rules[len(rules)-1]]
	for _, line := range box {
		line = strings.Trim(strings.TrimSpace(line), "│")
		line = strings.TrimLeft(strings.TrimSpace(line), ">❯ ")
		typed += line
	}
	if strings.Contains(typed, faintOpen) {
		// Faint text is the tool's placeholder: the input is empty.
		return "", want
	}
	return squeeze(typed), want
}

// isRule reports whether the line is a horizontal rule of a box: box-drawing
// characters only, and at least ten of them.
func isRule(line string) bool {
	line = strings.TrimSpace(line)
	if len([]rune(line)) < 10 {
		return false
	}
	for _, r := range line {
		if !strings.ContainsRune("─━╭╮╰╯", r) {
			return false
		}
	}
	return true
}

// squeeze drops every space, so that text the box wrapped onto several lines
// compares with the line that was typed.
func squeeze(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}
