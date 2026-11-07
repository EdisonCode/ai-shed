package supervisor

import (
	"path/filepath"
	"strings"

	"github.com/edisoncode/ai-shed/internal/config"
)

// quietSettings turn off the requests a Claude Code session makes on its
// own while nobody reads it: compacting a conversation left idle, which the
// supervisor clears instead once its cache is cold; suggesting the next
// prompt after each answer; and writing a recap for a person coming back.
const quietSettings = `{"idleCompaction":false,"promptSuggestionEnabled":false,"awaySummaryEnabled":false}`

// sessionCommand is the command that starts the worker's session. An
// unattended Claude Code worker gets quietSettings. A worker that talks with
// the owner, another tool, and a command that passes its own settings are
// left as they are.
func sessionCommand(w config.Worker) string {
	command := w.CommandOrDefault()
	fields := strings.Fields(command)
	if w.Ask || len(fields) == 0 || filepath.Base(fields[0]) != "claude" || strings.Contains(command, "--settings") {
		return command
	}
	return command + " --settings " + shellQuote(quietSettings)
}
