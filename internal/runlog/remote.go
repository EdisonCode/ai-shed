package runlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RemoteDir holds one file per worker whose screen said its Remote Control
// was down: when the supervisor saw it, what it did, and when it came back.
// Without Remote Control, peers on other machines cannot message the worker.
const RemoteDir = "remote"

// Remote is what the supervisor knows of a worker's lost Remote Control.
type Remote struct {
	// Since is when the screen was first seen to say it was down.
	Since time.Time `json:"since"`
	// Typed counts the times /remote-control was typed into the worker at
	// rest, the last at LastTyped.
	Typed     int       `json:"typed,omitempty"`
	LastTyped time.Time `json:"last_typed,omitzero"`
	// GaveUp is set when typing it did not bring Remote Control back: the
	// owner was told.
	GaveUp bool `json:"gave_up,omitempty"`
	// Back is when the screen no longer said it was down; zero while down.
	Back time.Time `json:"back,omitzero"`
}

// Down reports whether Remote Control is still down.
func (r Remote) Down() bool { return r.Back.IsZero() }

// WriteRemote records a worker's Remote Control. The file is replaced in one
// step, so a reader never sees half of it.
func WriteRemote(dir, worker string, r Remote) error {
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode remote control state: %w", err)
	}
	remote := filepath.Join(dir, RemoteDir)
	if err := os.MkdirAll(remote, 0o755); err != nil {
		return fmt.Errorf("write remote control state: %w", err)
	}
	tmp, err := os.CreateTemp(remote, "."+worker+"-*")
	if err != nil {
		return fmt.Errorf("write remote control state: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("write remote control state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write remote control state: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(remote, worker)); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write remote control state: %w", err)
	}
	return nil
}

// ReadRemote returns what is recorded of the worker's Remote Control; false
// when nothing is, or the file cannot be read.
func ReadRemote(dir, worker string) (Remote, bool) {
	data, err := os.ReadFile(filepath.Join(dir, RemoteDir, worker))
	if err != nil {
		return Remote{}, false
	}
	r, err := ParseRemote(string(data))
	return r, err == nil
}

// ParseRemote reads a line WriteRemote wrote.
func ParseRemote(line string) (Remote, error) {
	var r Remote
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		return Remote{}, fmt.Errorf("parse remote control state: %w", err)
	}
	if r.Since.IsZero() {
		return Remote{}, fmt.Errorf("parse remote control state: no time it went down")
	}
	return r, nil
}
