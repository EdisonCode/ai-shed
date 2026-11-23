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
// They also say what the session does with messages from the owner's other
// Claude Code sessions (inbound). Without a value, a session that bypasses
// permission prompts holds each message in an approval dialog that nobody
// answers, and the worker waits on it.
func quietSettings(inbound string) string {
	return `{"idleCompaction":false,"promptSuggestionEnabled":false,"awaySummaryEnabled":false,"crossSessionInbound":"` + inbound + `"}`
}

// inboundFor is the crossSessionInbound value of a worker: a worker with
// peers accepts messages, and one without refuses them.
func inboundFor(w config.Worker) string {
	if len(w.PeerList) > 0 {
		return "accept"
	}
	return "refuse"
}

// sessionCommand is the command that starts the worker's session. An
// unattended Claude Code worker gets quietSettings, and a worker with peers
// gets the session name they message it by. A worker that talks with the
// owner, another tool, and a command that passes its own settings get no
// settings.
func sessionCommand(w config.Worker) string {
	command := w.CommandOrDefault()
	fields := strings.Fields(command)
	if w.Ask || len(fields) == 0 || filepath.Base(fields[0]) != "claude" {
		return command
	}
	if _, own := settingsArg(fields); !own {
		command += " --settings " + shellQuote(quietSettings(inboundFor(w)))
	}
	if w.Session != "" {
		command += " --name " + shellQuote(w.Session)
	}
	return command
}
