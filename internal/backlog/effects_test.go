package backlog

import (
	"testing"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

func TestEachAskSaysWhatAnsweringItDoes(t *testing.T) {
	signals := config.DefaultSignals()
	t0 := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	at := func(comments ...string) Issue {
		i := Issue{Repo: "org/app", Number: 12, OpenPRs: map[int]PR{41: {}}}
		for n, body := range comments {
			i.Comments = append(i.Comments, Comment{Body: body, CreatedAt: t0.Add(time.Duration(n) * time.Minute)})
		}
		return i
	}
	effect := func(i Issue, now time.Time) string {
		asks := Effects(i, signals, now)
		if len(asks) != 1 {
			t.Fatalf("asks = %+v, want one", asks)
		}
		return asks[0].Effect
	}
	decision := at("## Hand-back\n**Decisions needed:** drop the column?")
	if got := effect(decision, t0.Add(5*time.Minute)); got != EffectGrace {
		t.Errorf("a decision inside its grace = %q", got)
	}
	if got := effect(decision, t0.Add(time.Hour)); got != EffectReturns {
		t.Errorf("a parked decision = %q", got)
	}
	if got := effect(at("## Hand-back\n**Not ready:** no acceptance"), t0); got != EffectReturns {
		t.Errorf("not ready = %q", got)
	}
	review := at("## Hand-back\n**PR:** #41 (ready)")
	if got := effect(review, t0); got != EffectRead {
		t.Errorf("a review that was not verified = %q", got)
	}
	review.Verify = true
	review.Comments = append(review.Comments, Comment{Body: "Verified (low): ran the tests", CreatedAt: t0.Add(time.Minute)})
	if got := effect(review, t0); got != EffectMerge {
		t.Errorf("a review verified at low = %q", got)
	}
	review.Comments[1].Body = "Verified (high): tried to break it"
	if got := effect(review, t0); got != EffectRead {
		t.Errorf("a review verified at high = %q", got)
	}
	if got := effect(at("## Hand-back\n**PR:** none\n**Needs eyes:** open /audit"), t0); got != EffectRead {
		t.Errorf("an eye check the owner does = %q", got)
	}
}
