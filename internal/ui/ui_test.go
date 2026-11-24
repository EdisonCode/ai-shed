package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/status"
)

func fleet() *config.Config {
	return &config.Config{Machines: []config.Machine{
		{Name: "box", Host: "local", Workers: []config.Worker{{Name: "app"}}},
	}}
}

type fakeRunner struct{ commands []string }

func (r *fakeRunner) Run(_ context.Context, m config.Machine, command string, _ io.Reader) ([]byte, error) {
	r.commands = append(r.commands, m.Name+": "+command)
	if strings.Contains(command, "capture-pane") {
		return []byte("worker screen\n"), nil
	}
	return []byte(`{"time":"2026-11-23T10:00:00Z","worker":"app","kind":"idle","verdict":"on_track","reason":"fine"}` + "\nnot json\n"), nil
}

func newServer(t *testing.T, collect func(context.Context) status.Report) (*Server, *fakeRunner) {
	t.Helper()
	r := &fakeRunner{}
	return &Server{Config: fleet(), Collect: collect, Runner: r, Every: time.Hour}, r
}

func report(name string) status.Report {
	return status.Report{Machines: []status.MachineReport{{Name: name, Attention: []string{"worker app needs you"}}}}
}

func TestTheReportIsServedAsJSON(t *testing.T) {
	s, _ := newServer(t, func(context.Context) status.Report { return report("box") })
	s.refresh(context.Background())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/report", nil))
	var got Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	if len(got.Report.Machines) != 1 || got.Report.Machines[0].Name != "box" || got.NeedsYou != 1 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestTheDeckIsReadFromTheReportAndSentWithTheNextOne(t *testing.T) {
	s, _ := newServer(t, func(context.Context) status.Report { return report("box") })
	read := make(chan status.Report, 1)
	s.DeckEvery = time.Hour
	s.Digest = func(_ context.Context, r status.Report) status.Digest {
		read <- r
		return status.Digest{Report: r, Goals: []status.GoalProgress{{Repo: "org/app", Label: "goal:export", Closed: 7, Total: 10}}}
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	s.refresh(context.Background())
	if r := <-read; r.Machines[0].Name != "box" {
		t.Fatalf("the deck read report %+v", r)
	}
	lines := bufio.NewScanner(resp.Body)
	for lines.Scan() {
		data, ok := strings.CutPrefix(lines.Text(), "data: ")
		if !ok {
			continue
		}
		var got Snapshot
		if err := json.Unmarshal([]byte(data), &got); err != nil {
			t.Fatal(err)
		}
		if got.Deck == nil {
			continue // the report, sent before the deck was read
		}
		if len(got.Deck.Goals) != 1 || len(got.Deck.Report.Machines) != 0 || got.Report.Machines[0].Name != "box" {
			t.Fatalf("snapshot = %+v", got)
		}
		break
	}
	// Within DeckEvery, a new report does not read the deck again, and keeps it.
	s.refresh(context.Background())
	select {
	case <-read:
		t.Fatal("the deck was read again within DeckEvery")
	case <-time.After(50 * time.Millisecond):
	}
	var got Snapshot
	json.Unmarshal(s.snapshot(), &got)
	if got.Deck == nil {
		t.Fatal("the next report dropped the deck")
	}
}

func TestBeforeTheFirstReportTheAPISaysSo(t *testing.T) {
	s, _ := newServer(t, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/report", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestTheEventStreamSendsEachNewReport(t *testing.T) {
	var n atomic.Int32
	s, _ := newServer(t, func(context.Context) status.Report {
		n.Add(1)
		return report("box" + strings.Repeat("x", int(n.Load())))
	})
	s.refresh(context.Background())
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := bufio.NewScanner(resp.Body)
	next := func() string {
		for lines.Scan() {
			if data, ok := strings.CutPrefix(lines.Text(), "data: "); ok {
				return data
			}
		}
		t.Fatal("the stream ended")
		return ""
	}
	if first := next(); !strings.Contains(first, `"name":"boxx"`) {
		t.Fatalf("first event = %s", first)
	}
	go s.refresh(context.Background())
	if second := next(); !strings.Contains(second, `"name":"boxxx"`) {
		t.Fatalf("second event = %s", second)
	}
}

func TestAWorkersScreenIsCapturedOverItsMachine(t *testing.T) {
	s, r := newServer(t, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/screen?machine=box&worker=app", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "worker screen\n" {
		t.Fatalf("code %d, body %q", rec.Code, rec.Body)
	}
	if len(r.commands) != 1 || !strings.Contains(r.commands[0], "box: tmux capture-pane") || !strings.Contains(r.commands[0], "'=shed:=app'") {
		t.Fatalf("commands = %q", r.commands)
	}
}

func TestAScreenOfAWorkerTheFleetFileDoesNotNameIsRefused(t *testing.T) {
	s, r := newServer(t, nil)
	for _, q := range []string{"machine=box&worker=nope", "machine=mars&worker=app", "machine=box&worker=app;rm%20-rf"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/screen?"+q, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: code = %d", q, rec.Code)
		}
	}
	if len(r.commands) != 0 {
		t.Fatalf("commands ran: %q", r.commands)
	}
}

func TestAMachinesLogIsItsLatestCheckins(t *testing.T) {
	s, _ := newServer(t, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/log?machine=box", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"reason":"fine"`) || strings.Contains(rec.Body.String(), "not json") {
		t.Fatalf("code %d, body %s", rec.Code, rec.Body)
	}
}

func TestOnlyGetIsServed(t *testing.T) {
	s, _ := newServer(t, func(context.Context) status.Report { return report("box") })
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/api/report", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestThePageIsServed(t *testing.T) {
	s, _ := newServer(t, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>shed</title>") {
		t.Fatalf("code %d", rec.Code)
	}
}

func TestTheDefaultAddressIsLoopback(t *testing.T) {
	if !Loopback(DefaultListen) {
		t.Fatalf("%s is not loopback", DefaultListen)
	}
	for addr, want := range map[string]bool{"127.0.0.1:7777": true, "localhost:80": true, "[::1]:7777": true, "0.0.0.0:7777": false, ":7777": false, "100.64.0.5:7777": false} {
		if Loopback(addr) != want {
			t.Errorf("Loopback(%q) = %v, want %v", addr, !want, want)
		}
	}
}

var _ = errors.New
