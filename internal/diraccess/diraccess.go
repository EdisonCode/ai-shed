// Package diraccess checks that each worker's directory can be read, both by
// the shell that asks and by the tmux server the workers run under. On macOS
// a directory on a removable volume needs a privacy grant for each program
// that reads it. Without one, a read over SSH fails with "Operation not
// permitted", and a read under tmux waits on a dialog on the machine's
// screen, which nobody sees on a machine with no one at it.
package diraccess

import (
	"fmt"
	"strings"
	"time"
)

// Wait is how long a read may take before the check counts it as blocked.
const Wait = 5 * time.Second

// Session is the tmux session the workers run in. The tmux read runs only
// when it exists: with no server there is nothing the workers inherit.
const Session = "shed"

// Dir is a worker's directory. Path may start with "~/".
type Dir struct {
	Worker string
	Path   string
}

// Who read the directory.
const (
	ViaShell = "shell" // the process that ran the check, such as sshd's
	ViaTmux  = "tmux"  // the tmux server, which every worker's tools inherit
)

// Problem is a read of a worker's directory that failed or did not finish.
type Problem struct {
	Worker string
	Path   string
	Via    string
	// Blocked is set when the read did not finish within Wait.
	Blocked bool
	// Err is the first line the read printed when it failed.
	Err string
}

// Host is what the check found about the machine, to name the fix.
type Host struct {
	OS   string // uname: "Darwin" or "Linux"
	Tmux string // the tmux binary, symlinks resolved; "" when not found
}

// Options shape the check script. The zero value is the real check.
type Options struct {
	// Tmux is the command that reaches the workers' tmux server; "tmux"
	// when empty. Tests point it at a private server.
	Tmux string
	// Wait overrides Wait; tests shorten it.
	Wait time.Duration
}

// The read runs from a file, so a directory's path never passes through
// tmux, which would expand a "#" in it. It writes its pid first, so a read
// that blocks can be stopped, and its exit code last, so the code's file
// appearing means the read is over.
const readScript = `echo $$ >"$1.pid"
d=$(cat "$1.dir")
case $d in "~/"*) d="$HOME/${d#??}";; esac
ls "$d" >/dev/null 2>"$1.err"
echo $? >"$1.tmp" && mv "$1.tmp" "$1.rc"
`

// Script builds the shell script that reads each directory and prints a
// "@@dir" line for each read that failed or blocked, and one "@@dirhost"
// line. It waits for the reads at most Wait, and stops any still running.
func Script(dirs []Dir, opt Options) string {
	if len(dirs) == 0 {
		return ""
	}
	tmux := opt.Tmux
	if tmux == "" {
		tmux = "tmux"
	}
	wait := opt.Wait
	if wait == 0 {
		wait = Wait
	}
	var b strings.Builder
	fmt.Fprintf(&b, "m=$(mktemp -d \"${TMPDIR:-/tmp}/shed-dircheck.XXXXXX\")\n")
	fmt.Fprintf(&b, "cat >\"$m/read.sh\" <<'SHED_READ'\n%sSHED_READ\n", readScript)
	fmt.Fprintf(&b, "t=; %s has-session -t =%s 2>/dev/null && t=1\n", tmux, Session)
	for i, d := range dirs {
		fmt.Fprintf(&b, "printf '%%s\\n' %s >\"$m/s%d.dir\"; cp \"$m/s%d.dir\" \"$m/t%d.dir\"\n", quote(d.Path), i, i, i)
		fmt.Fprintf(&b, "sh \"$m/read.sh\" \"$m/s%d\" </dev/null >/dev/null 2>&1 &\n", i)
		fmt.Fprintf(&b, "[ -n \"$t\" ] && %s run-shell -b \"sh $m/read.sh $m/t%d >/dev/null 2>&1\"\n", tmux, i)
	}
	// Wait for every read's exit code, in tenths of a second.
	fmt.Fprintf(&b, "n=0; while [ $n -lt %d ]; do a=1\n", int(wait/(100*time.Millisecond)))
	for i := range dirs {
		fmt.Fprintf(&b, "[ -f \"$m/s%d.rc\" ] || a=\n", i)
		fmt.Fprintf(&b, "[ -z \"$t\" ] || [ -f \"$m/t%d.rc\" ] || a=\n", i)
	}
	b.WriteString("[ -n \"$a\" ] && break; sleep 0.1; n=$((n+1)); done\n")
	b.WriteString(`r() {
if [ -f "$1.rc" ]; then
c=$(cat "$1.rc"); [ "$c" = 0 ] || echo "@@dir $2 $3 $c $(head -n 1 "$1.err")"
else
echo "@@dir $2 $3 blocked"
[ -f "$1.pid" ] && { p=$(cat "$1.pid"); pkill -P "$p"; kill "$p"; } 2>/dev/null
fi
}
`)
	for i, d := range dirs {
		fmt.Fprintf(&b, "r \"$m/s%d\" %s %s\n", i, d.Worker, ViaShell)
		fmt.Fprintf(&b, "[ -n \"$t\" ] && r \"$m/t%d\" %s %s\n", i, d.Worker, ViaTmux)
	}
	fmt.Fprintf(&b, "echo \"@@dirhost $(uname) $(readlink -f \"$(command -v %s)\" 2>/dev/null)\"\n", strings.Fields(tmux)[0])
	b.WriteString("rm -rf \"$m\"\n")
	return b.String()
}

// Parse reads the lines Script printed. Other lines are ignored, so it can
// read a longer report. A worker's path comes from dirs.
func Parse(out string, dirs []Dir) ([]Problem, Host) {
	paths := map[string]string{}
	for _, d := range dirs {
		paths[d.Worker] = d.Path
	}
	var problems []Problem
	var host Host
	for _, line := range strings.Split(out, "\n") {
		tag, rest, _ := strings.Cut(strings.TrimRight(line, "\r"), " ")
		switch tag {
		case "@@dir":
			f := strings.SplitN(rest, " ", 4)
			if len(f) < 3 {
				continue
			}
			p := Problem{Worker: f[0], Path: paths[f[0]], Via: f[1], Blocked: f[2] == "blocked"}
			if len(f) == 4 {
				p.Err = strings.TrimSpace(f[3])
			}
			if !p.Blocked && p.Err == "" {
				p.Err = "exit status " + f[2]
			}
			problems = append(problems, p)
		case "@@dirhost":
			host.OS, host.Tmux, _ = strings.Cut(rest, " ")
			host.Tmux = strings.TrimSpace(host.Tmux)
		}
	}
	return problems, host
}

const (
	macRemoteLogin = `over SSH, turn on System Settings → General → Sharing → Remote Login → ⓘ → "Allow full disk access for remote users"`
	macPrompt      = "click Allow on any prompt on the machine's screen"
)

// Describe says what failed and, on macOS, how to fix it.
func (p Problem) Describe(h Host) string {
	mac := h.OS == "Darwin"
	tmux := "tmux"
	if h.Tmux != "" {
		tmux += " (" + h.Tmux + ")"
	}
	switch {
	case p.Via == ViaTmux && p.Blocked:
		s := fmt.Sprintf("dir %s: a read by %s did not finish in %s", p.Path, tmux, Wait)
		if mac {
			s += fmt.Sprintf(", so workers will wait on a privacy prompt: %s, or grant %s Full Disk Access in System Settings → Privacy & Security", macPrompt, tmuxName(h))
		}
		return s
	case p.Via == ViaTmux:
		s := fmt.Sprintf("dir %s is not readable by %s (%s)", p.Path, tmux, p.Err)
		if mac {
			s += fmt.Sprintf(": grant %s Full Disk Access in System Settings → Privacy & Security, then %s", tmuxName(h), macPrompt)
		}
		return s
	case p.Blocked:
		s := fmt.Sprintf("dir %s: a read did not finish in %s", p.Path, Wait)
		if mac {
			s += ": " + macPrompt
		}
		return s
	}
	s := fmt.Sprintf("dir %s is not readable by shed's shell (%s)", p.Path, p.Err)
	if mac && strings.Contains(p.Err, "not permitted") {
		s += ": grant Full Disk Access to what runs shed there; " + macRemoteLogin
	}
	return s
}

func tmuxName(h Host) string {
	if h.Tmux != "" {
		return h.Tmux
	}
	return "tmux"
}

func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
