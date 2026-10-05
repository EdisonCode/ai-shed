package main

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/deploy"
	"github.com/edisoncode/ai-shed/internal/probe"
)

// cmdBump puts an open issue first in line on a machine: in the queue it is
// in, or in the queue of the worker the owner names. The machine's agent
// hands it over at the next point where a worker has nothing in progress.
func cmdBump(args []string) int {
	fs := flag.NewFlagSet("bump", flag.ContinueOnError)
	worker := fs.String("worker", "", "the worker the issue goes to (default: the workers whose queue it is in)")
	undo := fs.Bool("undo", false, "put the issue back in its usual place")
	cfg, path, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	const usage = "usage: shed bump [-worker name] <machine> <issue>\n       shed bump -undo <machine> <issue>"
	if fs.NArg() != 2 || (*undo && *worker != "") {
		return fail(fmt.Errorf("%s", usage))
	}
	m, ok := cfg.Machine(fs.Arg(0))
	if !ok {
		return fail(fmt.Errorf("machine %q is not in %s", fs.Arg(0), path))
	}
	issue, err := strconv.Atoi(strings.TrimPrefix(fs.Arg(1), "#"))
	if err != nil || issue <= 0 {
		return fail(fmt.Errorf("%q is not an issue number", fs.Arg(1)))
	}
	ctx := context.Background()
	runner := probe.ShellRunner{}

	if *undo {
		removed, err := deploy.Unbump(ctx, runner, m, issue)
		if err != nil {
			return fail(err)
		}
		if !removed {
			fmt.Printf("%s: #%d was not bumped\n", m.Name, issue)
			return exitOK
		}
		fmt.Printf("%s: #%d is back in its usual place in the queue\n", m.Name, issue)
		return exitOK
	}

	if len(m.Workers) == 0 {
		return fail(fmt.Errorf("machine %s has no supervised workers: nothing there hands out a queue", m.Name))
	}
	if *worker != "" {
		known := false
		for _, w := range m.Workers {
			known = known || w.Name == *worker
		}
		if !known {
			return fail(fmt.Errorf("machine %s has no worker %q", m.Name, *worker))
		}
	}
	// A bump of an issue no queue has would do nothing, and say nothing.
	found, open, err := backlog.Find(ctx, backlog.GH{}, m.AllSources(), issue)
	if err != nil {
		return fail(err)
	}
	if !open {
		return fail(fmt.Errorf("#%d is not open in any queue of machine %s: an issue is bumped on the machine whose `issues` select it", issue, m.Name))
	}
	if err := deploy.Bump(ctx, runner, m, issue, *worker); err != nil {
		return fail(err)
	}
	if *worker != "" {
		fmt.Printf("%s: #%d (%s) is first in line for worker %s, and out of the other workers' queues.\n", m.Name, issue, found.Title, *worker)
	} else {
		fmt.Printf("%s: #%d (%s) is first in its queue.\n", m.Name, issue, found.Title)
	}
	fmt.Println("It is handed over when a worker has nothing in progress. Nobody is interrupted, and an issue a worker already has in hand stays with it.")
	fmt.Printf("To take it back: shed bump -undo %s %d\n", m.Name, issue)
	return exitOK
}
