package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

type queued struct {
	repo    string
	issue   int
	pr      int
	method  string
	comment string
}

type fakeMerger struct{ queued []queued }

func (f *fakeMerger) QueueMerge(_ context.Context, repo string, issue, pr int, method, comment string) error {
	f.queued = append(f.queued, queued{repo, issue, pr, method, comment})
	return nil
}

// verifiedLow is #12, built by app, its pull request #41 verified at low
// risk with green checks.
func verifiedLow(tier string) backlog.Issue {
	i := handedBackWith(12, "")
	i.Comments = append(i.Comments, backlog.Comment{Body: "Verified (" + tier + "): ran the export tests, tried an empty file"})
	return i
}

func withMergeLow(t *testing.T, f *fixture, mergeLow bool) *fakeMerger {
	f.Repos = []config.Repo{{Name: "org/app", Verify: &config.Verify{MergeLow: mergeLow}}}
	merger := &fakeMerger{}
	f.Merger = merger
	built(t, f, 12)
	return merger
}

func TestAPullRequestVerifiedAtLowRiskIsQueuedForMergeByTheMachineThatBuiltIt(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	merger := withMergeLow(t, f, true)
	f.lister.issues = []backlog.Issue{verifiedLow("low")}
	f.tick(2 * time.Minute)
	if len(merger.queued) != 1 {
		t.Fatalf("queued = %+v, want #41 queued once", merger.queued)
	}
	q := merger.queued[0]
	if q.pr != 41 || q.issue != 12 || q.method != "merge" || !strings.HasPrefix(q.comment, backlog.QueuedPhrase) || !strings.Contains(q.comment, "ran the export tests") {
		t.Fatalf("queued = %+v", q)
	}
	f.term.running(f.now)
	f.tick(2 * time.Minute)
	if len(merger.queued) != 1 {
		t.Fatalf("queued %d times", len(merger.queued))
	}
}

func TestOnlyLowRiskAndOnlyWhenTheOwnerTurnedItOn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mergeLow bool
		issue    backlog.Issue
	}{
		{"off", false, verifiedLow("low")},
		{"high tier", true, verifiedLow("high")},
		{"checks running", true, func() backlog.Issue { i := verifiedLow("low"); i.OpenPRs[41] = backlog.PR{Pending: true}; return i }()},
		{"a question waits on the owner", true, func() backlog.Issue {
			i := verifiedLow("low")
			i.Comments = append(i.Comments, backlog.Comment{Body: "## Hand-back\n**PR:** #41 (ready)\n**Decisions needed:** keep the old flag?"}, backlog.Comment{Body: "Verified (low): fine"})
			return i
		}()},
	} {
		f := started(t, config.Supervisor{}, done)
		merger := withMergeLow(t, f, tc.mergeLow)
		f.lister.issues = []backlog.Issue{tc.issue}
		f.tick(2 * time.Minute)
		if len(merger.queued) != 0 {
			t.Errorf("%s: queued %+v", tc.name, merger.queued)
		}
	}
}

// askingEyes is verifiedLow whose hand-back asks for a look at staging.
func askingEyes() backlog.Issue {
	i := verifiedLow("low")
	i.Comments[0].Body += "\n**Needs eyes:** open /orders and check the total"
	return i
}

func TestALookAWorkerDoesAfterTheMergeDoesNotHoldTheMerge(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	merger := withMergeLow(t, f, true)
	f.EyeCheckRepos = []string{"org/app"}
	f.lister.issues = []backlog.Issue{askingEyes()}
	f.tick(2 * time.Minute)
	if len(merger.queued) != 1 {
		t.Fatalf("queued = %+v, want #41 queued: the look happens on staging after the merge", merger.queued)
	}
}

func TestALookTheOwnerMustDoHoldsTheMerge(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	merger := withMergeLow(t, f, true)
	f.lister.issues = []backlog.Issue{askingEyes()}
	f.tick(2 * time.Minute)
	if len(merger.queued) != 0 {
		t.Fatalf("queued %+v on a repository where no worker does eye checks", merger.queued)
	}
}

func TestWithEyeChecksAQuestionStillHoldsTheMerge(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	merger := withMergeLow(t, f, true)
	f.EyeCheckRepos = []string{"org/app"}
	i := askingEyes()
	i.Comments[0].Body += "\n**Decisions needed:** keep the old flag?"
	f.lister.issues = []backlog.Issue{i}
	f.tick(2 * time.Minute)
	if len(merger.queued) != 0 {
		t.Fatalf("queued %+v while a question waits on the owner", merger.queued)
	}
}

func TestAMachineThatDidNotBuildThePullRequestLeavesIt(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	f.Repos = []config.Repo{{Name: "org/app", Verify: &config.Verify{MergeLow: true}}}
	merger := &fakeMerger{}
	f.Merger = merger
	f.lister.issues = []backlog.Issue{verifiedLow("low")}
	f.tick(2 * time.Minute)
	if len(merger.queued) != 0 {
		t.Fatalf("queued %+v by a machine whose log does not show it built #12", merger.queued)
	}
}
