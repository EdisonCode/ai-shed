package supervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

// pfTerminal is a terminal whose windows behave as a started command would:
// a new window holds a shell, and the command takes it over when typed.
type pfTerminal struct {
	windows map[string]Observation
	// command and printsFor describe the program once it is started: what
	// the pane runs, and for how long it keeps printing.
	command   string
	printsFor time.Duration
	screen    string
	started   time.Time
	now       time.Time

	opened, sent, closed []string
}

func (p *pfTerminal) Observe(window string) (Observation, error) {
	obs := p.windows[window]
	if obs.Exists && strings.HasPrefix(window, preflightPrefix) && !p.started.IsZero() {
		obs.Command = p.command
		obs.LastActivity = p.started.Add(min(p.now.Sub(p.started), p.printsFor))
	}
	return obs, nil
}
func (p *pfTerminal) Capture(string, int) (string, error) { return p.screen, nil }
func (p *pfTerminal) Open(window, _ string) error {
	p.opened = append(p.opened, window)
	p.windows[window] = Observation{Exists: true, Command: "zsh", LastActivity: p.now}
	return nil
}
func (p *pfTerminal) Send(_, text string) error {
	p.sent = append(p.sent, text)
	p.started = p.now
	return nil
}
func (p *pfTerminal) Close(window string) error {
	p.closed = append(p.closed, window)
	delete(p.windows, window)
	return nil
}

func (p *pfTerminal) run(w config.Worker) PreflightResult {
	return Preflight(p, w, func() time.Time { return p.now }, func(d time.Duration) { p.now = p.now.Add(d) })
}

func newPF(command string) *pfTerminal {
	return &pfTerminal{windows: map[string]Observation{}, command: command, printsFor: 3 * time.Second, screen: "> ", now: t0}
}

func pfWorker(t *testing.T) config.Worker {
	return config.Worker{Name: "app", Dir: t.TempDir(), Command: "claude --permission-mode auto", Brief: "b"}
}

func TestPreflightStartsTheCommandWithNoPromptAndCleansUp(t *testing.T) {
	p := newPF("claude")
	res := p.run(pfWorker(t))

	if res.State != PreflightCaptured || res.Exited || res.Screen != "> " {
		t.Fatalf("result = %+v", res)
	}
	if !slices.Equal(p.opened, []string{"preflight-app"}) || !slices.Equal(p.closed, []string{"preflight-app"}) {
		t.Fatalf("opened %v, closed %v; want the throwaway window opened and closed", p.opened, p.closed)
	}
	if !slices.Equal(p.sent, []string{"claude --permission-mode auto"}) {
		t.Fatalf("sent = %q; the command must get no prompt, so no work starts", p.sent)
	}
}

func TestPreflightWaitsForTheScreenToSettle(t *testing.T) {
	p := newPF("claude")
	p.printsFor = 20 * time.Second
	p.run(pfWorker(t))
	if waited := p.now.Sub(t0); waited < 20*time.Second+settledFor {
		t.Fatalf("captured after %s, while the command was still printing", waited)
	}
}

func TestPreflightGivesUpWaitingOnAScreenThatNeverSettles(t *testing.T) {
	p := newPF("claude")
	p.printsFor = time.Hour
	res := p.run(pfWorker(t))
	if res.State != PreflightCaptured || p.now.Sub(t0) > settleTimeout+time.Minute {
		t.Fatalf("result = %+v after %s", res, p.now.Sub(t0))
	}
}

func TestPreflightReportsACommandThatEnded(t *testing.T) {
	p := newPF("zsh") // the command exited and left the shell
	p.screen = "zsh: command not found: claude"
	if res := p.run(pfWorker(t)); !res.Exited {
		t.Fatalf("result = %+v", res)
	}
}

func TestPreflightLeavesARunningWorkerAlone(t *testing.T) {
	p := newPF("claude")
	p.windows["app"] = Observation{Exists: true, Command: "claude", LastActivity: t0}
	res := p.run(pfWorker(t))
	if res.State != PreflightRunning || len(p.opened) != 0 || len(p.sent) != 0 {
		t.Fatalf("result = %+v, opened %v, sent %q", res, p.opened, p.sent)
	}
}

func TestPreflightReportsAMissingDirectory(t *testing.T) {
	p := newPF("claude")
	w := pfWorker(t)
	w.Dir = filepath.Join(w.Dir, "nope")
	if res := p.run(w); res.State != PreflightProblem || !strings.Contains(res.Problem, "does not exist") || len(p.opened) != 0 {
		t.Fatalf("result = %+v, opened %v", res, p.opened)
	}
}

func TestPreflightReportsUncommittedChanges(t *testing.T) {
	p := newPF("claude")
	w := pfWorker(t)
	if err := exec.Command("git", "-C", w.Dir, "init", "-q").Run(); err != nil {
		t.Skip("git is not available")
	}
	os.WriteFile(filepath.Join(w.Dir, "half-done.txt"), []byte("x"), 0o644)
	if res := p.run(w); res.State != PreflightProblem || !strings.Contains(res.Problem, "1 uncommitted change(s)") || len(p.opened) != 0 {
		t.Fatalf("result = %+v, opened %v", res, p.opened)
	}
}

func TestPreflightClosesAWindowLeftByAnEarlierRun(t *testing.T) {
	p := newPF("claude")
	p.windows["preflight-app"] = Observation{Exists: true, Command: "claude", LastActivity: t0}
	p.run(pfWorker(t))
	if !slices.Equal(p.closed, []string{"preflight-app", "preflight-app"}) {
		t.Fatalf("closed = %v; want the stale window closed, then the new one", p.closed)
	}
}

type fakeAsker struct {
	answer string
	err    error
	asked  string
}

func (f *fakeAsker) Ask(_ context.Context, prompt string) (string, error) {
	f.asked = prompt
	return f.answer, f.err
}

func TestClassify(t *testing.T) {
	captured := PreflightResult{Worker: "app", State: PreflightCaptured, Screen: "Do you trust this folder?\n1. Yes"}
	cases := []struct {
		name      string
		result    PreflightResult
		asker     fakeAsker
		wantReady bool
		wantLine  string
	}{
		{"running worker", PreflightResult{Worker: "app", State: PreflightRunning}, fakeAsker{}, true, "app: already running"},
		{"problem", PreflightResult{Worker: "app", State: PreflightProblem, Problem: "directory ~/x does not exist"}, fakeAsker{}, false, "app: needs you: directory ~/x does not exist"},
		{"command ended", PreflightResult{Worker: "app", State: PreflightCaptured, Exited: true, Screen: "command not found"}, fakeAsker{}, false, "its command ended"},
		{"ready", captured, fakeAsker{answer: `{"state":"ready","detail":"clean start"}`}, true, "app: ready (clean start)"},
		{"ready with a failed integration", captured, fakeAsker{answer: "```json\n{\"state\":\"ready\",\"detail\":\"1 MCP server failed to connect\"}\n```"}, true, "app: ready (1 MCP server failed to connect)"},
		{"first-run question", captured, fakeAsker{answer: `{"state":"question","detail":"asks to trust the folder"}`}, false, "app: needs you: question: asks to trust the folder\n    | Do you trust this folder?\n    | 1. Yes"},
		{"login problem", captured, fakeAsker{answer: `{"state":"login","detail":"not logged in"}`}, false, "app: needs you: login: not logged in"},
		{"reviewer unavailable", captured, fakeAsker{err: errors.New("not logged in")}, false, "could not be classified (not logged in)\n    | Do you trust this folder?"},
		{"reviewer rambles", captured, fakeAsker{answer: "Looks fine to me."}, false, "could not be classified"},
		{"reviewer invents a state", captured, fakeAsker{answer: `{"state":"probably","detail":"x"}`}, false, `unknown state "probably"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, ready := Classify(context.Background(), &tc.asker, tc.result)
			if ready != tc.wantReady || !strings.Contains(line, tc.wantLine) {
				t.Fatalf("ready = %v, line = %q; want ready %v and %q", ready, line, tc.wantReady, tc.wantLine)
			}
		})
	}
}

func TestClassifyShowsTheReviewerTheScreen(t *testing.T) {
	asker := &fakeAsker{answer: `{"state":"ready","detail":"clean start"}`}
	Classify(context.Background(), asker, PreflightResult{Worker: "app", State: PreflightCaptured, Screen: "auto mode on"})
	if !strings.Contains(asker.asked, "<terminal>\nauto mode on\n</terminal>") {
		t.Fatalf("prompt = %q", asker.asked)
	}
}
