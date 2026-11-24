package backlog

import (
	"testing"

	"github.com/edisoncode/ai-shed/internal/config"
)

func crewIssue(after ...string) Issue {
	i := Issue{Repo: "org/web", Number: 7, Comments: []Comment{{Body: "## Hand-back\n**PR:** #41 (ready)"}}}
	for _, body := range after {
		i.Comments = append(i.Comments, Comment{Body: body})
	}
	i.OpenPRs = map[int]PR{41: {Head: "abc"}}
	return i
}

func TestACrewReviewIsDueOnceAHandedBackPullRequestIsGreen(t *testing.T) {
	signals := config.DefaultSignals()
	if c := CrewReview(crewIssue(), signals); c.State != CrewDue || c.PR != 41 {
		t.Fatalf("green, unreviewed: %+v", c)
	}
	pending := crewIssue()
	pending.OpenPRs[41] = PR{Pending: true}
	if c := CrewReview(pending, signals); c.State != "" {
		t.Fatalf("checks still running: %+v", c)
	}
	if c := CrewReview(Issue{Number: 7}, signals); c.State != "" {
		t.Fatalf("no hand-back: %+v", c)
	}
}

func TestACrewMatesReviewPassesOrAsksForChanges(t *testing.T) {
	signals := config.DefaultSignals()
	if c := CrewReview(crewIssue("Crew review passed (low): the export matches the API's new field."), signals); c.State != CrewPassed || c.Tier != "low" || c.Note != "the export matches the API's new field." {
		t.Fatalf("pass: %+v", c)
	}
	if c := CrewReview(crewIssue("**Crew review: changes (high):** the button sends the old field name\n- web/export.ts:12"), signals); c.State != CrewChanges || c.Tier != "high" || c.Note != "the button sends the old field name" {
		t.Fatalf("changes: %+v", c)
	}
	// A new hand-back after changes needs a new review.
	again := crewIssue("Crew review: changes (low): wrong field", "## Hand-back\n**PR:** #41 (ready)\nRenamed the field.")
	if c := CrewReview(again, signals); c.State != CrewDue {
		t.Fatalf("after a new hand-back: %+v", c)
	}
}

func TestCrewChangesAreRework(t *testing.T) {
	got := Rework(crewIssue("Crew review: changes (low): the button sends the old field name"), config.DefaultSignals())
	if got != "a crew-mate's review of pull request #41 asked for changes: the button sends the old field name (see the Crew review comment); make them, or answer the crew-mate and hand back saying why not" {
		t.Fatalf("rework = %q", got)
	}
}

func TestWithCrewLowALowCrewPassIsTheVerification(t *testing.T) {
	signals := config.DefaultSignals()
	i := crewIssue("Crew review passed (low): matches the API.")
	i.Verify = true
	if v := Verification(i, signals); v.State != VerifyDue {
		t.Fatalf("without crew_low: %+v", v)
	}
	i.CrewLow = true
	if v := Verification(i, signals); v.State != VerifyPassed || v.Tier != "low" || v.Note != "crew review: matches the API." {
		t.Fatalf("with crew_low: %+v", v)
	}
	high := crewIssue("Crew review passed (high): matches the API.")
	high.Verify, high.CrewLow = true, true
	if v := Verification(high, signals); v.State != VerifyDue {
		t.Fatalf("a high crew pass still needs the verifier: %+v", v)
	}
}

func TestAnAskCarriesWhatTheWorkerWroteUnderIt(t *testing.T) {
	i := Issue{Number: 5, Comments: []Comment{{Body: "## Hand-back\n**PR:** none\n**Decisions needed:** Which currency is the default?\n- Recommend: USD, since every account bills in it.\n**Decided without you:** none"}}}
	asks := Asks(i, config.DefaultSignals())
	if len(asks) != 1 || asks[0].Text != "Which currency is the default?\n- Recommend: USD, since every account bills in it." {
		t.Fatalf("asks = %+v", asks)
	}
}

func TestOnlyADependencyItsTextNamesCanBeDropped(t *testing.T) {
	i := Issue{Repo: "org/app", Number: 5, Body: "Depends on #3", Deps: []Dep{{Repo: "org/app", Number: 3, Open: true}, {Repo: "org/app", Number: 4, Open: true}}}
	got := DroppableDeps(i)
	if len(got) != 1 || got[0].Number != 3 {
		t.Fatalf("droppable = %+v, want #3 only: #4 is GitHub's blocked-by", got)
	}
}
