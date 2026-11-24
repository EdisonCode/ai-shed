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
				if pm != mi && !(remoteControl(w.CommandOrDefault()) && remoteControl(other.CommandOrDefault())) {
					fail("%s: peer %q is on another machine, so both workers' commands need --remote-control: messages between machines go through it", where, name)
					continue
				}
				add(ref{mi, wi}, ref{pm, pw})
				add(ref{pm, pw}, ref{mi, wi})
			}
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

// remoteControl reports whether a Claude Code command starts its session
// with Remote Control.
func remoteControl(command string) bool {
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
