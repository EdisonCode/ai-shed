package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/deploy"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// cmdTask pushes a one-off task with a brief of its own to a machine, prints
// the report a worker wrote for one, or cancels one that still waits. The machine's agent hands the
// task to the worker ahead of its queue, when the worker has nothing in
// progress.
func cmdTask(args []string) int {
	fs := flag.NewFlagSet("task", flag.ContinueOnError)
	worker := fs.String("worker", "", "the worker the task is for (default: the first that is free)")
	report := fs.String("report", "", "print the report of the task with this id")
	cancel := fs.String("cancel", "", "cancel the task with this id, if no worker was handed it yet")
	cfg, path, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	const usage = "usage: shed task [-worker name] <machine> <brief-file|->\n       shed task -report <id> <machine>\n       shed task -cancel <id> <machine>"
	if fs.NArg() < 1 {
		return fail(fmt.Errorf("%s", usage))
	}
	m, ok := cfg.Machine(fs.Arg(0))
	if !ok {
		return fail(fmt.Errorf("machine %q is not in %s", fs.Arg(0), path))
	}
	ctx := context.Background()
	runner := probe.ShellRunner{}

	if *report != "" {
		if fs.NArg() != 1 || !runlog.ValidTaskID(*report) {
			return fail(fmt.Errorf("%s", usage))
		}
		text, found, err := deploy.TaskReport(ctx, runner, m, *report)
		if err != nil {
			return fail(err)
		}
		if !found {
			fmt.Fprintf(os.Stderr, "shed: no worker on %s has written a report for task %s yet; `shed status` shows where the task is\n", m.Name, *report)
			return exitAttention
		}
		fmt.Print(text)
		return exitOK
	}

	if *cancel != "" {
		if fs.NArg() != 1 || !runlog.ValidTaskID(*cancel) {
			return fail(fmt.Errorf("%s", usage))
		}
		outcome, err := deploy.CancelTask(ctx, runner, m, *cancel)
		switch {
		case err != nil:
			return fail(err)
		case outcome == deploy.TaskCancelled:
			fmt.Printf("%s: task %s is cancelled. No worker was handed it.\n", m.Name, *cancel)
			return exitOK
		case outcome == deploy.TaskTaken:
			fmt.Fprintf(os.Stderr, "shed: task %s was already handed to a worker on %s and is not cancelled; `shed status` shows who has it\n", *cancel, m.Name)
			return exitAttention
		}
		return fail(fmt.Errorf("machine %s has no task %s", m.Name, *cancel))
	}

	if fs.NArg() != 2 {
		return fail(fmt.Errorf("%s", usage))
	}
	if len(m.Workers) == 0 {
		return fail(fmt.Errorf("machine %s has no workers to take a task", m.Name))
	}
	if *worker != "" {
		known := false
		for _, w := range m.Workers {
			known = known || w.Name == *worker
		}
		if !known {
			return fail(fmt.Errorf("machine %s has no worker %q", m.Name, *worker))
		}
	}
	var brief []byte
	if fs.Arg(1) == "-" {
		brief, err = io.ReadAll(os.Stdin)
	} else {
		brief, err = os.ReadFile(fs.Arg(1))
	}
	if err != nil {
		return fail(fmt.Errorf("read the brief: %w", err))
	}
	if strings.TrimSpace(string(brief)) == "" {
		return fail(fmt.Errorf("the brief is empty: a worker has nothing else to go on"))
	}
	// The id is the time the task was pushed, to the millisecond: tasks sort
	// oldest first, and two pushed in one second do not share a name.
	now := time.Now()
	id := fmt.Sprintf("%s-%03d", now.Format("20060102-150405"), now.Nanosecond()/int(time.Millisecond))
	if err := deploy.PushTask(ctx, runner, m, *worker, id, brief); err != nil {
		return fail(err)
	}
	who := "the first worker that is free"
	if *worker != "" {
		who = "worker " + *worker
	}
	fmt.Printf("%s: task %s is queued for %s. It is handed over when that worker has nothing in progress.\n", m.Name, id, who)
	fmt.Printf("Its report: shed task -report %s %s\n", id, m.Name)
	return exitOK
}
