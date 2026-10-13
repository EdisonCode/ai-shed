package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/looks"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/status"
)

// cmdPromote prints what a production deploy of a repository would carry,
// for the owner to approve as one batch. It changes nothing and exits 1
// when something should stop the promotion.
func cmdPromote(args []string) int {
	fs := flag.NewFlagSet("promote", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the brief as JSON")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	if fs.NArg() != 1 {
		return fail(errors.New("usage: shed promote [-json] <owner/repo>"))
	}
	collector := status.Collector{Runner: probe.ShellRunner{}, Lister: backlog.GH{},
		Looks: &looks.Reader{GitHub: looks.GH{}, Run: looks.Shell, Now: time.Now}, Closed: looks.GH{}}
	p, err := collector.Promote(context.Background(), cfg, fs.Arg(0), looks.GH{}, looks.GH{})
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(p); err != nil {
			return fail(err)
		}
	} else {
		status.RenderPromotion(os.Stdout, p, time.Now())
	}
	if p.Blocked() {
		return exitAttention
	}
	return exitOK
}
