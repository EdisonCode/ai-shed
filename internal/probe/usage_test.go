package probe

import (
	"testing"
)

const usageOutput = `@@now 1790000000
@@pane 100 app
@@pane 200 web
@@pane 999 gone
@@ps     1     0   0.0   1200 /sbin/launchd
@@ps   100     1   0.0   2000 -zsh
@@ps   101   100  95.5 400000 /usr/local/bin/claude --permission-mode auto
@@ps   102   101 180.0 6000000 /usr/local/bin/node /repo/node_modules/.bin/vitest run
@@ps   103   102  50.0 900000 node worker.js
@@ps   200     1   0.0   2000 -zsh
@@ps   201   200   1.0 300000 claude
@@ps   300     1 300.0 9000000 Runner.Worker
@@end
`

func TestEachWorkersProcessTreeIsAddedUp(t *testing.T) {
	res, err := Parse(usageOutput, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := res.Usage["app"]
	if app.CPU != 325.5 || app.MemKB != 7302000 {
		t.Errorf("app = %.1f%% %d KB, want its tree only: 325.5%% 7302000 KB", app.CPU, app.MemKB)
	}
	if app.Top.PID != 102 || ShortCommand(app.Top.Command) != "node vitest" {
		t.Errorf("app's largest process = %+v, want the vitest run", app.Top)
	}
	if web := res.Usage["web"]; web.MemKB != 302000 || web.Top.PID != 201 {
		t.Errorf("web = %+v", web)
	}
	if _, ok := res.Usage["gone"]; ok {
		t.Error("a window whose pane is gone has no usage")
	}
}

func TestAMachineWithNoWorkerSessionHasNoUsage(t *testing.T) {
	res, err := Parse("@@now 1790000000\n@@end\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage != nil {
		t.Errorf("usage = %v, want none", res.Usage)
	}
}

func TestShortCommand(t *testing.T) {
	cases := map[string]string{
		"/usr/local/bin/node /repo/node_modules/.bin/vitest run": "node vitest",
		"claude --permission-mode auto":                          "claude",
		"go test -p 6 ./...":                                     "go test",
		"":                                                       "",
	}
	for in, want := range cases {
		if got := ShortCommand(in); got != want {
			t.Errorf("ShortCommand(%q) = %q, want %q", in, got, want)
		}
	}
}
