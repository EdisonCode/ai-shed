package runlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// PaceFile holds the machine's last reading of its weekly pace.
const PaceFile = "pace.json"

// Where a machine's spend stands against its pace line.
const (
	PaceOn       = "on pace"   // no more than the share of the week gone
	PaceAhead    = "ahead"     // only priority work is handed out
	PaceFarAhead = "far ahead" // new work is held on all but a few workers
)

// farAhead is how far over the pace line, as a part of the week's share,
// holds new work on most workers.
const farAhead = 0.25

// Pace is a machine's spend this week against its share of its account's
// weekly budget, in dollars at API rates.
type Pace struct {
	Account   string    `json:"account"`
	Budget    float64   `json:"budget"`
	Share     float64   `json:"share"`
	Spend     float64   `json:"spend"`
	Line      float64   `json:"line"`
	State     string    `json:"state"`
	WeekStart time.Time `json:"week_start"`
	Now       time.Time `json:"now"`
}

// NewPace reads the spend against the pace line: the share times the part
// of the week gone since weekStart.
func NewPace(account string, budget, share, spend float64, weekStart, now time.Time) Pace {
	gone := min(max(now.Sub(weekStart).Hours()/(7*24), 0), 1)
	p := Pace{Account: account, Budget: budget, Share: share, Spend: spend, Line: share * gone, WeekStart: weekStart, Now: now, State: PaceOn}
	switch {
	case spend > p.Line+farAhead*share:
		p.State = PaceFarAhead
	case spend > p.Line:
		p.State = PaceAhead
	}
	return p
}

// WritePace keeps the pace for `shed status`.
func WritePace(dir string, p Pace) error {
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("write pace: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("write pace: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, PaceFile), data, 0o644); err != nil {
		return fmt.Errorf("write pace: %w", err)
	}
	return nil
}

// ReadPace returns the pace the agent last wrote; ok is false for none.
func ReadPace(dir string) (Pace, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, PaceFile))
	if errors.Is(err, os.ErrNotExist) {
		return Pace{}, false, nil
	}
	if err != nil {
		return Pace{}, false, fmt.Errorf("read pace: %w", err)
	}
	return ParsePace(data)
}

// ParsePace reads a pace as WritePace wrote it.
func ParsePace(data []byte) (Pace, bool, error) {
	var p Pace
	if err := json.Unmarshal(data, &p); err != nil {
		return Pace{}, false, fmt.Errorf("read pace: %w", err)
	}
	return p, true, nil
}
