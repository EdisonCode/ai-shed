package config

import (
	"fmt"
	"regexp"
)

// cdThenGit is a shell line that changes directory and then runs git. A
// worker's tool stops on it to ask a person, as the cd could point git at
// another repository, and an unattended worker waits there until someone
// answers. A placeholder directory such as <dir> is a rule about the
// pattern, often one that forbids it, and not an order to run it. The
// directory is one word, so prose that names cd is not read as a command.
var cdThenGit = regexp.MustCompile(`\bcd\s+[^<\s;&|][^\s;&|]*\s*(&&|;)\s*git\b`)

// Warnings returns what in the fleet file is valid but will cost the owner:
// a worker that waits for them unseen, or an order that makes it wait.
func (c *Config) Warnings() []string {
	var out []string
	workers := false
	for _, m := range c.Machines {
		for _, w := range m.Workers {
			workers = true
			if cdThenGit.MatchString(w.Brief) {
				out = append(out, fmt.Sprintf("machine %q, worker %q: the brief runs `cd <dir> && git`, which stops the worker's tool to ask a person; use `git -C <dir>` instead", m.Name, w.Name))
			}
		}
	}
	if cdThenGit.MatchString(c.Defaults.StandingOrders) {
		out = append(out, "defaults.standing_orders runs `cd <dir> && git`, which stops each worker's tool to ask a person; use `git -C <dir>` instead")
	}
	if workers && c.Supervisor.CacheTTL == "" {
		out = append(out, "supervisor.cache_ttl is not set, so a worker idle past 5m is taken to have a cold cache and is cleared before new work. Claude Code keeps a subscription session's cache for 1h: set cache_ttl: 1h if the workers sign in with a subscription, or 5m to keep the default on an API key or usage credits")
	}
	if workers && c.Defaults.Notify == "" {
		out = append(out, "defaults.notify is not set: nobody is told when a worker waits for you, for example at a permission prompt, and it waits until you look")
	}
	return out
}
