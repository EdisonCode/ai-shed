package supervisor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

type fakeReadier struct{ ready []string }

func (f *fakeReadier) MarkReady(_ context.Context, repo string, issue, pr int, comment string) error {
	f.ready = append(f.ready, fmt.Sprintf("%s#%d (#%d): %s", repo, pr, issue, comment))
	return nil
}

// parksAt gives org/app a limit of ready pull requests, and each issue the
// repository's open pull requests: ready ones from #41, then drafts from #61.
func parksAt(f *fixture, limit, ready, drafts int, issues ...backlog.Issue) *fakeReadier {
	f.Repos = []config.Repo{{Name: "org/app", MaxOpenPRs: limit, MaxOpenPRsCount: config.PRCountReady}}
	readier := &fakeReadier{}
	f.Readier = readier
	for n := range issues {
		if issues[n].OpenPRs == nil {
			issues[n].OpenPRs = map[int]backlog.PR{}
		}
		for pr := 41; pr < 41+ready; pr++ {
			issues[n].OpenPRs[pr] = backlog.PR{}
		}
		for pr := 61; pr < 61+drafts; pr++ {
			if _, set := issues[n].OpenPRs[pr]; !set {
				issues[n].OpenPRs[pr] = backlog.PR{Draft: true}
			}
		}
	}
	f.lister.issues = issues
	return readier
}

// parkedAs is an issue handed back with its draft pull request parked.
func parkedAs(number, pr int, at time.Time) backlog.Issue {
	i := labeled(number)
	i.Comments = []backlog.Comment{{Body: fmt.Sprintf("## Hand-back\n**PR:** #%d (draft, parked)", pr), CreatedAt: at}}
	i.OpenPRs = map[int]backlog.PR{pr: {Draft: true}}
	return i
}

func TestAtTheLimitOfReadyPullRequestsANewIssueIsHandedOverToBeParked(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(11))
	parksAt(f, 2, 2, 3, labeled(11))
	f.tick(2 * time.Minute)
	if prompt := f.reviewer.prompts[0]; !strings.Contains(prompt, "org/app#11 Issue [park: org/app has 2 ready pull requests, at or over its limit of 2; its pull request is opened as a draft and left one]") {
		t.Fatalf("prompt does not show the park:\n%s", prompt)
	}
	c := f.lastCheckin(t)
	if c.Issue != 11 || !c.Sent || !strings.Contains(c.Message, "open the pull request of #11 as a draft and do not mark it ready") || !strings.Contains(c.Message, "(draft, parked)") {
		t.Fatalf("check-in = %+v, want #11 handed over with the park", c)
	}
}

func TestDraftsDoNotCountTowardALimitOfReadyPullRequests(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(11))
	parksAt(f, 2, 1, 5, labeled(11))
	f.tick(2 * time.Minute)
	if c := f.lastCheckin(t); c.Issue != 11 || strings.Contains(c.Message, "draft") {
		t.Fatalf("check-in = %+v, want #11 handed over with nothing parked", c)
	}
}

func TestTheOldestParkedDraftIsMarkedReadyWhenThereIsRoom(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	newer, older := parkedAs(12, 62, t0.Add(-time.Hour)), parkedAs(13, 61, t0.Add(-2*time.Hour))
	readier := parksAt(f, 2, 1, 2, newer, older)
	built(t, f, 12)
	built(t, f, 13)
	f.tick(2 * time.Minute)
	if len(readier.ready) != 1 || !strings.HasPrefix(readier.ready[0], "org/app#61 (#13): Unparked: pull request #61 is marked ready") {
		t.Fatalf("ready = %q, want the oldest parked draft, and only one: the limit is 2 and one is ready", readier.ready)
	}
	f.tick(2 * time.Minute) // the list still shows #61 as a draft
	if len(readier.ready) != 1 {
		t.Fatalf("ready = %q: #61 counts as ready once it was marked", readier.ready)
	}
}

func TestAParkedDraftWaitsAtTheLimit(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	readier := parksAt(f, 2, 2, 1, parkedAs(12, 61, t0))
	built(t, f, 12)
	f.tick(2 * time.Minute)
	if len(readier.ready) != 0 {
		t.Fatalf("ready = %q, want nothing marked ready at the limit", readier.ready)
	}
}

func TestADraftThatSaysOtherwiseIsNotUnparked(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	i := parkedAs(12, 61, t0)
	i.Comments[0].Body = "## Hand-back\n**PR:** #61 (draft, waits on the API change)"
	readier := parksAt(f, 2, 0, 1, i)
	built(t, f, 12)
	f.tick(2 * time.Minute)
	if !slices.Equal(readier.ready, nil) {
		t.Fatalf("ready = %q, want a draft for another reason left alone", readier.ready)
	}
}
