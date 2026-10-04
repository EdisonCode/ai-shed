package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendedRecordsReadBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	start := time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC)
	started := Record{Task: "nightly", Start: start, Next: start.Add(24 * time.Hour)}
	ended := started
	ended.End, ended.ExitCode, ended.Output = start.Add(2*time.Second), 3, "boom"
	for _, r := range []Record{started, ended} {
		if err := Append(dir, r); err != nil {
			t.Fatal(err)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, RunsFile))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	first, err := ParseLine(lines[0])
	if err != nil || !first.Running() {
		t.Fatalf("first record = %+v, err %v; want a running record", first, err)
	}
	second, err := ParseLine(lines[1])
	if err != nil || second.Running() || second.ExitCode != 3 || !second.Next.Equal(started.Next) {
		t.Fatalf("second record = %+v, err %v", second, err)
	}
}

func TestLatestKeepsLastRecordPerTask(t *testing.T) {
	got := Latest([]Record{{Task: "a", ExitCode: 1}, {Task: "b"}, {Task: "a", ExitCode: 0}})
	if len(got) != 2 || got["a"].ExitCode != 0 {
		t.Fatalf("latest = %+v", got)
	}
}
