// Package deploy puts the shed binary and the fleet file on a machine.
package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/release"
)

// Paths on the machine, under the user's home.
const (
	RemoteBinary  = ".local/bin/shed"
	RemoteConfig  = ".config/shed/shed.yaml"
	RemoteMachine = ".config/shed/machine"
)

var platforms = map[string]string{
	"Linux x86_64":  "linux-amd64",
	"Linux aarch64": "linux-arm64",
	"Darwin arm64":  "darwin-arm64",
	"Darwin x86_64": "darwin-amd64",
}

// Platform maps the output of `uname -sm` to the suffix of a dist binary.
func Platform(uname string) (string, error) {
	p, ok := platforms[strings.TrimSpace(uname)]
	if !ok {
		return "", fmt.Errorf("unsupported platform %q", strings.TrimSpace(uname))
	}
	return p, nil
}

// AgentLabel is the launchd label of the agent; UnitName its systemd unit.
const (
	AgentLabel = "com.edisoncode.shed-agent"
	UnitName   = "shed-agent"
)

// Result says what a deploy did on one machine.
type Result struct {
	Platform string
	// BinaryInstalled is false when the machine already ran this version.
	BinaryInstalled bool
}

// Machine puts the binary, the fleet file and the machine's name on a
// machine. binaryFor returns the local path of the binary for a platform.
// A machine that already runs this version keeps its binary. Each file is
// written beside its target and moved into place, so a running agent never
// sees half a file.
func Machine(ctx context.Context, r probe.Runner, m config.Machine, version string, binaryFor func(platform string) (string, error), configPath string) (Result, error) {
	uname, err := r.Run(ctx, m, "uname -sm", nil)
	if err != nil {
		return Result{}, err
	}
	platform, err := Platform(string(uname))
	if err != nil {
		return Result{}, err
	}
	res := Result{Platform: platform}
	cfg, err := os.ReadFile(configPath)
	if err != nil {
		return res, fmt.Errorf("read config: %w", err)
	}

	type file struct {
		path string
		data []byte
		mode string
	}
	files := []file{{RemoteConfig, cfg, "644"}, {RemoteMachine, []byte(m.Name + "\n"), "644"}}

	// A build from a working tree has no version to compare, so it is always sent.
	installed, _ := r.Run(ctx, m, `"$HOME/`+RemoteBinary+`" version 2>/dev/null`, nil)
	if !release.IsRelease(version) || strings.TrimSpace(string(installed)) != "shed "+version {
		path, err := binaryFor(platform)
		if err != nil {
			return res, err
		}
		binary, err := os.ReadFile(path)
		if err != nil {
			return res, fmt.Errorf("read binary: %w", err)
		}
		files = append(files, file{RemoteBinary, binary, "755"})
		res.BinaryInstalled = true
	}
	for _, f := range files {
		cmd := fmt.Sprintf(`f="$HOME/%s" && mkdir -p "$(dirname "$f")" && cat > "$f.new" && chmod %s "$f.new" && mv "$f.new" "$f"`, f.path, f.mode)
		if _, err := r.Run(ctx, m, cmd, bytes.NewReader(f.data)); err != nil {
			return res, fmt.Errorf("write ~/%s: %w", f.path, err)
		}
	}
	return res, nil
}

// restartCommands restart the agent service where it is installed. Each
// prints "restarted", or "absent" when the service was never set up.
var restartCommands = map[string]string{
	// Over SSH there is no session bus address; the runtime directory finds it.
	"linux":  `export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"; systemctl --user cat ` + UnitName + ` >/dev/null 2>&1 || { echo absent; exit 0; }; systemctl --user restart ` + UnitName + ` && echo restarted`,
	"darwin": `s="gui/$(id -u)/` + AgentLabel + `"; launchctl print "$s" >/dev/null 2>&1 || { echo absent; exit 0; }; launchctl kickstart -k "$s" && echo restarted`,
}

// RestartAgent restarts the machine's agent so it runs a newly installed
// binary. It reports false when the agent service is not installed.
func RestartAgent(ctx context.Context, r probe.Runner, m config.Machine, platform string) (bool, error) {
	osName, _, _ := strings.Cut(platform, "-")
	command, ok := restartCommands[osName]
	if !ok {
		return false, fmt.Errorf("no way to restart the agent on %q", platform)
	}
	out, err := r.Run(ctx, m, command, nil)
	if err != nil {
		return false, fmt.Errorf("restart agent: %w", err)
	}
	return strings.TrimSpace(string(out)) == "restarted", nil
}

// Hook runs the owner's deploy hook on this machine (the watcher) for one
// deployed machine. The hook learns which machine from SHED_MACHINE and
// SHED_HOST. An empty command does nothing.
func Hook(ctx context.Context, command string, m config.Machine, stdout, stderr io.Writer) error {
	if command == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = append(os.Environ(), "SHED_MACHINE="+m.Name, "SHED_HOST="+m.Host)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("deploy hook: %w", err)
	}
	return nil
}
