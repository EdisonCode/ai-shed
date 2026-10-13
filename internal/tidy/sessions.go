package tidy

import (
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/probe"
)

// Session is an agent session on a worker machine that shed does not
// supervise, with how long it has printed nothing.
type Session struct {
	// Name is "session:window".
	Name            string
	Session, Window string
	Idle            time.Duration
	// Close says it has been idle longer than the limit.
	Close bool
}

// Sessions returns the windows named in unsupervised, as "session:window",
// marked to close when idle longer than idle.
func Sessions(windows []probe.Window, unsupervised []string, now time.Time, idle time.Duration) []Session {
	var out []Session
	for _, w := range windows {
		name := w.Session + ":" + w.Name
		for _, u := range unsupervised {
			if u != name {
				continue
			}
			quiet := max(now.Sub(w.LastActivity), 0)
			out = append(out, Session{Name: name, Session: w.Session, Window: w.Name, Idle: quiet, Close: quiet > idle})
		}
	}
	return out
}

// CloseSessionsScript ends the windows marked to close, each by its exact
// session and window name. One that is gone already is not an error.
func CloseSessionsScript(sessions []Session) string {
	var b strings.Builder
	for _, s := range sessions {
		if s.Close {
			b.WriteString("tmux kill-window -t " + quote("="+s.Session+":="+s.Window) + " 2>/dev/null && echo \"@@closed " + strings.ReplaceAll(s.Name, `"`, "") + "\"\n")
		}
	}
	return b.String()
}
