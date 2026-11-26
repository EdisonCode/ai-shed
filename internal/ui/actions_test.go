package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/runlog"
	"github.com/edisoncode/ai-shed/internal/status"
)

type fakeCommenter struct{ posted []string }

func (f *fakeCommenter) Comment(_ context.Context, repo string, issue int, body string) error {
	f.posted = append(f.posted, fmt.Sprintf("%s#%d %s", repo, issue, body))
	return nil
}

func itoa(n int) string { return fmt.Sprint(n) }

func askingReport() status.Report {
	return status.Report{Machines: []status.MachineReport{{Name: "box", Issues: []status.IssueStatus{
		{Repo: "org/app", Number: 5, State: status.Waiting, Asks: []backlog.Ask{{Name: config.DecisionSignal, Text: "Default currency?\n- Recommend: USD"}}},
		{Repo: "org/app", Number: 6, State: status.Waiting, Asks: []backlog.Ask{{Name: config.EyesSignal, Text: "open /orders"}, {Name: "review", PR: 41}}},
		{Repo: "org/app", Number: 7, State: status.Held, Held: "waits for org/api#40, which no queue covers", Blockers: []string{"org/api#40"}, Droppable: []string{"org/api#40"}},
		{Repo: "org/app", Number: 8, State: status.Held, Held: "waits for #9 to close", Blockers: []string{"#9"}},
		{Repo: "org/app", Number: 9, State: status.Queued},
	}}, {Name: "mini", Issues: []status.IssueStatus{
		{Repo: "org/app", Number: 5, State: status.Waiting, Asks: []backlog.Ask{{Name: config.DecisionSignal, Text: "Default currency?\n- Recommend: USD"}}},
	}}}}
}

func TestTheOwnersAnswersAreWorkedOutFromTheReport(t *testing.T) {
	got := actionsFor(askingReport(), config.DefaultSignals())
	var lines []string
	for _, a := range got {
		line := fmt.Sprintf("%s %s#%d %s%s quick=%v", a.Kind, a.Repo, a.Number, a.Machine, a.Comment, a.Quick)
		if a.For != "" {
			line += " for " + a.For
		}
		lines = append(lines, line)
	}
	want := []string{
		"accept org/app#5 Owner ruling: accepted quick=true",
		"eyes org/app#6 Eyes checked: passed on staging. quick=true",
		"undepend org/app#7 No longer depends on org/api#40 quick=false",
		"bump org/app#9 box quick=false for org/app#8",
		"bump org/app#9 box quick=false",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("actions:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if got[0].Context != "Default currency?\n- Recommend: USD" {
		t.Fatalf("the accept does not show the recommendation: %+v", got[0])
	}
	if got[2].Warn == "" {
		t.Fatal("dropping a dependency carries no warning")
	}
}

func actingServer(t *testing.T) (*Server, *fakeCommenter) {
	t.Helper()
	c := &fakeCommenter{}
	s, _ := newServer(t, func(context.Context) status.Report { return askingReport() })
	s.Commenter, s.Token = c, "secret"
	s.refresh(context.Background())
	return s, c
}

func post(s *Server, host, origin, token, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "http://"+host+"/api/act", strings.NewReader(`{"id":`+string(mustJSON(id))+`}`))
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.Header.Set("X-Shed-Token", token)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestAnAnswerIsPostedOnceAndNotOfferedAgainUntilTheNextRead(t *testing.T) {
	s, c := actingServer(t)
	id := "accept:org/app#5:Owner ruling: accepted"
	rec := post(s, "127.0.0.1:7777", "http://127.0.0.1:7777", "secret", id)
	if rec.Code != http.StatusOK || len(c.posted) != 1 || c.posted[0] != "org/app#5 Owner ruling: accepted" {
		t.Fatalf("code %d, body %s, posted %q", rec.Code, rec.Body, c.posted)
	}
	var snap Snapshot
	json.Unmarshal(s.snapshot(), &snap)
	for _, a := range snap.Actions {
		if a.ID == id {
			t.Fatal("the posted answer is still offered before the next read")
		}
	}
}

func TestAnActionIsRefusedUnlessItComesFromThePageOnThisMachine(t *testing.T) {
	id := "accept:org/app#5:Owner ruling: accepted"
	for name, tc := range map[string]struct {
		host, origin, token, id string
		code                    int
	}{
		"wrong token":       {"127.0.0.1:7777", "http://127.0.0.1:7777", "guess", id, http.StatusForbidden},
		"another site":      {"127.0.0.1:7777", "https://evil.example", "secret", id, http.StatusForbidden},
		"no origin":         {"127.0.0.1:7777", "", "secret", id, http.StatusForbidden},
		"a rebound name":    {"evil.example:7777", "http://evil.example:7777", "secret", id, http.StatusForbidden},
		"an unknown action": {"localhost:7777", "http://localhost:7777", "secret", "accept:org/app#5:Owner ruling: delete everything", http.StatusConflict},
	} {
		s, c := actingServer(t)
		if rec := post(s, tc.host, tc.origin, tc.token, tc.id); rec.Code != tc.code || len(c.posted) != 0 {
			t.Errorf("%s: code %d (want %d), posted %q", name, rec.Code, tc.code, c.posted)
		}
	}
}

func TestWithoutACommenterThePageOffersNoActionsAndHasNoToken(t *testing.T) {
	s, _ := newServer(t, func(context.Context) status.Report { return askingReport() })
	s.Token = "secret"
	s.refresh(context.Background())
	var snap Snapshot
	json.Unmarshal(s.snapshot(), &snap)
	if len(snap.Actions) != 0 {
		t.Fatalf("actions offered with no way to post: %+v", snap.Actions)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(rec.Body.String(), "secret") || !strings.Contains(rec.Body.String(), `<meta name="shed-token" content="">`) {
		t.Fatal("the page carries the token while actions are off")
	}
	if rec := post(s, "127.0.0.1:7777", "http://127.0.0.1:7777", "secret", "accept:org/app#5:Owner ruling: accepted"); rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestABumpFromThePageIsMadeOnTheMachineAsShedBumpMakesIt(t *testing.T) {
	s, c := actingServer(t)
	s.Config = &config.Config{Machines: []config.Machine{{Name: "box", Host: "local"}}}
	r := s.Runner.(*fakeRunner)
	rec := post(s, "127.0.0.1:7777", "http://127.0.0.1:7777", "secret", "bump:org/app#9:box:for org/app#8")
	if rec.Code != http.StatusOK || len(c.posted) != 0 {
		t.Fatalf("code %d, body %s, posted %q", rec.Code, rec.Body, c.posted)
	}
	if len(r.commands) != 1 || !strings.Contains(r.commands[0], "box: ") || !strings.Contains(r.commands[0], "bumps") {
		t.Fatalf("commands = %q, want the bump written on box", r.commands)
	}
}

func TestEachGoalOffersWhatItsModeAllows(t *testing.T) {
	r := status.Report{Machines: []status.MachineReport{{Name: "box",
		Probe:  &probe.Result{Goals: map[string]string{"goal: export": runlog.GoalFocus, "goal: refunds": runlog.GoalTabled}},
		Issues: []status.IssueStatus{{Repo: "org/app", Number: 5, Goal: "goal: route planner"}}}}}
	deck := &status.Digest{Goals: []status.GoalProgress{{Repo: "org/app", Label: "goal: export"}}}
	var got []string
	for _, a := range goalActions(r, deck) {
		got = append(got, a.Goal+" → "+a.Label)
	}
	want := []string{
		"goal: export → Table this goal",
		"goal: export → Take it out of focus",
		"goal: refunds → Focus: finish this goal first",
		"goal: refunds → Take it off the table",
		"goal: route planner → Focus: finish this goal first",
		"goal: route planner → Table this goal",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("goal actions:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestFocusingOnAThirdGoalWarnsFirst(t *testing.T) {
	r := status.Report{Machines: []status.MachineReport{{Name: "box",
		Probe: &probe.Result{Goals: map[string]string{"goal: a": runlog.GoalFocus, "goal: b": runlog.GoalFocus}}}}}
	deck := &status.Digest{Goals: []status.GoalProgress{{Label: "goal: c"}}}
	for _, a := range goalActions(r, deck) {
		if a.Goal == "goal: c" && a.Mode == runlog.GoalFocus {
			if !strings.Contains(a.Warn, "2 goals are in focus already (goal: a, goal: b)") {
				t.Fatalf("warning = %q", a.Warn)
			}
			return
		}
	}
	t.Fatal("goal: c offers no focus")
}

func TestAGoalActionFromThePageSetsItsModeOnEveryMachine(t *testing.T) {
	c := &fakeCommenter{}
	s, r := newServer(t, func(context.Context) status.Report {
		return status.Report{Machines: []status.MachineReport{{Name: "box", Issues: []status.IssueStatus{{Repo: "org/app", Number: 5, Goal: "goal: export"}}}}}
	})
	s.Config = &config.Config{Machines: []config.Machine{
		{Name: "box", Host: "local", Workers: []config.Worker{{Name: "app"}}},
		{Name: "mini", Host: "local", Workers: []config.Worker{{Name: "web"}}},
	}}
	s.Commenter, s.Token = c, "secret"
	s.refresh(context.Background())
	rec := post(s, "127.0.0.1:7777", "http://127.0.0.1:7777", "secret", "goal:goal: export:focus")
	if rec.Code != http.StatusOK || len(c.posted) != 0 {
		t.Fatalf("code %d, body %s, posted %q", rec.Code, rec.Body, c.posted)
	}
	if len(r.commands) != 2 || !strings.Contains(r.commands[0], "box: ") || !strings.Contains(r.commands[1], "mini: ") || !strings.Contains(r.commands[1], "goals/goal:%20export") {
		t.Fatalf("commands = %q, want the goal's mode written on box and mini", r.commands)
	}
}

type fakeLabeler struct{ changed []string }

func (f *fakeLabeler) Relabel(_ context.Context, repo string, issue int, add, remove string) error {
	f.changed = append(f.changed, fmt.Sprintf("%s#%d +%s -%s", repo, issue, add, remove))
	return nil
}

func cardFleet() *config.Config {
	return &config.Config{Machines: []config.Machine{{Name: "box", Host: "local",
		Issues:  []config.IssueSource{{Repo: "org/app", Label: "machine:box"}},
		Workers: []config.Worker{{Name: "app"}}}}}
}

func cardReport() status.Report {
	return status.Report{Machines: []status.MachineReport{{Name: "box", Issues: []status.IssueStatus{
		{Repo: "org/app", Number: 3, State: status.Working, Worker: "app"},
		{Repo: "org/app", Number: 9, State: status.Queued},
	}}}}
}

func TestACardOffersToPauseRecycleReshapeAndCancel(t *testing.T) {
	var lines []string
	for _, a := range cardActions(cardReport(), cardFleet(), config.DefaultSignals(), true) {
		lines = append(lines, fmt.Sprintf("%s %s#%d %s %s %s%s", a.Kind, a.Repo, a.Number, a.Machine, a.Worker, a.Remove, a.Comment))
	}
	want := []string{
		"recycle org/app#3 box app ",
		"pause org/app#3 box  ",
		"pause org/app#9 box  ",
		"reshape org/app#9   Not ready: too big: split it into slices that a worker can each finish in one pull request, each with its own acceptance.",
		"cancel org/app#9 box  machine:box",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("actions:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestACancelIsNotOfferedWhenNoSingleLabelPutsTheIssueInTheQueue(t *testing.T) {
	cfg := cardFleet()
	cfg.Machines[0].Issues = append(cfg.Machines[0].Issues, config.IssueSource{Repo: "org/app", Query: "label:bug"})
	for _, a := range cardActions(cardReport(), cfg, config.DefaultSignals(), true) {
		if a.Kind == ActCancel {
			t.Fatalf("cancel offered with a query source: %+v", a)
		}
	}
}

func cardServer(t *testing.T) (*Server, *fakeRunner, *fakeLabeler) {
	t.Helper()
	s, r := newServer(t, func(context.Context) status.Report { return cardReport() })
	l := &fakeLabeler{}
	s.Config, s.Commenter, s.Labeler, s.Token = cardFleet(), &fakeCommenter{}, l, "secret"
	s.refresh(context.Background())
	return s, r, l
}

func TestACancelFromACardTakesTheQueueLabelOff(t *testing.T) {
	s, _, l := cardServer(t)
	rec := post(s, "127.0.0.1:7777", "http://127.0.0.1:7777", "secret", "cancel:org/app#9:box::machine:box")
	if rec.Code != http.StatusOK || len(l.changed) != 1 || l.changed[0] != "org/app#9 + -machine:box" {
		t.Fatalf("code %d, body %s, changed %q", rec.Code, rec.Body, l.changed)
	}
}

// timeRunner answers every command with a time in Unix seconds, as a
// machine answers a pause with when it ends.
type timeRunner struct{ commands []string }

func (r *timeRunner) Run(_ context.Context, m config.Machine, command string, _ io.Reader) ([]byte, error) {
	r.commands = append(r.commands, m.Name+": "+command)
	return []byte(fmt.Sprintln(time.Now().Add(30 * time.Minute).Unix())), nil
}

func TestAPauseAndARecycleFromACardReachTheMachine(t *testing.T) {
	s, _, _ := cardServer(t)
	r := &timeRunner{}
	s.Runner = r
	for _, id := range []string{"pause:org/app#3:box::", "recycle:org/app#3:box:app:"} {
		if rec := post(s, "127.0.0.1:7777", "http://127.0.0.1:7777", "secret", id); rec.Code != http.StatusOK {
			t.Fatalf("%s: code %d, body %s", id, rec.Code, rec.Body)
		}
	}
	if len(r.commands) != 2 {
		t.Fatalf("commands = %q, want a pause and a recycle on box", r.commands)
	}
}

type fakeGoalLabels struct {
	fakeLabeler
	labeled map[string][]int // by repo and label, as "org/app goal: a"
	has     map[string]bool
	done    []string
}

func (f *fakeGoalLabels) Labeled(_ context.Context, repo, label string, closedToo bool) ([]int, error) {
	f.done = append(f.done, fmt.Sprintf("list %s %s all=%v", repo, label, closedToo))
	return f.labeled[repo+" "+label], nil
}
func (f *fakeGoalLabels) HasLabel(_ context.Context, repo, label string) (bool, error) {
	return f.has[repo+" "+label], nil
}
func (f *fakeGoalLabels) RenameLabel(_ context.Context, repo, from, to string) error {
	f.done = append(f.done, fmt.Sprintf("rename %s %s -> %s", repo, from, to))
	return nil
}
func (f *fakeGoalLabels) DeleteLabel(_ context.Context, repo, label string) error {
	f.done = append(f.done, fmt.Sprintf("delete %s %s", repo, label))
	return nil
}

func goalDeck() *status.Digest {
	return &status.Digest{Goals: []status.GoalProgress{
		{Repo: "org/app", Label: "goal: a", Total: 5, Pace: backlog.Pace{Open: 2}},
		{Repo: "org/api", Label: "goal: a", Total: 3, Pace: backlog.Pace{Open: 1}},
		{Repo: "org/app", Label: "goal: b", Total: 4, Pace: backlog.Pace{Open: 4}},
	}}
}

func goalAction(t *testing.T, id string) Action {
	t.Helper()
	for _, a := range goalLabelActions(status.Report{}, goalDeck()) {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no action %s", id)
	return Action{}
}

func TestEndingAGoalSaysHowManyOpenIssuesChangeAndTakesItsLabelOffThem(t *testing.T) {
	a := goalAction(t, "goal-end:goal: a")
	if !strings.Contains(a.Warn, "3 open issue(s) in 2 repositories") {
		t.Fatalf("warn = %q", a.Warn)
	}
	g := &fakeGoalLabels{labeled: map[string][]int{"org/app goal: a": {4, 7}, "org/api goal: a": {2}}}
	s := &Server{GoalLabels: g}
	if err := s.do(context.Background(), a, ""); err != nil {
		t.Fatal(err)
	}
	if want := []string{"org/app#4 + -goal: a", "org/app#7 + -goal: a", "org/api#2 + -goal: a"}; strings.Join(g.changed, ";") != strings.Join(want, ";") {
		t.Fatalf("changed = %q, want %q", g.changed, want)
	}
}

func TestMergingAGoalRenamesWhereTheOtherIsMissingAndMovesTheIssuesWhereItIsThere(t *testing.T) {
	g := &fakeGoalLabels{labeled: map[string][]int{"org/app goal: a": {4}}, has: map[string]bool{"org/app goal: b": true}}
	s := &Server{GoalLabels: g}
	if err := s.do(context.Background(), goalAction(t, "goal-merge:goal: a:goal: b"), ""); err != nil {
		t.Fatal(err)
	}
	want := []string{"list org/app goal: a all=true", "delete org/app goal: a", "rename org/api goal: a -> goal: b"}
	if strings.Join(g.done, ";") != strings.Join(want, ";") || len(g.changed) != 1 || g.changed[0] != "org/app#4 +goal: b -goal: a" {
		t.Fatalf("done = %q, changed = %q", g.done, g.changed)
	}
}

func TestARenameKeepsTheGoalPrefixAndRefusesALabelThatIsThere(t *testing.T) {
	g := &fakeGoalLabels{has: map[string]bool{"org/api goal: taken": true}}
	s := &Server{GoalLabels: g, Config: &config.Config{Supervisor: config.Supervisor{GoalLabels: "goal:"}}}
	a := goalAction(t, "goal-rename:goal: a")
	for name, want := range map[string]string{
		"exports":     "does not start with the goal prefix",
		"goal: taken": "already has the label",
		"goal: a":     "give a new name",
	} {
		if err := s.do(context.Background(), a, name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", name, err, want)
		}
	}
	if len(g.done) != 0 {
		t.Fatalf("refused renames changed labels: %q", g.done)
	}
	if err := s.do(context.Background(), a, "goal: exports"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"rename org/app goal: a -> goal: exports", "rename org/api goal: a -> goal: exports"}; strings.Join(g.done, ";") != strings.Join(want, ";") {
		t.Fatalf("done = %q", g.done)
	}
}
