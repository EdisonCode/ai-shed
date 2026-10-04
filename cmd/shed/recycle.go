package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/edisoncode/ai-shed/internal/deploy"
	"github.com/edisoncode/ai-shed/internal/probe"
)

// cmdRecycle asks a machine's agent to replace a worker's session with a
// fresh one, so the worker picks up tools and settings that changed after it
// started.
func cmdRecycle(args []string) int {
	fs := flag.NewFlagSet("recycle", flag.ContinueOnError)
	now := fs.Bool("now", false, "end the session at once, even in the middle of an issue")
	cfg, path, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	if fs.NArg() != 2 {
		return fail(fmt.Errorf("usage: shed recycle [-now] <machine> <worker>"))
	}
	m, ok := cfg.Machine(fs.Arg(0))
	if !ok {
		return fail(fmt.Errorf("machine %q is not in %s", fs.Arg(0), path))
	}
	worker := fs.Arg(1)
	known := false
	for _, w := range m.Workers {
		known = known || w.Name == worker
	}
	if !known {
		return fail(fmt.Errorf("machine %s has no worker %q", m.Name, worker))
	}
	if err := deploy.Recycle(context.Background(), probe.ShellRunner{}, m, worker, *now); err != nil {
		return fail(err)
	}
	if *now {
		fmt.Printf("%s: worker %s gets a fresh session within a minute\n", m.Name, worker)
	} else {
		fmt.Printf("%s: worker %s gets a fresh session as soon as it has no issue in progress\n", m.Name, worker)
	}
	return exitOK
}
