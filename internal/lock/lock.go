// Package lock lets the heavy commands of a machine's workers take turns:
// one holder of a named lock at a time, machine-wide. It uses flock(2),
// which macOS has though it has no flock command. The kernel releases the
// lock when its holder exits, however it exits, so a holder that died
// leaves nothing to clean up.
package lock

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// Dir holds the lock files, under the shed state directory.
const Dir = "locks"

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// ValidName reports whether name can name a lock: lower case letters,
// digits, dots, dashes and underscores.
func ValidName(name string) bool {
	return nameRE.MatchString(name)
}

// Lock is a lock in hand. Release gives it up.
type Lock struct {
	f *os.File
}

// Acquire takes the named lock in dir, waiting as long as another holds it.
// Before it waits it calls waiting, once, with what the holder wrote about
// itself ("" when it wrote nothing). holder is what this process writes
// about itself, for the next one that waits.
func Acquire(dir, name, holder string, waiting func(heldBy string)) (*Lock, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("lock name %q must match %s", name, nameRE)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("lock %s: %w", name, err)
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", name, err)
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		if waiting != nil {
			held, _ := io.ReadAll(f)
			waiting(strings.TrimSpace(string(held)))
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", name, err)
	}
	l := &Lock{f: f}
	if err := l.write(holder + "\n"); err != nil {
		l.Release()
		return nil, fmt.Errorf("lock %s: write the holder: %w", name, err)
	}
	return l, nil
}

func (l *Lock) write(s string) error {
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	_, err := l.f.WriteAt([]byte(s), 0)
	return err
}

// Release clears the holder's note and gives the lock up.
func (l *Lock) Release() error {
	// The file stays: removing it would let a waiter that opened it lock a
	// file nobody else can find.
	l.write("")
	return l.f.Close()
}
