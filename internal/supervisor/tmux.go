package supervisor

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Session is the tmux session that holds the supervised workers, one window
// each. shed never types into a window outside this session.
const Session = "shed"

// Observation is what tmux knows about a worker's window.
type Observation struct {
	Exists bool
	// Command is the foreground program of the window's pane.
	Command string
	// LastActivity is when the window last printed anything. A session that
	// is working animates; one that waits for input is silent.
	LastActivity time.Time
}

// Terminal is the supervisor's view of the workers' windows.
type Terminal interface {
	Observe(window string) (Observation, error)
	// Capture returns the last lines of the window's visible text.
	Capture(window string, lines int) (string, error)
	// Open creates the window with a shell in dir.
	Open(window, dir string) error
	// Send types one line of text into the window and presses Enter.
	Send(window, text string) error
}

// Tmux is the real Terminal. Socket names a private tmux server (tests);
// empty means the user's default server.
type Tmux struct {
	Socket string
}

func (t Tmux) run(args ...string) (string, error) {
	if t.Socket != "" {
		args = append([]string{"-L", t.Socket}, args...)
	}
	cmd := exec.Command("tmux", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s", args[len(args)-1], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func (t Tmux) hasSession() bool {
	_, err := t.run("has-session", "-t", "="+Session)
	return err == nil
}

// target addresses a window by exact session and window name.
func target(window string) string {
	return "=" + Session + ":=" + window
}

func (t Tmux) Observe(window string) (Observation, error) {
	if _, err := exec.LookPath("tmux"); err != nil {
		return Observation{}, fmt.Errorf("tmux is not on PATH: %w", err)
	}
	if !t.hasSession() {
		return Observation{}, nil
	}
	out, err := t.run("list-windows", "-t", "="+Session, "-F", "#{window_activity}\t#{pane_current_command}\t#{window_name}")
	if err != nil {
		return Observation{}, err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 || f[2] != window {
			continue
		}
		epoch, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			return Observation{}, fmt.Errorf("tmux activity time %q: %w", f[0], err)
		}
		return Observation{Exists: true, Command: f[1], LastActivity: time.Unix(epoch, 0)}, nil
	}
	return Observation{}, nil
}

func (t Tmux) Capture(window string, lines int) (string, error) {
	out, err := t.run("capture-pane", "-p", "-S", "-"+strconv.Itoa(lines), "-t", target(window))
	if err != nil {
		return "", err
	}
	kept := strings.Split(strings.TrimRight(out, "\n "), "\n")
	if len(kept) > lines {
		kept = kept[len(kept)-lines:]
	}
	return strings.Join(kept, "\n"), nil
}

func (t Tmux) Open(window, dir string) error {
	if !t.hasSession() {
		_, err := t.run("new-session", "-d", "-s", Session, "-n", window, "-c", dir)
		return err
	}
	_, err := t.run("new-window", "-d", "-n", window, "-c", dir, "-t", "="+Session+":")
	return err
}

func (t Tmux) Send(window, text string) error {
	// The text goes as literal keys and Enter as a second call, so no part of
	// the text is read as a key name.
	if _, err := t.run("send-keys", "-l", "-t", target(window), "--", text); err != nil {
		return err
	}
	// A terminal program needs a moment to take pasted text before Enter.
	time.Sleep(300 * time.Millisecond)
	_, err := t.run("send-keys", "-t", target(window), "Enter")
	return err
}
