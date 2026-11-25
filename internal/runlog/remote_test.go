package runlog

import (
	"testing"
	"time"
)

func TestRemoteControlStateIsReadBackAsWritten(t *testing.T) {
	dir := t.TempDir()
	if _, ok := ReadRemote(dir, "app"); ok {
		t.Fatal("a worker with nothing recorded reads as recorded")
	}
	since := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	want := Remote{Since: since, Typed: 2, LastTyped: since.Add(10 * time.Minute)}
	if err := WriteRemote(dir, "app", want); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadRemote(dir, "app")
	if !ok || !got.Since.Equal(want.Since) || got.Typed != 2 || !got.LastTyped.Equal(want.LastTyped) || !got.Down() {
		t.Fatalf("read %+v, %v; want %+v, down", got, ok, want)
	}
	got.Back = since.Add(20 * time.Minute)
	if got.Down() {
		t.Fatal("Remote Control that came back still reads as down")
	}
}

func TestARemoteControlLineWithoutATimeIsRefused(t *testing.T) {
	if _, err := ParseRemote(`{"typed":1}`); err == nil {
		t.Fatal("a state with no time it went down parsed")
	}
}
