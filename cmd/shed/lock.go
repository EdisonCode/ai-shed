package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/edisoncode/ai-shed/internal/lock"
	"github.com/edisoncode/ai-shed/internal/runlog"
	"github.com/edisoncode/ai-shed/internal/supervisor"
)

// cmdLock runs a command while it holds a machine-wide lock, so the heavy
// commands of several workers (a full test run, a type-check) take turns
// instead of all running at once. It exits with the command's own code.
func cmdLock(args []string) int {
	if len(args) > 1 && args[1] == "--" {
		args = append(args[:1], args[2:]...)
	}
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: shed lock <name> -- <command> [args...]")
		return exitError
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	return runLocked(filepath.Join(home, runlog.StateDir, lock.Dir), args[0], args[1:], os.Stderr)
}

// runLocked takes the lock, runs the command and gives the lock up. It says
// on stderr whom it waits for, so a worker that waits knows why.
func runLocked(dir, name string, command []string, stderr io.Writer) int {
	holder := fmt.Sprintf("pid %d since %s", os.Getpid(), time.Now().Format("15:04"))
	if w := lockWorker(); w != "" {
		holder += ", worker " + w
	}
	holder += ": " + strings.Join(command, " ")
	l, err := lock.Acquire(dir, name, holder, func(heldBy string) {
		if heldBy == "" {
			heldBy = "another command"
		}
		fmt.Fprintf(stderr, "shed lock: waiting for lock %q, held by %s\n", name, heldBy)
	})
	if err != nil {
		return fail(err)
	}
	defer l.Release()

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "shed lock: %v\n", err)
		return 127
	}
	// An interrupt goes to the command; the lock is given up when it ends.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	go func() {
		for s := range signals {
			cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		return exit.ExitCode()
	default:
		fmt.Fprintf(stderr, "shed lock: %v\n", err)
		return exitAttention
	}
}

// lockWorker names the worker whose window the command runs in; "" outside
// one.
func lockWorker() string {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return ""
	}
	w, _ := supervisor.Tmux{}.WorkerOfPane(pane)
	return w
}
