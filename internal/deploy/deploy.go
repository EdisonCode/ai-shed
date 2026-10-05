// Package deploy puts the shed binary and the fleet file on a machine.
package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/edisoncode/ai-shed/contrib"
	"github.com/edisoncode/ai-shed/internal/config"
	"github.com/edisoncode/ai-shed/internal/probe"
	"github.com/edisoncode/ai-shed/internal/release"
	"github.com/edisoncode/ai-shed/internal/runlog"
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

// lockDir marks a deploy in progress on a machine. A lock older than
// lockStaleAfter was left by a deploy that died, and is taken over.
const (
	lockDir        = runlog.StateDir + "/deploy.lock"
	lockStaleAfter = 15 * 60 // seconds
)

// ErrBusy is returned by Lock when another deploy of the machine is running.
var ErrBusy = errors.New("another deploy of this machine is running")

// Lock takes the machine's deploy lock, so that two deploys never run their
// preflights and installs over each other. The lock lives on the machine: it
// holds against a deploy from any watcher. unlock releases it.
func Lock(ctx context.Context, r probe.Runner, m config.Machine) (unlock func(), err error) {
	// mkdir is the lock: it fails when the directory exists.
	command := fmt.Sprintf(`d="$HOME/%s"; mkdir -p "$(dirname "$d")"
if mkdir "$d" 2>/dev/null; then echo locked; exit 0; fi
age=$(( $(date +%%s) - $(stat -c %%Y "$d" 2>/dev/null || stat -f %%m "$d") ))
if [ "$age" -gt %d ]; then touch "$d" && echo locked; else echo "busy $age"; fi`, lockDir, lockStaleAfter)
	out, err := r.Run(ctx, m, command, nil)
	if err != nil {
		return nil, fmt.Errorf("take the deploy lock: %w", err)
	}
	if answer := strings.TrimSpace(string(out)); answer != "locked" {
		age := strings.TrimPrefix(answer, "busy ")
		return nil, fmt.Errorf("%w (started %ss ago). If none is, remove ~/%s on the machine", ErrBusy, age, lockDir)
	}
	return func() {
		// A deploy cut short leaves the lock; it goes stale by itself.
		r.Run(context.Background(), m, fmt.Sprintf(`rmdir "$HOME/%s"`, lockDir), nil)
	}, nil
}

// Recycle asks the machine's agent to replace a worker's session with a
// fresh one: after the issue it has in hand, or at once when now is set.
func Recycle(ctx context.Context, r probe.Runner, m config.Machine, worker string, now bool) error {
	mode := ""
	if now {
		mode = runlog.RecycleNow
	}
	return writeRemote(ctx, r, m, runlog.StateDir+"/"+runlog.RecycleDir+"/"+worker, []byte(mode), "644")
}

// Pause tells the machine's agent to leave its workers alone for a while:
// nothing is handed out and nobody is nudged. The time is counted by the
// machine's own clock; until is when the pause ends by that clock.
func Pause(ctx context.Context, r probe.Runner, m config.Machine, d time.Duration) (until time.Time, err error) {
	command := fmt.Sprintf(`f="$HOME/%s/%s" && mkdir -p "$(dirname "$f")" && u=$(( $(date +%%s) + %d )) && echo "$u" > "$f.new" && mv "$f.new" "$f" && echo "$u"`,
		runlog.StateDir, runlog.PauseFile, int(d.Seconds()))
	out, err := r.Run(ctx, m, command, nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("pause %s: %w", m.Name, err)
	}
	until, ok := runlog.ParsePause(string(out))
	if !ok {
		return time.Time{}, fmt.Errorf("pause %s: the machine answered %q, not a time", m.Name, strings.TrimSpace(string(out)))
	}
	return until, nil
}

// Resume ends a pause. It reports false when the machine was not paused.
func Resume(ctx context.Context, r probe.Runner, m config.Machine) (bool, error) {
	command := fmt.Sprintf(`f="$HOME/%s/%s"; [ -f "$f" ] || { echo absent; exit 0; }; u=$(cat "$f"); rm "$f" || exit 1
[ "$u" -gt "$(date +%%s)" ] 2>/dev/null && echo resumed || echo absent`, runlog.StateDir, runlog.PauseFile)
	out, err := r.Run(ctx, m, command, nil)
	if err != nil {
		return false, fmt.Errorf("resume %s: %w", m.Name, err)
	}
	return strings.TrimSpace(string(out)) == "resumed", nil
}

// Bump asks the machine's agent to put an issue first in line: in the queue
// of the named worker or, with worker empty, of the workers that have it.
func Bump(ctx context.Context, r probe.Runner, m config.Machine, issue int, worker string) error {
	return writeRemote(ctx, r, m, bumpPath(issue), []byte(worker), "644")
}

// Unbump takes a bump back. It reports false when the issue was not bumped.
func Unbump(ctx context.Context, r probe.Runner, m config.Machine, issue int) (bool, error) {
	out, err := r.Run(ctx, m, fmt.Sprintf(`f="$HOME/%s"; [ -f "$f" ] || { echo absent; exit 0; }; rm "$f" && echo removed`, bumpPath(issue)), nil)
	if err != nil {
		return false, fmt.Errorf("take back the bump of #%d: %w", issue, err)
	}
	return strings.TrimSpace(string(out)) == "removed", nil
}

func bumpPath(issue int) string {
	return fmt.Sprintf("%s/%s/%d", runlog.StateDir, runlog.BumpsDir, issue)
}

// PushTask queues a one-off task on a machine, for the named worker or, with
// worker empty, for any. The machine's agent hands it over.
func PushTask(ctx context.Context, r probe.Runner, m config.Machine, worker, id string, brief []byte) error {
	where := worker
	if where == "" {
		where = runlog.TaskAny
	}
	return writeRemote(ctx, r, m, runlog.StateDir+"/"+runlog.TasksDir+"/"+where+"/"+id+".md", brief, "644")
}

// What became of a task that was to be cancelled.
const (
	TaskCancelled = "cancelled" // it waited, and is gone
	TaskTaken     = "taken"     // a worker was handed it; it is too late
	TaskAbsent    = "absent"    // the machine has no such task
)

// CancelTask removes a one-off task that still waits for a worker, and says
// what became of it. A task a worker was handed is left alone: the worker
// has its brief and may be halfway through.
func CancelTask(ctx context.Context, r probe.Runner, m config.Machine, id string) (string, error) {
	script := fmt.Sprintf(`d="$HOME/%s/%s"
[ -f "$d/%s/%s.md" ] && { echo %s; exit 0; }
for f in "$d"/*/%s.md; do [ -f "$f" ] && rm "$f" && { echo %s; exit 0; }; done
echo %s
`, runlog.StateDir, runlog.TasksDir, runlog.TaskTaken, id, TaskTaken, id, TaskCancelled, TaskAbsent)
	out, err := r.Run(ctx, m, "sh -s", strings.NewReader(script))
	if err != nil {
		return "", fmt.Errorf("cancel task %s: %w", id, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// noReport is what the report script prints when no worker has written one.
const noReport = "@@no-report"

// TaskReport returns the report a worker wrote for a one-off task; found is
// false when there is none yet.
func TaskReport(ctx context.Context, r probe.Runner, m config.Machine, id string) (report string, found bool, err error) {
	var script strings.Builder
	for _, w := range m.Workers {
		// A leading ~/ is the user's home, as for the worker's session.
		dir := "'" + strings.ReplaceAll(w.Dir, "'", `'\''`) + "'"
		if rest, ok := strings.CutPrefix(w.Dir, "~/"); ok {
			dir = `"$HOME"/'` + strings.ReplaceAll(rest, "'", `'\''`) + "'"
		}
		fmt.Fprintf(&script, "f=%s/.shed/tasks/%s.report.md; [ -f \"$f\" ] && { cat \"$f\"; exit 0; }\n", dir, id)
	}
	script.WriteString("echo " + noReport + "\n")
	out, err := r.Run(ctx, m, "sh -s", strings.NewReader(script.String()))
	if err != nil {
		return "", false, fmt.Errorf("read the report of task %s: %w", id, err)
	}
	if strings.TrimSpace(string(out)) == noReport {
		return "", false, nil
	}
	return string(out), true, nil
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
