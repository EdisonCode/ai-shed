package audit

import (
	"context"
	"strings"
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/sources"
)

const shedSays = "Work one issue at a time. The supervisor names your first issue and each next one, so wait for it before you start anything else in the repository."

func fleet(orders string, briefs ...string) *config.Config {
	c := &config.Config{Defaults: config.Defaults{StandingOrders: orders}}
	m := config.Machine{Name: "box", Issues: []config.IssueSource{{Repo: "org/app", Label: "machine:box"}}}
	for n, b := range briefs {
		m.Workers = append(m.Workers, config.Worker{Name: []string{"app", "api", "web"}[n], Brief: b})
	}
	c.Machines = []config.Machine{m}
	return c
}

func shed(config.Machine, config.Worker) string { return "# Brief\n\n" + shedSays + "\n" }

func kinds(r Report) map[string]int {
	k := map[string]int{}
	for _, f := range r.Findings {
		k[f.Kind]++
	}
	return k
}

func TestProseThatShedAlreadyWritesIsFound(t *testing.T) {
	r := Run(fleet("Be careful.\n\n"+shedSays, "Fix billing bugs."), shed, nil)
	if kinds(r)[ShedWrites] != 1 || r.Findings[0].Where.Path != OrdersPath || r.Findings[0].Tokens == 0 {
		t.Fatalf("findings = %+v", r.Findings)
	}
}

func TestABriefThatRepeatsTheOrdersIsFound(t *testing.T) {
	rule := "Never push to the main branch directly; open a pull request and let the required checks run before anything merges into it."
	r := Run(fleet(rule, "Fix billing bugs.\n\n"+rule), shed, nil)
	if kinds(r)[InOrders] != 1 || r.Findings[0].Where.Path != "brief of box/app" || !strings.Contains(r.Findings[0].Why, "the standing orders say them") {
		t.Fatalf("findings = %+v", r.Findings)
	}
}

func TestTheSameTextInSeveralBriefsIsFoundOnce(t *testing.T) {
	shared := "Run the full test suite with make test before every hand-back, and paste the last lines of its output into the hand-back comment."
	r := Run(fleet("", "Billing.\n\n"+shared, "Search.\n\n"+shared, "Web.\n\n"+shared), shed, nil)
	// The later copies are the ones to cut, each against the first brief.
	if kinds(r)[Repeated] != 2 || r.Findings[0].Where.Path == "brief of box/app" || r.Findings[1].Where.Path == "brief of box/app" {
		t.Fatalf("findings = %+v, want api and web, each against app", r.Findings)
	}
	for _, f := range r.Findings {
		if !strings.Contains(f.Why, "also in the brief of box/app") || f.Tokens != 0 {
			t.Fatalf("finding = %+v, want it against the first brief only, at no cost a turn: a worker reads only its own brief", f)
		}
	}
}

func TestNamesTheFleetFileDoesNotHaveAreStale(t *testing.T) {
	r := Run(fleet("Take issues labeled machine:gone. Ask box-ghost first. Run `shed frobnicate` daily, and `shed status` after; shed holds what waits."), shed, []string{"status"})
	var whys []string
	for _, f := range r.Findings {
		if f.Kind == Stale {
			whys = append(whys, f.Why)
		}
	}
	want := []string{"no queue of the fleet file has the label machine:gone", "machine box has no worker ghost", "shed has no command \"frobnicate\""}
	if strings.Join(whys, "|") != strings.Join(want, "|") {
		t.Fatalf("stale = %q, want %q", whys, want)
	}
}

func TestTheModelIsToldWhichSettingsAreOnAndItsAnswerIsRead(t *testing.T) {
	c := fleet("Close the issue when its pull request merges.")
	c.Repos = []config.Repo{{Name: "org/app", CloseFinished: true}}
	var prompt string
	ask := func(_ context.Context, p string) (string, error) {
		prompt = p
		return `{"where": "standing_orders", "line": 1, "text": "Close the issue when its pull request merges.", "setting": "repos[].close_finished", "verdict": "done", "why": "shed closes it"}`, nil
	}
	found, err := ModelFindings(context.Background(), c, Prose(c), ask)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "repos[].close_finished (on for org/app only)") || !strings.Contains(prompt, "repos[].issue_quota (off)") || !strings.Contains(prompt, "=== standing_orders\n1: Close the issue") {
		t.Fatalf("prompt:\n%s", prompt)
	}
	if len(found) != 1 || found[0].Kind != SettingDoes || !strings.Contains(found[0].Why, "delete it") {
		t.Fatalf("found = %+v", found)
	}
}

func TestStartupFilesThatRepeatTheOrdersOrEachOtherAreFoundOnce(t *testing.T) {
	rule := "Never push to the main branch directly; open a pull request and let the required checks run first."
	memo := "The staging database is reset every night at two, so seed what an eye check needs before you look."
	c := fleet(rule, "Fix billing bugs.", "Fix search bugs.")
	claude := sources.Source{Path: "/w/app/CLAUDE.md", Kind: "repository", Tokens: 1500, Text: "# App\n\n" + rule + "\n" + memo}
	memory := sources.Source{Path: "/home/me/.claude/projects/x/memory/MEMORY.md", Kind: "memory", Tokens: 1200, Text: memo}
	brief := sources.Source{Path: "/w/app/.shed/BRIEF.md", Kind: "brief", Tokens: 2500, Text: rule}
	startups := []Startup{
		{Machine: "box", Worker: "app", Sources: []sources.Source{claude, memory, brief}, Stale: []sources.Stale{{Location: sources.Location{Path: "/w/app/CLAUDE.md", Line: 3}, Ref: "internal/gone.go", Why: "no such file"}}},
		{Machine: "box", Worker: "api", Sources: []sources.Source{claude, brief}},
	}

	r := RunWorkers(c, shed, nil, startups)

	if len(r.Workers) != 2 || r.Workers[0].Total != 5200 || r.Workers[1].Total != 4000 {
		t.Fatalf("workers = %+v", r.Workers)
	}
	k := kinds(r)
	if k[InOrders] != 1 || k[SaidTwice] != 1 || k[Stale] != 1 {
		t.Fatalf("kinds = %v, findings = %+v", k, r.Findings)
	}
	for _, f := range r.Findings {
		switch f.Kind {
		case InOrders:
			if f.Where.Path != "box:/w/app/CLAUDE.md" {
				t.Fatalf("in orders at %s, want the CLAUDE.md, once for both workers", f.Where.Path)
			}
		case SaidTwice:
			if !strings.HasSuffix(f.Where.Path, "MEMORY.md") || !strings.Contains(f.Why, "box:/w/app/CLAUDE.md") {
				t.Fatalf("said twice = %+v, want the memory, against the CLAUDE.md loaded before it", f)
			}
		}
	}
}

func TestARepositorysOrdersAreProseAndABriefThatRepeatsThemIsFound(t *testing.T) {
	rule := "In this repository the merge train merges every pull request once a verifier has passed it."
	c := fleet("", "Fix billing bugs.\n"+rule)
	c.Repos = []config.Repo{{Name: "org/app", StandingOrders: rule}}
	r := Run(c, shed, nil)
	if r.Prose[0].Path != "standing_orders of org/app" {
		t.Fatalf("prose = %+v", r.Prose)
	}
	if kinds(r)[InOrders] != 1 {
		t.Fatalf("findings = %+v", r.Findings)
	}
}
