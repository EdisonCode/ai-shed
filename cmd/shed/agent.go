package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/edisoncode/ai-shed/internal/agent"
	"github.com/edisoncode/ai-shed/internal/deploy"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

func cmdAgent(args []string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	machine := fs.String("machine", "", "this machine's name in the fleet file (default: the name `shed deploy` wrote)")
	_, path, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	if *machine == "" {
		name, err := os.ReadFile(filepath.Join(home, deploy.RemoteMachine))
		if err != nil {
			return fail(errors.New("which machine is this? Pass -machine <name>, or run `shed deploy` from the watcher"))
		}
		*machine = strings.TrimSpace(string(name))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := agent.Agent{ConfigPath: path, Machine: *machine, StateDir: filepath.Join(home, runlog.StateDir), Logf: log.Printf}
	if err := a.Run(ctx); err != nil {
		return fail(err)
	}
	return exitOK
}

func cmdDeploy(args []string) int {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	dist := fs.String("dist", "dist", "directory with the binaries from `make dist`")
	cfg, path, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	machines := cfg.Machines
	if names := fs.Args(); len(names) > 0 {
		machines = nil
		for _, name := range names {
			m, ok := cfg.Machine(name)
			if !ok {
				return fail(fmt.Errorf("machine %q is not in %s", name, path))
			}
			machines = append(machines, m)
		}
	}

	code := exitOK
	for _, m := range machines {
		if err := deploy.Machine(context.Background(), probe.ShellRunner{}, m, *dist, path); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", m.Name, err)
			code = exitError
			continue
		}
		fmt.Printf("%s: installed ~/%s and ~/%s\n", m.Name, deploy.RemoteBinary, deploy.RemoteConfig)
	}
	return code
}
