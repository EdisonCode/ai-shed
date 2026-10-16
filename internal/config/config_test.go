package config

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/pricing"
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
		{"issue source without filter", minimal + "    issues:\n      - {repo: org/app}\n", "needs a label, an assignee or a query"},
		{"check without run", minimal + "    checks:\n      - {name: c}\n", "name and run"},
		{"worker without dir", minimal + "    workers:\n      - {name: w, brief: do it}\n", "has no dir"},
		{"worker without brief", minimal + "    workers:\n      - {name: w, dir: /tmp}\n", "has no brief"},
		{"duplicate worker", minimal + "    workers:\n      - {name: w, dir: /tmp, brief: a}\n      - {name: w, dir: /tmp, brief: b}\n", "duplicate worker"},
		{"bad cache ttl", minimal + "supervisor: {cache_ttl: long}\n", "cache_ttl"},
		{"model command without placeholder", minimal + "supervisor: {model_command: /model}\n", "must contain {model}"},
		{"worker issue source without filter", minimal + "    workers:\n      - {name: w, dir: /tmp, brief: b, issues: [{repo: org/app}]}\n", `worker "w": issue source org/app needs a label`},
		{"negative max_load", minimal + "    capacity: {max_load: -1}\n", "max_load"},
		{"bad max_wait", minimal + "    capacity: {max_wait: later}\n", "max_wait"},
		{"bad release_after", minimal + "    capacity: {max_load: 1.5, release_after: soon}\n", "capacity.release_after \"soon\""},
		{"release_after with no test for busy", minimal + "    capacity: {release_after: 30m}\n", "needs max_load or busy_when"},
		{"release_after with no overflow", minimal + "    capacity: {max_load: 1.5, release_after: 30m}\n", "needs an account with overflow"},
		{"nice above 19", minimal + "    capacity: {nice: 20}\n", "capacity.nice 20"},
		{"negative nice", minimal + "    capacity: {nice: -5}\n", "capacity.nice -5"},
		{"bad models.apply", minimal + "supervisor: {models: {apply: maybe}}\n", "models.apply"},
		{"repo without a staging commit", minimal + "repos: [{repo: org/app, production: {commit: x}}]\n", "staging.commit is required"},
		{"duplicate repo", minimal + "repos: [{repo: org/app, staging: {commit: x}}, {repo: org/app, staging: {commit: x}}]\n", "duplicate entry"},
		{"bad look_back", minimal + "repos: [{repo: org/app, staging: {commit: x}, look_back: 14d}]\n", "look_back"},
		{"viewport that is not a size", minimal + "repos: [{repo: org/app, staging: {commit: x, viewport: wide}}]\n", "staging.viewport \"wide\" is not a size"},
		{"production with a viewport", minimal + "repos: [{repo: org/app, staging: {commit: x}, production: {commit: y, viewport: 1440x900}}]\n", "production takes only commit"},
		{"production with a url", minimal + "repos: [{repo: org/app, staging: {commit: x}, production: {commit: y, url: https://example.com}}]\n", "production takes only commit"},
		{"eye checks without a repo entry", minimal + "    issues: [{repo: org/app, label: x}]\n    workers:\n      - {name: w, dir: /tmp, brief: b, eye_checks: true}\n", "has no entry under repos"},
		{"eye checks without a failure phrase", minimal + "    issues: [{repo: org/app, label: x}]\n    workers:\n      - {name: w, dir: /tmp, brief: b, eye_checks: true}\nrepos: [{repo: org/app, staging: {commit: x}}]\nsignals: [{name: eyes, ask: \"Needs eyes:\", answered_by: Eyes checked}]\n", "answered_by and failed_by"},
		{"bad when_cold", minimal + "supervisor: {when_cold: maybe}\n", "when_cold"},
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

func TestSupervisorDefaults(t *testing.T) {
	var s Supervisor
	if s.CacheTTLOrDefault() != DefaultCacheTTL || s.ScopeEveryOrDefault() != DefaultScopeEvery || !s.ClearWhenCold() {
		t.Fatalf("defaults = %v, %v, clear %v", s.CacheTTLOrDefault(), s.ScopeEveryOrDefault(), s.ClearWhenCold())
	}
}

func TestSupervisorSettingsAreRead(t *testing.T) {
	cfg, err := Parse([]byte(minimal + "supervisor: {cache_ttl: 1h, scope_every: 10m, when_cold: resume}\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Supervisor
	if s.CacheTTLOrDefault() != time.Hour || s.ScopeEveryOrDefault() != 10*time.Minute || s.ClearWhenCold() {
		t.Fatalf("settings = %v, %v, clear %v", s.CacheTTLOrDefault(), s.ScopeEveryOrDefault(), s.ClearWhenCold())
	}
}

func TestModelIsPickedFromIssueLabels(t *testing.T) {
	m := Models{Default: "sonnet", Labels: map[string]string{"model:opus": "opus", "model:fable": "fable"}}
	cases := []struct {
		labels []string
		want   string
	}{
		{nil, "sonnet"},
		{[]string{"bug", "machine:box"}, "sonnet"},
		{[]string{"bug", "model:opus"}, "opus"},
		{[]string{"model:fable"}, "fable"},
	}
	for _, tc := range cases {
		if got := m.For(tc.labels); got != tc.want {
			t.Errorf("For(%v) = %q, want %q", tc.labels, got, tc.want)
		}
	}
	if got := (Models{}).For([]string{"model:opus"}); got != "" {
		t.Errorf("with no models configured For = %q, want none", got)
	}
}

func TestModelCommand(t *testing.T) {
	if got := (Supervisor{}).ModelCommandFor("opus"); got != "/model opus" {
		t.Fatalf("default command = %q", got)
	}
	if got := (Supervisor{ModelCommand: "use {model} now"}).ModelCommandFor("opus"); got != "use opus now" {
		t.Fatalf("custom command = %q", got)
	}
}

func TestPriorityRank(t *testing.T) {
	src := IssueSource{Priority: []string{"p0", "p1"}}
	for want, labels := range [][]string{{"bug", "p0"}, {"p1"}, {"bug"}} {
		if got := src.Rank(labels); got != want {
			t.Errorf("Rank(%v) = %d, want %d", labels, got, want)
		}
	}
	if got := (IssueSource{}).Rank([]string{"p0"}); got != 0 {
		t.Errorf("with no priority configured every issue ranks the same; got %d", got)
	}
}

func TestWorkerQueueIsItsOwnOrTheMachines(t *testing.T) {
	shared := []IssueSource{{Repo: "org/app", Label: "machine:box"}}
	own := []IssueSource{{Repo: "org/app", Label: "area:billing"}}
	m := Machine{Issues: shared, Workers: []Worker{{Name: "a"}, {Name: "b", Issues: own}, {Name: "c", Issues: own}}}
	if got := m.SourcesFor(m.Workers[0]); got[0].Label != "machine:box" {
		t.Errorf("worker with no queue of its own: %+v", got)
	}
	if got := m.SourcesFor(m.Workers[1]); got[0].Label != "area:billing" {
		t.Errorf("worker with its own queue: %+v", got)
	}
	if got := m.AllSources(); len(got) != 2 {
		t.Errorf("all sources = %+v, want the machine's and the shared worker source once each", got)
	}
}

func TestHandBackPhrase(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"omitted: the default", minimal, DefaultHandBack},
		{"set", minimal + "handback: Worker report\n", "Worker report"},
		{"empty: an ask counts in any comment", minimal + "handback: \"\"\n", ""},
		{"applies to the owner's own signals", minimal + "handback: Report\nsignals: [{name: q, ask: \"Question:\"}]\n", "Report"},
	}
	for _, tc := range cases {
		cfg, err := Parse([]byte(tc.yaml))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for _, s := range cfg.Signals {
			if s.HandBack != tc.want {
				t.Errorf("%s: signal %s counts in comments with %q, want %q", tc.name, s.Name, s.HandBack, tc.want)
			}
		}
	}
}

func TestCapacity(t *testing.T) {
	if (Capacity{}).Set() {
		t.Error("a machine with no test for busy must always have room")
	}
	if !(Capacity{MaxLoad: 1.5}).Set() || !(Capacity{BusyWhen: "pgrep x"}).Set() {
		t.Error("either test makes the capacity set")
	}
	if got := (Capacity{}).MaxWaitOrDefault(); got != DefaultMaxWait {
		t.Errorf("default max wait = %v", got)
	}
	if got := (Capacity{MaxWait: "5m"}).MaxWaitOrDefault(); got != 5*time.Minute {
		t.Errorf("max wait = %v", got)
	}
	if (Capacity{Nice: 10}).Set() {
		t.Error("a priority is no test for busy")
	}
	if got := (Capacity{}).NicePrefix(); got != "" {
		t.Errorf("no nice: prefix = %q", got)
	}
	if got := (Capacity{Nice: 10}).NicePrefix(); got != "nice -n 10 " {
		t.Errorf("nice 10: prefix = %q", got)
	}
}

func TestRepoDeploysAndEyeCheckWorkers(t *testing.T) {
	cfg, err := Parse([]byte(minimal + `    issues: [{repo: org/app, label: x}]
    workers:
      - {name: w, dir: /tmp, brief: b, eye_checks: true}
repos:
  - repo: org/app
    staging: {commit: "echo abc1234", url: "https://staging.example.com", orders: "Read only.", viewport: 1440x900}
    production: {commit: "echo def5678"}
`))
	if err != nil {
		t.Fatal(err)
	}
	repo, ok := cfg.Repo("org/app")
	if !ok || repo.Staging.URL != "https://staging.example.com" || repo.Staging.Viewport != "1440x900" || repo.LookBackOrDefault() != DefaultLookBack {
		t.Fatalf("repo = %+v", repo)
	}
	if !cfg.Machines[0].Workers[0].EyeChecks {
		t.Fatal("the worker does not do eye checks")
	}
}

func TestEyeCheckReposAreTheDeployedReposAWorkerLooksAt(t *testing.T) {
	cfg, err := Parse([]byte(minimal + `    issues: [{repo: org/app, label: x}, {repo: org/api, label: x}, {repo: org/web, label: x}]
    workers:
      - {name: looker, dir: /tmp, brief: b, eye_checks: true, issues: [{repo: org/app, label: x}, {repo: org/api, label: x}]}
      - {name: builder, dir: /tmp, brief: b, issues: [{repo: org/web, label: x}]}
repos:
  - {repo: org/app, staging: {commit: "echo abc1234"}}
  - {repo: org/api, gates: {command: make test}}
  - {repo: org/web, staging: {commit: "echo abc1234"}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.EyeCheckRepos(); !slices.Equal(got, []string{"org/app"}) {
		t.Fatalf("eye check repos = %v, want org/app: org/api is not on staging, and nobody looks at org/web", got)
	}
}

func TestEyesSignalOfAnOlderFleetFileGetsThePhrasesForABlockedLook(t *testing.T) {
	cfg, err := Parse([]byte(minimal + "signals: [{name: eyes, ask: \"Needs eyes:\", answered_by: Eyes checked, failed_by: Eyes failed, unblocked_by: Try again}, {name: decision, ask: \"Decisions needed:\"}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if eyes := cfg.Signals[0]; eyes.BlockedBy != DefaultBlockedBy || eyes.UnblockedBy != "Try again" {
		t.Fatalf("eyes = %+v", eyes)
	}
	if cfg.Signals[1].BlockedBy != "" {
		t.Fatalf("decision = %+v; only an eye check can be blocked", cfg.Signals[1])
	}
}

func TestDecisionSignalGetsItsDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal + "signals: [{name: decision, ask: \"Decisions needed:\", grace: 10m}, {name: eyes, ask: \"Needs eyes:\"}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.Signals[0]; d.GraceOrDefault() != 10*time.Minute || d.Risky != DefaultRisky || d.Decided != DefaultDecided {
		t.Fatalf("decision = %+v", d)
	}
	if e := cfg.Signals[1]; e.GraceOrDefault() != 0 || e.Risky != "" {
		t.Fatalf("eyes = %+v; only a decision has a grace period", e)
	}
	if d := DefaultSignals()[0]; d.GraceOrDefault() != DefaultGrace || d.Decided != DefaultDecided {
		t.Fatalf("default decision = %+v", d)
	}
}

func TestGraceMustBeADuration(t *testing.T) {
	_, err := Parse([]byte(minimal + "signals: [{name: decision, ask: \"Decisions needed:\", grace: soon}]\n"))
	if err == nil || !strings.Contains(err.Error(), "grace") {
		t.Fatalf("error = %v", err)
	}
}

const withBackfillSource = `machines:
  - name: box
    host: local
    issues:
      - {repo: org/app, label: "machine:box"}
      - {repo: org/app, query: "label:p1 no:assignee", backfill: true}
  - name: other
    host: local
    issues: [{repo: org/app, label: "machine:other"}]
`

func TestBackfillSourceIsAReserveBesideTheQueue(t *testing.T) {
	cfg, err := Parse([]byte(withBackfillSource + "signals: [{name: decision, ask: \"Decisions needed:\"}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	m, w := cfg.Machines[0], Worker{}
	if q := m.SourcesFor(w); len(q) != 1 || q[0].Label != "machine:box" || len(m.AllSources()) != 1 {
		t.Fatalf("queue = %+v, all = %+v; a backfill source is in neither", q, m.AllSources())
	}
	if b := m.BackfillFor(w); len(b) != 1 || b[0].Query != "label:p1 no:assignee" || m.ClaimLabel(w, "org/app") != "machine:box" {
		t.Fatalf("backfill = %+v, claim label = %q", b, m.ClaimLabel(w, "org/app"))
	}
	if got := cfg.QueueLabels(); !slices.Equal(got, []string{"machine:box", "machine:other"}) {
		t.Fatalf("queue labels = %v", got)
	}
	if last := cfg.Signals[len(cfg.Signals)-1]; last.Name != ReadySignal {
		t.Fatalf("signals = %+v; a fleet with a backfill source needs the ready signal", cfg.Signals)
	}
}

func TestBackfillSourceNeedsALabelToMarkWhatItTakes(t *testing.T) {
	_, err := Parse([]byte("machines:\n  - name: box\n    host: local\n    issues: [{repo: org/app, query: \"label:p1\", backfill: true}]\n"))
	if err == nil || !strings.Contains(err.Error(), "backfill source org/app needs a source with a label") {
		t.Fatalf("error = %v", err)
	}
}

func TestAskWorkerTakesNoWork(t *testing.T) {
	base := "machines:\n  - name: box\n    host: local\n    issues:\n      - {repo: org/app, label: x}\n    workers:\n"
	for field, want := range map[string]string{
		"issues: [{repo: org/app, label: y}]": "answers questions and has no queue: remove issues",
		"eye_checks: true":                    "answers questions and does no eye checks",
		"fresh_per_issue: true":               "answers questions and is handed no issue: remove fresh_per_issue",
	} {
		_, err := Parse([]byte(base + "      - {name: ask, dir: /tmp, brief: b, ask: true, " + field + "}\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", field, err, want)
		}
	}

	c, err := Parse([]byte(base + "      - {name: app, dir: /tmp, brief: b}\n      - {name: ask, dir: /tmp, brief: b, ask: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := c.Machines[0]
	if got := m.SourcesFor(m.Workers[1]); len(got) != 0 {
		t.Errorf("an ask worker's queue = %v, want none: it must not take the machine's", got)
	}
	if got := m.Builders(); len(got) != 1 || got[0].Name != "app" {
		t.Errorf("builders = %v, want only app", got)
	}
	if err := m.TakesWork("app"); err != nil {
		t.Errorf("app: %v", err)
	}
	if err := m.TakesWork("ask"); err == nil || !strings.Contains(err.Error(), "answers questions and is handed no work") {
		t.Errorf("ask: err = %v", err)
	}
	if err := m.TakesWork("nobody"); err == nil || !strings.Contains(err.Error(), `has no worker "nobody"`) {
		t.Errorf("nobody: err = %v", err)
	}
}

func TestFleetFileRatesReplaceThePublishedOnes(t *testing.T) {
	cfg, err := Parse([]byte("pricing:\n  claude-opus-5-5: {input: 1, cache_write_5m: 1, cache_write_1h: 1, cache_read: 1, output: 2}\nmachines:\n  - name: box\n    host: local\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := cfg.Prices().Cost("claude-opus-5-5", pricing.Tokens{Output: 1_000_000}); !ok || got != 2 {
		t.Fatalf("opus-5-5 output = %v (%v), want the fleet file's 2", got, ok)
	}
}

func TestApplyCanBeSetPerLabel(t *testing.T) {
	m := Models{Default: "opus", Labels: map[string]string{"model:sonnet": "sonnet"}, Apply: ModelTell,
		ApplyLabels: map[string]string{"model:sonnet": ModelSwitch}, Session: "opus"}
	if got := m.ApplyFor([]string{"bug", "model:sonnet"}); got != ModelSwitch {
		t.Errorf("model:sonnet applies %q, want switch", got)
	}
	if got := m.ApplyFor([]string{"bug"}); got != ModelTell {
		t.Errorf("an unlabelled issue applies %q, want the default tell", got)
	}
}

func TestMixedApplyNeedsTheSessionModel(t *testing.T) {
	base := "machines:\n  - name: box\n    host: local\nsupervisor:\n  models:\n    default: opus\n    apply: tell\n    labels: {model:sonnet: sonnet}\n    apply_labels: {model:sonnet: switch}\n"
	if _, err := Parse([]byte(base)); err == nil || !strings.Contains(err.Error(), "models.session") {
		t.Fatalf("err = %v, want models.session to be asked for", err)
	}
	if _, err := Parse([]byte(base + "    session: opus\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(strings.Replace(base, "{model:sonnet: switch}", "{model:sonnet: maybe}", 1) + "    session: opus\n")); err == nil {
		t.Fatal("an apply_labels value that is neither switch nor tell was accepted")
	}
}

const twoMachines = "machines:\n  - name: box\n    host: local\n  - name: mini\n    host: me@mini\n"

func TestAnAccountSharesItsWeeklyBudgetAmongItsMachines(t *testing.T) {
	cfg, err := Parse([]byte(twoMachines + "accounts:\n  - name: max\n    machines: [box, mini]\n    weekly_budget: 400\n    resets: Wed 18:00\n"))
	if err != nil {
		t.Fatal(err)
	}
	a, ok := cfg.AccountOf("mini")
	if !ok || a.Name != "max" || a.Share() != 200 || a.HoldAllButOrDefault() != 1 {
		t.Fatalf("account = %+v, %v", a, ok)
	}
	if _, ok := cfg.AccountOf("pc"); ok {
		t.Fatal("a machine in no account has one")
	}
}

func TestTheWeekStartsAtTheLastReset(t *testing.T) {
	a := Account{Resets: "Wed 18:00"}
	loc := time.FixedZone("PDT", -7*60*60)
	for now, want := range map[time.Time]time.Time{
		time.Date(2026, 10, 12, 9, 0, 0, 0, loc):  time.Date(2026, 10, 7, 18, 0, 0, 0, loc),  // Monday
		time.Date(2026, 10, 14, 17, 0, 0, 0, loc): time.Date(2026, 10, 7, 18, 0, 0, 0, loc),  // Wednesday, before
		time.Date(2026, 10, 14, 19, 0, 0, 0, loc): time.Date(2026, 10, 14, 18, 0, 0, 0, loc), // Wednesday, after
	} {
		if got := a.WeekStart(now); !got.Equal(want) {
			t.Errorf("week of %s starts %s, want %s", now, got, want)
		}
	}
}

func TestAccountsAreChecked(t *testing.T) {
	for name, body := range map[string]string{
		"unknown machine": "accounts:\n  - name: a\n    machines: [pc]\n    weekly_budget: 1\n    resets: Wed 18:00\n",
		"no budget":       "accounts:\n  - name: a\n    machines: [box]\n    resets: Wed 18:00\n",
		"bad reset":       "accounts:\n  - name: a\n    machines: [box]\n    weekly_budget: 1\n    resets: someday\n",
		"in two accounts": "accounts:\n  - name: a\n    machines: [box]\n    weekly_budget: 1\n    resets: Wed 18:00\n  - name: b\n    machines: [box]\n    weekly_budget: 1\n    resets: Wed 18:00\n",
	} {
		if _, err := Parse([]byte(twoMachines + body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestARepoMayBeListedForItsGatesAlone(t *testing.T) {
	cfg, err := Parse([]byte(minimal + "repos:\n  - repo: org/app\n    gates: {command: make lint test, timeout: 20m}\n"))
	if err != nil {
		t.Fatalf("a repo with gates and no staging: %v", err)
	}
	repo, _ := cfg.Repo("org/app")
	if repo.Deployed() || repo.Gates.TimeoutOrDefault() != 20*time.Minute {
		t.Fatalf("repo = %+v", repo)
	}
}

func TestARepoNeedsStagingOrGatesAndAGatesTimeoutIsADuration(t *testing.T) {
	for _, tc := range []struct{ repo, want string }{
		{"{repo: org/app}", "give it staging, gates or verify"},
		{"{repo: org/app, gates: {command: make, timeout: soon}}", "gates.timeout"},
		{"{repo: org/app, gates: {timeout: 5m}, staging: {commit: echo x}}", "gates.timeout needs gates.command"},
		{"{repo: org/app, staging: {url: https://s.example.com}}", "staging.commit is required"},
	} {
		_, err := Parse([]byte(minimal + "repos: [" + tc.repo + "]\n"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.repo, err, tc.want)
		}
	}
}

func TestAnEyeCheckModelNeedsAModelToGoBackTo(t *testing.T) {
	for _, tc := range []struct{ models, want string }{
		{"{eye_checks: haiku}", "models.eye_checks needs models.default"},
		{"{eye_checks: haiku, default: opus, apply: tell}", "models.eye_checks needs models.session"},
	} {
		_, err := Parse([]byte(minimal + "supervisor: {models: " + tc.models + "}\n"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.models, err, tc.want)
		}
	}
	if _, err := Parse([]byte(minimal + "supervisor: {models: {eye_checks: haiku, default: opus}}\n")); err != nil {
		t.Fatalf("eye_checks with a default: %v", err)
	}
}

func TestAWorkerEnvNeedsVariableNames(t *testing.T) {
	_, err := Parse([]byte(minimal + "    workers: [{name: w, dir: /tmp, brief: b, env: {\"BAD NAME\": x}}]\n"))
	if err == nil || !strings.Contains(err.Error(), "env") {
		t.Fatalf("err = %v", err)
	}
}

func TestAStagingDeployNeedsAMachineThatDeploys(t *testing.T) {
	for _, tc := range []struct{ staging, want string }{
		{"{commit: echo x, deploy: make deploy}", "needs staging.deploy_from"},
		{"{commit: echo x, deploy_from: box}", "need staging.deploy"},
		{"{commit: echo x, deploy: make deploy, deploy_from: elsewhere}", `deploy_from "elsewhere" is not a machine`},
		{"{commit: echo x, deploy: make deploy, deploy_from: box, deploy_when: {or_after: soon}}", "or_after"},
	} {
		_, err := Parse([]byte(minimal + "repos: [{repo: org/app, staging: " + tc.staging + "}]\n"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.staging, err, tc.want)
		}
	}
	cfg, err := Parse([]byte(minimal + "repos: [{repo: org/app, staging: {commit: echo x, deploy: make deploy, deploy_from: box}}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.Repos[0].Staging.DeployWhen; w.WaitingLooksOrDefault() != 5 || w.OrAfterOrDefault() != 4*time.Hour {
		t.Fatalf("deploy_when defaults = %d, %s", w.WaitingLooksOrDefault(), w.OrAfterOrDefault())
	}
}

func TestVerifyNeedsAVerifierThatTakesTheRepository(t *testing.T) {
	repo := "repos: [{repo: org/app, verify: {areas: [{name: billing, paths: [internal/billing/]}]}}]\n"
	_, err := Parse([]byte(minimal + "    issues: [{repo: org/app, label: x}]\n    workers: [{name: w, dir: /tmp, brief: b}]\n" + repo))
	if err == nil || !strings.Contains(err.Error(), "verify needs a worker with verifies: true") {
		t.Fatalf("err = %v", err)
	}
	cfg, err := Parse([]byte(minimal + "    issues: [{repo: org/app, label: x}]\n    workers: [{name: w, dir: /tmp, brief: b, verifies: true}]\n" + repo))
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := cfg.Repo("org/app"); r.Verify == nil || r.Verify.HighLinesOrDefault() != DefaultHighLines {
		t.Fatalf("repo = %+v", r)
	}
	_, err = Parse([]byte(minimal + "    issues: [{repo: org/app, label: x}]\n    workers: [{name: w, dir: /tmp, brief: b, verifies: true}]\n"))
	if err == nil || !strings.Contains(err.Error(), "no repository of its queue has verify") {
		t.Fatalf("a verifier with nothing to verify: err = %v", err)
	}
	_, err = Parse([]byte(minimal + "supervisor: {models: {verify: {medium: x}, default: opus}}\n"))
	if err == nil || !strings.Contains(err.Error(), `models.verify "medium"`) {
		t.Fatalf("a tier that does not exist: err = %v", err)
	}
}

func TestAMergeTrainNeedsMergeLow(t *testing.T) {
	queue := minimal + "    issues: [{repo: org/app, label: x}]\n    workers: [{name: w, dir: /tmp, brief: b, verifies: true}]\n"
	_, err := Parse([]byte(queue + "repos: [{repo: org/app, verify: {merge_train: true}}]\n"))
	if err == nil || !strings.Contains(err.Error(), "turn on merge_low too") {
		t.Fatalf("err = %v", err)
	}
	cfg, err := Parse([]byte(queue + "repos: [{repo: org/app, verify: {merge_low: true, merge_train: true}}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := cfg.Repo("org/app"); r.Verify.MergeTrain != TrainOne || r.Batches() {
		t.Fatalf("repo = %+v", r)
	}
}

func TestABatchTrainTakesItsSizeAndThePathsThatGoAlone(t *testing.T) {
	queue := minimal + "    issues: [{repo: org/app, label: x}]\n    workers: [{name: w, dir: /tmp, brief: b, verifies: true}]\n"
	cfg, err := Parse([]byte(queue + "repos: [{repo: org/app, verify: {merge_low: true, merge_train: batch, merge_train_alone: [db/migrations]}}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := cfg.Repo("org/app")
	if !r.Batches() || r.Verify.BatchSizeOrDefault() != DefaultBatchSize || len(r.Verify.MergeTrainAlone) != 1 {
		t.Fatalf("repo = %+v", r.Verify)
	}
	for yaml, want := range map[string]string{
		"{merge_low: true, merge_train: fast}":                  "must be true, false or batch",
		"{merge_low: true, merge_train: true, batch_size: 3}":   "are for merge_train: batch",
		"{merge_low: true, merge_train: batch, batch_size: -1}": "must be 1 or more",
	} {
		_, err := Parse([]byte(queue + "repos: [{repo: org/app, verify: " + yaml + "}]\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", yaml, err, want)
		}
	}
}

func TestOverflowNeedsAMachineThatTakesItAsBackfill(t *testing.T) {
	account := "accounts: [{name: max-1, machines: [box], overflow: \"fleet:ready\"}]\n"
	_, err := Parse([]byte(minimal + "    issues: [{repo: org/app, label: \"machine:box\"}]\n" + account))
	if err == nil || !strings.Contains(err.Error(), "nothing would take a released issue") {
		t.Fatalf("err = %v", err)
	}
	_, err = Parse([]byte(minimal + "    issues: [{repo: org/app, label: \"fleet:ready\"}]\n" + account))
	if err == nil || !strings.Contains(err.Error(), "list it with backfill: true") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Parse([]byte(minimal + "    issues: [{repo: org/app, label: \"machine:box\"}, {repo: org/app, label: \"fleet:ready\", backfill: true}]\n" + account)); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseAfterWithAnOverflowAccountLoads(t *testing.T) {
	account := "accounts: [{name: max-1, machines: [box], overflow: \"fleet:ready\"}]\n"
	cfg, err := Parse([]byte(minimal + "    capacity: {max_load: 1.5, release_after: 30m}\n    issues: [{repo: org/app, label: \"machine:box\"}, {repo: org/app, label: \"fleet:ready\", backfill: true}]\n" + account))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Machines[0].Capacity.ReleaseAfterOrZero(); got != 30*time.Minute {
		t.Fatalf("release_after = %v", got)
	}
}
