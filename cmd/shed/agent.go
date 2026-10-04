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
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/deploy"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/release"
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
	dist := fs.String("dist", "", "send locally built binaries from this directory (see `make dist`) instead of the release")
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
	binaryFor, err := binarySource(*dist)
	if err != nil {
		return fail(err)
	}

	ctx := context.Background()
	runner := probe.ShellRunner{}
	code := exitOK
	for _, m := range machines {
		if err := deployOne(ctx, runner, cfg, path, m, binaryFor); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", m.Name, err)
			code = exitError
		}
	}
	return code
}

// binarySource says where deploy gets a machine's binary: a local directory
// of builds, or the published release of this shed's own version.
func binarySource(dist string) (func(platform string) (string, error), error) {
	if dist != "" {
		return func(platform string) (string, error) { return filepath.Join(dist, "shed-"+platform), nil }, nil
	}
	if !release.IsRelease(version) {
		return nil, fmt.Errorf("this shed is an unreleased build (%s): run `shed update` to get a release, or `make dist` and pass -dist dist", version)
	}
	cache, err := release.CacheDir()
	if err != nil {
		return nil, err
	}
	return func(platform string) (string, error) {
		return release.Binary(context.Background(), release.GH{}, cache, version, platform)
	}, nil
}

func deployOne(ctx context.Context, runner probe.Runner, cfg *config.Config, path string, m config.Machine, binaryFor func(string) (string, error)) error {
	res, err := deploy.Machine(ctx, runner, m, version, binaryFor, path)
	if err != nil {
		return err
	}
	if !res.BinaryInstalled {
		// The agent loads a changed fleet file by itself.
		fmt.Printf("%s: fleet file sent; shed %s was already installed\n", m.Name, version)
	} else {
		restarted, err := deploy.RestartAgent(ctx, runner, m, res.Platform)
		switch {
		case err != nil:
			return err
		case restarted:
			fmt.Printf("%s: shed %s installed, fleet file sent, agent restarted\n", m.Name, version)
		default:
			fmt.Printf("%s: shed %s installed, fleet file sent; the agent service is not set up on this machine (see the README)\n", m.Name, version)
		}
	}
	// The hook runs only after a good copy: it adds to a deployed machine.
	return deploy.Hook(ctx, cfg.Defaults.DeployHook, m, os.Stdout, os.Stderr)
}
