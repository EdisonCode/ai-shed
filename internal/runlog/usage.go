package runlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/pricing"
)

// ReviewerLedger is the ledger of the reviewer's own calls. A worker's name
// cannot start with "_", so it is no worker's.
const ReviewerLedger = "_reviewer"

// RecordReviewerUsage adds the reviewer's calls to its ledger.
func RecordReviewerUsage(dir string, calls []Usage) error {
	found := make(map[string]Usage, len(calls))
	for _, u := range calls {
		found[u.ID] = u
	}
	return appendUsage(dir, ReviewerLedger, found)
}

// UsageDir holds one ledger per worker: the tokens its tool reported, and
// the transcript files those tokens were read from.
const UsageDir = "usage"

// Usage is one model call, as the session transcript recorded it.
type Usage struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	Session string    `json:"session,omitempty"`
	Model   string    `json:"model,omitempty"`
	// Input is uncached input tokens. CacheWrite and CacheRead are the
	// prompt-cache tokens, which are billed apart from Input.
	Input      int `json:"input,omitempty"`
	Output     int `json:"output,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
	CacheRead  int `json:"cache_read,omitempty"`
	// CacheWrite1h is the part of CacheWrite written for an hour, which is
	// billed higher than a five-minute write.
	CacheWrite1h int `json:"cache_write_1h,omitempty"`
	// Agent is the type of the sub-agent that made the call, such as
	// "Explore"; "" for the worker's own session.
	Agent string `json:"agent,omitempty"`
	// Dispatches counts the sub-agents the call started.
	Dispatches int `json:"dispatches,omitempty"`
}

// Tokens are the call's tokens, as pricing reads them.
func (u Usage) Tokens() pricing.Tokens {
	return pricing.Tokens{Input: u.Input, CacheWrite: u.CacheWrite, CacheWrite1h: u.CacheWrite1h, CacheRead: u.CacheRead, Output: u.Output}
}

func (u Usage) billed() int { return u.Input + u.Output + u.CacheWrite + u.CacheRead }

// Hook is the part of an agent tool's hook input that shed reads.
type Hook struct {
	Message        string
	SessionID      string
	TranscriptPath string
	// AgentTranscriptPath and AgentType are set when the hook is about a
	// sub-agent: its own transcript, and its type.
	AgentTranscriptPath string
	AgentType           string
}

// ParseHook reads the JSON an agent tool passes a hook. A body that is not
// JSON is an empty hook: the mark still stands, and no usage is recorded.
func ParseHook(input io.Reader) Hook {
	var raw struct {
		Message        string `json:"message"`
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		AgentPath      string `json:"agent_transcript_path"`
		AgentType      string `json:"agent_type"`
	}
	if err := json.NewDecoder(io.LimitReader(input, maxHookInput)).Decode(&raw); err != nil {
		return Hook{}
	}
	return Hook{Message: raw.Message, SessionID: raw.SessionID, TranscriptPath: raw.TranscriptPath,
		AgentTranscriptPath: raw.AgentPath, AgentType: raw.AgentType}
}

// RecordTranscript remembers the transcript and adds any calls in it that
// the ledger does not already have. The same file is read again later: a
// hook can run before the turn that just finished has been written out.
func RecordTranscript(dir, worker, path, session string) error {
	if path == "" {
		return nil
	}
	t := transcript{path: path}
	if err := noteTranscript(dir, worker, t); err != nil {
		return err
	}
	return scrapeTranscript(dir, worker, t, session)
}

// RecordAgentTranscript is RecordTranscript for a sub-agent's own
// transcript. Its calls are tagged with the agent's type: the one given,
// else the one in the transcript's meta file, else "sub-agent".
func RecordAgentTranscript(dir, worker, path, session, agentType string) error {
	if path == "" {
		return nil
	}
	if agentType == "" {
		agentType = metaAgentType(path)
	}
	t := transcript{path: path, agent: agentType}
	if err := noteTranscript(dir, worker, t); err != nil {
		return err
	}
	return scrapeTranscript(dir, worker, t, session)
}

// metaAgentType reads the agent type Claude Code keeps beside a sub-agent's
// transcript; "sub-agent" when it says none.
func metaAgentType(path string) string {
	data, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".meta.json")
	if err == nil {
		var meta struct {
			AgentType string `json:"agentType"`
		}
		if json.Unmarshal(data, &meta) == nil && strings.TrimSpace(meta.AgentType) != "" {
			return strings.TrimSpace(meta.AgentType)
		}
	}
	return "sub-agent"
}

// transcript is a recorded transcript file. agent is set for a
// sub-agent's own transcript, whose rows are all sidechains.
type transcript struct {
	path, agent string
}

// RefreshUsage re-reads the transcripts already recorded for the worker, so
// a turn that was still being written when the hook ran is counted before
// the cost is posted.
func RefreshUsage(dir, worker string) error {
	paths, err := transcriptPaths(dir, worker)
	if err != nil {
		return err
	}
	var first error
	for _, t := range paths {
		if err := scrapeTranscript(dir, worker, t, ""); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ReadUsage returns the worker's calls, one per id. A call written twice
// keeps the larger token counts. A missing ledger is no calls.
func ReadUsage(dir, worker string) ([]Usage, error) {
	data, err := os.ReadFile(usagePath(dir, worker))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read usage: %w", err)
	}
	byID := map[string]Usage{}
	var order []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		u, err := parseUsage(line)
		if err != nil || u.ID == "" {
			continue
		}
		prev, seen := byID[u.ID]
		if !seen {
			order = append(order, u.ID)
			byID[u.ID] = u
			continue
		}
		if u.billed() > prev.billed() {
			byID[u.ID] = u
		}
	}
	out := make([]Usage, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out, nil
}

func usagePath(dir, worker string) string {
	return filepath.Join(dir, UsageDir, worker+".jsonl")
}

func transcriptsFile(dir, worker string) string {
	return filepath.Join(dir, UsageDir, worker+".transcripts")
}

func noteTranscript(dir, worker string, t transcript) error {
	paths, err := transcriptPaths(dir, worker)
	if err != nil {
		return err
	}
	for _, have := range paths {
		if have.path == t.path {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, UsageDir), 0o755); err != nil {
		return fmt.Errorf("create usage directory: %w", err)
	}
	f, err := os.OpenFile(transcriptsFile(dir, worker), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("record transcript: %w", err)
	}
	line := t.path
	if t.agent != "" {
		line += "\t" + t.agent
	}
	_, err = fmt.Fprintln(f, line)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("record transcript: %w", err)
	}
	return nil
}

// transcriptPaths reads the recorded transcripts: one per line, a path, and
// for a sub-agent's a tab and its type.
func transcriptPaths(dir, worker string) ([]transcript, error) {
	data, err := os.ReadFile(transcriptsFile(dir, worker))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read transcripts: %w", err)
	}
	var paths []transcript
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		path, agent, _ := strings.Cut(strings.TrimSpace(line), "\t")
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, transcript{path: path, agent: agent})
	}
	return paths, nil
}

// maxTranscriptLine is how long a transcript line may be. A longer one is
// skipped: the call's usage sits on its own small line, and a tool result
// can be far larger than the usage that billed it.
const maxTranscriptLine = 16 << 20

func scrapeTranscript(dir, worker string, t transcript, session string) error {
	f, err := os.Open(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read transcript: %w", err)
	}
	defer f.Close()
	found := map[string]Usage{}
	// A message is written one content block per line, each with the same
	// usage: its dispatches are the sum over its lines.
	dispatched := map[string]map[string]bool{}
	if err := scanLines(f, maxTranscriptLine, func(line []byte) {
		u, tools, ok := usageFromLine(line, session, t.agent)
		if !ok {
			return
		}
		if dispatched[u.ID] == nil {
			dispatched[u.ID] = map[string]bool{}
		}
		for _, id := range tools {
			dispatched[u.ID][id] = true
		}
		if prev, seen := found[u.ID]; !seen || u.billed() > prev.billed() {
			found[u.ID] = u
		}
	}); err != nil {
		return fmt.Errorf("read transcript: %w", err)
	}
	for id, u := range found {
		u.Dispatches = len(dispatched[id])
		found[id] = u
	}
	return appendUsage(dir, worker, found)
}

// scanLines calls fn with each line that fits in max. A longer line, such as
// a tool result, is skipped so the calls after it are still read.
func scanLines(r io.Reader, max int, fn func([]byte)) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var acc []byte
	skipping := false
	for {
		chunk, err := br.ReadSlice('\n')
		full := errors.Is(err, bufio.ErrBufferFull)
		switch {
		case skipping:
			if !full {
				skipping = false
			}
		case len(acc)+len(chunk) > max:
			acc = nil
			skipping = full
		default:
			acc = append(acc, chunk...)
			if !full {
				if len(acc) > 0 {
					fn(bytes.TrimRight(acc, "\r\n"))
				}
				acc = nil
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil && !full {
			return err
		}
	}
}

func appendUsage(dir, worker string, found map[string]Usage) error {
	if len(found) == 0 {
		return nil
	}
	have, err := ReadUsage(dir, worker)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, u := range have {
		known[u.ID] = true
	}
	var fresh []Usage
	for id, u := range found {
		if !known[id] {
			fresh = append(fresh, u)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	sort.Slice(fresh, func(i, j int) bool {
		if !fresh[i].Time.Equal(fresh[j].Time) {
			return fresh[i].Time.Before(fresh[j].Time)
		}
		return fresh[i].ID < fresh[j].ID
	})
	if err := os.MkdirAll(filepath.Join(dir, UsageDir), 0o755); err != nil {
		return fmt.Errorf("create usage directory: %w", err)
	}
	f, err := os.OpenFile(usagePath(dir, worker), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("write usage: %w", err)
	}
	for _, u := range fresh {
		line, err := json.Marshal(u)
		if err != nil {
			f.Close()
			return fmt.Errorf("write usage: %w", err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			f.Close()
			return fmt.Errorf("write usage: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write usage: %w", err)
	}
	return nil
}

func parseUsage(line string) (Usage, error) {
	var u Usage
	if err := json.Unmarshal([]byte(line), &u); err != nil {
		return Usage{}, err
	}
	return u, nil
}

// usageFromLine reads one transcript line, and the ids of the sub-agents it
// starts. ok is false for a line that is not a billed model call: a person,
// an error with no tokens, a call with no id, or, in the worker's own
// transcript, a side chain (a sub-agent's transcript is recorded apart, with
// agent set).
func usageFromLine(line []byte, session, agent string) (u Usage, dispatches []string, ok bool) {
	var row struct {
		Type        string `json:"type"`
		UUID        string `json:"uuid"`
		Timestamp   string `json:"timestamp"`
		IsSidechain bool   `json:"isSidechain"`
		Message     struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Content []struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content"`
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreation            struct {
					OneHour int `json:"ephemeral_1h_input_tokens"`
				} `json:"cache_creation"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &row); err != nil || row.Type != "assistant" || (row.IsSidechain && agent == "") {
		return Usage{}, nil, false
	}
	tokens := row.Message.Usage
	if tokens.InputTokens+tokens.OutputTokens+tokens.CacheCreationInputTokens+tokens.CacheReadInputTokens == 0 {
		return Usage{}, nil, false
	}
	id := row.Message.ID
	if id == "" {
		id = row.UUID
	}
	if id == "" {
		return Usage{}, nil, false
	}
	for _, c := range row.Message.Content {
		// The tool was named Task before Claude Code 2.1.63.
		if c.Type == "tool_use" && (c.Name == "Agent" || c.Name == "Task") && c.ID != "" {
			dispatches = append(dispatches, c.ID)
		}
	}
	when := time.Now()
	if t, err := time.Parse(time.RFC3339Nano, row.Timestamp); err == nil {
		when = t
	} else if t, err := time.Parse(time.RFC3339, row.Timestamp); err == nil {
		when = t
	}
	return Usage{
		ID: id, Time: when, Session: session, Model: row.Message.Model,
		Input: tokens.InputTokens, Output: tokens.OutputTokens,
		CacheWrite: tokens.CacheCreationInputTokens, CacheRead: tokens.CacheReadInputTokens,
		CacheWrite1h: min(tokens.CacheCreation.OneHour, tokens.CacheCreationInputTokens),
		Agent:        agent,
	}, dispatches, true
}
