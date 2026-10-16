package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readPeaks(t *testing.T, dir string) []LoadPeak {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, LoadsFile))
	if err != nil {
		t.Fatal(err)
	}
	var peaks []LoadPeak
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		p, err := ParseLoadPeak(line)
		if err != nil {
			t.Fatal(err)
		}
		peaks = append(peaks, p)
	}
	return peaks
}

func TestTheSamplerWritesEachHoursPeakOnceTheHourIsOver(t *testing.T) {
	dir := t.TempDir()
	readings := []struct {
		load float64
		swap int
	}{{2, 100}, {9, 50}, {4, 300}, {1, 0}}
	n := 0
	s := &LoadSampler{Dir: dir, Cores: 4, Logf: t.Logf, Read: func() (float64, int, error) {
		r := readings[n]
		n++
		return r.load, r.swap, nil
	}}
	at := time.Date(2026, 10, 16, 9, 10, 0, 0, time.UTC)
	s.Sample(at)
	s.Sample(at.Add(20 * time.Minute))
	s.Sample(at.Add(40 * time.Minute))
	if _, err := os.Stat(filepath.Join(dir, LoadsFile)); err == nil {
		t.Fatal("the peak was written before its hour was over")
	}
	s.Sample(at.Add(55 * time.Minute)) // 10:05, the next hour

	peaks := readPeaks(t, dir)
	want := LoadPeak{Hour: at.Truncate(time.Hour), Load: 9, Cores: 4, SwapKB: 300}
	if len(peaks) != 1 || !peaks[0].Hour.Equal(want.Hour) || peaks[0].Load != 9 || peaks[0].SwapKB != 300 || peaks[0].PerCore() != 2.25 {
		t.Fatalf("peaks = %+v, want %+v", peaks, want)
	}
}

func TestTheLoadsFileKeepsAWeek(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for h := range KeepLoadHours + 5 {
		if err := WriteLoadPeak(dir, LoadPeak{Hour: start.Add(time.Duration(h) * time.Hour), Load: 1, Cores: 1}); err != nil {
			t.Fatal(err)
		}
	}
	peaks := readPeaks(t, dir)
	if len(peaks) != KeepLoadHours || !peaks[0].Hour.Equal(start.Add(5*time.Hour)) {
		t.Fatalf("%d peaks from %v, want the last %d hours", len(peaks), peaks[0].Hour, KeepLoadHours)
	}
}
