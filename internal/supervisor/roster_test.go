package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

func rosterWorker() config.Worker {
	return config.Worker{Name: "app", Session: "box-app", PeerList: []config.Peer{
		{Machine: "box", Worker: "api", Session: "box-api"},
		{Machine: "box", Worker: "docs", Session: "box-docs"},
		{Machine: "mini", Worker: "web", Session: "mini-web", Remote: true},
		{Machine: "mini", Worker: "ops", Session: "mini-ops", Remote: true},
	}}
}

func TestTheRosterSaysWhatEachPeerHasInHand(t *testing.T) {
	billing := backlog.Issue{Repo: "org/app", Number: 12, Title: "Split invoices", Comments: []backlog.Comment{{Body: "**Paths:** internal/billing/"}}}
	signed := backlog.Issue{Repo: "org/app", Number: 20, Title: "Web checkout", Comments: []backlog.Comment{{Body: "**Paths:** web/checkout/\n**Session:** mini-web"}}}
	checkins := []runlog.Checkin{{Time: time.Now(), Worker: "api", Issue: 12, Sent: true}}
	got := rosterText(rosterWorker(), []backlog.Issue{billing}, []backlog.Issue{signed}, checkins)
	for _, want := range []string{
		"- `box-api`: org/app#12 Split invoices (paths: internal/billing/)",
		"- `box-docs`: nothing in hand",
		"- `mini-web` (on mini): org/app#20 Web checkout (paths: web/checkout/)",
		"- `mini-ops` (on mini): no signed plan in flight",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("roster lacks %q:\n%s", want, got)
		}
	}
}

func TestARosterIsRewrittenOnlyWhenItChanges(t *testing.T) {
	dir := t.TempDir()
	w := rosterWorker()
	w.Dir = dir
	if changed, err := writeRoster(w, "roster one\n"); err != nil || !changed {
		t.Fatalf("first write: changed %v, err %v", changed, err)
	}
	if changed, err := writeRoster(w, "roster one\n"); err != nil || changed {
		t.Fatalf("same roster: changed %v, err %v", changed, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, RosterFile))
	if string(data) != "roster one\n" {
		t.Fatalf("roster = %q", data)
	}
}

func TestAWorkerWithoutPeersHasNoRoster(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".shed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, RosterFile), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeRoster(config.Worker{Name: "app", Dir: dir}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, RosterFile)); !os.IsNotExist(err) {
		t.Fatalf("a roster is left for a worker with no peers: %v", err)
	}
}

func TestAnOverlapNoteNamesThePeerThatSignedThePlan(t *testing.T) {
	f := overlapElsewhere(t)
	f.Machine.Workers[0].Session = "box-app"
	f.Machine.Workers[0].PeerList = []config.Peer{
		{Machine: "mini", Worker: "web", Session: "mini-web", Remote: true},
		{Machine: "mini", Worker: "ops", Session: "mini-ops", Remote: true},
	}
	elsewhere := f.lister.byLabel[""][0]
	elsewhere.Comments = []backlog.Comment{{Body: "## Plan\n**Paths:** internal/billing/\n**Session:** mini-web"}}
	f.lister.byLabel[""][0] = elsewhere
	f.tick(2 * time.Minute)
	prompt := f.reviewer.prompts[0]
	if !strings.Contains(prompt, "#20 is built by `mini-web`: tell it what you change that #20 builds on") || strings.Contains(prompt, "mini-ops") {
		t.Fatalf("prompt:\n%s", prompt)
	}
}
