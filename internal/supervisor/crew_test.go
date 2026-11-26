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

func TestTheRosterGivesEachGoalItsBriefOrSaysItHasNone(t *testing.T) {
	issues := map[string][]backlog.Issue{
		"goal:export": {{Repo: "org/api", Number: 39, Title: "Goal brief: export", Body: "**Slices:**\n1. #40 the endpoint\n\n**Contracts:**\n- GET /export returns CSV, provided by #40\n\n**Done when:**\n- a user downloads a CSV on staging"}},
	}
	got := crewText(crewWorker(), []string{"goal:export", "goal:refunds"}, issues)
	for _, want := range []string{
		"Goal brief: org/api#39. It is the crew's shared plan",
		"edit the brief and comment on it what changed and why",
		"Slices:\n- #40 the endpoint\n",
		"Contracts:\n- GET /export returns CSV, provided by #40\n",
		"Done when:\n- a user downloads a CSV on staging\n",
		"## goal:refunds\n\nNo crew-mate has signed a plan on this goal yet.\n\nThis goal has no brief",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("crew text lacks %q:\n%s", want, got)
		}
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

// withCrew makes the fixture's app and a second worker, web, a crew on
// goal:export: app built #12 and handed it back; web signed #13.
func withCrew(t *testing.T, f *fixture) {
	f.Settings.GoalLabels = "goal:"
	f.CrewRepos = []string{"org/app"}
	app := &f.Machine.Workers[0]
	app.Crew, app.Session = true, "box-app"
	app.PeerList = []config.Peer{{Machine: "box", Worker: "web", Session: "box-web"}}
	f.Machine.Workers = append(f.Machine.Workers, config.Worker{Name: "web", Dir: t.TempDir(), Brief: "Build.", Crew: true, Session: "box-web",
		PeerList: []config.Peer{{Machine: "box", Worker: "app", Session: "box-app"}}})
	built(t, f, 12)
	mine := handedBackWith(12, "")
	mine.Labels = append(mine.Labels, backlog.Label{Name: "goal:export"})
	mine.Comments = append([]backlog.Comment{{Body: "## Plan\n**Session:** box-app"}}, mine.Comments...)
	theirs := signedBy(labeled(13, "goal:export"), "box-web", "web/export/")
	f.lister.issues = []backlog.Issue{mine, theirs}
}

func TestAHandedBackPullRequestOfAGoalGoesToACrewMateKeepingItsContext(t *testing.T) {
	// An hour of cache: the rest before the review does not clear a cold
	// context either.
	f := started(t, config.Supervisor{CacheTTL: "1h"}, done)
	withCrew(t, f)
	f.tick(2 * time.Minute)
	f.tick(10 * time.Minute) // the reviewer is asked, and hands over nothing
	f.tick(20 * time.Minute) // so the code hands the review over
	c := lastOf(t, f, "web")
	if c.Issue != 12 || !c.Verify {
		t.Fatalf("web's check-in = %+v, want the crew review of #12", c)
	}
	for _, want := range []string{"Crew review: pull request #41 of org/app#12, which box-app built", "message box-app what you find", "`Crew review passed (low):`", "`Crew review: changes (low):`"} {
		if !strings.Contains(c.Message, want) {
			t.Fatalf("message = %q, lacks %q", c.Message, want)
		}
	}
	for n, sent := range f.term.sent {
		if sent == "/clear" && n+1 < len(f.term.sent) && strings.Contains(f.term.sent[n+1], "Crew review:") {
			t.Fatalf("sent = %q: a crew review keeps the reviewer's context", f.term.sent)
		}
	}
	if c := lastOf(t, f, "app"); c.Issue == 12 && c.Verify {
		t.Fatalf("app was handed its own pull request to review: %+v", c)
	}
}

func TestTheVerifierWaitsForTheCrewReviewAndCrewChangesAreRework(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	withCrew(t, f)
	withVerifier(t, f)
	f.lister.issues[0].Verify = true
	f.tick(2 * time.Minute)
	f.tick(10 * time.Minute)
	if c := lastOf(t, f, "rev"); c.Issue == 12 {
		t.Fatalf("rev verified #12 before its crew review: %+v", c)
	}
	// The crew-mate asks for changes: the author gets the rework.
	f.lister.issues[0].Comments = append(f.lister.issues[0].Comments, backlog.Comment{Body: "Crew review: changes (low): the export drops the new field"})
	f.tick(20 * time.Minute)
	prompt := f.reviewer.prompts[len(f.reviewer.prompts)-1]
	if !strings.Contains(prompt, "a crew-mate's review of pull request #41 asked for changes: the export drops the new field") {
		t.Fatalf("prompt lacks the crew rework:\n%s", prompt)
	}
}

func TestEveryMachineChoosesTheSameCrewMateToReview(t *testing.T) {
	s := &Supervisor{Settings: config.Supervisor{GoalLabels: "goal:"}, CrewRepos: []string{"org/app"}, Now: time.Now, Logf: func(string, ...any) {},
		Machine: config.Machine{Workers: []config.Worker{{Name: "a", Crew: true, Session: "box-a", PeerList: []config.Peer{{Session: "box-b"}, {Session: "mini-c"}}}}}}
	goal := func(n int, session string) backlog.Issue { return signedBy(labeled(n, "goal:export"), session, "x/") }
	s.Lister = &fakeLister{issues: []backlog.Issue{goal(1, "box-a"), goal(2, "box-b"), goal(3, "mini-c")}}
	for author, want := range map[string]string{"box-a": "box-b", "box-b": "mini-c", "mini-c": "box-a"} {
		if got := s.crewReviewer(context.Background(), goal(9, author)); got != want {
			t.Errorf("reviewer of %s's work = %q, want %q", author, got, want)
		}
	}
	if got := s.crewReviewer(context.Background(), goal(9, "box-docs")); got != "" {
		t.Errorf("an author outside the crew has reviewer %q", got)
	}
	s.Lister = &fakeLister{issues: []backlog.Issue{goal(1, "box-a")}}
	s.crew = nil
	if got := s.crewReviewer(context.Background(), goal(9, "box-a")); got != "" {
		t.Errorf("a goal with no other crew-mate has reviewer %q", got)
	}
}
