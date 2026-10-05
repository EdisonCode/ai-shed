package probe

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

var twoChecks = []config.Check{{Name: "gh auth", Run: "gh auth status"}, {Name: "claude", Run: "command -v claude"}}

const linuxOutput = `Welcome banner from the init line
@@now 1790000000
@@cores 8
@@load  10:58:01 up 12 days,  3:02,  2 users,  load average: 0.52, 0.40, 0.31
@@disk /dev/nvme0n1p2 487652000 201000000 286652000 42% /home
@@check 0 0
@@detail 0 github.com
@@check 1 127
@@detail 1 sh: claude: not found
@@win|1789999400|work|claude|app-123
@@win|1789990000|work|zsh|my notes
@@heartbeat 1789999990
@@run {"task":"nightly","start":"2026-09-21T03:00:00Z","end":"2026-09-21T03:00:02Z","exit_code":0}
@@run {"task":"nigh
@@checkin {"time":"2026-09-21T03:10:00Z","worker":"app","kind":"idle","verdict":"nudge","reason":"it stopped","message":"Take #12.","sent":true}
@@recycle app
@@end
`

func TestParseLinuxOutput(t *testing.T) {
	res, err := Parse(linuxOutput, twoChecks)
	if err != nil {
		t.Fatal(err)
	}
	if res.Now.Unix() != 1790000000 || res.Cores != 8 || res.Load1 != 0.52 || res.DiskUsedPct != 42 {
		t.Fatalf("basics = %+v", res)
	}
	if !res.Checks[0].OK || res.Checks[1].OK || res.Checks[1].Detail != "sh: claude: not found" {
		t.Fatalf("checks = %+v", res.Checks)
	}
	if res.Heartbeat.Unix() != 1789999990 {
		t.Fatalf("heartbeat = %v", res.Heartbeat)
	}
}

func TestParseKeepsSpacesInWindowNames(t *testing.T) {
	res, err := Parse(linuxOutput, twoChecks)
	if err != nil {
		t.Fatal(err)
	}
	want := Window{Session: "work", Name: "my notes", Command: "zsh", LastActivity: time.Unix(1789990000, 0)}
	if len(res.Windows) != 2 || res.Windows[1] != want {
		t.Fatalf("windows = %+v", res.Windows)
	}
}

func TestParseSkipsHalfWrittenRunRecord(t *testing.T) {
	res, err := Parse(linuxOutput, twoChecks)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Runs) != 1 || res.Runs[0].Task != "nightly" {
		t.Fatalf("runs = %+v", res.Runs)
	}
}

func TestParseMacLoadFormat(t *testing.T) {
	out := "@@now 1790000000\n@@load 10:58  up 3 days,  1:02, 2 users, load averages: 1.83 1.92 2.01\n@@end\n"
	res, err := Parse(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Load1 != 1.83 {
		t.Fatalf("load = %v", res.Load1)
	}
}

func TestParseRejectsTruncatedOutput(t *testing.T) {
	if _, err := Parse("@@now 1790000000\n@@cores 8\n", nil); err == nil {
		t.Fatal("output without the end marker must be an error")
	}
}

func TestParseRejectsUnknownCheckIndex(t *testing.T) {
	if _, err := Parse("@@now 1\n@@check 5 0\n@@end\n", twoChecks); err == nil {
		t.Fatal("a check index outside the config must be an error")
	}
}

func TestScriptQuotesCheckCommands(t *testing.T) {
	script := Script(config.Machine{}, []config.Check{{Name: "q", Run: "test 'a b' = 'a b'"}})
	if !strings.Contains(script, `sh -c 'test '\''a b'\'' = '\''a b'\'''`) {
		t.Fatalf("script does not quote the check:\n%s", script)
	}
}

// The generated script must work in a real shell, not only match our parser.
func TestRunAgainstLocalShell(t *testing.T) {
	m := config.Machine{Name: "here", Host: config.LocalHost, Init: "export SHED_TEST_VALUE=7"}
	checks := []config.Check{
		{Name: "passes", Run: `test "$SHED_TEST_VALUE" = 7`},
		{Name: "fails", Run: `echo "it's broken" >&2; exit 3`},
		{Name: "syntax error", Run: "if then"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := Run(ctx, ShellRunner{}, m, checks)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Checks[0].OK || res.Checks[1].OK || res.Checks[2].OK {
		t.Fatalf("checks = %+v", res.Checks)
	}
	if res.Checks[1].Detail != "it's broken" {
		t.Fatalf("detail = %q", res.Checks[1].Detail)
	}
	if res.Cores < 1 || res.DiskUsedPct < 1 || time.Since(res.Now).Abs() > time.Minute {
		t.Fatalf("basics = %+v", res)
	}
}

func TestParseReadsCheckins(t *testing.T) {
	res, err := Parse(linuxOutput, twoChecks)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Checkins) != 1 || res.Checkins[0].Worker != "app" || !res.Checkins[0].Sent {
		t.Fatalf("check-ins = %+v", res.Checkins)
	}
}

func TestParseReadsRecycleRequests(t *testing.T) {
	res, err := Parse(linuxOutput, twoChecks)
	if err != nil || len(res.Recycle) != 1 || res.Recycle[0] != "app" {
		t.Fatalf("recycle = %v, %v", res.Recycle, err)
	}
}

func TestParseReadsWhatEachWorkersToolReported(t *testing.T) {
	out := "@@now 1790000000\n" +
		`@@mark app {"time":"2026-10-04T12:00:00Z","state":"waiting","detail":"Allow Bash?"}` + "\n" +
		"@@mark docs not json\n@@end\n"
	res, err := Parse(out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := res.Marks["app"]; m.State != runlog.MarkWaiting || m.Detail != "Allow Bash?" || len(res.Marks) != 1 {
		t.Fatalf("marks = %+v", res.Marks)
	}
}
