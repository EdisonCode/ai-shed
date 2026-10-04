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
	name, err := thisMachine(*machine, home)
	if err != nil {
		return fail(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := agent.Agent{ConfigPath: path, Machine: name, StateDir: filepath.Join(home, runlog.StateDir), Logf: log.Printf}
	if err := a.Run(ctx); err != nil {
		return fail(err)
	}
	return exitOK
}

// thisMachine is the name of the machine a command runs on: the flag, or the
// name that `shed deploy` wrote.
func thisMachine(flagValue, home string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	name, err := os.ReadFile(filepath.Join(home, deploy.RemoteMachine))
	if err != nil {
		return "", errors.New("which machine is this? Pass -machine <name>, or run `shed deploy` from the watcher")
	}
	return strings.TrimSpace(string(name)), nil
}

func cmdDeploy(args []string) int {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	dist := fs.String("dist", "", "send locally built binaries from this directory (see `make dist`) instead of the release")
	preflight := fs.Bool("preflight", true, "start each worker's command once and check that it comes up ready")
	installAgent := fs.Bool("install-agent", false, "set up and start the agent service on a machine that has none")
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
		fmt.Fprintln(os.Stderr, "Deploy FAILED: nothing was sent to any machine.")
		return fail(err)
	}

	ctx := context.Background()
	runner := probe.ShellRunner{}
	var ok, failed []string
	for _, m := range machines {
		if err := deployOne(ctx, runner, cfg, path, m, binaryFor, *preflight, *installAgent); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", m.Name, err)
			failed = append(failed, m.Name)
			continue
		}
		ok = append(ok, m.Name)
	}
	// The last line says the outcome in full, for output that is cut or piped.
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "Deploy FAILED for: %s. Deployed and ready: %s.\n", strings.Join(failed, ", "), orNone(ok))
		return exitError
	}
	fmt.Printf("Deployed and ready: %s.\n", strings.Join(ok, ", "))
	return exitOK
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// binarySource says where deploy gets a machine's binary: a local directory
// of builds, or the published release of this shed's own version.
func binarySource(dist string) (func(platform string) (string, error), error) {
	if dist != "" {
		return func(platform string) (string, error) { return filepath.Join(dist, "shed-"+platform), nil }, nil
	}
	if !release.IsRelease(version) {
		return nil, fmt.Errorf("this shed is an unreleased build (%s). Run `shed update` to get a release, or `make dist` and pass -dist dist", version)
	}
	cache, err := release.CacheDir()
	if err != nil {
		return nil, err
	}
	return func(platform string) (string, error) {
		return release.Binary(context.Background(), release.GH{}, cache, version, platform)
	}, nil
}

func deployOne(ctx context.Context, runner probe.Runner, cfg *config.Config, path string, m config.Machine, binaryFor func(string) (string, error), preflight, installAgent bool) error {
	res, err := deploy.Machine(ctx, runner, m, version, binaryFor, path)
	if err != nil {
		return err
	}
	if !res.BinaryInstalled {
		// The agent loads a changed fleet file by itself.
		fmt.Printf("%s: fleet file sent; shed %s was already installed\n", m.Name, version)
	} else {
		restarted, err := deploy.RestartAgent(ctx, runner, m, res.Platform)
		if err != nil {
			return err
		}
		fmt.Printf("%s: shed %s installed, fleet file sent", m.Name, version)
		if restarted {
			fmt.Print(", agent restarted")
		}
		fmt.Println()
	}
	// The hook runs only after a good copy: it adds to a deployed machine.
	if err := deploy.Hook(ctx, cfg.Defaults.DeployHook, m, os.Stdout, os.Stderr); err != nil {
		return err
	}

	notReady := 0
	if preflight && len(m.Workers) > 0 {
		if notReady, err = preflightMachine(ctx, runner, cfg, m); err != nil {
			return err
		}
	}
	installed, err := deploy.AgentInstalled(ctx, runner, m, res.Platform)
	if err != nil {
		return err
	}
	switch {
	case installed:
	case notReady > 0:
		// An agent started now would run a worker that cannot work.
		fmt.Printf("%s: the agent service is not set up, and was not installed because a worker is not ready\n", m.Name)
	case !installAgent:
		fmt.Printf("%s: the agent service is not set up; run `shed deploy -install-agent %s` to install and start it\n", m.Name, m.Name)
	default:
		note, err := deploy.InstallAgent(ctx, runner, m, res.Platform)
		if err != nil {
			return err
		}
		fmt.Printf("%s: agent service installed and started\n", m.Name)
		if note != "" {
			fmt.Printf("%s: %s\n", m.Name, note)
		}
	}
	if notReady > 0 {
		return fmt.Errorf("%d worker(s) not ready", notReady)
	}
	return nil
}
