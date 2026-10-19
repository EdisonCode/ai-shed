package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/backlog"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/risk"
	"github.com/edisoncode/ai-shed/internal/runlog"
)

type fakeDiffs struct {
	diffs map[int]risk.Diff
	asked int
}

func (f *fakeDiffs) Diff(_ context.Context, _ string, pr int) (risk.Diff, error) {
	f.asked++
	return f.diffs[pr], nil
}

// withVerifier makes the fixture's repository verify, and gives the machine
// a builder (app) and a verifier (rev).
func withVerifier(t *testing.T, f *fixture) *fakeDiffs {
	f.Repos = []config.Repo{{Name: "org/app", Verify: &config.Verify{Areas: []config.Area{{Name: "billing", Paths: []string{"internal/billing/"}}}}}}
	f.Machine.Workers = append(f.Machine.Workers, config.Worker{Name: "rev", Dir: t.TempDir(), Brief: "Verify.", Verifies: true})
	diffs := &fakeDiffs{diffs: map[int]risk.Diff{41: {Files: []string{"internal/billing/refund.go", "internal/billing/refund_test.go"}, Lines: 30}}}
	f.Diffs = diffs
	return diffs
}

// built records that app was handed the issue to build it.
func built(t *testing.T, f *fixture, issue int) {
	appendCheckin(t, f, runlog.Checkin{Time: f.now.Add(-time.Hour), Worker: "app", Kind: KindIdle, Verdict: runlog.VerdictNudge, Sent: true, Issue: issue})
}

func lastOf(t *testing.T, f *fixture, worker string) runlog.Checkin {
	t.Helper()
	var last runlog.Checkin
	for _, c := range f.checkins(t) {
		if c.Worker == worker {
			last = c
		}
	}
	return last
}

func TestAHandedBackPullRequestGoesToAVerifierAtItsTier(t *testing.T) {
	f := started(t, config.Supervisor{Models: config.Models{Default: "sonnet", Verify: map[string]string{"high": "opus", "low": "haiku"}}}, done, assign(12))
	withVerifier(t, f)
	built(t, f, 12)
	f.lister.issues = []backlog.Issue{handedBackWith(12, "")}
	f.tick(2 * time.Minute)
	f.tick(10 * time.Minute)
	c := lastOf(t, f, "rev")
	if c.Issue != 12 || !c.Verify || c.Model != "opus" {
		t.Fatalf("rev's check-in = %+v, want #12 handed over to verify, on the high-tier model", c)
	}
	for _, want := range []string{"Verify pull request #41 of #12 at tier high (touches billing)", "`Verified (high):`", "`Verification failed (high):`"} {
		if !strings.Contains(c.Message, want) {
			t.Fatalf("message = %q, lacks %q", c.Message, want)
		}
	}
	if !slices.Contains(f.term.sent, "/clear") {
		t.Fatalf("sent = %q, want a fresh context for the verification", f.term.sent)
	}
}

func TestTheBuilderIsNotHandedItsOwnPullRequestToVerify(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	withVerifier(t, f)
	f.Machine.Workers[0].Verifies = true // app verifies too, and built #12
	built(t, f, 12)
	f.lister.issues = []backlog.Issue{handedBackWith(12, "")}
	f.tick(2 * time.Minute)
	f.tick(10 * time.Minute)
	if c := lastOf(t, f, "app"); c.Issue == 12 {
		t.Fatalf("app's check-in = %+v: it was handed its own work to verify", c)
	}
	if c := lastOf(t, f, "rev"); c.Issue != 12 || !c.Verify {
		t.Fatalf("rev's check-in = %+v, want the verification", c)
	}
}

func TestAnIssueWaitingForItsVerificationIsNotBuiltAgain(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	withVerifier(t, f)
	f.Machine.Workers = f.Machine.Workers[:1] // the verifier is elsewhere
	f.lister.issues = []backlog.Issue{handedBackWith(12, ""), labeled(13)}
	f.tick(2 * time.Minute)
	if prompt := f.reviewer.prompts[0]; !strings.Contains(prompt, "org/app#12 Issue [held: pull request #41 waits for a verifier]") {
		t.Fatalf("prompt:\n%s", prompt)
	}
	f.tick(10 * time.Minute)
	if c := f.lastCheckin(t); c.Issue != 13 {
		t.Fatalf("check-in = %+v, want #13: #12 waits for its verification", c)
	}
}

func TestAPullRequestWhoseChecksStillRunWaitsForThem(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	diffs := withVerifier(t, f)
	running := handedBackWith(12, "")
	running.OpenPRs[41] = backlog.PR{Pending: true}
	f.lister.issues = []backlog.Issue{running}
	f.tick(2 * time.Minute)
	f.tick(10 * time.Minute)
	if c := lastOf(t, f, "rev"); c.Verify || diffs.asked != 0 {
		t.Fatalf("rev's check-in = %+v, diff asked %d times: verified before the checks finished", c, diffs.asked)
	}
}

func TestAFailedVerificationSendsTheWorkBackToABuilder(t *testing.T) {
	f := started(t, config.Supervisor{}, done)
	withVerifier(t, f)
	appendCheckin(t, f, runlog.Checkin{Time: f.now, Worker: "rev", Kind: KindIdle, Verdict: runlog.VerdictNudge, Sent: true, Issue: 12, Verify: true})
	failed := handedBackWith(12, "")
	failed.Comments = append(failed.Comments, backlog.Comment{Body: "Verification failed (high): a refund of 0 is accepted"})
	f.lister.issues = []backlog.Issue{failed}
	f.tick(2 * time.Minute)
	prompt := f.reviewer.prompts[0]
	if !strings.Contains(prompt, "[rework: the verifier sent pull request #41 back: a refund of 0 is accepted") {
		t.Fatalf("app's queue does not carry the rework, although rev, which only verified it, has it in hand:\n%s", prompt)
	}
}

func TestBriefOfAVerifierSaysHowToVerify(t *testing.T) {
	f := newFixture(t, config.Supervisor{})
	withVerifier(t, f)
	f.tick(0)
	brief, _ := os.ReadFile(filepath.Join(f.Machine.Workers[1].Dir, BriefFile))
	for _, want := range []string{"## Verification", "Low tier", "High tier: try to break it", "`Verified (<tier>):`", "`Verification failed (<tier>):`", "`Verified (<tier>, owner merges):`", "Change no code"} {
		if !strings.Contains(string(brief), want) {
			t.Fatalf("brief lacks %q:\n%s", want, brief)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, BriefFile)); strings.Contains(string(b), "## Verification") {
		t.Fatal("a worker that does not verify has the Verification section")
	}
}

func TestAHandOverAfterAHandBackSaysWhatTheWorkerHadAndWhyNothingIsLeftInIt(t *testing.T) {
	f := started(t, config.Supervisor{CacheTTL: "1h"}, assign(12), onTrack, onTrack)
	withVerifier(t, f)
	f.Machine.Workers = f.Machine.Workers[:1] // the verifier is elsewhere
	f.lister.issues = []backlog.Issue{labeled(12), labeled(13)}
	f.tick(2 * time.Minute) // it takes #12

	f.lister.issues = []backlog.Issue{handedBackWith(12, ""), labeled(13)}
	f.term.running(f.now.Add(time.Minute))
	for range 6 {
		f.tick(5 * time.Minute)
	}
	c := f.lastCheckin(t)
	want := "with nothing it can act on in #12 (pull request #41 waits for a verifier), after a check-in that found it on_track; handed #13 over without the reviewer"
	if c.Issue != 13 || !c.Sent || !strings.Contains(c.Reason, want) || !strings.HasPrefix(c.Reason, "it rested ") {
		t.Fatalf("check-in = %+v, want #13 handed over with reason %q", c, want)
	}
}

func TestAPullRequestTakenOffTheTrainForNewCommitsGoesToAVerifierAgain(t *testing.T) {
	f := started(t, config.Supervisor{}, done, assign(12))
	withVerifier(t, f)
	built(t, f, 12)
	reboard := handedBackWith(12, "")
	reboard.Comments = append(reboard.Comments,
		backlog.Comment{Body: "Verified (low): ran the export tests"},
		backlog.Comment{Body: backlog.OffTrainPhrase + " pull request #41, at c0ffee4, had new commits (f1x, was c0ffee4) while batch pull request #101 tested it. " + backlog.ReverifyPhrase + " at its new head."})
	f.lister.issues = []backlog.Issue{reboard}
	f.tick(2 * time.Minute)
	f.tick(10 * time.Minute)
	c := lastOf(t, f, "rev")
	if c.Issue != 12 || !c.Verify {
		t.Fatalf("rev's check-in = %+v, want #12 verified again", c)
	}
	if !strings.Contains(c.Message, "re-verify: a verifier passed it at c0ffee4") {
		t.Fatalf("message = %q, want the head it was verified at named", c.Message)
	}
}

func TestALookAskedOfUnmergedWorkDoesNotHoldItsVerification(t *testing.T) {
	f := started(t, config.Supervisor{}, done, assign(12))
	withVerifier(t, f)
	built(t, f, 12)
	eyes := handedBackWith(12, "")
	eyes.Comments[0].Body += "\n**Needs eyes:** open /orders and check the total"
	f.lister.issues = []backlog.Issue{eyes}
	f.tick(2 * time.Minute)
	f.tick(10 * time.Minute)
	if c := lastOf(t, f, "rev"); c.Issue != 12 || !c.Verify {
		t.Fatalf("rev's check-in = %+v, want #12 verified while its look waits for the merge", c)
	}
}
