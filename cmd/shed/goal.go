package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/edisoncode/ai-shed/internal/deploy"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// cmdGoal focuses the fleet on a goal, tables one, or clears either, on every
// machine that hands out a queue. A goal in focus goes ahead of every other
// goal in each queue; a tabled goal's issues that nobody started are held.
func cmdGoal(args []string) int {
	fs := flag.NewFlagSet("goal", flag.ContinueOnError)
	cfg, path, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	const usage = "usage: shed goal focus|table|clear <goal label>"
	if fs.NArg() < 2 {
		return fail(fmt.Errorf("%s", usage))
	}
	modes := map[string]string{"focus": runlog.GoalFocus, "table": runlog.GoalTabled, "clear": ""}
	mode, ok := modes[fs.Arg(0)]
	if !ok {
		return fail(fmt.Errorf("%s", usage))
	}
	label := strings.Join(fs.Args()[1:], " ")
	prefix := cfg.Supervisor.GoalLabels
	if prefix == "" {
		return fail(fmt.Errorf("%s sets no goal_labels: the fleet has no goals", path))
	}
	if !strings.HasPrefix(label, prefix) {
		return fail(fmt.Errorf("%q is not a goal label: goal labels start with %q", label, prefix))
	}
	done, err := deploy.SetFleetGoalMode(context.Background(), probe.ShellRunner{}, cfg, label, mode)
	if len(done) > 0 {
		switch mode {
		case runlog.GoalFocus:
			fmt.Printf("%s is in focus on %s: its issues go ahead of every other goal's.\n", label, strings.Join(done, ", "))
		case runlog.GoalTabled:
			fmt.Printf("%s is tabled on %s: its issues that nobody started are held; work already started goes on.\n", label, strings.Join(done, ", "))
		default:
			fmt.Printf("%s is neither in focus nor tabled on %s.\n", label, strings.Join(done, ", "))
		}
	}
	if err != nil {
		return fail(err)
	}
	return exitOK
}
