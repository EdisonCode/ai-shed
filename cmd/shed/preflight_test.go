package main

import (
	"strings"
	"testing"
)

func TestDeployNamesEachPermissionRuleThatOnlySomeMachinesHave(t *testing.T) {
	got := ruleDifferences(map[string][]string{
		"box":  {"allow Bash(gh issue comment:*)", "allow Bash(gh pr ready:*)", "allow Bash(gh pr merge --auto:*)"},
		"mini": {"allow Bash(gh issue comment:*)", "allow Bash(gh pr ready:*)"},
	})
	want := []string{"allow Bash(gh pr merge --auto:*): on box; not on mini"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("differences = %q, want %q", got, want)
	}
}

func TestOneMachineHasNothingToCompare(t *testing.T) {
	if got := ruleDifferences(map[string][]string{"box": {"allow Bash(gh pr ready:*)"}}); len(got) != 0 {
		t.Fatalf("differences = %q", got)
	}
}
