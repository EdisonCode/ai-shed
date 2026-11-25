package runlog

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// GoalsDir holds one file per goal the owner focused on or tabled, named for
// the goal's label, path-escaped. The file's content is the goal's mode.
const GoalsDir = "goals"

// Goal modes.
const (
	// GoalFocus puts the goal's issues ahead of every other goal's in each
	// queue, so the fleet finishes it before it starts more.
	GoalFocus = "focus"
	// GoalTabled holds the goal's issues that nobody has started; work in
	// hand on it goes on to its end.
	GoalTabled = "tabled"
)

// GoalFile is the name of the file that holds a goal's mode.
func GoalFile(label string) string {
	return url.PathEscape(label)
}

// GoalModes returns the mode of each goal the owner focused on or tabled, by
// label. A missing directory is none; a file of another mode is skipped.
func GoalModes(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, GoalsDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read goal modes: %w", err)
	}
	modes := map[string]string{}
	for _, e := range entries {
		label, err := url.PathUnescape(e.Name())
		if err != nil || e.IsDir() || strings.HasSuffix(e.Name(), ".new") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, GoalsDir, e.Name()))
		if err != nil {
			continue
		}
		if mode := ParseGoalMode(string(b)); mode != "" {
			modes[label] = mode
		}
	}
	return modes, nil
}

// ParseGoalMode returns the mode a goal file names, or "" for none.
func ParseGoalMode(s string) string {
	switch mode := strings.TrimSpace(s); mode {
	case GoalFocus, GoalTabled:
		return mode
	}
	return ""
}
