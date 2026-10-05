package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/edisoncode/ai-shed/internal/probe"
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
	cfg, path, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	if fs.NArg() != 1 {
		return fail(fmt.Errorf("usage: shed tidy [-apply] <machine>"))
	}
	m, ok := cfg.Machine(fs.Arg(0))
	if !ok {
		return fail(fmt.Errorf("machine %q is not in %s", fs.Arg(0), path))
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
