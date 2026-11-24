package supervisor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

type fakeCloser struct {
	merge       backlog.Merge
	regressions []int
	closed      []string
	comments    []string
	asked       int
}

func (f *fakeCloser) MergeOf(context.Context, string, int) (backlog.Merge, error) {
	f.asked++
	return f.merge, nil
}

func (f *fakeCloser) Regressions(context.Context, string, int) ([]int, error) {
	return f.regressions, nil
}

func (f *fakeCloser) Close(_ context.Context, repo string, number int, comment string) error {
	f.closed = append(f.closed, fmt.Sprintf("%s#%d", repo, number))
	f.comments = append(f.comments, comment)
	return nil
}

// finishedIssue is an issue whose pull request #41 merged, with nothing
// left to do.
func finishedIssue(number int, handedBack time.Time, after ...string) backlog.Issue {
	i := labeled(number)
	i.Comments = []backlog.Comment{{Body: "## Hand-back\n**PR:** #41 (ready)\n**Needs eyes:** nothing\n**Decisions needed:** none", CreatedAt: handedBack}}
	for _, body := range after {
		i.Comments = append(i.Comments, backlog.Comment{Body: body, CreatedAt: handedBack})
	}
	i.OpenPRs, i.MergedPRs = map[int]backlog.PR{}, map[int]bool{41: true}
	return i
}

func closing(t *testing.T, closer *fakeCloser, issue backlog.Issue, repo config.Repo) *fixture {
	t.Helper()
	f := started(t, config.Supervisor{}, done, done)
	f.Closer = closer
	f.Repos = []config.Repo{repo}
	built(t, f, issue.Number)
	f.lister.issues = []backlog.Issue{issue}
	return f
}

func TestAFinishedIssueIsClosedAfterItsQuietPeriodWithGreenChecksAndNoRegression(t *testing.T) {
	closer := &fakeCloser{merge: backlog.Merge{At: t0.Add(-30 * time.Hour), Commit: "abc1234", Checks: backlog.CIPassed}}
	f := closing(t, closer, finishedIssue(12, t0.Add(-31*time.Hour)), config.Repo{Name: "org/app", CloseFinished: true})
	f.tick(2 * time.Minute)
	if len(closer.closed) != 1 || closer.closed[0] != "org/app#12" {
		t.Fatalf("closed = %q, want #12", closer.closed)
	}
	for _, want := range []string{backlog.ClosedPhrase, "pull request #41 merged", "checks on its merge commit abc1234 passed", "Regression of #12"} {
		if !strings.Contains(closer.comments[0], want) {
			t.Errorf("comment lacks %q:\n%s", want, closer.comments[0])
		}
	}
}

func TestAFinishedIssueStaysOpenWhileAnyConditionIsMissing(t *testing.T) {
	quiet := t0.Add(-30 * time.Hour)
	for _, tc := range []struct {
		name  string
		merge backlog.Merge
		regs  []int
		issue backlog.Issue
		repo  config.Repo
	}{
		{"close_finished is off", backlog.Merge{At: quiet, Checks: backlog.CIPassed}, nil, finishedIssue(12, quiet), config.Repo{Name: "org/app"}},
		{"inside the quiet period", backlog.Merge{At: t0.Add(-2 * time.Hour), Checks: backlog.CIPassed}, nil, finishedIssue(12, quiet), config.Repo{Name: "org/app", CloseFinished: true}},
		{"a shorter quiet period has not passed", backlog.Merge{At: t0.Add(-2 * time.Hour), Checks: backlog.CIPassed}, nil, finishedIssue(12, quiet), config.Repo{Name: "org/app", CloseFinished: true, CloseAfter: "3h"}},
		{"the merge commit's checks failed", backlog.Merge{At: quiet, Checks: backlog.CIFailed}, nil, finishedIssue(12, quiet), config.Repo{Name: "org/app", CloseFinished: true}},
		{"the merge commit's checks still run", backlog.Merge{At: quiet, Checks: backlog.CIPending}, nil, finishedIssue(12, quiet), config.Repo{Name: "org/app", CloseFinished: true}},
		{"a regression names it", backlog.Merge{At: quiet, Checks: backlog.CIPassed}, []int{30}, finishedIssue(12, quiet), config.Repo{Name: "org/app", CloseFinished: true}},
		{"an owner act is left", backlog.Merge{At: quiet, Checks: backlog.CIPassed}, nil, finishedIssue(12, quiet, "## Hand-back\n**PR:** #41 (merged)\n**Owner act:** run the backfill"), config.Repo{Name: "org/app", CloseFinished: true}},
	} {
		closer := &fakeCloser{merge: tc.merge, regressions: tc.regs}
		f := closing(t, closer, tc.issue, tc.repo)
		f.tick(2 * time.Minute)
		if len(closer.closed) != 0 {
			t.Errorf("%s: closed = %q, want none", tc.name, closer.closed)
		}
	}
}

func TestTheMergeOfAFinishedIssueIsNotAskedOnEveryCheckIn(t *testing.T) {
	closer := &fakeCloser{merge: backlog.Merge{At: t0.Add(-time.Hour), Checks: backlog.CIPassed}}
	f := closing(t, closer, finishedIssue(12, t0.Add(-2*time.Hour)), config.Repo{Name: "org/app", CloseFinished: true})
	f.tick(2 * time.Minute)
	f.tick(5 * time.Minute)
	if closer.asked != 1 {
		t.Fatalf("asked = %d, want once within half an hour", closer.asked)
	}
}

func TestAHandBackAfterReworkThatLeftTheHeadWhereItWasGoesBack(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"},
		Verdict{Verdict: runlog.VerdictNudge, Message: "PR #41 conflicts with main. Rebase it and nothing else.", Issue: 12, Reason: "its pull request went stale"},
		done)
	stale := handedBackWith(12, "conflicts with the base branch")
	stale.OpenPRs[41] = backlog.PR{Problem: "conflicts with the base branch", Head: "abc1234def"}
	f.lister.issues = []backlog.Issue{stale}
	f.tick(2 * time.Minute)
	if c := f.lastCheckin(t); c.Rework == "" || c.Head != "abc1234def" {
		t.Fatalf("check-in = %+v, want the rework sent with its head", c)
	}
	// The worker reports a pushed fix, but the head never moved.
	stale.Comments = append(stale.Comments, backlog.Comment{Body: "## Hand-back\n**PR:** #41 (ready)\nPushed the rebase.", CreatedAt: f.now.Add(time.Minute)})
	stale.OpenPRs[41] = backlog.PR{Head: "abc1234def"}
	f.lister.issues = []backlog.Issue{stale}
	f.term.running(f.now.Add(40 * time.Minute))
	f.tick(42 * time.Minute)
	prompt := f.reviewer.prompts[len(f.reviewer.prompts)-1]
	if !strings.Contains(prompt, "[rework: the hand-back after rework left pull request #41 at abc1234") {
		t.Fatalf("the unmoved head is not rework:\n%s", prompt)
	}
}
