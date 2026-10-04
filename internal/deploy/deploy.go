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

	"github.com/edisoncode/ai-shed/contrib"
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
		if err := writeRemote(ctx, r, m, f.path, f.data, f.mode); err != nil {
			return res, err
		}
	}
	return res, nil
}

// writeRemote writes a file under the machine user's home.
func writeRemote(ctx context.Context, r probe.Runner, m config.Machine, path string, data []byte, mode string) error {
	cmd := fmt.Sprintf(`f="$HOME/%s" && mkdir -p "$(dirname "$f")" && cat > "$f.new" && chmod %s "$f.new" && mv "$f.new" "$f"`, path, mode)
	if _, err := r.Run(ctx, m, cmd, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("write ~/%s: %w", path, err)
	}
	return nil
}

// restartCommands restart the agent service where it is installed, by asking
// it to stop: the agent finishes a message it is typing to a worker before it
// exits. Each prints "restarted", or "absent" when the service was never set up.
var restartCommands = map[string]string{
	// Over SSH there is no session bus address; the runtime directory finds it.
	"linux": `export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"; systemctl --user cat ` + UnitName + ` >/dev/null 2>&1 || { echo absent; exit 0; }; systemctl --user restart ` + UnitName + ` && echo restarted`,
	// A TERM lets the agent finish what it is typing; launchd then starts it
	// again (KeepAlive). kickstart -k would kill it outright.
	"darwin": `s="gui/$(id -u)/` + AgentLabel + `"; launchctl print "$s" >/dev/null 2>&1 || { echo absent; exit 0; }; launchctl kill SIGTERM "$s" && echo restarted`,
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

// Over SSH there is no session bus address; the runtime directory finds it.
const systemdEnv = `export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"; `

// service is how one platform runs the agent.
type service struct {
	// path of the service file under the user's home.
	path string
	data []byte
	// present prints "present" when the service is set up, else "absent".
	present string
	// load starts the service now and at every login or boot.
	load string
}

var services = map[string]service{
	"linux": {
		path:    ".config/systemd/user/" + UnitName + ".service",
		data:    contrib.SystemdUnit,
		present: systemdEnv + `systemctl --user cat ` + UnitName + ` >/dev/null 2>&1 && echo present || echo absent`,
		// Without lingering the agent stops when the user logs out.
		load: systemdEnv + `systemctl --user daemon-reload && systemctl --user enable --now ` + UnitName + ` && { loginctl enable-linger "$(id -un)" >/dev/null 2>&1 || echo "The agent will stop when you log out: run 'loginctl enable-linger' on the machine."; }`,
	},
	"darwin": {
		path:    "Library/LaunchAgents/" + AgentLabel + ".plist",
		data:    contrib.LaunchdPlist,
		present: `launchctl print "gui/$(id -u)/` + AgentLabel + `" >/dev/null 2>&1 && echo present || echo absent`,
		// The GUI domain is the one that can read the login keychain.
		load: `launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/` + AgentLabel + `.plist"`,
	},
}

func serviceFor(platform string) (service, error) {
	osName, _, _ := strings.Cut(platform, "-")
	svc, ok := services[osName]
	if !ok {
		return service{}, fmt.Errorf("no agent service for %q", platform)
	}
	return svc, nil
}

// AgentInstalled reports whether the agent service is set up on the machine.
func AgentInstalled(ctx context.Context, r probe.Runner, m config.Machine, platform string) (bool, error) {
	svc, err := serviceFor(platform)
	if err != nil {
		return false, err
	}
	out, err := r.Run(ctx, m, svc.present, nil)
	if err != nil {
		return false, fmt.Errorf("look for the agent service: %w", err)
	}
	return strings.TrimSpace(string(out)) == "present", nil
}

// InstallAgent writes the agent's service file on the machine and starts it.
// The returned note is something the owner must still do, or empty.
func InstallAgent(ctx context.Context, r probe.Runner, m config.Machine, platform string) (note string, err error) {
	svc, err := serviceFor(platform)
	if err != nil {
		return "", err
	}
	if err := writeRemote(ctx, r, m, svc.path, svc.data, "644"); err != nil {
		return "", err
	}
	out, err := r.Run(ctx, m, svc.load, nil)
	if err != nil {
		return "", fmt.Errorf("start the agent service: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
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
