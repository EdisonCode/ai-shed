package runlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// MessagesDir holds one file per worker: a line for each message its session
// sent to another session, as its tool's hook reported it.
const MessagesDir = "messages"

// maxMessageLines bounds a worker's message log. Past it, the lines older
// than a day are dropped.
const maxMessageLines = 500

// Message is one message a worker's session sent: when, to which session,
// and how long it was. The text is not kept.
type Message struct {
	Time  time.Time `json:"time"`
	To    string    `json:"to"`
	Chars int       `json:"chars"`
	// Failed is the error the send returned, when it did not reach the
	// session: the session's Remote Control is down, or it is not running.
	// A failed send is no message.
	Failed string `json:"failed,omitempty"`
}

// agentIDRE matches the id Claude Code gives a sub-agent, such as
// a1b35a3a05d21e82a. A session shed names is always machine-worker, so it
// never matches.
var agentIDRE = regexp.MustCompile(`^a[0-9a-f]{16}$`)

// ToSubagent reports whether a message went to one of the session's own
// sub-agents (SendMessage continues a sub-agent too), not to a peer.
func ToSubagent(to string) bool {
	return agentIDRE.MatchString(to)
}

// RecordMessage appends a message to the worker's log.
func RecordMessage(dir, worker string, m Message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}
	logs := filepath.Join(dir, MessagesDir)
	if err := os.MkdirAll(logs, 0o755); err != nil {
		return fmt.Errorf("create messages directory: %w", err)
	}
	path := filepath.Join(logs, worker)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("record message: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("record message: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("record message: %w", err)
	}
	return trimMessages(path, m.Time.Add(-24*time.Hour))
}

// trimMessages drops the lines older than cutoff once the log is longer
// than maxMessageLines, and keeps the newest maxMessageLines whatever their
// age. The file is replaced in one step.
func trimMessages(path string, cutoff time.Time) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("trim messages: %w", err)
	}
	if bytes.Count(data, []byte("\n")) <= maxMessageLines {
		return nil
	}
	var kept [][]byte
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if m, err := ParseMessage(string(line)); err == nil && !m.Time.Before(cutoff) {
			kept = append(kept, line)
		}
	}
	kept = kept[max(0, len(kept)-maxMessageLines):]
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(bytes.Join(kept, []byte("\n")), '\n'), 0o644); err != nil {
		return fmt.Errorf("trim messages: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("trim messages: %w", err)
	}
	return nil
}

// ParseMessage reads one line of a message log.
func ParseMessage(line string) (Message, error) {
	var m Message
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return Message{}, fmt.Errorf("parse message: %w", err)
	}
	return m, nil
}

// Messages returns the worker's recorded messages, oldest first. A worker
// that sent none has none; a line that cannot be read is skipped.
func Messages(dir, worker string) ([]Message, error) {
	f, err := os.Open(filepath.Join(dir, MessagesDir, worker))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}
	defer f.Close()
	var out []Message
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m, err := ParseMessage(sc.Text()); err == nil {
			out = append(out, m)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}
	return out, nil
}

// CountSince counts the messages sent since a time, by the session they
// went to. A failed send is not counted, and neither is one to the session's
// own sub-agent, which an older shed recorded.
func CountSince(ms []Message, since time.Time) map[string]int {
	counts := map[string]int{}
	for _, m := range ms {
		if !m.Time.Before(since) && m.Failed == "" && !ToSubagent(m.To) {
			counts[m.To]++
		}
	}
	return counts
}
