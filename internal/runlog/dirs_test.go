package runlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/diraccess"
)

func TestDirCheckRoundTrips(t *testing.T) {
	dir := t.TempDir()
	want := DirCheck{Time: time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC),
		Problems: []diraccess.Problem{{Worker: "app", Via: diraccess.ViaAgent, Blocked: true}},
		Host:     diraccess.Host{OS: "Darwin", Shed: "/Users/x/.local/bin/shed"}}
	if err := WriteDirCheck(dir, want); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, DirsFile))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseDirCheck(data)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Time.Equal(want.Time) || len(got.Problems) != 1 || got.Problems[0] != want.Problems[0] || got.Host != want.Host {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDirCheckWithNoProblemsReplacesOneWithProblems(t *testing.T) {
	dir := t.TempDir()
	WriteDirCheck(dir, DirCheck{Problems: []diraccess.Problem{{Worker: "app", Via: diraccess.ViaAgent, Err: "Operation not permitted"}}})
	if err := WriteDirCheck(dir, DirCheck{}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, DirsFile))
	if got, err := ParseDirCheck(data); err != nil || len(got.Problems) != 0 {
		t.Fatalf("got %+v, %v; a fixed problem must go away", got, err)
	}
}
