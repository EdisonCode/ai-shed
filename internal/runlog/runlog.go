// Package runlog is the record of scheduled task runs that the agent writes
// on a machine and the watcher reads back.
package runlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State files live in this directory under the machine user's home.
const (
	StateDir      = ".local/state/shed"
	RunsFile      = "runs.jsonl"
	HeartbeatFile = "heartbeat"
)

// Record is one line of runs.jsonl. The agent writes a record when a task
// starts (End is zero) and another when it ends.
type Record struct {
	Task     string    `json:"task"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end,omitzero"`
	ExitCode int       `json:"exit_code"`
	// Next is the scheduled run after this one, as the agent computed it.
	Next   time.Time `json:"next,omitzero"`
	Output string    `json:"output,omitempty"`
}

func (r Record) Running() bool { return r.End.IsZero() }

func Append(dir string, r Record) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode run record: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, RunsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open run log: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("write run log: %w", err)
	}
	return f.Close()
}

func ParseLine(line string) (Record, error) {
	var r Record
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		return Record{}, fmt.Errorf("parse run record: %w", err)
	}
	return r, nil
}

// Latest returns the last record of each task. Records are in file order.
func Latest(records []Record) map[string]Record {
	latest := make(map[string]Record)
	for _, r := range records {
		latest[r.Task] = r
	}
	return latest
}
