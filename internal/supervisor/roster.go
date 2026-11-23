package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// RosterFile lists what each of a worker's peers has in hand, inside the
// worker's directory. It is kept apart from the brief: a brief edit
// interrupts the worker, and the roster changes as the peers work.
const RosterFile = ".shed/PEERS.md"

// rosterText is the roster of a worker with peers. A peer on this machine
// shows the issue the check-ins say it builds; a peer on another machine
// shows the in-flight issue whose plan it signed. open are the issues of
// this machine's queues, and elsewhere those in flight on other machines.
func rosterText(w config.Worker, open, elsewhere []backlog.Issue, checkins []runlog.Checkin) string {
	if len(w.PeerList) == 0 {
		return ""
	}
	describe := func(i backlog.Issue) string {
		s := fmt.Sprintf("%s#%d %s", i.Repo, i.Number, i.Title)
		if paths := backlog.Paths(i); len(paths) > 0 {
			s += " (paths: " + strings.Join(paths, ", ") + ")"
		}
		return s
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Your peers\n\nThe supervisor keeps this file current; read it before you message a peer. Your session is `%s`.\n\n", w.Session)
	for _, p := range w.PeerList {
		if p.Remote {
			fmt.Fprintf(&b, "- `%s` (on %s): ", p.Session, p.Machine)
			var signed []string
			for _, i := range elsewhere {
				if backlog.PlanSession(i) == p.Session {
					signed = append(signed, describe(i))
				}
			}
			if len(signed) == 0 {
				b.WriteString("no signed plan in flight\n")
			} else {
				b.WriteString(strings.Join(signed, "; ") + "\n")
			}
			continue
		}
		fmt.Fprintf(&b, "- `%s`: ", p.Session)
		n := runlog.BuildingInHand(checkins, p.Worker)
		switch {
		case n == 0:
			b.WriteString("nothing in hand\n")
		default:
			line := fmt.Sprintf("#%d\n", n)
			for _, i := range open {
				if i.Number == n {
					line = describe(i) + "\n"
					break
				}
			}
			b.WriteString(line)
		}
	}
	return b.String()
}

// writeRoster puts the roster in the worker's directory when it changed,
// and removes the file of a worker that has no peers.
func writeRoster(w config.Worker, text string) (changed bool, err error) {
	path := filepath.Join(expandHome(w.Dir), RosterFile)
	if text == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("worker %s: remove the peer roster: %w", w.Name, err)
		}
		return false, nil
	}
	if old, err := os.ReadFile(path); err == nil && string(old) == text {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("worker %s: write the peer roster: %w", w.Name, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return false, fmt.Errorf("worker %s: write the peer roster: %w", w.Name, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("worker %s: write the peer roster: %w", w.Name, err)
	}
	return true, nil
}
