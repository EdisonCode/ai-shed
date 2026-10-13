package runlog

import (
	"testing"
	"time"
)

func TestPaceLineIsTheShareOfTheWeekGone(t *testing.T) {
	start := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	mid := start.Add(84 * time.Hour) // half the week
	for _, tc := range []struct {
		spend float64
		want  string
	}{{90, PaceOn}, {100, PaceOn}, {110, PaceAhead}, {160, PaceFarAhead}} {
		p := NewPace("max", 400, 200, tc.spend, start, mid)
		if p.Line != 100 || p.State != tc.want {
			t.Errorf("spend %v: line %v state %q, want 100 and %q", tc.spend, p.Line, p.State, tc.want)
		}
	}
}

func TestPaceIsKeptForStatusToRead(t *testing.T) {
	dir := t.TempDir()
	p := NewPace("max", 400, 200, 50, time.Now().Add(-time.Hour), time.Now())
	if err := WritePace(dir, p); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadPace(dir)
	if err != nil || !ok || got.Account != "max" || got.Spend != 50 {
		t.Fatalf("pace = %+v, %v, %v", got, ok, err)
	}
}
