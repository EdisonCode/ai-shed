package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/looks"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/status"
	"github.com/edisoncode/ai-shed/internal/ui"
)

// cmdUI serves shed observe: a live view of the fleet in the browser. It reads
// what shed status reads, from this machine, with the owner's own ssh and
// gh: nothing on the fleet depends on it.
func cmdUI(args []string) int {
	fs := flag.NewFlagSet("observe", flag.ContinueOnError)
	listen := fs.String("listen", ui.DefaultListen, "address to serve the page on; another than loopback lets others on that network see the fleet")
	every := fs.Duration("every", 2*time.Minute, "how often to read the fleet; each read lists every queue on GitHub, on the same account the supervisors use")
	deckEvery := fs.Duration("deck-every", 5*time.Minute, "how often to read goals, flow and spend, which take more GitHub calls than the fleet")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return fail(err)
	}
	if *every < 5*time.Second {
		return fail(fmt.Errorf("-every %s is too often: each read is one ssh round trip per machine and a GitHub search per queue; use 5s or more", *every))
	}
	if !ui.Loopback(*listen) {
		fmt.Fprintf(os.Stderr, "shed observe: %s is not loopback: anyone who can reach it sees the fleet, its issues and its workers' screens\n", *listen)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	collector := status.Collector{Runner: probe.ShellRunner{}, Lister: backlog.GH{}, Batches: backlog.GH{}, Pulls: backlog.GH{},
		Looks: &looks.Reader{GitHub: looks.GH{}, Run: looks.Shell, Now: time.Now}}
	// The deck reads what the digest reads beyond the report, over the
	// last day: goals, flow, idle hours and spend.
	deck := collector
	deck.Flow, deck.Goals, deck.Times, deck.Closed = looks.GH{}, backlog.GH{}, looks.GH{}, looks.GH{}
	deck.Usage = remoteUsage{Runner: probe.ShellRunner{}, Now: time.Now}
	srv := &ui.Server{
		Config:  cfg,
		Collect: func(ctx context.Context) status.Report { return collector.CollectAll(ctx, cfg) },
		Runner:  probe.ShellRunner{},
		Every:   *every,
		Digest: func(ctx context.Context, r status.Report) status.Digest {
			now := time.Now()
			return deck.DigestFrom(ctx, cfg, looks.GH{}, r, now.Add(-24*time.Hour), now)
		},
		DeckEvery: *deckEvery,
	}
	go srv.Run(ctx)

	httpSrv := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdown)
	}()
	fmt.Printf("shed observe on http://%s (ctrl-c to stop)\n", *listen)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fail(fmt.Errorf("serve on %s: %w", *listen, err))
	}
	return exitOK
}
