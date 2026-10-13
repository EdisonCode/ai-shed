package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/status"
	"github.com/edisoncode/ai-shed/internal/tidy"
)

// tidyTimeout bounds one machine: a fetch per clone, then one question to
// GitHub per clean worktree and branch.
const tidyTimeout = 10 * time.Minute

// cmdTidy lists the worktrees and branches on a machine whose pull request
// is merged, and removes them with -apply. Without it nothing is changed.
func cmdTidy(args []string) int {
	fs := flag.NewFlagSet("tidy", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "remove what is listed; without it tidy only lists")
	sessions := fs.Bool("sessions", false, "tidy the agent sessions shed does not supervise, not the clones")
	idle := fs.Duration("idle", 72*time.Hour, "with -sessions: close those idle longer than this")
	cfg, path, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	if fs.NArg() != 1 {
		return fail(fmt.Errorf("usage: shed tidy [-apply] [-sessions [-idle 72h]] <machine>"))
	}
	m, ok := cfg.Machine(fs.Arg(0))
	if !ok {
		return fail(fmt.Errorf("machine %q is not in %s", fs.Arg(0), path))
	}
	if *sessions {
		return tidySessions(cfg, m, *apply, *idle)
	}
	if len(m.Workers) == 0 {
		fmt.Printf("%s has no workers, so shed knows no clone on it to tidy.\n", m.Name)
		return exitOK
	}

	ctx, cancel := context.WithTimeout(context.Background(), tidyTimeout)
	defer cancel()
	runner := probe.ShellRunner{}
	out, err := runner.Run(ctx, m, m.Command(tidy.CollectCommand(m)), strings.NewReader(tidy.CollectScript()))
	if err != nil {
		return fail(fmt.Errorf("tidy: %w", err))
	}
	repos, err := tidy.Parse(string(out))
	if err != nil {
		return fail(err)
	}
	items, err := tidy.Plan(ctx, repos, tidy.GH{})
	if err != nil {
		return fail(err)
	}

	worktrees, branches := 0, 0
	for _, repo := range repos {
		name := repo.Name
		if name == "" {
			name = "not a GitHub repository: nothing in it can be shown to be merged"
		}
		fmt.Printf("%s  %s  (%s)\n", m.Name, repo.Dir, name)
		for _, section := range []struct {
			title  string
			remove bool
		}{{"merged, " + verb(*apply), true}, {"stays", false}} {
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			printed := false
			for _, it := range items {
				if it.Repo != repo || it.Remove != section.remove {
					continue
				}
				if !printed {
					fmt.Printf("  %s\n", section.title)
					printed = true
				}
				kind, what := "branch", it.Branch
				if it.Path != "" {
					kind, what = "worktree", it.Path
					if it.Branch != "" {
						what += "  [" + it.Branch + "]"
					}
				}
				fmt.Fprintf(tw, "    %s\t%s\t%s\n", kind, what, it.Why)
				if it.Remove && it.Path != "" {
					worktrees++
				} else if it.Remove {
					branches++
				}
			}
			tw.Flush()
		}
		fmt.Println()
	}

	switch {
	case worktrees+branches == 0:
		fmt.Printf("%s: nothing to tidy.\n", m.Name)
		return exitOK
	case !*apply:
		fmt.Printf("%s: %d worktree(s) and %d branch(es) are merged. Nothing was changed. Run `shed tidy -apply %s` to remove them.\n", m.Name, worktrees, branches, m.Name)
		return exitOK
	}

	out, err = runner.Run(ctx, m, m.Command("sh -s"), strings.NewReader(tidy.ApplyScript(items)))
	res, parseErr := tidy.ParseApplied(string(out))
	fmt.Printf("%s: removed %d worktree(s) and deleted %d branch(es).\n", m.Name, len(res.RemovedWorktrees), len(res.DeletedBranches))
	for _, kept := range res.Kept {
		fmt.Printf("  left alone: %s\n", kept)
	}
	if err != nil {
		return fail(fmt.Errorf("tidy: %w", err))
	}
	if parseErr != nil {
		return fail(parseErr)
	}
	if len(res.Kept) > 0 {
		return exitAttention
	}
	return exitOK
}

func verb(apply bool) string {
	if apply {
		return "to be removed"
	}
	return "would be removed"
}

// tidySessions lists the agent sessions on the machine that shed does not
// supervise, and with apply closes those idle longer than idle. They share
// the machine's account limit and CPU with its workers.
func tidySessions(cfg *config.Config, m config.Machine, apply bool, idle time.Duration) int {
	if len(m.Workers) == 0 {
		fmt.Printf("%s has no supervised workers: every session on it is yours.\n", m.Name)
		return exitOK
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	runner := probe.ShellRunner{}
	res, err := probe.Run(ctx, runner, m, cfg.ChecksFor(m))
	if err != nil {
		return fail(fmt.Errorf("tidy: %w", err))
	}
	found := tidy.Sessions(res.Windows, status.Unsupervised(m, res.Windows), res.Now, idle)
	if len(found) == 0 {
		fmt.Printf("%s: no unsupervised agent sessions.\n", m.Name)
		return exitOK
	}
	closing := 0
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, s := range found {
		what := "stays"
		if s.Close {
			what = "would be closed"
			if apply {
				what = "to be closed"
			}
			closing++
		}
		fmt.Fprintf(tw, "  %s\tidle %s\t%s\n", s.Name, status.Short(s.Idle), what)
	}
	tw.Flush()
	switch {
	case closing == 0:
		fmt.Printf("%s: none idle longer than %s.\n", m.Name, status.Short(idle))
		return exitOK
	case !apply:
		fmt.Printf("%s: %d session(s) idle longer than %s. Nothing was changed. Run `shed tidy -sessions -apply %s` to close them.\n", m.Name, closing, status.Short(idle), m.Name)
		return exitOK
	}
	out, err := runner.Run(ctx, m, m.Command("sh -s"), strings.NewReader(tidy.CloseSessionsScript(found)))
	closed := strings.Count(string(out), "@@closed ")
	fmt.Printf("%s: closed %d of %d session(s).\n", m.Name, closed, closing)
	if err != nil || closed < closing {
		return exitAttention
	}
	return exitOK
}
