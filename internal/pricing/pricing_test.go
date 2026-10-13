package pricing

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCostAddsEachKindOfTokenAtItsRate(t *testing.T) {
	// Claude Opus 5.5: $4 input, $5 5m write, $8 1h write, $0.20 read, $20 output.
	got, ok := Default().Cost("claude-opus-5-5", Tokens{Input: 1_000_000, CacheWrite: 3_000_000, CacheWrite1h: 1_000_000, CacheRead: 10_000_000, Output: 100_000})
	if want := 4 + 2*5 + 8 + 10*0.20 + 0.1*20; !ok || !near(got, want) {
		t.Fatalf("cost = %v (priced %v), want %v", got, ok, want)
	}
}

func TestModelIdsWithADateOrAContextSuffixAreFound(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-5-20250929", "claude-opus-5-5[1m]", "claude-3-5-haiku-20241022", "claude-opus-4-20250514"} {
		if _, ok := Default().Cost(model, Tokens{Output: 1}); !ok {
			t.Errorf("%s has no price", model)
		}
	}
}

func TestAPrefixOfAnotherModelIsNotTakenForIt(t *testing.T) {
	five, _ := Default().Cost("claude-opus-5", Tokens{Output: 1_000_000})
	fiveFive, _ := Default().Cost("claude-opus-5-5", Tokens{Output: 1_000_000})
	if !near(five, 25) || !near(fiveFive, 20) {
		t.Fatalf("opus-5 = %v, opus-5-5 = %v; want 25 and 20", five, fiveFive)
	}
}

func TestHaikuFiveFiveAboveTheLongPromptLinePaysTheHigherRates(t *testing.T) {
	short, _ := Default().Cost("claude-haiku-5-5", Tokens{Input: 50_000, CacheRead: 50_000, Output: 1_000_000})
	long, _ := Default().Cost("claude-haiku-5-5", Tokens{Input: 50_000, CacheRead: 50_001, Output: 1_000_000})
	if !near(short, 0.05*0.10+0.05*0.01+0.50) || !near(long, 0.05*0.50+0.050001*0.05+2.50) {
		t.Fatalf("short = %v, long = %v", short, long)
	}
}

func TestAnUnknownModelHasNoPrice(t *testing.T) {
	if _, ok := Default().Cost("gpt-9", Tokens{Output: 1}); ok {
		t.Fatal("an unknown model was priced")
	}
}

func TestTheFleetFileCanSetAndAddRates(t *testing.T) {
	table := Default().With(map[string]Rate{
		"claude-opus-5-5": {Input: 1, Output: 2},
		"my-local-model":  {Output: 1},
	})
	if got, _ := table.Cost("claude-opus-5-5", Tokens{Output: 1_000_000}); !near(got, 2) {
		t.Fatalf("overridden opus-5-5 = %v, want 2", got)
	}
	if got, ok := table.Cost("my-local-model", Tokens{Output: 1_000_000}); !ok || !near(got, 1) {
		t.Fatalf("added model = %v (%v)", got, ok)
	}
	if got, _ := Default().Cost("claude-opus-5-5", Tokens{Output: 1_000_000}); !near(got, 20) {
		t.Fatalf("the default table changed: %v", got)
	}
}
