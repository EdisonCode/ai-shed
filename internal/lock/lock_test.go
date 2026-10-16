package lock

import (
	"testing"
	"time"
)

func TestASecondHolderWaitsForTheFirstAndIsToldWhoHoldsIt(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir, "test", "pid 1: make test", nil)
	if err != nil {
		t.Fatal(err)
	}
	heldBy := make(chan string, 1)
	acquired := make(chan *Lock)
	go func() {
		second, err := Acquire(dir, "test", "pid 2: make test", func(h string) { heldBy <- h })
		if err != nil {
			t.Error(err)
		}
		acquired <- second
	}()

	if got := <-heldBy; got != "pid 1: make test" {
		t.Errorf("held by %q, want the first holder's note", got)
	}
	select {
	case <-acquired:
		t.Fatal("the second holder got the lock while the first held it")
	case <-time.After(100 * time.Millisecond):
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case second := <-acquired:
		second.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("the second holder did not get the lock after the first released it")
	}
}

func TestLocksWithDifferentNamesDoNotWaitForEachOther(t *testing.T) {
	dir := t.TempDir()
	a, err := Acquire(dir, "test", "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	done := make(chan struct{})
	go func() {
		b, err := Acquire(dir, "typecheck", "b", func(string) { t.Error("a different lock waited") })
		if err == nil {
			b.Release()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a different lock waited")
	}
}

func TestALockNameMustBeSafeAsAFileName(t *testing.T) {
	for _, name := range []string{"", "../x", "Test", "a b"} {
		if _, err := Acquire(t.TempDir(), name, "", nil); err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
}

func TestALockWhoseHolderDiedIsFree(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir, "test", "pid 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	first.f.Close() // what the kernel does when the holder dies
	done := make(chan struct{})
	go func() {
		second, err := Acquire(dir, "test", "pid 2", nil)
		if err == nil {
			second.Release()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock of a holder that died was not free")
	}
}
