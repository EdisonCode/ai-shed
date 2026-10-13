package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/deploy"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/runlog"
	"github.com/edisoncode/ai-shed/internal/sources"
	"github.com/edisoncode/ai-shed/internal/supervisor"
)

// contextReport is what `shed context -here -json` prints on a machine.
type contextReport struct {
	Version string        `json:"version"`
	Audit   sources.Audit `json:"audit"`
}

// cmdContext audits what a worker's tool loads before its first item. It
// changes nothing. From the owner's machine it asks the worker's machine;
// with -here it runs there.
func cmdContext(args []string) int {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	here := fs.Bool("here", false, "run on the worker's machine (shed context runs this over SSH)")
	contradictions := fs.Bool("contradictions", false, "also ask the reviewer model which statements contradict each other")
	asJSON := fs.Bool("json", false, "print the audit as JSON")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	if fs.NArg() != 2 {
		return fail(errors.New("usage: shed context [-contradictions] [-json] <machine> <worker>"))
	}
	m, ok := cfg.Machine(fs.Arg(0))
	if !ok {
		return fail(fmt.Errorf("machine %q is not in the fleet file", fs.Arg(0)))
	}
	w, ok := workerOf(m, fs.Arg(1))
	if !ok {
		return fail(fmt.Errorf("machine %s has no worker %q", m.Name, fs.Arg(1)))
	}
	ctx := context.Background()
	var audit sources.Audit
	if *here {
		audit, err = localAudit(ctx, cfg, m, w, *contradictions)
	} else {
		audit, err = remoteAudit(ctx, probe.ShellRunner{}, m, w, *contradictions)
	}
	if err != nil {
		return fail(err)
	}
	if *asJSON || *here {
		if err := json.NewEncoder(os.Stdout).Encode(contextReport{Version: version, Audit: audit}); err != nil {
			return fail(err)
		}
	} else {
		renderAudit(m, w, audit)
	}
	if *here {
		// The report carries the findings; an exit code would read as a
		// failure on the owner's side.
		return exitOK
	}
	if len(audit.Duplicates)+len(audit.Stale)+len(audit.Contradictions)+len(audit.Hints) > 0 {
		return exitAttention
	}
	return exitOK
}

func workerOf(m config.Machine, name string) (config.Worker, bool) {
	for _, w := range m.Workers {
		if w.Name == name {
			return w, true
		}
	}
	return config.Worker{}, false
}

// localAudit runs on the worker's machine, where its files are.
func localAudit(ctx context.Context, cfg *config.Config, m config.Machine, w config.Worker, withContradictions bool) (sources.Audit, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return sources.Audit{}, err
	}
	dir := w.Dir
	if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		dir = filepath.Join(home, rest)
	}
	var issueOpen sources.IssueOpen
	if srcs := m.SourcesFor(w); len(srcs) > 0 {
		repo := srcs[0].Repo
		issueOpen = func(ctx context.Context, n int) (bool, error) {
			out, err := exec.CommandContext(ctx, "gh", "issue", "view", strconv.Itoa(n), "--repo", repo, "--json", "state", "--jq", ".state").Output()
			if err != nil {
				return false, fmt.Errorf("gh issue view %s#%d: %w", repo, n, err)
			}
			return strings.TrimSpace(string(out)) == "OPEN", nil
		}
	}
	audit := sources.Run(ctx, dir, home, issueOpen)
	if withContradictions {
		reviewer := supervisor.CommandReviewer{Command: m.Command(cfg.Supervisor.CommandOrDefault()), Dir: filepath.Join(home, runlog.StateDir)}
		found, err := sources.Contradictions(ctx, audit.Sources, reviewer.Ask)
		if err != nil {
			audit.Problems = append(audit.Problems, "contradictions: "+err.Error())
		}
		audit.Contradictions = found
	}
	return audit, nil
}

// remoteAudit asks the worker's machine for its audit over SSH.
func remoteAudit(ctx context.Context, runner probe.Runner, m config.Machine, w config.Worker, withContradictions bool) (sources.Audit, error) {
	command := `"$HOME/` + deploy.RemoteBinary + `" context -here `
	if withContradictions {
		command += "-contradictions "
	}
	command += shellWord(m.Name) + " " + shellWord(w.Name)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, runErr := runner.Run(ctx, m, m.Command(command), nil)
	// A report is a report whatever the exit code: an older shed exits 1
	// when it finds something.
	var report contextReport
	if err := json.Unmarshal(out, &report); err != nil {
		if runErr != nil {
			return sources.Audit{}, runErr
		}
		return sources.Audit{}, fmt.Errorf("read the machine's audit: %w", err)
	}
	if report.Version != version {
		return sources.Audit{}, fmt.Errorf("the machine runs shed %s, this is %s: deploy it", report.Version, version)
	}
	return report.Audit, nil
}

func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func renderAudit(m config.Machine, w config.Worker, a sources.Audit) {
	fmt.Printf("%s %s: about %s tokens before its first item, read again on every turn\n", m.Name, w.Name, kilo(a.Total))
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, s := range a.Sources {
		fmt.Fprintf(tw, "  %s\t~%s\t%s\n", s.Kind, kilo(s.Tokens), s.Path)
	}
	tw.Flush()
	if len(a.Duplicates) > 0 {
		fmt.Printf("\nSaid twice: %d (one home per rule; every copy costs tokens on every turn)\n", len(a.Duplicates))
		// Grouped by the two sources, a few lines each: two copies of one
		// document would otherwise list every line of it.
		var pairs []string
		byPair := map[string][]sources.Duplicate{}
		for _, d := range a.Duplicates {
			pair := d.A.Path + " and " + d.B.Path
			if byPair[pair] == nil {
				pairs = append(pairs, pair)
			}
			byPair[pair] = append(byPair[pair], d)
		}
		for _, pair := range pairs {
			ds := byPair[pair]
			fmt.Printf("  %s: %d\n", pair, len(ds))
			for _, d := range ds[:min(len(ds), 3)] {
				fmt.Printf("    lines %d and %d: %s\n", d.A.Line, d.B.Line, d.Excerpt)
			}
			if len(ds) > 3 {
				fmt.Printf("    and %d more\n", len(ds)-3)
			}
		}
	}
	if len(a.Stale) > 0 {
		fmt.Printf("\nStale references: %d\n", len(a.Stale))
		for _, s := range a.Stale {
			fmt.Printf("  %s:%d  %s: %s\n", s.Path, s.Line, s.Ref, s.Why)
		}
	}
	if len(a.Contradictions) > 0 {
		fmt.Printf("\nLikely contradictions: %d (read by the reviewer model; settle each yourself)\n", len(a.Contradictions))
		for _, c := range a.Contradictions {
			fmt.Printf("  %q\n    against %q\n    %s\n", c.A, c.B, c.Why)
		}
	}
	for _, h := range a.Hints {
		fmt.Printf("\n%s\n", h)
	}
	for _, p := range a.Problems {
		fmt.Printf("\n! %s\n", p)
	}
}

// kilo writes a token count as a person scans it: 13200 is 13.2k.
func kilo(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1000), ".0") + "k"
}
