package probe

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/edisoncode/ai-shed/internal/config"
)

// Runner runs a shell command on a machine and returns its standard output.
type Runner interface {
	Run(ctx context.Context, m config.Machine, command string, stdin io.Reader) ([]byte, error)
}

// ShellRunner reaches a machine with the system ssh client, or with sh when
// the machine's host is "local". It uses the user's own ssh config and keys.
type ShellRunner struct{}

func (ShellRunner) Run(ctx context.Context, m config.Machine, command string, stdin io.Reader) ([]byte, error) {
	var cmd *exec.Cmd
	if m.Host == config.LocalHost {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	} else {
		cmd = exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", m.Host, command)
	}
	var stderr bytes.Buffer
	cmd.Stdin = stdin
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", m.Host, err, lastLine(stderr.String()))
	}
	return out, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
