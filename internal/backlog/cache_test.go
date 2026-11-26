package backlog

import (
	"errors"
	"testing"
	"time"
)

func TestAReadIsSharedWithinItsTimeAndReadAgainAfter(t *testing.T) {
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	c := readCache[int]{now: func() time.Time { return now }}
	reads := 0
	read := func() (int, error) { reads++; return reads, nil }

	a, _ := c.get("org/app", read)
	now = now.Add(20 * time.Second)
	b, _ := c.get("org/app", read)
	if a != 1 || b != 1 || reads != 1 {
		t.Fatalf("within the time: got %d, %d after %d reads, want one shared read", a, b, reads)
	}
	if v, _ := c.get("org/api", read); v != 2 {
		t.Fatalf("another key shared a read: %d", v)
	}
	now = now.Add(10 * time.Second)
	if v, _ := c.get("org/app", read); v != 3 {
		t.Fatalf("after the time: got %d, want a fresh read", v)
	}
}

func TestAFailedReadIsNotKept(t *testing.T) {
	c := readCache[int]{}
	if _, err := c.get("org/app", func() (int, error) { return 0, errors.New("timeout") }); err == nil {
		t.Fatal("the failure was not returned")
	}
	if v, err := c.get("org/app", func() (int, error) { return 7, nil }); err != nil || v != 7 {
		t.Fatalf("got %d, %v; want the read after a failure to run", v, err)
	}
}

func TestASharedListIsACopy(t *testing.T) {
	kept := []Issue{{Number: 12}}
	got := sharedList(kept)
	got[0].Verify = true
	if kept[0].Verify {
		t.Fatal("setting a field of a shared issue set it for every reader")
	}
}
