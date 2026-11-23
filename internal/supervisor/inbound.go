package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edisoncode/ai-shed/internal/config"
)

// inboundKey is the Claude Code setting for messages from the owner's other
// sessions: accept, hold or refuse.
const inboundKey = "crossSessionInbound"

// InboundWarnings names an unattended Claude Code worker whose own
// --settings leave messages from other sessions to Claude Code's default, or
// hold them. shed adds its settings only to a command without --settings, so
// such a worker gets no value from shed. Held messages wait for an approval
// that nobody gives.
func InboundWarnings(home string, w config.Worker) []string {
	command := w.CommandOrDefault()
	fields := strings.Fields(command)
	if w.Ask || len(fields) == 0 || filepath.Base(fields[0]) != "claude" {
		return nil
	}
	arg, ok := settingsArg(fields)
	if !ok {
		return nil
	}
	fix := fmt.Sprintf(`; add "%s": "refuse" to it`, inboundKey)
	if strings.HasPrefix(arg, "{") {
		if strings.Contains(command, inboundKey) {
			return nil
		}
		return []string{"the worker's own --settings does not set " + inboundKey + ", so a peer message can stall it in an approval dialog" + fix}
	}
	path := expandPath(home, w.Dir, arg)
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{fmt.Sprintf("cannot read the worker's own --settings %s to check %s: %v", path, inboundKey, err)}
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return []string{fmt.Sprintf("cannot read the worker's own --settings %s to check %s: %v", path, inboundKey, err)}
	}
	switch settings[inboundKey] {
	case nil:
		return []string{path + " does not set " + inboundKey + ", so a peer message can stall the worker in an approval dialog" + fix}
	case "hold":
		return []string{path + " holds peer messages for an approval that nobody gives on an unattended worker" + fix}
	}
	return nil
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
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return filepath.Join(home, rest)
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(expandPath(home, "", dir), path)
}
