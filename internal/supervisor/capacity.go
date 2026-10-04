package supervisor

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
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
