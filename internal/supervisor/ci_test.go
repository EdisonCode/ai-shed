package supervisor

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
)

type fakeCI struct{ restarted []string }

func (f *fakeCI) RestartCI(_ context.Context, repo string, pr int) error {
	f.restarted = append(f.restarted, fmt.Sprintf("%s#%d", repo, pr))
	return nil
}

func TestAReadyPullRequestWhoseHeadItsCISkippedHasItsCIStartedOncePerHead(t *testing.T) {
	f := started(t, config.Supervisor{}, done, done, done)
	ci := &fakeCI{}
	f.CI = ci
	built(t, f, 12)
	skipped := handedBackWith(12, "")
	skipped.OpenPRs[41] = backlog.PR{NoCI: true, Head: "c0ffee1"}
	f.lister.issues = []backlog.Issue{skipped}
	f.tick(2 * time.Minute)
	f.tick(10 * time.Minute)
	if !slices.Equal(ci.restarted, []string{"org/app#41"}) {
		t.Fatalf("restarted = %q, want #41's CI started again once", ci.restarted)
	}
	skipped.OpenPRs[41] = backlog.PR{NoCI: true, Head: "f1x"}
	f.tick(10 * time.Minute)
	if len(ci.restarted) != 2 {
		t.Fatalf("restarted = %q, want a new head started again too", ci.restarted)
	}
}

func TestSkippedCIIsLeftAloneOnADraftOrWhatAnotherMachineBuilt(t *testing.T) {
	f := started(t, config.Supervisor{}, done, done)
	ci := &fakeCI{}
	f.CI = ci
	built(t, f, 12)
	draft := handedBackWith(12, "")
	draft.OpenPRs[41] = backlog.PR{NoCI: true, Draft: true, Head: "c0ffee1"}
	elsewhere := handedBackWith(13, "")
	elsewhere.Comments[0].Body = "## Hand-back\n**PR:** #42 (ready)"
	elsewhere.OpenPRs = map[int]backlog.PR{42: {NoCI: true, Head: "c0ffee2"}}
	f.lister.issues = []backlog.Issue{draft, elsewhere}
	f.tick(2 * time.Minute)
	f.tick(10 * time.Minute)
	if len(ci.restarted) != 0 {
		t.Fatalf("restarted = %q, want none", ci.restarted)
	}
}
