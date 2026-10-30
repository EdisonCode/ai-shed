package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/looks"
	"github.com/edisoncode/ai-shed/internal/risk"
)

func TestAnEyeCheckRunsOnItsOwnModelAndTheNextIssueSwitchesBack(t *testing.T) {
	m := config.Models{Default: "opus", EyeChecks: "haiku"}
	f := started(t, config.Supervisor{Models: m}, assign(7), assign(12))
	withStaging(f, look(7, 41, looks.Due))
	f.tick(2 * time.Minute)
	if len(f.term.sent) != 3 || f.term.sent[0] != "/clear" || f.term.sent[1] != "/model haiku" || !strings.Contains(f.term.sent[2], "Do the eye check of #7") {
		t.Fatalf("sent = %q, want a clear, the eye-check model, then the look", f.term.sent)
	}
	if c := f.lastCheckin(t); c.Model != "haiku" || !c.Look {
		t.Fatalf("check-in = %+v", c)
	}

	f.Looks.(*fakeLooks).looks = nil
	f.term.sent = nil
	f.term.running(f.now.Add(10 * time.Minute))
	f.tick(12 * time.Minute)
	if len(f.term.sent) != 3 || f.term.sent[1] != "/model opus" {
		t.Fatalf("sent = %q, want the issue's own model back", f.term.sent)
	}
}

// lookOfTier hands app the look of #7, whose pull request #41 has this diff.
func lookOfTier(t *testing.T, m config.Models, diffs *fakeDiffs) *fixture {
	f := started(t, config.Supervisor{Models: m}, assign(7), assign(12))
	withStaging(f, look(7, 41, looks.Due))
	f.Diffs = diffs
	f.tick(2 * time.Minute)
	return f
}

var (
	riskyDiff = map[int]risk.Diff{41: {Files: []string{"internal/billing/refund.go"}, Lines: 30}}
	safeDiff  = map[int]risk.Diff{41: {Files: []string{"internal/billing/refund.go", "internal/billing/refund_test.go"}, Lines: 30}}
)

func TestTheLookOfARiskyPullRequestRunsOnItsTiersModel(t *testing.T) {
	m := config.Models{Default: "sonnet", EyeChecks: "haiku", EyeChecksByTier: map[string]string{config.TierHigh: "opus"}}
	f := lookOfTier(t, m, &fakeDiffs{diffs: riskyDiff})
	if !slices.Contains(f.term.sent, "/model opus") {
		t.Fatalf("sent = %q, want the look of a high-risk pull request on opus", f.term.sent)
	}
}

func TestTheLookOfATierWithNoModelRunsOnTheEyeCheckModel(t *testing.T) {
	m := config.Models{Default: "sonnet", EyeChecks: "haiku", EyeChecksByTier: map[string]string{config.TierHigh: "opus"}}
	f := lookOfTier(t, m, &fakeDiffs{diffs: safeDiff})
	if !slices.Contains(f.term.sent, "/model haiku") {
		t.Fatalf("sent = %q, want the look of a low-risk pull request on the eye-check model", f.term.sent)
	}
}

func TestALookWhoseTierCannotBeReadIsStillHandedOver(t *testing.T) {
	m := config.Models{Default: "sonnet", EyeChecks: "haiku", EyeChecksByTier: map[string]string{config.TierHigh: "opus"}}
	f := lookOfTier(t, m, &fakeDiffs{err: errors.New("gh: HTTP 502")})
	if !slices.Contains(f.term.sent, "/model haiku") || !strings.Contains(f.term.sent[len(f.term.sent)-1], "Do the eye check of #7") {
		t.Fatalf("sent = %q, want the look on the eye-check model", f.term.sent)
	}
}

func TestAnOrchestratorGoesBackToItsSessionModelAfterAnEyeCheck(t *testing.T) {
	m := config.Models{Default: "opus", Apply: config.ModelTell, Session: "opus", EyeChecks: "haiku"}
	f := started(t, config.Supervisor{Models: m}, assign(7), assign(12))
	withStaging(f, look(7, 41, looks.Due))
	f.tick(2 * time.Minute)
	if !slices.Contains(f.term.sent, "/model haiku") {
		t.Fatalf("sent = %q, want the look on the eye-check model, not told", f.term.sent)
	}

	f.Looks.(*fakeLooks).looks = nil
	f.term.sent = nil
	f.term.running(f.now.Add(10 * time.Minute))
	f.tick(12 * time.Minute)
	if !slices.Contains(f.term.sent, "/model opus") || !strings.Contains(f.term.sent[len(f.term.sent)-1], "The model for this issue is opus.") {
		t.Fatalf("sent = %q, want the session model back, and the issue's model told", f.term.sent)
	}
}

func TestOrdersNameTheWorkersOwnTestIdentity(t *testing.T) {
	f := newFixture(t, config.Supervisor{}, onTrack)
	withStaging(f)
	f.Repos[0].Staging.Orders = "Sign in as qa-{worker}@example.com. Never use another worker's user."
	f.tick(0)
	brief, _ := os.ReadFile(filepath.Join(f.dir, BriefFile))
	if !strings.Contains(string(brief), "Sign in as qa-app@example.com.") {
		t.Fatalf("brief lacks the worker's own identity:\n%s", brief)
	}
}

func TestAWorkersEnvironmentReachesItsSession(t *testing.T) {
	f := newFixture(t, config.Supervisor{}, onTrack)
	f.Machine.Workers[0].Env = map[string]string{"QA_USER": "qa-app", "QA_NOTE": "it's mine"}
	f.tick(0)
	want := `QA_NOTE='it'\''s mine' QA_USER='qa-app' claude --permission-mode auto`
	if len(f.term.sent) != 1 || !strings.HasPrefix(f.term.sent[0], want) {
		t.Fatalf("sent = %q, want the session started with %s", f.term.sent, want)
	}
}
