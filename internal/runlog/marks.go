package runlog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// MarksDir holds one file per worker: the state its tool last reported
// through `shed mark`.
const MarksDir = "marks"

// States a worker's tool reports.
const (
	MarkWorking = "working" // a turn is in progress
	MarkIdle    = "idle"    // the turn ended; it waits for the next message
	MarkWaiting = "waiting" // it stopped in the middle of a turn to ask a person
)

// markSlack is how long after a mark the screen may still be drawing what
// the mark is about.
const markSlack = 5 * time.Second

// maxHookInput bounds what is read of a hook's input.
const maxHookInput = 1 << 20

// Mark is what a worker's tool last said about itself.
type Mark struct {
	Time  time.Time `json:"time"`
	State string    `json:"state"`
	// Detail is the tool's own words, such as the text of a permission prompt.
	Detail string `json:"detail,omitempty"`
}

// ValidMark reports whether state is one a tool can report.
func ValidMark(state string) bool {
	return state == MarkWorking || state == MarkIdle || state == MarkWaiting
}

// StateAt is the state the mark stands for, given when the worker's window
// last printed anything; "" for no mark. A tool reports that it waits for a
// person but not that it got its answer: output after the mark means the
// worker moved on.
func (m Mark) StateAt(lastActivity time.Time) string {
	if m.State == MarkWaiting && lastActivity.After(m.Time.Add(markSlack)) {
		return MarkWorking
	}
	return m.State
}

// WriteMark records a worker's state. The file is replaced in one step, so a
// reader never sees half of it.
func WriteMark(dir, worker string, m Mark) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode mark: %w", err)
	}
	marks := filepath.Join(dir, MarksDir)
	if err := os.MkdirAll(marks, 0o755); err != nil {
		return fmt.Errorf("create marks directory: %w", err)
	}
	tmp, err := os.CreateTemp(marks, "."+worker+"-*")
	if err != nil {
		return fmt.Errorf("write mark: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("write mark: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write mark: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(marks, worker)); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write mark: %w", err)
	}
	return nil
}

// ReadMark returns the worker's mark. A worker whose tool has no hooks has
// none, and a mark that cannot be read counts as none: the supervisor then
// judges from the screen, as it does without hooks.
func ReadMark(dir, worker string) (Mark, bool) {
	data, err := os.ReadFile(filepath.Join(dir, MarksDir, worker))
	if err != nil {
		return Mark{}, false
	}
	m, err := ParseMark(string(data))
	return m, err == nil
}

func ParseMark(line string) (Mark, error) {
	var m Mark
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return Mark{}, fmt.Errorf("parse mark: %w", err)
	}
	if !ValidMark(m.State) {
		return Mark{}, fmt.Errorf("parse mark: unknown state %q", m.State)
	}
	return m, nil
}

// RemoveMark forgets the worker's mark: it was about a session that is over.
func RemoveMark(dir, worker string) error {
	if err := os.Remove(filepath.Join(dir, MarksDir, worker)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove mark: %w", err)
	}
	return nil
}

// HookMessage returns the message in the JSON that an agent tool passes to
// a hook on its input, or "" when there is none.
func HookMessage(input io.Reader) string {
	var hook struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(input, maxHookInput)).Decode(&hook); err != nil {
		return ""
	}
	return hook.Message
}
