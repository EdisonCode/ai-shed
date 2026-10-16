package supervisor

import "testing"

func TestSwapInUseIsReadOnMacOSAndLinux(t *testing.T) {
	if got, err := parseSwapusage("total = 10240.00M  used = 9339.00M  free = 901.00M  (encrypted)"); err != nil || got != 9339*1024 {
		t.Errorf("macOS: %d, %v", got, err)
	}
	if got, err := parseSwapusage("total = 0.00M  used = 0.00M  free = 0.00M"); err != nil || got != 0 {
		t.Errorf("macOS without swap: %d, %v", got, err)
	}
	meminfo := "MemTotal:       32768000 kB\nSwapTotal:       8388604 kB\nSwapFree:        2097152 kB\n"
	if got, err := parseMeminfo(meminfo); err != nil || got != 6291452 {
		t.Errorf("Linux: %d, %v", got, err)
	}
	if _, err := parseMeminfo("MemTotal: 1 kB\n"); err == nil {
		t.Error("Linux: a meminfo with no swap line must be an error")
	}
}
