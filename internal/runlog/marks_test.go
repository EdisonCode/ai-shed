package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var marked = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func TestMarkIsReadBackAndReplaced(t *testing.T) {
	dir := t.TempDir()
	if _, ok := ReadMark(dir, "app"); ok {
		t.Fatal("a worker whose tool has reported nothing has a mark")
	}
	for _, state := range []string{MarkWorking, MarkWaiting} {
		if err := WriteMark(dir, "app", Mark{Time: marked, State: state, Detail: "Allow Bash?\nYes / No"}); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := ReadMark(dir, "app")
	if !ok || got.State != MarkWaiting || !got.Time.Equal(marked) || got.Detail != "Allow Bash?\nYes / No" {
		t.Fatalf("mark = %+v, %v", got, ok)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, MarksDir)); len(entries) != 1 {
		t.Fatalf("%d files in the marks directory, want only the worker's", len(entries))
	}
	if err := RemoveMark(dir, "app"); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadMark(dir, "app"); ok {
		t.Fatal("the mark is still there")
	}
	if err := RemoveMark(dir, "app"); err != nil {
		t.Fatalf("removing a mark that is not there: %v", err)
	}
}

func TestMarkThatCannotBeReadCountsAsNone(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, MarksDir), 0o755)
	for _, content := range []string{"", "not json", `{"state":"sleeping"}`} {
		os.WriteFile(filepath.Join(dir, MarksDir, "app"), []byte(content), 0o644)
		if _, ok := ReadMark(dir, "app"); ok {
			t.Errorf("%q was read as a mark", content)
		}
	}
}

func TestWaitingEndsWhenTheScreenMovesOn(t *testing.T) {
	waiting := Mark{Time: marked, State: MarkWaiting}
	for _, tc := range []struct {
		name         string
		mark         Mark
		lastActivity time.Duration
		want         string
	}{
		{"no mark", Mark{}, 0, ""},
		{"the prompt was just drawn", waiting, 2 * time.Second, MarkWaiting},
		{"nothing since the prompt", waiting, -time.Minute, MarkWaiting},
		{"output after the prompt: it was answered", waiting, time.Minute, MarkWorking},
		{"idle stays idle whatever the screen does", Mark{Time: marked, State: MarkIdle}, time.Minute, MarkIdle},
	} {
		if got := tc.mark.StateAt(marked.Add(tc.lastActivity)); got != tc.want {
			t.Errorf("%s: state = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestHookMessage(t *testing.T) {
	for input, want := range map[string]string{
		`{"hook_event_name":"Notification","message":"Claude needs your permission to use Bash"}`: "Claude needs your permission to use Bash",
		`{"hook_event_name":"Stop"}`: "",
		"":                           "",
		"not json":                   "",
	} {
		if got := HookMessage(strings.NewReader(input)); got != want {
			t.Errorf("message of %q = %q, want %q", input, got, want)
		}
	}
}
