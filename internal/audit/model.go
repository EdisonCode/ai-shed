package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/sources"
)

// ModelPrompt asks the reviewer model which sentences of the owner's prose
// a setting does, describe a setting that is off, or contradict one.
func ModelPrompt(cfg *config.Config, prose []sources.Source) string {
	var b strings.Builder
	b.WriteString("You audit the owner's prose in a fleet file for shed, a supervisor of unattended coding agents. Every worker reads this prose on every turn, so a sentence that a setting already does costs tokens and says nothing.\n\n")
	b.WriteString("These are the settings, what shed does with each, and whether this fleet file has it on:\n\n")
	for _, s := range Catalogue {
		state := "off"
		if s.On(cfg) {
			state = "on"
		}
		fmt.Fprintf(&b, "- %s (%s): %s\n", s.Name, state, s.Does)
	}
	b.WriteString("\nThis is the prose, each line numbered, under the name of its block:\n")
	for _, s := range prose {
		fmt.Fprintf(&b, "\n=== %s\n", s.Path)
		for n, line := range strings.Split(s.Text, "\n") {
			fmt.Fprintf(&b, "%d: %s\n", n+1, line)
		}
	}
	b.WriteString("\nFind the sentences that: (done) state a rule that a setting which is on already makes shed do; (off) describe something a setting which is off would do, so the owner could turn it on; (contradicts) say something that a setting which is on contradicts. Leave out a sentence that adds something the setting does not say, such as a reason, a repository-specific detail or a stricter rule. Answer with one JSON object per line and nothing else: {\"where\": \"<block name>\", \"line\": <number>, \"text\": \"<the sentence>\", \"setting\": \"<setting name>\", \"verdict\": \"done|off|contradicts\", \"why\": \"<one sentence>\"}. Answer NONE when there are none.\n")
	return b.String()
}

// ModelFindings asks the reviewer model, and reads its answer into findings.
func ModelFindings(ctx context.Context, cfg *config.Config, prose []sources.Source, ask func(context.Context, string) (string, error)) ([]Finding, error) {
	answer, err := ask(ctx, ModelPrompt(cfg, prose))
	if err != nil {
		return nil, err
	}
	return parseModel(answer)
}

func parseModel(answer string) ([]Finding, error) {
	kinds := map[string]string{"done": SettingDoes, "off": SettingOff, "contradicts": Contradicts}
	var out []Finding
	for _, line := range strings.Split(answer, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var f struct {
			Where, Text, Setting, Verdict, Why string
			Line                               int
		}
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			return out, fmt.Errorf("read the model's answer: %w: %s", err, line)
		}
		kind, ok := kinds[f.Verdict]
		if !ok {
			continue
		}
		why := f.Setting + ": " + f.Why
		switch kind {
		case SettingDoes:
			why += " (delete it)"
		case SettingOff:
			why += " (turn the setting on and delete it, or keep it)"
		case Contradicts:
			why += " (settle which is right)"
		}
		out = append(out, Finding{Kind: kind, Where: sources.Location{Path: f.Where, Line: f.Line}, Text: f.Text, Why: why, Tokens: len(f.Text) / 4})
	}
	return out, nil
}
