package supervisor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

type postedComments struct{ posted []string }

func (p *postedComments) Comment(_ context.Context, repo string, issue int, body string) error {
	p.posted = append(p.posted, fmt.Sprintf("%s#%d %s", repo, issue, body))
	return nil
}

const pricingBrief = "**Contracts:**\n- GET /prices returns cents, provided by #41, used by #43\n- the price event, provided by #42"

// contractFleet is a machine with a crew worker, whose queue owns the
// slices of goal:pricing given, and the goal's open issues as the crew
// reads them.
func contractFleet(own []backlog.Issue, open []backlog.Issue) (*Supervisor, *postedComments) {
	p := &postedComments{}
	l := &countingLister{byQuery: map[string][]backlog.Issue{
		"org/app ":                       own,
		"org/app label:\"goal:pricing\"": open,
	}}
	return &Supervisor{
		Machine: config.Machine{Name: "box", Issues: []config.IssueSource{{Repo: "org/app", Label: "machine:box"}},
			Workers: []config.Worker{{Name: "app", Crew: true, Session: "box-app"}}},
		Settings: config.Supervisor{GoalLabels: "goal:"}, Signals: config.DefaultSignals(), Lister: l, CrewRepos: []string{"org/app"},
		Contracts: p, Logf: func(string, ...any) {}, Now: func() time.Time { return t0 },
	}, p
}

func pricingSlice(n int, started bool) backlog.Issue {
	i := labeled(n, "machine:box", "goal:pricing")
	if started {
		i = signedBy(i, "box-app", "web/")
	}
	return i
}

func TestAStartedSliceIsToldOnceWhenAContractItUsesChanged(t *testing.T) {
	brief := labeled(40, "goal:pricing")
	brief.Title, brief.Body = "Goal brief: pricing", pricingBrief
	user, other, unstarted := pricingSlice(43, true), pricingSlice(44, true), pricingSlice(45, false)
	// #41 has closed; #42 is still open.
	s, p := contractFleet([]backlog.Issue{user, other, unstarted}, []backlog.Issue{brief, labeled(42, "goal:pricing"), user, other, unstarted})

	s.tellConsumers(context.Background())

	if len(p.posted) != 1 || !strings.HasPrefix(p.posted[0], "org/app#43 Contract changed: GET /prices returns cents") || !strings.Contains(p.posted[0], "org/app#41, which provides it, is closed") {
		t.Fatalf("posted = %q, want #43, the one named user of the closed provider's contract", p.posted)
	}
	user.Comments = append(user.Comments, backlog.Comment{Body: strings.SplitN(p.posted[0], " ", 2)[1]})
	s2, p2 := contractFleet([]backlog.Issue{user, other, unstarted}, []backlog.Issue{brief, labeled(42, "goal:pricing"), user, other, unstarted})
	s2.tellConsumers(context.Background())
	if len(p2.posted) != 0 {
		t.Fatalf("posted again: %q", p2.posted)
	}
}

func TestAProviderGoesBeforeTheSlicesThatUseIt(t *testing.T) {
	brief := labeled(40, "goal:pricing")
	brief.Title, brief.Body = "Goal brief: pricing", pricingBrief
	s, _ := contractFleet(nil, []backlog.Issue{brief})
	items := []QueueItem{{Repo: "org/app", Number: 43}, {Repo: "org/app", Number: 41}}
	s.markProviders(context.Background(), items, nil, []string{"goal:pricing"})
	if items[0].provides || !items[1].provides {
		t.Fatalf("items = %+v, want #41 marked as a provider", items)
	}
}

func TestAHandBackAfterAContractChangeMustSayItChecked(t *testing.T) {
	i := backlog.Issue{Repo: "org/app", Number: 43, OpenPRs: map[int]backlog.PR{51: {}}, Comments: []backlog.Comment{
		{Body: "## Hand-back\n**PR:** #51 (ready)"},
		{Body: "Contract changed: GET /prices returns cents, provided by #41"},
	}}
	if r := backlog.Rework(i, config.DefaultSignals()); !strings.Contains(r, "a contract this slice uses changed (GET /prices returns cents") {
		t.Fatalf("rework = %q", r)
	}
	i.Comments = append(i.Comments, backlog.Comment{Body: "## Hand-back\n**PR:** #51 (ready)\nContract checked: it reads cents now."})
	if r := backlog.Rework(i, config.DefaultSignals()); strings.Contains(r, "contract") {
		t.Fatalf("rework = %q after the check", r)
	}
}
