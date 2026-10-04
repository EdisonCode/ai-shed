// Package runlog is the record of scheduled task runs that the agent writes
// on a machine and the watcher reads back.
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

// State files live in this directory under the machine user's home.
const (
	StateDir      = ".local/state/shed"
	RunsFile      = "runs.jsonl"
	CheckinsFile  = "checkins.jsonl"
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
	return appendJSON(dir, RunsFile, r)
}

func appendJSON(dir, file string, v any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s line: %w", file, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, file), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", file, err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", file, err)
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

// Verdicts of a check-in on a worker.
const (
	VerdictOnTrack    = "on_track"    // working inside its brief; nothing was said
	VerdictNudge      = "nudge"       // the supervisor sent it a message
	VerdictNeedsOwner = "needs_owner" // nothing can move without the owner
	VerdictDone       = "done"        // nothing left that it can act on
	VerdictStuck      = "stuck"       // nudges or restarts did not get it moving
	VerdictLimited    = "limited"     // at a usage limit; left alone until it resets
	VerdictStarted    = "started"     // the supervisor started its session
	VerdictError      = "error"       // the check-in itself failed
)

// Checkin is one line of checkins.jsonl: what the supervisor saw and did.
type Checkin struct {
	Time    time.Time `json:"time"`
	Worker  string    `json:"worker"`
	Kind    string    `json:"kind"`
	Verdict string    `json:"verdict"`
	Reason  string    `json:"reason,omitempty"`
	// Message is what the supervisor typed into the worker, when Sent.
	Message string `json:"message,omitempty"`
	Sent    bool   `json:"sent,omitempty"`
	// Cold is set when the worker's prompt cache had expired.
	Cold bool `json:"cold,omitempty"`
	// Issue and Model are set when the message gave the worker an issue.
	Issue int    `json:"issue,omitempty"`
	Model string `json:"model,omitempty"`
	// Activity and Queue record what a review saw: the time of the window's
	// last output, and a digest of the queue. A restarted agent reads them
	// back, so it does not review again what it has already reviewed.
	Activity time.Time `json:"activity,omitzero"`
	Queue    string    `json:"queue,omitempty"`
	// Until is when a limited worker is looked at again.
	Until time.Time `json:"until,omitzero"`
	// Rework is set when the issue was handed back to a worker because its
	// pull request could not merge; it says why.
	Rework string `json:"rework,omitempty"`
}

func AppendCheckin(dir string, c Checkin) error {
	return appendJSON(dir, CheckinsFile, c)
}

func ParseCheckin(line string) (Checkin, error) {
	var c Checkin
	if err := json.Unmarshal([]byte(line), &c); err != nil {
		return Checkin{}, fmt.Errorf("parse check-in: %w", err)
	}
	return c, nil
}

// LatestCheckins returns the last check-in of each worker.
func LatestCheckins(checkins []Checkin) map[string]Checkin {
	latest := make(map[string]Checkin)
	for _, c := range checkins {
		latest[c.Worker] = c
	}
	return latest
}

// ReadCheckins returns the check-ins in the log, oldest first. A missing log
// is no check-ins; a line that does not parse is skipped.
func ReadCheckins(dir string) ([]Checkin, error) {
	data, err := os.ReadFile(filepath.Join(dir, CheckinsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read check-ins: %w", err)
	}
	var checkins []Checkin
	for _, line := range strings.Split(string(data), "\n") {
		if c, err := ParseCheckin(line); err == nil {
			checkins = append(checkins, c)
		}
	}
	return checkins, nil
}

// IssueInHand returns the issue the worker was last handed since its session
// last started, or 0. Check-ins are in log order.
func IssueInHand(checkins []Checkin, worker string) int {
	issue := 0
	for _, c := range checkins {
		switch {
		case c.Worker != worker:
		case c.Verdict == VerdictStarted:
			issue = 0
		case c.Sent && c.Issue != 0:
			issue = c.Issue
		}
	}
	return issue
}

// An issue is sent back to a worker over its pull request at most
// ReworkLimit times in ReworkWindow. A check that fails for a reason no
// worker can fix must not keep one busy all night.
const (
	ReworkLimit  = 2
	ReworkWindow = 6 * time.Hour
)

// ReworkSpent reports whether the issue has been sent back as often as
// allowed.
func ReworkSpent(checkins []Checkin, issue int, now time.Time) bool {
	count := 0
	for _, c := range checkins {
		if c.Sent && c.Issue == issue && c.Rework != "" && now.Sub(c.Time) < ReworkWindow {
			count++
		}
	}
	return count >= ReworkLimit
}
