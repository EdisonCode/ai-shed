package config

import (
	"strings"
	"testing"
	"time"
)

const minimal = `
machines:
  - name: box
    host: local
`

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load("../../shed.example.yaml")
	if err != nil {
		t.Fatalf("example config must stay valid: %v", err)
	}
	if len(cfg.Machines) != 2 {
		t.Fatalf("machines = %d, want 2", len(cfg.Machines))
	}
}

func TestSignalsDefaultWhenOmitted(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Signals) != len(DefaultSignals()) {
		t.Fatalf("signals = %d, want the defaults", len(cfg.Signals))
	}
}

func TestInvalidConfigIsRejected(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"no machines", "machines: []", "no machines"},
		{"unknown field", minimal + "    hots: x\n", "hots"},
		{"missing host", "machines:\n  - name: box\n", "host is required"},
		{"bad machine name", "machines:\n  - name: My Box\n    host: local\n", "must match"},
		{"duplicate machine", minimal + "  - name: box\n    host: local\n", "duplicate name"},
		{"bad schedule", minimal + "    tasks:\n      - {name: t, schedule: 'every day', run: 'true'}\n", "schedule"},
		{"task without run", minimal + "    tasks:\n      - {name: t, schedule: '@daily'}\n", "no run command"},
		{"duplicate task", minimal + "    tasks:\n      - {name: t, schedule: '@daily', run: 'true'}\n      - {name: t, schedule: '@daily', run: 'true'}\n", "duplicate task"},
		{"bad timeout", minimal + "    tasks:\n      - {name: t, schedule: '@daily', run: 'true', timeout: soon}\n", "timeout"},
		{"bad repo", minimal + "    issues:\n      - {repo: app, label: x}\n", "owner/name"},
		{"issue source without filter", minimal + "    issues:\n      - {repo: org/app}\n", "label or an assignee"},
		{"check without run", minimal + "    checks:\n      - {name: c}\n", "name and run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestAllProblemsAreReportedTogether(t *testing.T) {
	_, err := Parse([]byte("machines:\n  - name: a\n  - name: b\n"))
	if err == nil || strings.Count(err.Error(), "host is required") != 2 {
		t.Fatalf("error = %v, want both missing hosts", err)
	}
}

func TestChecksForPutsDefaultsFirst(t *testing.T) {
	cfg, err := Parse([]byte(`
defaults:
  checks: [{name: shared, run: "true"}]
machines:
  - name: box
    host: local
    checks: [{name: own, run: "true"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.ChecksFor(cfg.Machines[0])
	if len(got) != 2 || got[0].Name != "shared" || got[1].Name != "own" {
		t.Fatalf("checks = %+v", got)
	}
}

func TestCommandPrependsInit(t *testing.T) {
	m := Machine{Init: "export A=1"}
	if got := m.Command("echo $A"); got != "export A=1\necho $A" {
		t.Fatalf("command = %q", got)
	}
}

func TestTimeoutOrDefault(t *testing.T) {
	if got := (Task{Timeout: "30m"}).TimeoutOrDefault(); got != 30*time.Minute {
		t.Fatalf("timeout = %v", got)
	}
	if got := (Task{}).TimeoutOrDefault(); got != DefaultTaskTimeout {
		t.Fatalf("default timeout = %v", got)
	}
}
