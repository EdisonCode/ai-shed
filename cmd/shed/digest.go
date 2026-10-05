package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/looks"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/status"
)

// cmdDigest prints what merged in a window, how the eye checks went, and
// what waits on the owner now. It changes nothing; run it from a schedule or
// send its output wherever the owner reads.
func cmdDigest(args []string) int {
	fs := flag.NewFlagSet("digest", flag.ContinueOnError)
	since := fs.Duration("since", 24*time.Hour, "how far back to look, for example 12h")
	asJSON := fs.Bool("json", false, "print the digest as JSON")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	if *since <= 0 {
		return fail(fmt.Errorf("-since must be a positive duration such as 24h"))
	}
	collector := status.Collector{Runner: probe.ShellRunner{}, Lister: backlog.GH{},
		Looks: &looks.Reader{GitHub: looks.GH{}, Run: looks.Shell, Now: time.Now}}
	now := time.Now()
	d := collector.Digest(context.Background(), cfg, looks.GH{}, now.Add(-*since), now)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(d); err != nil {
			return fail(err)
		}
	} else {
		status.RenderDigest(os.Stdout, d)
	}
	if status.NeedsOwner(d.Report.Machines, d.Report.Repos) || len(d.Problems) > 0 {
		return exitAttention
	}
	return exitOK
}
