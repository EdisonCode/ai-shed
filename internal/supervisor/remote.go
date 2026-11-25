package supervisor

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

const (
	// remoteCheckEvery is how often a worker that runs with Remote Control
	// has its screen read for a lost connection.
	remoteCheckEvery = 2 * time.Minute
	// remoteRows is how much of the bottom of the screen is read: a notice
	// further up is old.
	remoteRows = 20
	// remoteRetypeAfter is how long a typed /remote-control is given to
	// work before it is typed again, and remoteTries how often it is typed
	// before the owner is told.
	remoteRetypeAfter = 5 * time.Minute
	remoteTries       = 3
	// reconnectRemote is what reconnects Remote Control, typed at rest.
	reconnectRemote = "/remote-control"
)

var (
	// remoteLostRE matches the lines Claude Code prints when Remote Control
	// drops: "Remote Control disconnected — signed-in claude.ai account or
	// organization changed ...", "Remote Control disconnected: <reason>",
	// and "Couldn't reconnect to your Remote Control session".
	remoteLostRE = regexp.MustCompile(`(?i)remote control disconnected|couldn.t reconnect to your remote control|remote control isn.t connected`)
	// remoteTypedRE matches the prompt line of a typed /remote-control.
	remoteTypedRE = regexp.MustCompile(`^\s*[>❯›]\s*/remote-control\s*$`)
)

// RemoteControlLost reports whether the bottom of a worker's screen says its
// Remote Control is down: a notice of it that no later /remote-control at the
// prompt answers. The notice stays in the conversation after a reconnect, so
// only one below the last typed /remote-control counts.
func RemoteControlLost(screen string) bool {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if len(lines) > remoteRows {
		lines = lines[len(lines)-remoteRows:]
	}
	lost := false
	for _, line := range lines {
		switch {
		case remoteTypedRE.MatchString(line):
			lost = false
		case remoteLostRE.MatchString(line):
			lost = true
		}
	}
	return lost
}

// watchRemote reads the screen of a worker that runs with Remote Control.
// Without it, peers on other machines cannot message the worker. A worker at
// rest is typed /remote-control, which reconnects it; a busy worker is typed
// it once it rests, since a command typed into a turn would be read as a
// message. When typing it does not help, the owner is told. It reports
// whether it typed into the worker.
func (s *Supervisor) watchRemote(w config.Worker, st *workerState, obs Observation, now time.Time, toolState string) (bool, error) {
	if !config.RemoteControl(w.CommandOrDefault()) || now.Sub(st.remoteChecked) < remoteCheckEvery {
		return false, nil
	}
	st.remoteChecked = now
	screen, err := s.Terminal.Capture(w.Name, remoteRows)
	if err != nil {
		return false, fmt.Errorf("read the screen for Remote Control: %w", err)
	}
	r, known := runlog.ReadRemote(s.StateDir, w.Name)
	if !RemoteControlLost(screen) {
		if known && r.Down() {
			r.Back = now
			s.Logf("worker %s: Remote Control is back, down since %s", w.Name, r.Since.Format(time.RFC3339))
			return false, runlog.WriteRemote(s.StateDir, w.Name, r)
		}
		return false, nil
	}
	if !known || !r.Down() {
		r = runlog.Remote{Since: now}
		s.Logf("worker %s: its screen says Remote Control is down", w.Name)
		if err := runlog.WriteRemote(s.StateDir, w.Name, r); err != nil {
			return false, err
		}
	}
	atRest := toolState == runlog.MarkIdle && now.Sub(obs.LastActivity) >= settle
	if r.GaveUp || !atRest || (!r.LastTyped.IsZero() && now.Sub(r.LastTyped) < remoteRetypeAfter) {
		return false, nil
	}
	if r.Typed >= remoteTries {
		r.GaveUp = true
		if s.Notify != nil {
			s.Notify(w.Name, fmt.Sprintf("%s: worker %s lost Remote Control at %s and typing %s %d times did not bring it back: peers on other machines cannot message it. Give it a fresh session with shed recycle %s %s",
				s.Machine.Name, w.Name, r.Since.Local().Format("15:04"), reconnectRemote, r.Typed, s.Machine.Name, w.Name))
		}
		return false, runlog.WriteRemote(s.StateDir, w.Name, r)
	}
	if err := s.Terminal.Send(w.Name, reconnectRemote); err != nil {
		return false, fmt.Errorf("reconnect Remote Control: %w", err)
	}
	r.Typed, r.LastTyped = r.Typed+1, now
	s.Logf("worker %s: typed %s (try %d of %d)", w.Name, reconnectRemote, r.Typed, remoteTries)
	return true, runlog.WriteRemote(s.StateDir, w.Name, r)
}
