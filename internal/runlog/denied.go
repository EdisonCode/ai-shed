package runlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DeniedDir holds one file per worker: a line for each tool call its tool's
// permission check refused, as the tool's hook reported it.
const DeniedDir = "denied"

// maxDeniedLines bounds a worker's log of refusals.
const maxDeniedLines = 500

// maxDenialReason keeps the tool's reason to a line a status can show.
const maxDenialReason = 200

// Denial is one tool call the worker's tool refused: when, what kind of
// call, and the tool's reason. Only the first words of a command are kept,
// never an argument that could hold a secret.
type Denial struct {
	Time time.Time `json:"time"`
	// Kind is the command's first words, such as "gh pr ready" or
	// "git push --force-with-lease", or the tool's name for a call that is
	// not a shell command.
	Kind string `json:"kind"`
	// Issue is the issue a refused "gh issue comment" was for; 0 for none.
	Issue  int    `json:"issue,omitempty"`
	Reason string `json:"reason,omitempty"`
}

var (
	wordRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]*$`)
	flagRE  = regexp.MustCompile(`^--?[a-z][a-z-]*$`)
	envRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	splitRE = regexp.MustCompile(`&&|\|\||[;|\n]`)
)

// maxKindWords is how many words of a command its kind keeps.
const maxKindWords = 5

// DenialKind names a refused tool call by its first words, and the issue a
// refused "gh issue comment" was for. Words stop at the first that could
// be a value: a number, a path, a quoted string, a flag with a value.
func DenialKind(tool string, input json.RawMessage) (kind string, issue int) {
	var shell struct {
		Command string `json:"command"`
	}
	if tool != "Bash" || json.Unmarshal(input, &shell) != nil || strings.TrimSpace(shell.Command) == "" {
		return tool, 0
	}
	first := strings.TrimSpace(splitRE.Split(shell.Command, 2)[0])
	fields := strings.Fields(first)
	for len(fields) > 0 && envRE.MatchString(fields[0]) {
		fields = fields[1:]
	}
	var words []string
	for n, f := range fields {
		if len(words) == maxKindWords || !(wordRE.MatchString(f) || (n > 0 && flagRE.MatchString(f))) {
			if strings.Join(words, " ") == "gh issue comment" {
				issue, _ = strconv.Atoi(strings.TrimPrefix(f, "#"))
			}
			break
		}
		words = append(words, f)
	}
	if len(words) == 0 {
		return tool, 0
	}
	return strings.Join(words, " "), issue
}

// DenialReason keeps the tool's reason to one short line.
func DenialReason(reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	if len(reason) > maxDenialReason {
		reason = reason[:maxDenialReason]
	}
	return reason
}

// RecordDenial appends a refusal to the worker's log, keeping its newest
// maxDeniedLines.
func RecordDenial(dir, worker string, d Denial) error {
	data, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode refusal: %w", err)
	}
	logs := filepath.Join(dir, DeniedDir)
	if err := os.MkdirAll(logs, 0o755); err != nil {
		return fmt.Errorf("create refusals directory: %w", err)
	}
	path := filepath.Join(logs, worker)
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("record refusal: %w", err)
	}
	lines := append(bytes.Split(bytes.TrimSpace(old), []byte("\n")), data)
	if len(lines[0]) == 0 {
		lines = lines[1:]
	}
	lines = lines[max(0, len(lines)-maxDeniedLines):]
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(bytes.Join(lines, []byte("\n")), '\n'), 0o644); err != nil {
		return fmt.Errorf("record refusal: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("record refusal: %w", err)
	}
	return nil
}

// ParseDenial reads one line of a worker's log of refusals.
func ParseDenial(line string) (Denial, error) {
	var d Denial
	if err := json.Unmarshal([]byte(line), &d); err != nil || d.Kind == "" {
		return Denial{}, fmt.Errorf("parse refusal: %q", line)
	}
	return d, nil
}
