package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/deploy"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/runlog"
	"github.com/edisoncode/ai-shed/internal/status"
)

// cmdQueue prints each worker's queue as the machine's agent last computed
// it: what it would hand over, why it holds the rest, and what became of
// the reserve. `shed status` reads the issues from the watcher; this is
// the machine's own view, with its pace and its log of what is in hand.
func cmdQueue(args []string) int {
	fs := flag.NewFlagSet("queue", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the queues as JSON")
	cfg, path, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return fail(fmt.Errorf("usage: shed queue [-json] <machine> [worker]"))
	}
	m, ok := cfg.Machine(fs.Arg(0))
	if !ok {
		return fail(fmt.Errorf("machine %q is not in %s", fs.Arg(0), path))
	}
	workers := m.Builders()
	if name := fs.Arg(1); name != "" {
		if err := m.TakesWork(name); err != nil {
			return fail(err)
		}
		workers = []config.Worker{{Name: name}}
	}
	queues, err := deploy.Queues(context.Background(), probe.ShellRunner{}, m)
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		out := []runlog.QueueView{}
		for _, w := range workers {
			if v, ok := queues[w.Name]; ok {
				out = append(out, v)
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return fail(err)
		}
		return exitOK
	}
	for _, w := range workers {
		renderQueue(os.Stdout, m.Name, w.Name, queues, time.Now())
	}
	return exitOK
}

// renderQueue prints one worker's queue, workable items first as the agent
// orders them.
func renderQueue(out io.Writer, machine, worker string, queues map[string]runlog.QueueView, now time.Time) {
	v, ok := queues[worker]
	if !ok {
		fmt.Fprintf(out, "%s worker %s: no queue yet: its agent has not read one since it started, or runs a shed older than this command\n\n", machine, worker)
		return
	}
	fmt.Fprintf(out, "%s worker %s: %d of %d item(s) workable, read %s ago\n", machine, worker, v.Workable(), len(v.Items), status.Short(now.Sub(v.Time)))
	if len(v.Items) == 0 {
		fmt.Fprintln(out, "  (no open issues)")
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, e := range v.Items {
		state := "workable"
		if e.Hold != "" {
			state = e.Hold
		}
		kind := e.Kind
		if e.Backfill {
			kind = "reserve"
		}
		fmt.Fprintf(tw, "  %s#%d\t%s\t%s\t%s\n", e.Repo, e.Number, kind, e.Title, state)
	}
	tw.Flush()
	if v.Reserve != "" {
		fmt.Fprintf(out, "  reserve: %s\n", v.Reserve)
	}
	fmt.Fprintln(out)
}
