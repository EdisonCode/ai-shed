# ai-shed

`shed` keeps the worker machines in a homelab productive while you are away.
Give each machine a queue of GitHub issues or a brief with a theme, close the
laptop, and a small agent on each machine keeps its AI coding sessions moving:
it starts them, checks in when they go quiet, pulls them back when they leave
their brief, and writes down what needs you. One command shows you the result.

```
$ shed status
linux-box  me@linux-box  load 0.52/8  disk 42%  agent ok
  checks   ok gh auth, ok git identity, ok claude, FAIL docker
  windows
    shed:app  claude  active 40s ago
  supervised
    app  nudge  12m ago  it finished #123 and the queue has #130
  issues
    my-org/my-app#123  review             Retry the export when the API times out
    my-org/my-app#124  decision           Drop the legacy column
    my-org/my-app#130  working   app-130  Paginate the audit log
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
  checks it must pass, the issues it owns, the workers it runs and the tasks
  it schedules.
- **Watching needs nothing installed.** `shed status` makes one SSH round trip
  per machine with your own `ssh` and keys, and reads issues with your own
  `gh` login. No server, no database, no tokens to manage.
- **The agent does the work that must happen while you are away.** `shed agent`
  is the same binary, running on the machine. It supervises the machine's
  workers and runs its scheduled tasks, and writes what it did to two log
  files that `shed status` reads back.
- **Any agent tool.** A worker is a command in a tmux window; the reviewer is
  a command that reads a prompt and prints an answer. The defaults are Claude
  Code. Every command that shed types or runs is a setting.

## Supervised workers

A worker is a long-running agent session with a brief. The agent keeps one
tmux window per worker in a session named `shed`, and it never types into a
window outside that session.

```yaml
machines:
  - name: linux-box
    host: me@linux-box
    issues:
      - repo: my-org/my-app
        label: "machine:linux-box"
    workers:
      - name: app
        dir: ~/Projects/my-app
        command: claude --permission-mode auto
        brief: |
          Work the audit log issues. Do not change the billing module.
```

Every 30 seconds the agent looks at each worker:

| It sees | It does |
| --- | --- |
| no window, or the session exited to the shell | writes the brief to `.shed/BRIEF.md` in the worker's directory and starts the session |
| output still flowing | nothing, until the next scope check (every 30 minutes): is the work still inside the brief? |
| silent for 90 seconds | a check-in |
| resting after a check-in | asks GitHub every 5 minutes whether the queue changed; a new issue or an answer from you triggers a check-in |
| an edited brief | rewrites `.shed/BRIEF.md` and tells the worker to read it again |

A **check-in** shows a reviewer model the brief, your standing orders, the
queue, and the last 80 lines of the worker's terminal. The reviewer returns
one verdict:

| Verdict | Meaning | The agent |
| --- | --- | --- |
| `on_track` | working inside the brief, or waiting on something it started | says nothing |
| `nudge` | one message would get work moving: next issue, an answer the brief already gives, a pull back into scope | types the message |
| `needs_owner` | nothing can move without you, or a permission prompt or menu is open | types nothing; `shed status` shows it |
| `done` | nothing left that it can act on | leaves it idle |

The reviewer may not make your decisions. It tells the worker to write the
question in the issue and take the next item. It never tells a worker to
merge, deploy, or weaken a test.

Guards against wasted tokens:

- Three nudges in 30 minutes that do not get a worker moving end with `stuck`, not a fourth nudge.
- A session that exits three times in 30 minutes is not started a fourth time.
- A failed check-in (the reviewer hit a rate limit, `gh` is logged out) is retried after 10 minutes, not every tick.
- An idle worker is reviewed once per silence, not once per tick.

### Prompt cache

A long session is cheap only while its prompt cache is warm. After the cache
lifetime passes, the next message makes the model read the whole conversation
again at full price.

- The first check-in on an idle worker comes about two minutes after it goes
  quiet. That is inside any cache lifetime, so a nudge lands on a warm cache.
- A worker with nothing to do is left alone. Its cache expires and that costs nothing.
- When work arrives for a worker whose cache has expired, the agent clears the
  worker's context first and tells it to re-read its brief and the state of the
  work. Set `when_cold: resume` to keep the context and pay for the re-read.

shed cannot see the cache. `supervisor.cache_ttl` (default `5m`) is what you
tell it your plan gives.

### A model per issue

Optional. Map issue labels to models, and the agent switches the worker's
model when it hands the worker an issue:

```yaml
supervisor:
  models:
    default: sonnet           # an issue with no model label
    labels:
      "model:opus": opus
      "model:fable": fable
```

A model switch empties the prompt cache, so the agent clears the context at
the same moment (the brief tells the worker to keep its plan and progress in
the issue for this reason): the new model starts from the brief, not from a re-read of
the old conversation. Two issues in a row on the same model keep the warm
context. With no `models` block the agent never touches the model.

This switches the model of the worker's own session. **Leave `models` out when
the worker is an orchestrator** that hands work to sub-agents on models of its
own choosing: a switch would change the orchestrator, not the sub-agents. Set
the orchestrator's model in the worker's `command`, and put the rule for
choosing a sub-agent's model, by label or otherwise, in the standing orders.

### Standing orders

`defaults.standing_orders` is text that goes into every worker's brief on
every machine, and the reviewer judges scope against it. Use it for the rules
that do not change from task to task: your architecture principles, your
definition of done, what a worker must never do.

### Settings

```yaml
supervisor:
  command: claude -p --model sonnet   # the reviewer: prompt on stdin, answer on stdout
  cache_ttl: 5m
  scope_every: 30m
  when_cold: clear                    # or: resume
  clear_command: /clear               # typed into a worker to empty its context
  model_command: "/model {model}"     # typed into a worker to switch model
```

For another agent tool, set the worker's `command`, the reviewer's `command`,
and the two typed commands to what that tool understands.

## What needs you

`shed status` puts a `!` line on a machine when:

| Rule | Meaning |
| --- | --- |
| unreachable | SSH failed |
| check failed | a readiness check (auth, config, a tool) exited non-zero |
| disk is N% full | 90% or more of the home filesystem is used |
| agent is not running | the machine has tasks or workers but no fresh heartbeat (3 minutes) |
| worker needs you | the last check-in ended with `needs_owner` |
| worker is stuck | nudges or restarts did not get it moving |
| worker could not be checked | the check-in itself failed |
| task failed | the last run exited non-zero |
| task missed its run | a scheduled time passed with no run |
| issue waits on you | a comment asks for a decision, a look or a review, and nothing answers it |
| issues queued and no worker is running | there is a backlog and nobody is on it |
| worker has been quiet | an issue's window had no output for 30 minutes |

An issue counts as *working* when a tmux window on its machine has the issue
number in its name (`app-123`).

### Waiting on you: comment signals

A worker cannot ask you a question, so it writes one in its issue. shed looks
for these phrases in issue comments (configurable under `signals`):

| Signal | Opens when a comment has | Stays closed when followed by | Closes when a later comment has |
| --- | --- | --- | --- |
| decision | `Decisions needed:` | `none` | `Owner ruling` |
| eyes | `Needs eyes:` | `nothing`, `none` | |
| review | `**PR:** #41` | | that pull request is merged or closed |

A signal with no closing phrase stays open until the issue closes or a later
comment has the opening phrase with a clear value. Give it an `answered_by`
phrase in the fleet file to close it with one comment.

The review signal follows its pull request (`follows_pr`). An issue that takes
several pull requests comes back into the queue each time one is merged, and
the worker picks up the rest from what the issue says.

An issue with an open signal is not handed to a worker.

## Setup

Needs Go 1.25+ to build, and `ssh` and [`gh`](https://cli.github.com) on the
watcher. A machine with workers needs `tmux`, `gh`, and the agent tool.

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

### The agent

Machines with `workers` or `tasks` need the agent.

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

**macOS and the keychain.** An agent tool that keeps its login in the keychain
cannot read it from an SSH session. Two things follow. Load the agent into the
GUI domain, as the command above does, so the reviewer it runs can log in.
And the tmux server that holds the workers must have been started from the GUI
domain too. The agent uses the default tmux server: if one is already running
(for example from your own launchd job), the workers join it; if none is, the
agent starts it, from the GUI domain. Do not start that server over SSH.

**Before the first unattended run**, start the worker's command once by hand
in the worker's directory. An agent tool asks first-run questions (trust this
folder, enable these integrations) that only a person may answer. The
supervisor reports a worker stopped at one as `needs_owner` and types nothing.

After that, run `shed deploy` again whenever the fleet file changes. The agent
loads a changed fleet file within 30 seconds; a new binary takes effect when
the agent restarts. Workers keep running across an agent restart.

Watch a worker with `tmux attach -t shed` on its machine. What the supervisor
saw and did is in `~/.local/state/shed/checkins.jsonl`.

### Scheduled tasks

- A task runs through `sh -c`, after the machine's `init` line.
- `timeout` defaults to 1 hour. A task that passes it is killed with its children.
- A task that is still running when its next time comes is skipped (and shows as missed).
- Runs are recorded in `~/.local/state/shed/runs.jsonl` with the last 2000 bytes of output.

## A skill for your agent

[`skills/shed/SKILL.md`](skills/shed/SKILL.md) teaches an AI assistant to
operate the shed for you: label issues for a machine and a model, write a
brief, deploy, and pick up the results. It is plain markdown.

- **Claude Code:** `cp -r skills/shed ~/.claude/skills/`
- **Other tools:** give the file to the agent, or include it from your `AGENTS.md`.

## The repository matters more than the supervisor

shed keeps workers moving; it does not make their work safe to accept.
[`docs/REPO-RECIPE.md`](docs/REPO-RECIPE.md) is the author's recipe for a
repository that unattended workers can work in: a protected main branch, pull
requests only, pre-push gates, exhaustive CI, staging with browser smoke tests, and owner
sign-off before production.

## About this project

ai-shed is built with AI and built for working with AI. Its author is a solo
founder who has built software with AI tools for a year. This tool comes out
of the frustrations of that year: sessions that stop at a question nobody is
there to answer, sessions that wander off the task, and machines that sit idle
overnight. It exists to keep that work productive when nobody is at the
keyboard of the machines that do it.

The code in this repository was written by an AI coding agent under the
author's direction.

## Limits

- The supervisor reads a terminal. A reviewer model can misjudge it. The
  guards above bound the cost of a wrong nudge; they do not make it right.
- Workers on one machine share that machine's queue. With two workers, split
  the work in their briefs.
- An issue is matched to a window by number only. Two repos with the same
  issue number on one machine share a match.
- A missed task run is known only after the task has run once.
- The log files are not rotated.
- Linux and macOS only.

## Development

```sh
make hooks        # once per clone: the pre-push hook runs the gates below
make lint test
```
