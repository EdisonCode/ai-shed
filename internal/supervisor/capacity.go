package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/internal/config"
)

// busyCheckTimeout bounds the owner's busy_when command. One that hangs
// counts as "not busy", so it cannot hold work back by hanging.
const busyCheckTimeout = 10 * time.Second

// Linux prints "load average: 0.52, ..."; macOS prints "load averages: 1.83 ...".
var loadRE = regexp.MustCompile(`load averages?: ([0-9]+\.[0-9]+)`)

// systemLoad reads the machine's 1-minute load average.
func systemLoad() (float64, error) {
	out, err := exec.Command("uptime").Output()
	if err != nil {
		return 0, fmt.Errorf("read the load average: %w", err)
	}
	m := loadRE.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("no load average in %q", out)
	}
	return strconv.ParseFloat(string(m[1]), 64)
}

// MachineBusy returns the machine's test for "too busy for new work": it
// gives the reason, or "" when there is room. A test that cannot be run
// counts as room: a broken test must never stop the machine's work.
func MachineBusy(m config.Machine, load func() (float64, error), cores int) func(context.Context) string {
	return func(ctx context.Context) string {
		if limit := m.Capacity.MaxLoad; limit > 0 && cores > 0 {
			if l, err := load(); err == nil && l/float64(cores) > limit {
				return fmt.Sprintf("the load is %.2f per core, above the limit of %.2f", l/float64(cores), limit)
			}
		}
		if command := m.Capacity.BusyWhen; command != "" {
			ctx, cancel := context.WithTimeout(ctx, busyCheckTimeout)
			defer cancel()
			if exec.CommandContext(ctx, "sh", "-c", m.Command(command)).Run() == nil {
				return "the machine's busy_when check says it is busy"
			}
		}
		return ""
	}
}

// SystemBusy is MachineBusy with the real load average and core count.
func SystemBusy(m config.Machine, cores int) func(context.Context) string {
	return MachineBusy(m, systemLoad, cores)
}

// macOS prints "total = 2048.00M  used = 1536.25M  free = 511.75M".
var swapusageRE = regexp.MustCompile(`used = ([0-9.]+)([KMG])`)

// parseSwapusage reads `sysctl -n vm.swapusage` and returns the swap in use,
// in kilobytes.
func parseSwapusage(out string) (int, error) {
	m := swapusageRE.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("no swap in use in %q", out)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	return int(n * map[string]float64{"K": 1, "M": 1 << 10, "G": 1 << 20}[m[2]]), nil
}

// parseMeminfo reads /proc/meminfo and returns the swap in use, in
// kilobytes.
func parseMeminfo(out string) (int, error) {
	fields := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if f := strings.Fields(rest); ok && len(f) > 0 {
			fields[name], _ = strconv.Atoi(f[0])
		}
	}
	total, ok := fields["SwapTotal"]
	if !ok {
		return 0, fmt.Errorf("no SwapTotal in /proc/meminfo")
	}
	return total - fields["SwapFree"], nil
}

// systemSwap reads the swap in use, in kilobytes, on Linux or macOS.
func systemSwap() (int, error) {
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		return parseMeminfo(string(data))
	}
	out, err := exec.Command("sysctl", "-n", "vm.swapusage").Output()
	if err != nil {
		return 0, fmt.Errorf("read the swap in use: %w", err)
	}
	return parseSwapusage(string(out))
}

// SystemLoadAndSwap reads the machine's 1-minute load average and the swap
// in use, in kilobytes.
func SystemLoadAndSwap() (float64, int, error) {
	load, err := systemLoad()
	if err != nil {
		return 0, 0, err
	}
	swap, err := systemSwap()
	return load, swap, err
}
