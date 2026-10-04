package supervisor

import (
	"strconv"
	"strings"
)

// Markers around text that the terminal draws faint. A tool draws its own
// hints that way: a placeholder in an empty input line, a suggestion of what
// might be typed next. Nobody typed that text and nobody sent it, but in a
// plain capture it reads exactly like a message at the prompt.
const (
	faintOpen  = "[greyed out: "
	faintClose = "]"
)

// plain turns a capture that carries terminal escape sequences into text.
// Every sequence is removed; text drawn faint is put between markers.
func plain(styled string) string {
	var out, span strings.Builder
	faint := false
	flush := func() {
		if text := span.String(); strings.TrimSpace(text) != "" {
			// The markers go inside the span's own spacing.
			lead := text[:len(text)-len(strings.TrimLeft(text, " "))]
			trail := text[len(strings.TrimRight(text, " ")):]
			out.WriteString(lead + faintOpen + strings.TrimSpace(text) + faintClose + trail)
		} else {
			out.WriteString(text)
		}
		span.Reset()
	}
	// Bytes are copied one by one, so text of more than one byte stays whole.
	write := func(b byte) {
		if faint {
			span.WriteByte(b)
		} else {
			out.WriteByte(b)
		}
	}
	for i := 0; i < len(styled); {
		c := styled[i]
		switch {
		case c == '\n':
			// A faint span ends with its line, so a marker never runs on.
			flush()
			out.WriteByte('\n')
			i++
		case c != 0x1b || i+1 >= len(styled):
			write(c)
			i++
		case styled[i+1] == '[':
			// A control sequence: parameters, then one final byte.
			end := i + 2
			for end < len(styled) && (styled[end] < 0x40 || styled[end] > 0x7e) {
				end++
			}
			if end < len(styled) && styled[end] == 'm' {
				if now := faintAfter(styled[i+2:end], faint); now != faint {
					flush()
					faint = now
				}
			}
			i = min(end+1, len(styled))
		case styled[i+1] == ']':
			// An operating system command, such as a hyperlink: it ends
			// with a bell or with ESC backslash.
			end := i + 2
			for end < len(styled) && styled[end] != 0x07 && !(styled[end] == 0x1b && end+1 < len(styled) && styled[end+1] == '\\') {
				end++
			}
			if end < len(styled) && styled[end] == 0x1b {
				end++
			}
			i = min(end+1, len(styled))
		default:
			i += 2
		}
	}
	flush()
	return out.String()
}

// faintAfter applies the parameters of a "select graphic rendition" sequence
// to the faint state. 2 turns faint on; 22 and 0 turn it off. A 2 inside a
// colour (38;5;2 or 38;2;r;g;b) is a colour value and not the attribute.
func faintAfter(params string, faint bool) bool {
	if params == "" {
		return false
	}
	p := strings.Split(params, ";")
	for i := 0; i < len(p); i++ {
		n, _ := strconv.Atoi(p[i])
		switch n {
		case 0, 22:
			faint = false
		case 2:
			faint = true
		case 38, 48, 58:
			if i+1 < len(p) {
				switch p[i+1] {
				case "5":
					i += 2
				case "2":
					i += 4
				}
			}
		}
	}
	return faint
}
