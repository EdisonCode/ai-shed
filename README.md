# ai-shed

`shed` watches the worker machines in a homelab: the boxes where AI coding
agents work through a backlog of GitHub issues and run scheduled chores. One
command tells you whether each machine is on track and what it needs from you.

```
$ shed status
linux-box  me@linux-box  load 0.52/8  disk 42%  agent ok
  checks   ok gh auth, ok git identity, ok claude, FAIL docker
  workers
    work:app-123  claude  active 40s ago
  issues
    my-org/my-app#123  working   app-123  Retry the export when the API times out
    my-org/my-app#124  decision           Drop the legacy column
    my-org/my-app#130  queued             Paginate the audit log
  tasks
    nightly-deps  0 3 * * *  ok  9h ago, took 41s
  ! check failed: docker (Cannot connect to the Docker daemon)
  ! my-org/my-app#124 waits on you (decision): Drop the legacy column

mac-mini  me@mac-mini  UNREACHABLE
  ! unreachable: me@mac-mini: exit status 255: Operation timed out

3 item(s) need you.
```

The exit code is 1 when anything needs you, so a script or a notifier can act on it.

## How it works

- **One fleet file.** `shed.yaml` lists every machine: how to reach it, the
  checks it must pass, the issues it owns, the tasks it runs.
- **Watching needs nothing installed.** `shed status` makes one SSH round trip
  per machine with your own `ssh` and keys, and reads issues with your own
  `gh` login. No server, no database, no tokens to manage.
- **Workers are tmux windows.** An issue counts as *working* when a tmux
  window on its machine has the issue number in its name (`app-123`).
- **Scheduled tasks need a small agent.** `shed agent` is the same binary,
  running on the machine. It runs the machine's tasks on their cron schedule
  and writes each run to a log file that `shed status` reads back.

## What needs you

`shed status` puts a `!` line on a machine when:

| Rule | Meaning |
| --- | --- |
| unreachable | SSH failed |
| check failed | a readiness check (auth, config, a tool) exited non-zero |
| disk is N% full | 90% or more of the home filesystem is used |
| agent is not running | the machine has tasks but no fresh heartbeat (3 minutes) |
| task failed | the last run exited non-zero |
| task missed its run | a scheduled time passed with no run |
| issue waits on you | a comment asks for a decision, a look or a review, and nothing answers it |
| issues queued and no worker is running | there is a backlog and nobody is on it |
| worker has been quiet | a worker's window had no output for 30 minutes |

### Waiting on you: comment signals

A worker cannot ask you a question, so it writes one in its issue. shed looks
for these phrases in issue comments (configurable under `signals`):

| Signal | Opens when a comment has | Stays closed when followed by | Closes when a later comment has |
| --- | --- | --- | --- |
| decision | `Decisions needed:` | `none` | `Owner ruling` |
| eyes | `Needs eyes:` | `nothing`, `none` | |
| review | `**PR:**` | | |

A signal with no closing phrase stays open until the issue closes or a later
comment has the opening phrase with a clear value.

## Setup

Needs Go 1.25+ to build, and `ssh` and [`gh`](https://cli.github.com) on the watcher.

```sh
git clone https://github.com/edisoncode/ai-shed && cd ai-shed
make build                                  # ./shed
mkdir -p ~/.config/shed
cp shed.example.yaml ~/.config/shed/shed.yaml   # then edit
./shed validate
./shed status
./shed status -watch 1m                     # live view
./shed status -json                         # for scripts
```

shed reads the fleet file from `-config`, then `$SHED_CONFIG`, then
`./shed.yaml`, then `~/.config/shed/shed.yaml`. See
[`shed.example.yaml`](shed.example.yaml) for every field.

Use `host: local` for a machine that is also the watcher.

### Scheduled tasks

Only machines with `tasks` need the agent.

```sh
make dist                 # binaries for linux and macOS in dist/
./shed deploy             # or: ./shed deploy linux-box
```

`deploy` copies the right binary to `~/.local/bin/shed`, the fleet file to
`~/.config/shed/shed.yaml`, and the machine's name to `~/.config/shed/machine`.
Then start the agent once on each machine:

- **Linux (systemd):** copy [`contrib/shed-agent.service`](contrib/shed-agent.service)
  to `~/.config/systemd/user/`, then
  `systemctl --user enable --now shed-agent` and `loginctl enable-linger $USER`
  (so it runs without a login session).
- **macOS (launchd):** copy [`contrib/com.edisoncode.shed-agent.plist`](contrib/com.edisoncode.shed-agent.plist)
  to `~/Library/LaunchAgents/`, then
  `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.edisoncode.shed-agent.plist`.

After that, run `shed deploy` again whenever the fleet file changes. The agent
loads a changed fleet file within 30 seconds; a new binary takes effect when
the agent restarts.

Task rules:

- A task runs through `sh -c`, after the machine's `init` line.
- `timeout` defaults to 1 hour. A task that passes it is killed with its children.
- A task that is still running when its next time comes is skipped (and shows as missed).
- Runs are recorded in `~/.local/state/shed/runs.jsonl` with the last 2000 bytes of output.

## Limits

- shed watches and schedules. It does not start workers on issues; a
  scheduled task can do that.
- An issue is matched to a worker by number only. Two repos with the same
  issue number on one machine share a match.
- A missed run is known only after the task has run once.
- `runs.jsonl` is not rotated. It grows by two lines per run.
- Linux and macOS only.

## Development

```sh
make lint test
```
