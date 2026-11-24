package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
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
		{Repo: "org/app", Number: 7, State: status.Held, Held: "waits for org/api#40, which no queue covers", Droppable: []string{"org/api#40"}},
	}}, {Name: "mini", Issues: []status.IssueStatus{
		{Repo: "org/app", Number: 5, State: status.Waiting, Asks: []backlog.Ask{{Name: config.DecisionSignal, Text: "Default currency?\n- Recommend: USD"}}},
	}}}}
}

func TestTheOwnersAnswersAreWorkedOutFromTheReport(t *testing.T) {
	got := actionsFor(askingReport(), config.DefaultSignals())
	var lines []string
	for _, a := range got {
		lines = append(lines, a.Kind+" "+a.Repo+"#"+itoa(a.Number)+" "+a.Comment)
	}
	want := []string{
		"accept org/app#5 Owner ruling: accepted",
		"eyes org/app#6 Eyes checked: passed on staging.",
		"undepend org/app#7 No longer depends on org/api#40",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("actions:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if got[0].Context != "Default currency?\n- Recommend: USD" {
		t.Fatalf("the accept does not show the recommendation: %+v", got[0])
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
