package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/deploy"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/supervisor"
)

// preflightReport is what `shed preflight -json` prints on a machine and
// `shed deploy` reads on the watcher.
type preflightReport struct {
	Version string                       `json:"version"`
	Workers []supervisor.PreflightResult `json:"workers"`
	// Rules are the tool permission rules the machine's workers load, for
	// deploy to compare across machines.
	Rules []string `json:"rules,omitempty"`
}

// cmdPreflight runs on a worker machine. It starts each worker's command once
// in a throwaway window and reports whether it came up ready.
func cmdPreflight(args []string) int {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	machine := fs.String("machine", "", "this machine's name in the fleet file (default: the name `shed deploy` wrote)")
	asJSON := fs.Bool("json", false, "print the captured screens as JSON and do not classify them")
	cfg, path, err := loadConfig(fs, args)
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
	m, ok := cfg.Machine(name)
	if !ok {
		return fail(fmt.Errorf("machine %q is not in %s", name, path))
	}

	report := preflightReport{Version: version, Workers: []supervisor.PreflightResult{}}
	rules := map[string]bool{}
	for _, w := range m.Workers {
		res := supervisor.Preflight(supervisor.Tmux{}, w, supervisor.DirReadable, time.Now, time.Sleep)
		res.Warnings = append(supervisor.HookWarnings(home, w), supervisor.InboundWarnings(home, w)...)
		found, err := supervisor.PermissionRules(home, w)
		if err != nil {
			res.Warnings = append(res.Warnings, err.Error())
		}
		for _, r := range found {
			rules[r] = true
		}
		report.Workers = append(report.Workers, res)
	}
	report.Rules = slices.Sorted(maps.Keys(rules))
	if *asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			return fail(err)
		}
		return exitOK
	}
	reviewer := supervisor.CommandReviewer{Command: m.Command(cfg.Supervisor.CommandOrDefault()), Dir: os.TempDir()}
	if classify(context.Background(), reviewer, "", report.Workers) > 0 {
		return exitAttention
	}
	return exitOK
}

// classify prints one line per worker and returns how many are not ready.
func classify(ctx context.Context, reviewer supervisor.Asker, prefix string, results []supervisor.PreflightResult) int {
	notReady := 0
	for _, r := range results {
		line, ready := supervisor.Classify(ctx, reviewer, r)
		fmt.Println(prefix + line)
		if !ready {
			notReady++
		}
	}
	return notReady
}

// preflightMachine runs the preflight on a machine and reads the screens
// here, on the watcher, where the reviewer is known to be logged in.
func preflightMachine(ctx context.Context, runner probe.Runner, cfg *config.Config, m config.Machine) (notReady int, rules []string, err error) {
	fmt.Printf("%s: starting each worker's command once to check it comes up ready...\n", m.Name)
	out, err := runner.Run(ctx, m, m.Command(`"$HOME/`+deploy.RemoteBinary+`" preflight -json`), nil)
	if err != nil {
		return 0, nil, fmt.Errorf("preflight: %w", err)
	}
	var report preflightReport
	if err := json.Unmarshal(out, &report); err != nil {
		return 0, nil, fmt.Errorf("preflight: read the machine's report: %w", err)
	}
	if report.Version != version {
		return 0, nil, fmt.Errorf("preflight: the machine runs shed %s, this is %s", report.Version, version)
	}
	reviewer := supervisor.CommandReviewer{Command: cfg.Supervisor.CommandOrDefault(), Dir: os.TempDir()}
	return classify(ctx, reviewer, m.Name+": worker ", report.Workers), report.Rules, nil
}

// ruleDifferences lists, for the machines whose rules were read, each
// permission rule that some of them have and others lack: a worker on a
// machine without it meets a refusal that one on another machine does not.
func ruleDifferences(rules map[string][]string) []string {
	if len(rules) < 2 {
		return nil
	}
	machines := slices.Sorted(maps.Keys(rules))
	on := map[string][]string{}
	for _, m := range machines {
		for _, r := range rules[m] {
			on[r] = append(on[r], m)
		}
	}
	var lines []string
	for _, r := range slices.Sorted(maps.Keys(on)) {
		if len(on[r]) == len(machines) {
			continue
		}
		missing := slices.DeleteFunc(slices.Clone(machines), func(m string) bool { return slices.Contains(on[r], m) })
		lines = append(lines, fmt.Sprintf("%s: on %s; not on %s", r, strings.Join(on[r], ", "), strings.Join(missing, ", ")))
	}
	return lines
}
