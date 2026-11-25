package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// Peer is a worker that another worker may message.
type Peer struct {
	Machine string
	Worker  string
	// Session is the name the peer's session answers to.
	Session string
	// Remote is set when the peer is on another machine: the message goes
	// through Remote Control.
	Remote bool
}

// SessionName is the name shed gives the session of a worker with peers.
func SessionName(machine, worker string) string {
	return machine + "-" + worker
}

// resolvePeers checks every worker's peers and fills in PeerList and
// Session on both ends of each pair.
func (c *Config) resolvePeers() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	type ref struct{ m, w int }
	pairs := map[ref][]ref{}
	add := func(a, b ref) {
		if !slices.Contains(pairs[a], b) {
			pairs[a] = append(pairs[a], b)
		}
	}
	for mi, m := range c.Machines {
		for wi, w := range m.Workers {
			where := fmt.Sprintf("machine %q: worker %q", m.Name, w.Name)
			if len(w.Peers) > 0 && w.Ask {
				fail("machine %q: worker %q answers questions and has no peers: remove peers, or remove ask", m.Name, w.Name)
				continue
			}
			for _, name := range w.Peers {
				machine, worker, remote := strings.Cut(name, "/")
				if !remote {
					machine, worker = m.Name, name
				}
				pm := slices.IndexFunc(c.Machines, func(o Machine) bool { return o.Name == machine })
				if pm < 0 {
					fail("%s: peer %q names no machine %q", where, name, machine)
					continue
				}
				pw := slices.IndexFunc(c.Machines[pm].Workers, func(o Worker) bool { return o.Name == worker })
				switch {
				case pw < 0:
					fail("%s: peer %q is not a worker of machine %q", where, name, machine)
					continue
				case pm == mi && pw == wi:
					fail("%s: peer %q is the worker itself", where, name)
					continue
				case c.Machines[pm].Workers[pw].Ask:
					fail("%s: peer %q answers questions and has no peers", where, name)
					continue
				}
				other := c.Machines[pm].Workers[pw]
				if pm != mi && !(RemoteControl(w.CommandOrDefault()) && RemoteControl(other.CommandOrDefault())) {
					fail("%s: peer %q is on another machine, so both workers' commands need --remote-control: messages between machines go through it", where, name)
					continue
				}
				add(ref{mi, wi}, ref{pm, pw})
				add(ref{pm, pw}, ref{mi, wi})
			}
		}
	}
	// A crew is every crew worker of the fleet, each a peer of the others.
	var crew []ref
	for mi, m := range c.Machines {
		for wi, w := range m.Workers {
			if !w.Crew {
				continue
			}
			if w.Ask {
				fail("machine %q: worker %q answers questions and cannot be crew: remove crew, or remove ask", m.Name, w.Name)
				continue
			}
			crew = append(crew, ref{mi, wi})
		}
	}
	if len(crew) > 0 && c.Supervisor.GoalLabels == "" {
		fail("crew workers build goals together: set supervisor.goal_labels to the prefix of your goal labels, for example \"goal:\"")
	}
	for x, a := range crew {
		for _, b := range crew[x+1:] {
			wa, wb := c.Machines[a.m].Workers[a.w], c.Machines[b.m].Workers[b.w]
			if a.m != b.m && !(RemoteControl(wa.CommandOrDefault()) && RemoteControl(wb.CommandOrDefault())) {
				fail("machine %q: worker %q and machine %q: worker %q are crew on different machines, so both commands need --remote-control: messages between machines go through it", c.Machines[a.m].Name, wa.Name, c.Machines[b.m].Name, wb.Name)
				continue
			}
			add(a, b)
			add(b, a)
		}
	}
	for a := range pairs {
		m, w := c.Machines[a.m], c.Machines[a.m].Workers[a.w]
		if ownName(w.CommandOrDefault()) {
			fail("machine %q: worker %q has peers and sets its own session name: remove --name, and shed names it %s", m.Name, w.Name, SessionName(m.Name, w.Name))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	for a, peers := range pairs {
		w := &c.Machines[a.m].Workers[a.w]
		w.Session = SessionName(c.Machines[a.m].Name, w.Name)
		for _, b := range peers {
			pm := c.Machines[b.m]
			w.PeerList = append(w.PeerList, Peer{Machine: pm.Name, Worker: pm.Workers[b.w].Name, Session: SessionName(pm.Name, pm.Workers[b.w].Name), Remote: b.m != a.m})
		}
		slices.SortFunc(w.PeerList, func(x, y Peer) int { return strings.Compare(x.Session, y.Session) })
	}
	return nil
}

// RemoteControl reports whether a Claude Code command starts its session
// with Remote Control.
func RemoteControl(command string) bool {
	return slices.ContainsFunc(strings.Fields(command), func(f string) bool {
		return f == "--remote-control" || strings.HasPrefix(f, "--remote-control=")
	})
}

// ownName reports whether a Claude Code command names its own session.
func ownName(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 || filepath.Base(fields[0]) != "claude" {
		return false
	}
	return slices.ContainsFunc(fields, func(f string) bool {
		return f == "--name" || f == "-n" || strings.HasPrefix(f, "--name=")
	})
}
