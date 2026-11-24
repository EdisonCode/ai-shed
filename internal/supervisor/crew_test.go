package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

func crewWorker() config.Worker {
	return config.Worker{Name: "app", Session: "box-app", Crew: true, PeerList: []config.Peer{
		{Machine: "mini", Worker: "web", Session: "mini-web", Remote: true},
		{Machine: "box", Worker: "api", Session: "box-api"},
	}}
}

func signedBy(i backlog.Issue, session, paths string) backlog.Issue {
	i.Comments = append(i.Comments, backlog.Comment{Body: "## Plan\n**Paths:** " + paths + "\n**Session:** " + session})
	return i
}

// countingLister counts its reads and answers by query.
type countingLister struct {
	byQuery map[string][]backlog.Issue
	reads   int
}

func (l *countingLister) List(_ context.Context, src config.IssueSource) ([]backlog.Issue, error) {
	l.reads++
	return l.byQuery[src.Repo+" "+src.Query], nil
}

func TestACrewWorkersGoalsAreOfTheIssueInHandAndThePlansItSigned(t *testing.T) {
	s := &Supervisor{Settings: config.Supervisor{GoalLabels: "goal:"}}
	open := []backlog.Issue{
		labeled(10, "goal:export"),
		signedBy(labeled(11, "goal:refunds"), "box-app", "billing/"),
		signedBy(labeled(12, "goal:search"), "box-api", "search/"),
		labeled(13),
	}
	checkins := []runlog.Checkin{{Time: time.Now(), Worker: "app", Issue: 10, Sent: true}}
	got := s.crewGoals(crewWorker(), open, checkins)
	if strings.Join(got, ",") != "goal:export,goal:refunds" {
		t.Fatalf("goals = %v", got)
	}
	notCrew := crewWorker()
	notCrew.Crew = false
	if got := s.crewGoals(notCrew, open, checkins); got != nil {
		t.Fatalf("a worker that is not crew has goals %v", got)
	}
}

func TestTheRosterNamesWhichCrewMateBuildsWhichPartOfEachGoal(t *testing.T) {
	export := map[string][]backlog.Issue{"goal:export": {
		signedBy(backlog.Issue{Repo: "org/api", Number: 40, Title: "Export endpoint"}, "box-api", "internal/export/"),
		signedBy(backlog.Issue{Repo: "org/web", Number: 7, Title: "Export button"}, "mini-web", "web/export/"),
		signedBy(backlog.Issue{Repo: "org/web", Number: 8, Title: "My own part"}, "box-app", "web/"),
		signedBy(backlog.Issue{Repo: "org/web", Number: 9, Title: "Not crew"}, "box-docs", "docs/"),
	}}
	got := crewText(crewWorker(), []string{"goal:export", "goal:refunds"}, export)
	for _, want := range []string{
		"# Your crew",
		"## goal:export\n\n- `box-api` builds org/api#40 Export endpoint (paths: internal/export/)\n- `mini-web` builds org/web#7 Export button (paths: web/export/)\n",
		"## goal:refunds\n\nNo crew-mate has signed a plan on this goal yet.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("crew text lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "My own part") || strings.Contains(got, "box-docs") {
		t.Errorf("the crew text names the worker itself or one outside the crew:\n%s", got)
	}
	if crewText(crewWorker(), nil, export) != "" {
		t.Error("a crew worker on no goal has a crew section")
	}
}

func TestAGoalsIssuesAreReadInEveryCrewRepositoryAndReused(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	l := &countingLister{byQuery: map[string][]backlog.Issue{
		`org/api label:"goal:export"`: {{Repo: "org/api", Number: 40}},
		`org/web label:"goal:export"`: {{Repo: "org/web", Number: 7}},
	}}
	s := &Supervisor{Lister: l, CrewRepos: []string{"org/api", "org/web"}, Now: func() time.Time { return now }, Logf: func(string, ...any) {}}
	got := s.crewIssues(context.Background(), []string{"goal:export"})
	if len(got["goal:export"]) != 2 || l.reads != 2 {
		t.Fatalf("issues = %+v after %d reads", got, l.reads)
	}
	s.crewIssues(context.Background(), []string{"goal:export"})
	if l.reads != 2 {
		t.Fatalf("read again within crewKept: %d reads", l.reads)
	}
	now = now.Add(crewKept)
	s.crewIssues(context.Background(), []string{"goal:export"})
	if l.reads != 4 {
		t.Fatalf("not read again after crewKept: %d reads", l.reads)
	}
}
