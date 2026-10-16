package runlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LoadsFile holds the machine's peak load and swap of each hour, one line
// each, for shed digest.
const LoadsFile = "loads.jsonl"

// KeepLoadHours is how many hours of peaks the file keeps: a week.
const KeepLoadHours = 7 * 24

// LoadPeak is the highest load and swap the agent saw in one hour.
type LoadPeak struct {
	// Hour is the start of the hour.
	Hour   time.Time `json:"hour"`
	Load   float64   `json:"load"`
	Cores  int       `json:"cores"`
	SwapKB int       `json:"swap_kb"`
}

// PerCore is the peak load per core; 0 when the cores are not known.
func (p LoadPeak) PerCore() float64 {
	if p.Cores <= 0 {
		return 0
	}
	return p.Load / float64(p.Cores)
}

func ParseLoadPeak(line string) (LoadPeak, error) {
	var p LoadPeak
	if err := json.Unmarshal([]byte(line), &p); err != nil {
		return LoadPeak{}, fmt.Errorf("parse load peak: %w", err)
	}
	return p, nil
}

// WriteLoadPeak adds the hour's peak and keeps the last KeepLoadHours. The
// file is written whole, once an hour, so it never grows past a week.
func WriteLoadPeak(dir string, p LoadPeak) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("write load peak: %w", err)
	}
	path := filepath.Join(dir, LoadsFile)
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("write load peak: %w", err)
	}
	line, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("write load peak: %w", err)
	}
	var lines []string
	if s := strings.TrimSpace(string(data)); s != "" {
		lines = strings.Split(s, "\n")
	}
	lines = append(lines, string(line))
	if len(lines) > KeepLoadHours {
		lines = lines[len(lines)-KeepLoadHours:]
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return fmt.Errorf("write load peak: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write load peak: %w", err)
	}
	return nil
}

// LoadSampler keeps the peak of the hour in hand and writes it once the
// hour is over. The hour in hand when the agent stops is not written.
type LoadSampler struct {
	Dir   string
	Cores int
	// Read returns the machine's 1-minute load average and the swap in use.
	Read func() (load float64, swapKB int, err error)
	Logf func(format string, args ...any)

	peak LoadPeak
	// failed is set once a failed reading was logged, so it is logged once.
	failed bool
}

// Sample reads the machine once.
func (s *LoadSampler) Sample(now time.Time) {
	hour := now.Truncate(time.Hour)
	if !s.peak.Hour.IsZero() && hour.After(s.peak.Hour) {
		if err := WriteLoadPeak(s.Dir, s.peak); err != nil {
			s.Logf("%v", err)
		}
		s.peak = LoadPeak{}
	}
	load, swap, err := s.Read()
	if err != nil {
		if !s.failed {
			s.failed = true
			s.Logf("read load and swap: %v", err)
		}
		return
	}
	s.failed = false
	if s.peak.Hour.IsZero() {
		s.peak = LoadPeak{Hour: hour, Cores: s.Cores}
	}
	s.peak.Load = max(s.peak.Load, load)
	s.peak.SwapKB = max(s.peak.SwapKB, swap)
}
