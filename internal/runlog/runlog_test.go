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

func TestIssueInHand(t *testing.T) {
	handed := func(worker string, issue int) Checkin {
		return Checkin{Worker: worker, Verdict: VerdictNudge, Sent: true, Issue: issue}
	}
	started := Checkin{Worker: "app", Verdict: VerdictStarted, Sent: true}
	cases := []struct {
		name     string
		checkins []Checkin
		want     int
	}{
		{"no check-ins", nil, 0},
		{"handed an issue", []Checkin{started, handed("app", 12)}, 12},
		{"handed a second issue", []Checkin{started, handed("app", 12), handed("app", 13)}, 13},
		{"a nudge that names no issue changes nothing", []Checkin{handed("app", 12), {Worker: "app", Verdict: VerdictNudge, Sent: true}}, 12},
		{"a message that was not sent hands nothing over", []Checkin{{Worker: "app", Verdict: VerdictStuck, Issue: 12}}, 0},
		{"a new session has no issue in hand", []Checkin{handed("app", 12), started}, 0},
		{"another worker's issue is not this worker's", []Checkin{handed("docs", 12)}, 0},
	}
	for _, tc := range cases {
		if got := IssueInHand(tc.checkins, "app"); got != tc.want {
			t.Errorf("%s: issue in hand = %d, want %d", tc.name, got, tc.want)
		}
	}
}
