package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/edisoncode/ai-shed/internal/config"
)

// HookWarnings reads the Claude Code settings a worker's session loads and
// names a hook shed relies on that is missing. With the state hooks and no
// SubagentStop hook, the cost comment counts the worker's own session and
// leaves out every sub-agent, and nothing else says so. Another tool's
// settings are not read.
func HookWarnings(home string, w config.Worker) []string {
	fields := strings.Fields(w.CommandOrDefault())
	if len(fields) == 0 || filepath.Base(fields[0]) != "claude" {
		return nil
	}
	dir := w.Dir
	if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		dir = filepath.Join(home, rest)
	}
	var warnings []string
	state, usage := false, false
	for _, path := range []string{
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(dir, ".claude", "settings.json"),
		filepath.Join(dir, ".claude", "settings.local.json"),
	} {
		hooks, err := readHooks(path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("cannot read the hooks in %s: %v", path, err))
			continue
		}
		for event, commands := range hooks {
			for _, command := range commands {
				switch {
				case event == "SubagentStop" && strings.Contains(command, "shed mark usage"):
					usage = true
				case strings.Contains(command, "shed mark working"), strings.Contains(command, "shed mark idle"), strings.Contains(command, "shed mark waiting"):
					state = true
				}
			}
		}
	}
	if state && !usage {
		warnings = append(warnings, "cost comments leave out sub-agents: add the SubagentStop hook that runs `shed mark usage` (see the README)")
	}
	return warnings
}

// readHooks returns the commands of a settings file's hooks, by event. A
// missing file has none.
func readHooks(path string) (map[string][]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, err
	}
	commands := map[string][]string{}
	for event, groups := range settings.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				commands[event] = append(commands[event], h.Command)
			}
		}
	}
	return commands, nil
}
