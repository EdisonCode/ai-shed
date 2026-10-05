package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/runlog"
	"github.com/edisoncode/ai-shed/internal/supervisor"
)

// maxMarkDetail keeps a mark to something a status line can show.
const maxMarkDetail = 300

// cmdMark records what a worker's tool says about itself. The tool's hooks
// run it: at the start of a turn, at the end, and when the tool stops to ask
// a person. The worker is the one whose tmux window the hook runs in, so the
// same hooks can be set for every session on the machine: outside a worker's
// window the command does nothing.
//
// It never exits 2. To an agent tool, a hook that exits 2 blocks what the
// hook is about, and a mark must never stop a worker.
func cmdMark(args []string) int {
	if len(args) == 0 || !runlog.ValidMark(args[0]) {
		fmt.Fprintf(os.Stderr, "usage: shed mark %s|%s|%s [detail]\n", runlog.MarkWorking, runlog.MarkIdle, runlog.MarkWaiting)
		return exitAttention
	}
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return exitOK
	}
	worker, err := supervisor.Tmux{}.WorkerOfPane(pane)
	if err != nil {
		return markFailed(err)
	}
	if worker == "" {
		return exitOK
	}
	detail := strings.Join(args[1:], " ")
	if detail == "" && piped(os.Stdin) {
		detail = runlog.HookMessage(os.Stdin)
	}
	if len(detail) > maxMarkDetail {
		detail = detail[:maxMarkDetail]
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return markFailed(err)
	}
	mark := runlog.Mark{Time: time.Now(), State: args[0], Detail: detail}
	if err := runlog.WriteMark(filepath.Join(home, runlog.StateDir), worker, mark); err != nil {
		return markFailed(err)
	}
	return exitOK
}

func markFailed(err error) int {
	fmt.Fprintf(os.Stderr, "shed mark: %v\n", err)
	return exitAttention
}

// piped reports whether the file is something a hook wrote to, not a
// terminal someone would have to type into.
func piped(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice == 0
}
