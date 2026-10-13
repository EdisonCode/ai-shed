package main

import (
	"context"
	"encoding/json"
	"flag"
	"maps"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/deploy"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

// usageReport is what `shed usage -json` prints on a machine and the
// digest reads.
type usageReport struct {
	Version string              `json:"version"`
	Usage   runlog.UsageSummary `json:"usage"`
}

// cmdUsage runs on a worker machine. It totals what the machine's workers
// spent in a window, priced at the published API rates and the fleet file's.
func cmdUsage(args []string) int {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	since := fs.Duration("since", 7*24*time.Hour, "how far back to look, for example 168h")
	asJSON := fs.Bool("json", false, "print the totals as JSON")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	if *since <= 0 {
		return fail(fmt.Errorf("-since must be a positive duration such as 168h"))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	now := time.Now()
	s, err := runlog.Summarize(filepath.Join(home, runlog.StateDir), now.Add(-*since), now, cfg.Prices())
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(usageReport{Version: version, Usage: s}); err != nil {
			return fail(err)
		}
		return exitOK
	}
	for _, w := range s.Workers {
		fmt.Printf("%s  $%.2f at API rates\n", w.Worker, w.Dollars)
		models := slices.Sorted(maps.Keys(w.Models))
		for _, model := range models {
			t := w.Models[model]
			fmt.Printf("  %s  in %d  write %d  read %d  out %d  $%.2f\n", model, t.Input, t.CacheWrite, t.CacheRead, t.Output, t.Dollars)
		}
	}
	return exitOK
}

// remoteUsage reads a machine's usage with `shed usage -json` over its
// runner.
type remoteUsage struct {
	Runner probe.Runner
	Now    func() time.Time
}

func (r remoteUsage) ReadUsage(ctx context.Context, m config.Machine, since time.Time) (runlog.UsageSummary, error) {
	window := max(r.Now().Sub(since), time.Second)
	command := `"$HOME/` + deploy.RemoteBinary + `" usage -json -since ` + strconv.Itoa(int(window.Seconds())) + "s"
	out, err := r.Runner.Run(ctx, m, m.Command(command), nil)
	if err != nil {
		return runlog.UsageSummary{}, err
	}
	var report usageReport
	if err := json.Unmarshal(out, &report); err != nil {
		return runlog.UsageSummary{}, fmt.Errorf("read the machine's report: %w", err)
	}
	if report.Version != version {
		return runlog.UsageSummary{}, fmt.Errorf("the machine runs shed %s, this is %s: deploy it", report.Version, version)
	}
	return report.Usage, nil
}
