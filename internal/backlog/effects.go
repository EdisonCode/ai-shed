package backlog

import (
	"strings"
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

// DefaultOutcome is what came of the safe defaults one hand-back recorded:
// they held, or the owner overruled them before the next hand-back. A
// ruling cannot say which of several entries it overrules, so the
// hand-back is the unit.
type DefaultOutcome struct {
	Entries   []string `json:"entries"`
	Overruled bool     `json:"overruled"`
	// Ruling is the owner's words after the answering phrase.
	Ruling string `json:"ruling,omitempty"`
}

// DefaultOutcomes reads the safe defaults of each of the issue's hand-backs
// and whether the owner overruled them. An acceptance holds them, and so
// does silence: a default nobody looked at shipped as the worker chose it.
func DefaultOutcomes(issue Issue, signals []config.Signal) []DefaultOutcome {
	var s config.Signal
	for _, sig := range signals {
		if sig.Decided != "" {
			s = sig
		}
	}
	if s.Decided == "" {
		return nil
	}
	var out []DefaultOutcome
	open := -1 // the outcome the next ruling applies to
	for _, c := range issue.Comments {
		body := strings.ToLower(c.Body)
		if s.HandBack == "" || strings.Contains(body, strings.ToLower(s.HandBack)) {
			open = -1
			if entries := decidedIn(c.Body, s); len(entries) > 0 {
				out = append(out, DefaultOutcome{Entries: entries})
				open = len(out) - 1
			}
			continue
		}
		if open >= 0 && s.AnsweredBy != "" && strings.Contains(body, strings.ToLower(s.AnsweredBy)) {
			if !accepts(c.Body, s) {
				value, _ := afterPhrase(body, strings.ToLower(s.AnsweredBy))
				out[open].Overruled = true
				out[open].Ruling = firstLine(strings.TrimLeft(c.Body[len(c.Body)-len(value):], ": \t*_"))
			}
			open = -1
		}
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}
