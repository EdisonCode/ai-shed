package runlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PauseFile holds the time, in seconds since the epoch by the machine's own
// clock, until which the owner paused the machine. While it is paused the
// supervisor types nothing into a worker and hands out nothing.
const PauseFile = "pause"

// PausedUntil returns when the machine's pause ends. ok is false when it is
// not paused, or when the pause has run out.
func PausedUntil(dir string, now time.Time) (until time.Time, ok bool) {
	data, err := os.ReadFile(filepath.Join(dir, PauseFile))
	if err != nil {
		return time.Time{}, false
	}
	until, ok = ParsePause(string(data))
	return until, ok && now.Before(until)
}

// ParsePause reads the content of the pause file.
func ParsePause(content string) (time.Time, bool) {
	seconds, err := strconv.ParseInt(strings.TrimSpace(content), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}

// RemovePause ends a pause. A machine that is not paused is no error.
func RemovePause(dir string) error {
	err := os.Remove(filepath.Join(dir, PauseFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("end the pause: %w", err)
	}
	return nil
}
