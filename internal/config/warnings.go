package config

import (
	"fmt"
	"regexp"
)

// cdThenGit is a shell line that changes directory and then runs git. A
// worker's tool stops on it to ask a person, as the cd could point git at
// another repository, and an unattended worker waits there until someone
// answers.
var cdThenGit = regexp.MustCompile(`\bcd\s+[^;&|\n]+(&&|;)\s*git\b`)

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
	if workers && c.Defaults.Notify == "" {
		out = append(out, "defaults.notify is not set: nobody is told when a worker waits for you, for example at a permission prompt, and it waits until you look")
	}
	return out
}
