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
	// Close ends the window and whatever runs in it.
	Close(window string) error
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

// fieldSep separates the fields of a window line. It must be a printable
// character: a tmux client with no UTF-8 locale, which is what a service
// manager gives the agent, prints "_" in place of a tab.
const fieldSep = "|"

// windows lists the windows of the session; nil when there is no session. A
// line that cannot be read is an error and never a missing window: acting on
// "the worker is gone" when it is not opens a second one.
func (t Tmux) windows() ([]Observation, []string, error) {
	if _, err := exec.LookPath("tmux"); err != nil {
		return nil, nil, fmt.Errorf("tmux is not on PATH: %w", err)
	}
	if !t.hasSession() {
		return nil, nil, nil
	}
	// The name comes before the command: shed's window names have no
	// separator in them, and a command may.
	format := strings.Join([]string{"#{window_activity}", "#{window_name}", "#{pane_current_command}"}, fieldSep)
	out, err := t.run("list-windows", "-t", "="+Session, "-F", format)
	if err != nil {
		return nil, nil, err
	}
	var found []Observation
	var names []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.SplitN(line, fieldSep, 3)
		if len(f) != 3 {
			return nil, nil, fmt.Errorf("tmux printed a window line that shed cannot read: %q", line)
		}
		epoch, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("tmux printed a window line that shed cannot read: %q", line)
		}
		found = append(found, Observation{Exists: true, Command: f[2], LastActivity: time.Unix(epoch, 0)})
		names = append(names, f[1])
	}
	return found, names, nil
}

func (t Tmux) Observe(window string) (Observation, error) {
	found, names, err := t.windows()
	if err != nil {
		return Observation{}, err
	}
	var match Observation
	count := 0
	for i, name := range names {
		if name == window {
			match = found[i]
			count++
		}
	}
	// Two windows with one name cannot be told apart: typing into "the"
	// worker could reach either.
	if count > 1 {
		return Observation{}, fmt.Errorf("%d windows named %s in tmux session %s: close the extra one", count, window, Session)
	}
	return match, nil
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
	_, names, err := t.windows()
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == window {
			return fmt.Errorf("window %s already exists in tmux session %s", window, Session)
		}
	}
	if !t.hasSession() {
		_, err := t.run("new-session", "-d", "-s", Session, "-n", window, "-c", dir)
		return err
	}
	_, err = t.run("new-window", "-d", "-n", window, "-c", dir, "-t", "="+Session+":")
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

func (t Tmux) Close(window string) error {
	_, err := t.run("kill-window", "-t", target(window))
	return err
}
