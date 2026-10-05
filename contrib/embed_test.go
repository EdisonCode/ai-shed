package contrib

import (
	"strings"
	"testing"
)

func TestWorkerIssueTemplateIsOneGitHubAccepts(t *testing.T) {
	text := string(WorkerIssue)
	front, body, found := strings.Cut(strings.TrimPrefix(text, "---\n"), "\n---\n")
	if !strings.HasPrefix(text, "---\n") || !found {
		t.Fatalf("the template has no front matter:\n%s", text)
	}
	for _, key := range []string{"name: ", "about: "} {
		if !strings.Contains(front, key) {
			t.Errorf("front matter lacks %q, which GitHub needs to list the template", key)
		}
	}
	for _, section := range []string{"## Goal", "## Where", "## Acceptance", "## Out of scope", "## Decided already", "## Needs eyes"} {
		if !strings.Contains(body, section) {
			t.Errorf("the template lacks the section %q", section)
		}
	}
}
