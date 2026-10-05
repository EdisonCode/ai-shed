package runlog

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMachineIsPausedUntilTheTimeInThePauseFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1790000000, 0)
	if _, ok := PausedUntil(dir, now); ok {
		t.Fatal("a machine with no pause file is paused")
	}
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(dir, PauseFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(fmt.Sprintf("%d\n", now.Add(30*time.Minute).Unix()))
	if until, ok := PausedUntil(dir, now); !ok || !until.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("paused until %v, %v", until, ok)
	}
	if _, ok := PausedUntil(dir, now.Add(30*time.Minute)); ok {
		t.Fatal("the pause has run out and still holds")
	}
	write("soon")
	if _, ok := PausedUntil(dir, now); ok {
		t.Fatal("a pause file that names no time pauses the machine")
	}
	if err := RemovePause(dir); err != nil {
		t.Fatal(err)
	}
	if err := RemovePause(dir); err != nil {
		t.Fatalf("ending a pause that is not there = %v", err)
	}
}
