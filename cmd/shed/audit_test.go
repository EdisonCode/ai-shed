package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/sources"
)

func TestTheAuditKnowsEveryCommandTheUsageNames(t *testing.T) {
	aliases := map[string]bool{"ui": true} // shed observe's other name
	for _, c := range shedCommands {
		if !aliases[c] && !strings.Contains(usage, "shed "+c) {
			t.Errorf("shedCommands has %q, which the usage does not name", c)
		}
	}
	for _, line := range strings.Split(usage, "\n") {
		f := strings.Fields(line)
		if strings.HasPrefix(line, "  shed ") && len(f) > 1 && !strings.HasPrefix(f[1], "-") {
			found := false
			for _, c := range shedCommands {
				found = found || c == f[1]
			}
			if !found {
				t.Errorf("the usage names shed %s, which shedCommands lacks", f[1])
			}
		}
	}
}

func TestTheAuditReadsEachWorkersStartupFilesFromItsMachine(t *testing.T) {
	report, _ := json.Marshal(contextReport{Version: version, Audit: sources.Audit{Sources: []sources.Source{{Path: "/w/CLAUDE.md", Kind: "repository", Tokens: 10, Body: "# App"}}}})
	cfg := &config.Config{Machines: []config.Machine{{Name: "box", Workers: []config.Worker{{Name: "app"}}}, {Name: "far", Workers: []config.Worker{{Name: "web"}}}}}
	runner := byMachine{"box": fakeRunner{out: report}, "far": fakeRunner{err: errors.New("far: exit status 255: Connection refused")}}

	startups, problems := collectStartups(context.Background(), runner, cfg)

	if len(startups) != 1 || startups[0].Sources[0].Text != "# App" || startups[0].Sources[0].Body != "" {
		t.Fatalf("startups = %+v", startups)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "far/web: cannot read what it loads") {
		t.Fatalf("problems = %q", problems)
	}
}

// byMachine answers each machine with its own runner.
type byMachine map[string]fakeRunner

func (b byMachine) Run(ctx context.Context, m config.Machine, command string, in io.Reader) ([]byte, error) {
	return b[m.Name].Run(ctx, m, command, in)
}
