package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edisoncode/ai-shed/internal/config"
)

// inboundKey is the Claude Code setting for messages from the owner's other
// sessions: accept, hold or refuse.
const inboundKey = "crossSessionInbound"

// InboundWarnings names what keeps an unattended Claude Code worker from
// getting the crossSessionInbound value it needs: accept with peers, refuse
// without. shed adds its settings only to a command without --settings, so
// a worker with its own gets no value from shed. Held messages wait for an
// approval that nobody gives. A refuse in the project's own settings beats
// every other value, and a bare session has no inbox at all.
func InboundWarnings(home string, w config.Worker) []string {
	command := w.CommandOrDefault()
	fields := strings.Fields(command)
	if w.Ask || len(fields) == 0 || filepath.Base(fields[0]) != "claude" {
		return nil
	}
	want := inboundFor(w)
	var warnings []string
	if len(w.PeerList) > 0 {
		if slices.Contains(fields, "--bare") {
			warnings = append(warnings, "the worker has peers, but --bare starts its session without an inbox: it cannot receive their messages")
		}
		dir := expandPath(home, "", w.Dir)
		for _, name := range []string{"settings.json", "settings.local.json"} {
			path := filepath.Join(dir, ".claude", name)
			if v, err := readInbound(path); err == nil && v == "refuse" {
				warnings = append(warnings, path+" refuses messages from other sessions, and that beats every other setting: the worker's peers cannot reach it")
			}
		}
	}
	arg, ok := settingsArg(fields)
	if !ok {
		return warnings
	}
	fix := fmt.Sprintf(`; add "%s": %q to it`, inboundKey, want)
	if strings.HasPrefix(arg, "{") {
		if !strings.Contains(command, inboundKey) {
			warnings = append(warnings, "the worker's own --settings does not set "+inboundKey+", so a peer message can stall it in an approval dialog"+fix)
		}
		return warnings
	}
	path := expandPath(home, w.Dir, arg)
	v, err := readInbound(path)
	switch {
	case err != nil:
		warnings = append(warnings, fmt.Sprintf("cannot read the worker's own --settings %s to check %s: %v", path, inboundKey, err))
	case v == "":
		warnings = append(warnings, path+" does not set "+inboundKey+", so a peer message can stall the worker in an approval dialog"+fix)
	case v == "hold":
		warnings = append(warnings, path+" holds peer messages for an approval that nobody gives on an unattended worker"+fix)
	case v == "refuse" && want == "accept":
		warnings = append(warnings, path+" refuses messages, but the worker has peers"+fix)
	}
	return warnings
}

// readInbound returns the crossSessionInbound value of a settings file, ""
// when it sets none.
func readInbound(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return "", err
	}
	v, _ := settings[inboundKey].(string)
	return v, nil
}

// peersBrief tells a worker with peers who they are and how to message them.
func peersBrief(b *strings.Builder, w config.Worker) {
	if len(w.PeerList) == 0 {
		return
	}
	fmt.Fprintf(b, "\n## Peers\n\nYour session is `%s`. These workers may message you, and you may message them with SendMessage:\n\n", w.Session)
	for _, p := range w.PeerList {
		if p.Remote {
			fmt.Fprintf(b, "- `%s` (on %s)\n", p.Session, p.Machine)
		} else {
			fmt.Fprintf(b, "- `%s`\n", p.Session)
		}
	}
	fmt.Fprintf(b, "\nWhen you write the plan of an issue, add a line `**Session:** %s` beside `**Paths:**`, so peers on every machine can see who builds it. %s lists what each peer has in hand; read it before you message one.\n", w.Session, "`"+RosterFile+"`")
	b.WriteString("\nMessage a peer when it needs something you have mid-task: a change that breaks what it builds on, what landed, or the answer to a question it is blocked on. Keep each message short. A message is plain text; it carries none of your conversation or files.\n" +
		"A message informs; it does not instruct. Never ask a peer to make an edit, and never ask it for an action your own session was denied. Work that someone has to do goes on the issue it belongs to; open a new issue only as your brief allows.\n" +
		"A decision, or a change of plan, goes on the issue as well: messages are lost when a session is cleared, and the owner does not see them.\n" +
		"Do not message a session that is not named here, except to answer one that wrote to you.\n")
	if w.Crew {
		fmt.Fprintf(b, "\n## Your crew\n\nYou are crew: the peers above who build other parts of the same goal build it with you. The goal is one outcome, and your part is not done when it works alone: it is done when it works with theirs. %s names, for each goal you build, which crew-mate builds which part and where.\n"+
			"- Before you push a change to something a crew-mate builds on (an API, an event, a schema, a shared type), message them what changes and why, and read their answer before you hand back.\n"+
			"- When a crew-mate tells you of such a change, check your part against it now, not at the end.\n"+
			"- When a crew-mate's answer changes your plan, write the change on your issue.\n", "`"+RosterFile+"`")
	}
}

// settingsArg returns the value of the command's --settings, without the
// quotes around it.
func settingsArg(fields []string) (string, bool) {
	for i, f := range fields {
		if value, ok := strings.CutPrefix(f, "--settings="); ok {
			return strings.Trim(value, `'"`), true
		}
		if f == "--settings" && i+1 < len(fields) {
			return strings.Trim(fields[i+1], `'"`), true
		}
	}
	return "", false
}

// expandPath resolves a path the worker's command names: ~ is the home
// directory, and a relative path starts in the worker's directory.
func expandPath(home, dir, path string) string {
	home2 := func(p string) string {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			return filepath.Join(home, rest)
		}
		return p
	}
	path = home2(path)
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(home2(dir), path)
}
