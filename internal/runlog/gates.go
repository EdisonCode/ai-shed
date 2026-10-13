package runlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GatesFile is the record of the gate runs the agent made on its machine's
// pull requests.
const GatesFile = "gates.jsonl"

// Gate is one line of gates.jsonl: one run of a repository's gates on the
// head commit of a pull request.
type Gate struct {
	Repo   string    `json:"repo"`
	PR     int       `json:"pr"`
	Commit string    `json:"commit"`
	Worker string    `json:"worker"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	// ExitCode is the gate command's exit code; -1 when it timed out or
	// could not start.
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output,omitempty"`
}

func (g Gate) Passed() bool { return g.ExitCode == 0 }

func AppendGate(dir string, g Gate) error {
	return appendJSON(dir, GatesFile, g)
}

func ParseGate(line string) (Gate, error) {
	var g Gate
	if err := json.Unmarshal([]byte(line), &g); err != nil {
		return Gate{}, fmt.Errorf("parse gate record: %w", err)
	}
	return g, nil
}

// ReadGates returns the gate runs in file order.
func ReadGates(dir string) ([]Gate, error) {
	data, err := os.ReadFile(filepath.Join(dir, GatesFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read gate runs: %w", err)
	}
	var gates []Gate
	for _, line := range strings.Split(string(data), "\n") {
		if g, err := ParseGate(line); err == nil {
			gates = append(gates, g)
		}
	}
	return gates, nil
}
