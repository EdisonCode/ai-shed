package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/edisoncode/ai-shed/internal/audit"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
	"github.com/edisoncode/ai-shed/internal/supervisor"
)

// shedCommands are the commands the usage names, for the audit's check of
// a "shed <command>" in the owner's prose.
var shedCommands = []string{"status", "observe", "ui", "digest", "validate", "deploy", "task", "bump", "goal", "queue", "pause", "resume",
	"recycle", "login", "tidy", "template", "update", "skill", "answer", "audit", "preflight", "agent", "usage", "promote", "context", "lock", "mark", "version"}

// cmdAudit reads the owner's prose in the fleet file for what costs tokens
// on every turn and says nothing a worker needs. It changes nothing.
func cmdAudit(args []string) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	withModel := fs.Bool("model", false, "also ask the reviewer model which sentences a setting already does, describe a setting that is off, or contradict one")
	asJSON := fs.Bool("json", false, "print the audit as JSON")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	report := audit.Run(cfg, func(m config.Machine, w config.Worker) string { return supervisor.ShedBrief(cfg, m, w) }, shedCommands)
	if *withModel {
		home, err := os.UserHomeDir()
		if err != nil {
			return fail(err)
		}
		reviewer := supervisor.CommandReviewer{Command: cfg.Supervisor.CommandOrDefault(), Dir: filepath.Join(home, runlog.StateDir)}
		found, err := audit.ModelFindings(context.Background(), cfg, report.Prose, reviewer.Ask)
		if err != nil {
			report.Problems = append(report.Problems, "the model's reading: "+err.Error())
		}
		report.Findings = append(report.Findings, found...)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return fail(err)
		}
	} else {
		renderFleetAudit(report, *withModel)
	}
	if len(report.Findings) > 0 || len(report.Problems) > 0 {
		return exitAttention
	}
	return exitOK
}

func renderFleetAudit(r audit.Report, withModel bool) {
	total := 0
	for _, s := range r.Prose {
		total += s.Tokens
	}
	fmt.Printf("The owner's prose in the fleet file: about %s tokens, read by the workers it reaches on every turn\n", kilo(total))
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, s := range r.Prose {
		fmt.Fprintf(tw, "  ~%s\t%s\n", kilo(s.Tokens), s.Path)
	}
	tw.Flush()
	saved := 0
	groups := []string{audit.ShedWrites, audit.InOrders, audit.Repeated, audit.SettingDoes, audit.SettingOff, audit.Contradicts, audit.Stale}
	for _, kind := range groups {
		var lines []string
		for _, f := range r.Findings {
			if f.Kind != kind {
				continue
			}
			saved += f.Tokens
			lines = append(lines, fmt.Sprintf("  %s line %d  ~%d tokens  %q\n    %s", f.Where.Path, f.Where.Line, f.Tokens, f.Text, f.Why))
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Printf("\n%s: %d\n%s\n", strings.ToUpper(kind[:1])+kind[1:], len(lines), strings.Join(lines, "\n"))
	}
	if len(r.Findings) == 0 {
		fmt.Println("\nNothing found.")
	} else {
		fmt.Printf("\nAbout %d tokens a turn could go. Nothing was changed: edit the fleet file, then shed validate and shed deploy.\n", saved)
	}
	if withModel {
		fmt.Println("What a setting does, and contradictions, are the reviewer model's reading: check each before you delete anything.")
	} else {
		fmt.Println("shed audit -model also asks the reviewer model which sentences a setting already does.")
	}
	for _, p := range r.Problems {
		fmt.Printf("! %s\n", p)
	}
}
