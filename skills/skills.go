// Package skills holds the skills shed ships, so a shed can tell whether
// the copy an assistant has installed is the one it was built with.
package skills

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Shed is the shed skill: how an assistant operates the fleet.
//
//go:embed shed/SKILL.md
var Shed []byte

// ShedPath is where Claude Code reads the shed skill from, under a home
// directory.
func ShedPath(home string) string {
	return filepath.Join(home, ".claude", "skills", "shed", "SKILL.md")
}

// ShedStale reports whether the shed skill installed under home differs
// from the one this shed ships. A skill that is not installed is not stale:
// the owner may use another assistant, or none.
func ShedStale(home string) (bool, error) {
	installed, err := os.ReadFile(ShedPath(home))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the installed shed skill: %w", err)
	}
	return !bytes.Equal(installed, Shed), nil
}

// InstallShed writes the shed skill under home, replacing the one there.
func InstallShed(home string) (string, error) {
	path := ShedPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("install the shed skill: %w", err)
	}
	if err := os.WriteFile(path, Shed, 0o644); err != nil {
		return "", fmt.Errorf("install the shed skill: %w", err)
	}
	return path, nil
}
