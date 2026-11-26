package main

import (
	"strings"
	"testing"
)

func TestTheAuditKnowsEveryCommandTheUsageNames(t *testing.T) {
	aliases := map[string]bool{"ui": true} // shed observe's other name
	for _, c := range shedCommands {
		if !aliases[c] && !strings.Contains(usage, "shed "+c) {
			t.Errorf("shedCommands has %q, which the usage does not name", c)
		}
	}
	for _, line := range strings.Split(usage, "\n") {
		f := strings.Fields(line)
		if strings.HasPrefix(line, "  shed ") && len(f) > 1 && !strings.HasPrefix(f[1], "-") {
			found := false
			for _, c := range shedCommands {
				found = found || c == f[1]
			}
			if !found {
				t.Errorf("the usage names shed %s, which shedCommands lacks", f[1])
			}
		}
	}
}
