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
| `limited` | it reached a usage limit and must wait for the reset | types nothing, looks again just after the reset (every 15 minutes when the screen names no time), then tells it to continue |

The reviewer may not make your decisions. It tells the worker to write the
question in the issue and take the next item. It never tells a worker to
merge, deploy, or weaken a test.

Guards against wasted tokens:

- Three nudges in 30 minutes that do not get a worker moving end with `stuck`, not a fourth nudge.
- A session that exits three times in 30 minutes is not started a fourth time.
- A failed check-in (`gh` is logged out, the reviewer command is broken) is retried after 10 minutes, not every tick.
- A usage limit costs no nudges. A limited worker is left alone until its reset. A reviewer that is itself at its limit is tried again after 15 minutes and is not reported as a failure.
- An idle worker is reviewed once per silence, not once per tick.

### The queue

A worker's queue is the open issues of the machine's `issues` sources, or of
the worker's own `issues` when it has them. The supervisor hands them out one
at a time, in this order:

1. **Rework.** An issue that was handed back with a pull request that can no
   longer merge: it conflicts with its base branch, or a check failed.
   Finishing started work comes before starting more. The worker is told what
   is wrong and to fix only that. An issue is sent back at most twice in six
   hours; after that it is yours, and `shed status` says so. A check that
   fails for a reason no worker can fix must not keep one busy all night.
2. **Priority.** The labels in the source's `priority` list, first to last.
3. **Age.** Oldest first.

```yaml
machines:
  - name: linux-box
    issues:
      - repo: my-org/my-app
        label: "machine:linux-box"
        priority: ["urgent", "quick"]
    workers:
      - name: app            # takes from the machine's queue
      - name: docs           # has a queue of its own
        issues:
          - repo: my-org/my-app
            label: "area:docs"
```

An issue that waits on you is skipped. An issue that one worker has in hand is
never handed to another, whatever their queues.

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

- A worker that was stopped by a usage limit keeps its context when it
  resumes, however cold: it was cut off mid-task and its context is the only
  record of that work.

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
the same moment. The brief tells the worker to keep its plan and progress in
the issue for this reason, and the reviewer asks for it when a worker stops
partway through an item: the new model starts from the brief, not from a re-read of
the old conversation. Two issues in a row on the same model keep the warm
context. With no `models` block the agent never touches the model.

This switches the model of the worker's own session. For a worker that is
an orchestrator and hands work to sub-agents on models it chooses itself, set
`apply: tell`: the agent leaves the session and its context alone, and adds
"The model for this issue is opus." to the message that hands over the issue.
Say in the standing orders what the worker does with that. Set the
orchestrator's own model in the worker's `command`.

### Fresh context per issue

```yaml
workers:
  - name: app
    fresh_per_issue: true
```

With this, the agent clears the worker's context each time it hands over a
new issue. The worker starts the issue from its brief and the issue itself,
without the last issue's files, dead ends and assumptions, and without paying
to carry that conversation through every turn. Use it for a queue of
independent issues. Leave it off for themed work, where what the worker
learned on one issue helps with the next.

A message about the issue already in hand never clears anything, and the
first issue of a session is not cleared, since that context is already empty.
The agent remembers the issue in hand across its own restarts.

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
| eyes | `Needs eyes:` | `nothing`, `none` | `Eyes checked` |
| review | `**PR:** #41` | | that pull request is merged or closed |

While that pull request is open but cannot merge (a conflict, a failed check),
the issue does not wait on you: it goes back to a worker. See *The queue*.

The review signal follows its pull request (`follows_pr`). An issue that takes
several pull requests comes back into the queue each time one is merged, and
the worker picks up the rest from what the issue says.

An issue with an open signal is not handed to a worker.

## Setup

The watcher needs `ssh` and [`gh`](https://cli.github.com), logged in. A
machine with workers needs `tmux`, `gh`, and the agent tool. Nothing needs Go.

Install the latest release into `~/.local/bin`:

```sh
platform="$(uname -s | tr A-Z a-z)-$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"
gh release download --repo EdisonCode/ai-shed --pattern "shed-$platform" --output ~/.local/bin/shed --clobber
chmod +x ~/.local/bin/shed
```

Then:

```sh
mkdir -p ~/.config/shed
gh api repos/EdisonCode/ai-shed/contents/shed.example.yaml --jq .content | base64 -d > ~/.config/shed/shed.yaml   # then edit
shed validate
shed status
shed status -watch 1m                       # live view
shed status -json                           # for scripts
```

To update: `shed update` replaces the binary with the latest release, checked
against the release's published checksum. Then `shed deploy` brings the
machines to the same version.

shed reads the fleet file from `-config`, then `$SHED_CONFIG`, then
`./shed.yaml`, then `~/.config/shed/shed.yaml`. See
[`shed.example.yaml`](shed.example.yaml) for every field.

Use `host: local` for a machine that is also the watcher.

### The agent

Machines with `workers` or `tasks` need the agent.

```sh
shed deploy               # or: shed deploy linux-box
shed deploy -install-agent linux-box    # first time on a machine
```

For each machine, `deploy`:

1. Sends the fleet file to `~/.config/shed/shed.yaml` and the machine's name
   to `~/.config/shed/machine`.
2. Installs this shed's own version at `~/.local/bin/shed`, unless the machine
   already runs it. The binary for the machine's platform comes from the
   release, checked against its checksum.
3. Restarts the agent when it installed a new binary, so the agent runs it.
   Workers keep running across that restart.
4. Runs your deploy hook, if you set one.
5. Runs a preflight of each worker that has no live session.
6. With `-install-agent`, sets up and starts the agent service on a machine
   that has none, if every worker is ready.

**The deploy hook.** A worker's tool usually needs more from your machine than
shed sends: agent definitions, skills, notes. shed does not know your tool's
layout, so it runs a command of yours. `defaults.deploy_hook` runs on the
watcher after each machine's copy succeeds, with `SHED_MACHINE` and
`SHED_HOST` set. A failing hook fails the deploy of that machine.

```yaml
defaults:
  deploy_hook: rsync -a ~/.claude/agents ~/.claude/skills "$SHED_HOST:.claude/"
```

**The preflight.** An agent tool asks first-run questions (trust this folder,
enable these integrations) that only a person may answer, and a worker stopped
at one does no work. So `deploy` starts each worker's command once, with no
prompt, in a throwaway tmux window on the machine, waits for the screen to
settle, and closes the window. It types nothing else. The reviewer reads each
screen and `deploy` prints one line per worker:

```
mini: worker app: ready (clean start)
mini: worker docs: needs you: question: asks whether to trust the folder
    | Do you trust the files in this folder?
    | ❯ No, exit
```

- A worker that already has a live session is left alone.
- A worker whose directory is missing or has uncommitted changes is not started.
- A worker that is not ready fails the deploy of its machine. Answer the
  question on the machine (`tmux attach`, start the command, answer, exit) and
  deploy again. `-preflight=false` skips the check.
- The reviewer runs on the watcher, so it must work there.
- `shed preflight` on a machine runs the same check by hand.

**The agent service.** `-install-agent` writes the service file and starts it:
a systemd user unit on Linux (with lingering, so it runs without a login
session), a launchd agent in the GUI domain on macOS. The files are in
[`contrib/`](contrib/) if you want to install them yourself.

**macOS and the keychain.** An agent tool that keeps its login in the keychain
cannot read it from an SSH session. The agent service runs in the GUI domain,
which can. The tmux server that holds the workers must have been started from
the GUI domain too. The agent uses the default tmux server: if one is already
running (for example from your own launchd job), the workers join it; if none
is, the agent starts it, from the GUI domain. Do not start that server over
SSH. For the same reason, a preflight on a Mac with no tmux server running
reports a login problem that the agent itself will not have: start the server
from the GUI domain first, or pass `-preflight=false`.

After that, run `shed deploy` whenever the fleet file changes or after a
`shed update`. The agent loads a changed fleet file within 30 seconds without
a restart.

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
- A stale pull request is judged by any failed check, required or not.
- An issue is matched to a window by number only. Two repos with the same
  issue number on one machine share a match.
- A missed task run is known only after the task has run once.
- The log files are not rotated.
- Linux and macOS only.

## License

[Apache-2.0](LICENSE).

## Development

Needs Go 1.25+.

```sh
make hooks        # once per clone: the pre-push hook runs the gates below
make lint test
make install      # this working tree's build into ~/.local/bin
make dist && shed deploy -dist dist   # send an unreleased build to the machines
```

A release is a version tag. Pushing `v1.2.3` runs the gates, builds the
binaries for Linux and macOS, and publishes them with checksums on the GitHub
release.
