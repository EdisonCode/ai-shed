package runlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edisoncode/ai-shed/internal/diraccess"
)

// DirsFile holds the agent's last read of each worker's dir.
const DirsFile = "dirs.json"

// DirCheck is what the agent found when it read each worker's dir itself.
// On macOS the agent has a privacy grant of its own, apart from tmux's and
// SSH's.
type DirCheck struct {
	Time     time.Time           `json:"time"`
	Problems []diraccess.Problem `json:"problems"`
	Host     diraccess.Host      `json:"host"`
}

// WriteDirCheck keeps the agent's last dir check for `shed status`. It is
// written whole each time, so problems that are fixed go away.
func WriteDirCheck(dir string, c DirCheck) error {
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("write dir check: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("write dir check: %w", err)
	}
	tmp := filepath.Join(dir, DirsFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write dir check: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, DirsFile)); err != nil {
		return fmt.Errorf("write dir check: %w", err)
	}
	return nil
}

// ParseDirCheck reads a dir check as WriteDirCheck wrote it.
func ParseDirCheck(data []byte) (DirCheck, error) {
	var c DirCheck
	if err := json.Unmarshal(data, &c); err != nil {
		return DirCheck{}, fmt.Errorf("read dir check: %w", err)
	}
	return c, nil
}
