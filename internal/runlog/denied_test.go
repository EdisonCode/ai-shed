package runlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestARefusedCommandIsNamedByItsFirstWordsAndNoValue(t *testing.T) {
	for command, want := range map[string]string{
		"gh pr ready 41": "gh pr ready",
		"git push --force-with-lease origin feature/x":      "git push --force-with-lease origin",
		"TOKEN=abc123 curl -H 'Authorization: x' https://a": "curl",
		"cat /home/me/.env && echo done":                    "cat",
		"gh pr merge --auto --squash 41":                    "gh pr merge --auto --squash",
		"psql --password=hunter2":                           "psql",
	} {
		input, _ := json.Marshal(map[string]string{"command": command})
		if got, _ := DenialKind("Bash", input); got != want {
			t.Errorf("%q: kind = %q, want %q", command, got, want)
		}
	}
}

func TestARefusedCommentNamesItsIssue(t *testing.T) {
	input, _ := json.Marshal(map[string]string{"command": "gh issue comment 41 --body-file /tmp/handback.md"})
	if kind, issue := DenialKind("Bash", input); kind != "gh issue comment" || issue != 41 {
		t.Fatalf("kind = %q, issue = %d", kind, issue)
	}
}

func TestACallThatIsNotAShellCommandIsNamedByItsTool(t *testing.T) {
	if kind, _ := DenialKind("WebFetch", json.RawMessage(`{"url":"https://x"}`)); kind != "WebFetch" {
		t.Fatalf("kind = %q", kind)
	}
}

func TestRefusalsAreRecordedAndReadBack(t *testing.T) {
	dir := t.TempDir()
	d := Denial{Time: time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC), Kind: "gh pr ready", Reason: "auto mode blocked it"}
	if err := RecordDenial(dir, "app", d); err != nil {
		t.Fatal(err)
	}
	if err := RecordDenial(dir, "app", d); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, DeniedDir, "app"))
	if err != nil {
		t.Fatal(err)
	}
	data := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(data) != 2 {
		t.Fatalf("lines = %q", data)
	}
	got, err := ParseDenial(data[1])
	if err != nil || got != d {
		t.Fatalf("read back %+v, %v", got, err)
	}
}
