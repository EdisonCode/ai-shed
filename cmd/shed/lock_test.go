package main

import (
	"bytes"
	"testing"
)

func TestALockedCommandExitsWithItsOwnCode(t *testing.T) {
	var stderr bytes.Buffer
	if got := runLocked(t.TempDir(), "test", []string{"sh", "-c", "exit 3"}, &stderr); got != 3 {
		t.Errorf("exit code = %d, want the command's 3; stderr %q", got, stderr.String())
	}
	if got := runLocked(t.TempDir(), "test", []string{"true"}, &stderr); got != exitOK {
		t.Errorf("exit code = %d, want 0", got)
	}
}

func TestALockedCommandThatCannotStartExits127(t *testing.T) {
	var stderr bytes.Buffer
	if got := runLocked(t.TempDir(), "test", []string{"no-such-command-shed-test"}, &stderr); got != 127 {
		t.Errorf("exit code = %d, want 127", got)
	}
}
