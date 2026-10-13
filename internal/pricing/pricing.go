// Package pricing turns a model call's tokens into dollars at Anthropic's
// published API rates. A fleet on a subscription pays no such price; the
// dollars are the weights that make tokens of different kinds and models
// comparable, since a subscription's limits count them differently too.
package pricing

import (
	"regexp"
	"strings"
)

// Rate is what each kind of token costs, in US dollars per million tokens.
type Rate struct {
	Input        float64 `yaml:"input" json:"input"`
	CacheWrite5m float64 `yaml:"cache_write_5m" json:"cache_write_5m"`
	CacheWrite1h float64 `yaml:"cache_write_1h" json:"cache_write_1h"`
	CacheRead    float64 `yaml:"cache_read" json:"cache_read"`
	Output       float64 `yaml:"output" json:"output"`
}

// Price is a model's rate. A model priced by prompt length pays Long for a
// call whose prompt (uncached input, cache writes and cache reads) is over
// LongOver tokens.
type Price struct {
	Rate
	LongOver int
	Long     Rate
}

// Tokens are one call's, or a sum of calls'. CacheWrite is every cache
// write; CacheWrite1h is the part of it written for an hour.
type Tokens struct {
	Input, CacheWrite, CacheWrite1h, CacheRead, Output int
}

// Table holds a price per model id.
type Table map[string]Price

// published is the model pricing table of
// https://platform.claude.com/docs/en/about-claude/pricing, read on
// 2026-10-12: first-party API rates, global routing, standard speed. Fast
// mode, US-only inference and batch discounts are not applied.
var published = Table{
	"claude-fable-5-1":  {Rate: Rate{10, 12.50, 20, 0.25, 50}},
	"claude-mythos-5-1": {Rate: Rate{10, 12.50, 20, 0.25, 50}},
	"claude-fable-5":    {Rate: Rate{10, 12.50, 20, 1, 50}},
	"claude-mythos-5":   {Rate: Rate{10, 12.50, 20, 1, 50}},
	"claude-opus-5-5":   {Rate: Rate{4, 5, 8, 0.20, 20}},
	"claude-opus-5":     {Rate: Rate{5, 6.25, 10, 0.50, 25}},
	"claude-opus-4-8":   {Rate: Rate{5, 6.25, 10, 0.50, 25}},
	"claude-opus-4-7":   {Rate: Rate{5, 6.25, 10, 0.50, 25}},
	"claude-opus-4-6":   {Rate: Rate{5, 6.25, 10, 0.50, 25}},
	"claude-opus-4-5":   {Rate: Rate{5, 6.25, 10, 0.50, 25}},
	"claude-opus-4-1":   {Rate: Rate{15, 18.75, 30, 1.50, 75}},
	"claude-opus-4":     {Rate: Rate{15, 18.75, 30, 1.50, 75}},
	"claude-sonnet-5-5": {Rate: Rate{2, 2.50, 4, 0.10, 10}},
	"claude-sonnet-5":   {Rate: Rate{2, 2.50, 4, 0.20, 10}},
	"claude-sonnet-4-6": {Rate: Rate{3, 3.75, 6, 0.30, 15}},
	"claude-sonnet-4-5": {Rate: Rate{3, 3.75, 6, 0.30, 15}},
	"claude-sonnet-4":   {Rate: Rate{3, 3.75, 6, 0.30, 15}},
	"claude-haiku-5-5":  {Rate: Rate{0.10, 0.125, 0.20, 0.01, 0.50}, LongOver: 100_000, Long: Rate{0.50, 0.625, 1, 0.05, 2.50}},
	"claude-haiku-4-5":  {Rate: Rate{1, 1.25, 2, 0.10, 5}},
	"claude-3-5-haiku":  {Rate: Rate{0.80, 1, 1.60, 0.08, 4}},
}

// Default is the published table. Each call returns a copy of its own.
func Default() Table {
	return published.With(nil)
}

// With returns a copy of the table in which each rate given replaces the
// model's price, or adds a model the table does not have.
func (t Table) With(rates map[string]Rate) Table {
	out := make(Table, len(t)+len(rates))
	for model, p := range t {
		out[model] = p
	}
	for model, r := range rates {
		out[model] = Price{Rate: r}
	}
	return out
}

// Cost is what the tokens cost on the model. ok is false for a model the
// table has no price for.
func (t Table) Cost(model string, tok Tokens) (dollars float64, ok bool) {
	p, ok := t[Normalize(model)]
	if !ok {
		return 0, false
	}
	r := p.Rate
	if p.LongOver > 0 && tok.Input+tok.CacheWrite+tok.CacheRead > p.LongOver {
		r = p.Long
	}
	fiveMinute := tok.CacheWrite - tok.CacheWrite1h
	return (float64(tok.Input)*r.Input + float64(fiveMinute)*r.CacheWrite5m + float64(tok.CacheWrite1h)*r.CacheWrite1h +
		float64(tok.CacheRead)*r.CacheRead + float64(tok.Output)*r.Output) / 1e6, true
}

var (
	dateSuffix    = regexp.MustCompile(`-\d{8}$`)
	bracketSuffix = regexp.MustCompile(`\[[^\]]*\]$`)
)

// Normalize turns a model name as a transcript records it into a table
// key: claude-sonnet-4-5-20250929 is claude-sonnet-4-5, and
// claude-opus-5-5[1m] is claude-opus-5-5.
func Normalize(model string) string {
	model = strings.TrimSpace(model)
	model = bracketSuffix.ReplaceAllString(model, "")
	return dateSuffix.ReplaceAllString(model, "")
}
