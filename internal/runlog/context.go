package runlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ContextDir holds, per worker, the estimated tokens of what its tool loads
// at session start, written when the session starts.
const ContextDir = "context"

func WriteContext(dir, worker string, tokens int) error {
	path := filepath.Join(dir, ContextDir, worker)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("write context size: %w", err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(tokens)+"\n"), 0o644); err != nil {
		return fmt.Errorf("write context size: %w", err)
	}
	return nil
}

// ParseContext reads a context size line: the tokens, as WriteContext wrote
// them.
func ParseContext(text string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(text))
	return n, err == nil
}
