package main

import (
	"context"
	"flag"
	"fmt"
	"slices"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/deploy"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/status"
)

// defaultPause is how long a pause lasts when the owner names no time. A
// pause always runs out: one that was forgotten must not idle a machine for
// the whole of an absence.
const defaultPause = 30 * time.Minute

// cmdPause tells a machine's agent to leave its workers alone for a while,
// so that something heavy on the machine can finish.
func cmdPause(args []string) int {
	fs := flag.NewFlagSet("pause", flag.ContinueOnError)
	length := fs.Duration("for", defaultPause, "how long the pause lasts, for example 45m")
	cfg, path, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	if fs.NArg() != 1 || *length <= 0 {
		return fail(fmt.Errorf("usage: shed pause [-for 30m] <machine>"))
	}
	m, ok := cfg.Machine(fs.Arg(0))
	if !ok {
		return fail(fmt.Errorf("machine %q is not in %s", fs.Arg(0), path))
	}
	if len(m.Workers) == 0 {
		return fail(fmt.Errorf("machine %s has no supervised workers to pause", m.Name))
	}
	if _, err := deploy.Pause(context.Background(), probe.ShellRunner{}, m, *length); err != nil {
		return fail(err)
	}
	fmt.Printf("%s: paused for %s. Nothing is handed out and no worker is nudged or started.\n", m.Name, pauseWords(*length))
	fmt.Println("A worker finishes the turn it is in and then waits; work in progress is not stopped.")
	fmt.Printf("To end it early: shed resume %s\n", m.Name)
	return exitOK
}

// pauseWords says a pause's length as the owner would: 30m, 2h, 1h30m.
func pauseWords(d time.Duration) string {
	if d < time.Hour || d%time.Hour == 0 {
		return status.Short(d)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// cmdResume ends a machine's pause before it runs out, and ends the hold on
// workers that wait for a usage limit the owner knows is over.
func cmdResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	cfg, path, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	if fs.NArg() != 1 && fs.NArg() != 2 {
		return fail(fmt.Errorf("usage: shed resume <machine> [worker]"))
	}
	m, ok := cfg.Machine(fs.Arg(0))
	if !ok {
		return fail(fmt.Errorf("machine %q is not in %s", fs.Arg(0), path))
	}
	ctx, runner := context.Background(), probe.ShellRunner{}
	// With a worker named, only its limit hold ends: a pause is the machine's.
	if worker := fs.Arg(1); worker != "" {
		if !slices.ContainsFunc(m.Workers, func(w config.Worker) bool { return w.Name == worker }) {
			return fail(fmt.Errorf("machine %s has no worker %q", m.Name, worker))
		}
		if err := deploy.EndLimitHolds(ctx, runner, m, []string{worker}); err != nil {
			return fail(err)
		}
		fmt.Printf("%s: worker %s is looked at again within a minute if it was held at a usage limit, and told to continue.\n", m.Name, worker)
		return exitOK
	}
	resumed, err := deploy.Resume(ctx, runner, m)
	if err != nil {
		return fail(err)
	}
	if resumed {
		fmt.Printf("%s: resumed. Its workers are looked at again within a minute.\n", m.Name)
	} else {
		fmt.Printf("%s: was not paused\n", m.Name)
	}
	if len(m.Workers) == 0 {
		return exitOK
	}
	workers := make([]string, len(m.Workers))
	for i, w := range m.Workers {
		workers[i] = w.Name
	}
	if err := deploy.EndLimitHolds(ctx, runner, m, workers); err != nil {
		return fail(err)
	}
	fmt.Printf("%s: a worker held at a usage limit is looked at again within a minute, and told to continue.\n", m.Name)
	return exitOK
}
