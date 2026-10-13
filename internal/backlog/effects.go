package backlog

import (
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

// What answering an ask does, which decides the order the owner takes them
// in. A worker waits behind a return; nobody waits behind a read or a merge.
const (
	// EffectReturns: answering hands the issue back to a worker, ahead of
	// new work. A parked decision, an issue that was not ready.
	EffectReturns = "returns"
	// EffectGrace: a decision the worker still waits on, until its grace
	// period ends.
	EffectGrace = "in grace"
	// EffectRead: the owner reads something: a diff, a page on staging.
	EffectRead = "read"
	// EffectMerge: a review a verifier passed at low risk, with its checks
	// green. Merging stays the owner's call.
	EffectMerge = "merge"
)

// Effects returns the issue's asks, each with what answering it does.
func Effects(issue Issue, signals []config.Signal, now time.Time) []Ask {
	asks := Asks(issue, signals)
	for n := range asks {
		asks[n].Effect = effect(issue, signals, asks[n], now)
	}
	return asks
}

func effect(issue Issue, signals []config.Signal, a Ask, now time.Time) string {
	var s config.Signal
	for _, sig := range signals {
		if sig.Name == a.Name {
			s = sig
		}
	}
	switch {
	case s.FollowsPR:
		v := Verification(issue, signals)
		pr := issue.OpenPRs[a.PR]
		if v.State == VerifyPassed && v.Tier == config.TierLow && pr.Problem == "" && !pr.Pending && !pr.Draft {
			return EffectMerge
		}
		return EffectRead
	case s.SendsBack:
		if grace := s.GraceOrDefault(); grace > 0 && !a.Since.IsZero() && now.Before(a.Since.Add(grace)) {
			return EffectGrace
		}
		return EffectReturns
	case s.Name == config.ReadySignal:
		return EffectReturns
	}
	return EffectRead
}
