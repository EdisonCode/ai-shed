package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/looks"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/status"
)

const clearScreen = "\033[H\033[2J"

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	watch := fs.Duration("watch", 0, "refresh at this interval, for example 1m")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	collector := status.Collector{Runner: probe.ShellRunner{}, Lister: backlog.GH{},
		Looks: &looks.Reader{GitHub: looks.GH{}, Run: looks.Shell, Now: time.Now}}

	for {
		reports := collector.Collect(ctx, cfg)
		repos := collector.CollectRepos(ctx, cfg)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(reports); err != nil {
				return fail(err)
			}
		} else {
			if *watch > 0 {
				fmt.Print(clearScreen)
				fmt.Printf("shed  %s  (every %s, ctrl-c to stop)\n\n", time.Now().Format("Mon 15:04:05"), *watch)
			}
			status.Render(os.Stdout, reports, repos)
		}
		if *watch <= 0 {
			if status.NeedsOwner(reports, repos) {
				return exitAttention
			}
			return exitOK
		}
		select {
		case <-ctx.Done():
			return exitOK
		case <-time.After(*watch):
		}
	}
}
