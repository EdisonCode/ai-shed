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
  ! my-org/my-app#123 waits on you (review #131 5h): Retry the export when the API times out
  ! my-org/my-app#124 waits on you (decision 2h): Drop the legacy column

mac-mini  me@mac-mini  UNREACHABLE
  ! unreachable: me@mac-mini: exit status 255: Operation timed out

4 item(s) need you.
Waiting on you: 1 review (oldest 5h), 1 decision (oldest 2h).
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
| `held` | (set by the agent, not the reviewer) its next issue waits for the machine to have room | delivers it when there is room, or after `max_wait` |
| `limited` | it reached a usage limit and must wait for the reset | types nothing, looks again just after the reset (every 15 minutes when the screen names no time), then tells it to continue |

The terminal is captured with its styles. Text that the worker's tool draws
faint, such as a placeholder or a suggestion of what to type next in an empty
input line, reaches the reviewer marked `[greyed out: ...]`, so it is not
taken for something that was typed or sent. The reviewer is also told which
issue the supervisor's own log says the worker has in hand; the screen is not
the record of what was handed over.

The reviewer may not make your decisions. It tells the worker to write the
question in the issue and take the next item. It never tells a worker to
merge, deploy, or weaken a test.

Guards against wasted tokens:

- Three nudges in 30 minutes about the same work that do not get a worker moving end with `stuck`, not a fourth nudge. A hand-over of the next issue starts the count again, and so does a new session: a worker that finishes short issues quickly is not stuck.
- A session that exits three times in 30 minutes is not started a fourth time.
- A failed check-in (`gh` is logged out, the reviewer command is broken) is retried after 10 minutes, not every tick.
- A usage limit costs no nudges. A limited worker is left alone until its reset. A reviewer that is itself at its limit is tried again after 15 minutes and is not reported as a failure.
- An idle worker is reviewed once per silence, not once per tick.
- A resting worker is reviewed again only when its queue has something new it can act on. A label, a comment or a hand-back that leaves an issue waiting on you costs no review.

### The queue

A worker's queue is the open issues of the machine's `issues` sources, or of
the worker's own `issues` when it has them. The supervisor hands them out one
at a time, in this order:

1. **Rework.** An issue that was handed back and needs a worker again while
   its pull request is still open. Finishing started work comes before
   starting more. There are two causes:
   - *You answered.* A comment with the answering phrase of a `sends_back`
     signal (by default `Owner ruling`) came after the hand-back. The worker
     is told to read your answer and apply it. So "yes, and also fix X" needs
     no typing on the machine. An answer that asks for no change says so
     right after the phrase, `Owner ruling: accepted`: it closes the decision
     and costs no worker turn.
   - *The pull request went stale.* It conflicts with its base branch, or a
     check failed. The worker is told what is wrong and to fix only that.

   The worker hands back again when it is done, which ends the rework. It
   does so even when your answer left nothing to change, and says that. An
   issue is sent back at most twice in six hours; after that it is yours, and
   `shed status` says so. A check that fails for a reason no worker can fix
   must not keep one busy all night.

   While an issue is due back to a worker, its `eyes` and `review` signals do
   not count as waiting on you: the work is about to change. An open
   `decision` still does.
2. **Priority.** The labels in the source's `priority` list, first to last.
3. **Age.** Oldest first.

An issue you bumped goes ahead of all three; see
[Putting an issue first](#putting-an-issue-first).

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
never handed to another, whatever their queues. A worker's brief also tells it
to look, before it starts an issue, for an open pull request or an unmerged
branch that already covers it. If it finds one that it was not sent back to,
it does not start: it reports the overlap in the issue as a decision for you.

### Putting an issue first

A priority label orders a queue. It cannot help an urgent issue that sits in
the queue of a worker with hours of work in hand while another worker on the
machine is free. For that, say it directly:

```sh
shed bump linux-box 41                # first in the queue it is in
shed bump -worker docs linux-box 41   # first for this worker, whatever its queue
shed bump -undo linux-box 41          # back to its usual place
```

- **It goes ahead of everything in the queue**: rework, eye checks and
  priority labels. Several bumped issues keep their usual order among
  themselves.
- **With `-worker` the issue moves.** It joins that worker's queue, from any
  queue of the machine, and leaves the other workers' queues. It may be
  outside that worker's brief: the worker and the reviewer are both told
  that the owner put it there. Standing orders still hold.
- **Nobody is interrupted.** The issue is handed over at the next point where
  the worker has nothing in progress. A resting worker picks it up at its
  next queue check, within five minutes. An issue that a worker already has
  in hand stays with that worker.
- **It lasts as long as the issue is open**, so a moved issue that comes back
  as rework comes back to the worker that built it. A move ends by itself
  once the issue is closed or leaves the machine's queues.
- The issue must be open in one of the machine's queues. To send an issue to
  another machine, change its label.

`shed status` marks the issue `(bumped)` or `(bumped to docs)`.

### Eye checks on staging

A worker tests what it builds, and some work still needs someone to look at
it running: a page, a flow, a number on a screen. The worker asks for that in
its hand-back with `Needs eyes:`. You merge on the diff, staging is deployed
when you deploy it, and by then the issue is usually closed. Tell shed how to
see what staging and production serve, and a worker with a browser does the
look on staging before the work goes to production.

```yaml
repos:
  - repo: my-org/my-app
    staging:
      url: https://staging.example.com
      # Any command that prints the commit staging serves.
      commit: "curl -fsS https://staging.example.com/healthz | jq -r .commit"
      orders: |
        You may sign in as the test user, open any page and save a draft.
        Never place an order, send a message or change another user's data.
    production:
      commit: "curl -fsS https://app.example.com/healthz | jq -r .commit"
    look_back: 336h      # the default: 14 days

machines:
  - name: mac-mini
    workers:
      - name: app
        eye_checks: true   # this worker has a browser
```

- **What owes a look.** An issue, open or closed, whose last hand-back names
  a pull request and asks for eyes, when that pull request was merged after
  production's commit. Work that production already serves is not looked at:
  it is too late to gate. With no `production` command, or when production is
  older than `look_back`, the window is `look_back`.
- **When it is due.** When staging's commit has the pull request's merge
  commit in its history. Until then the look waits for a staging deploy, and
  nothing is asked of anyone.
- **Who does it.** A worker with `eye_checks: true`, ahead of new issues.
  Where another such worker shares the queue, not the worker that built the
  work: a second reader catches a hand-back that is too vague to check.
- **What the worker may do.** Its brief gets the staging address and your
  `orders`. An act the orders do not name is not allowed. Name the writes a
  check needs and the ones that must never happen.
- **A pass.** The worker comments `Eyes checked:` with what it saw.
- **A failure.** The worker reopens the issue and hands it back with
  `Eyes failed:` and a `Decisions needed:` for you. It does not fix it: a
  failed look is new information, and whether it is a regression, an older
  defect or the feature working as specified but wrong is yours to scope.
  Your `Owner ruling` then sends the issue to a worker as usual.
- **A check that could not be done.** The browser is signed out, staging is
  down, the description cannot be judged, or the check needs an act the
  orders forbid. The worker comments `Eyes blocked:` with what stopped it
  and takes its next item: a look only you can clear does not hold a worker
  idle. The look is yours now. `shed status` counts it as not done and lists
  it with the worker's reason, and your `notify` command hears of it. When
  the cause is gone, comment `Eyes unblocked` on the issue and a worker
  tries again; or look yourself and comment `Eyes checked:`. A blocked look
  never counts as passed.
- **When staging's commit cannot be read**, no look is handed out, the
  building goes on, and `shed status` says so.

`shed status` ends with each repository's release state, the go or no-go for
a production deploy:

```
my-org/my-app  staging 3f2a1c9  production 9b1e0d4
  14 pull request(s) merged since production's commit, 2d ago
  eye checks: 2 not done (1 on staging, 1 wait for a staging deploy), 1 failed
    #123  PR #131  on staging         5h  Retry the export when the API times out
    #127  PR #140  waits for staging  2h  Show the total on the order page
    #125  PR #138  FAILED             1h  Paginate the audit log
```

The commit commands run on the watcher for `shed status` and on the worker's
machine for the queue, after its `init` line, so both must reach the
environments. `shed status -json` carries the release state under `repos`.

### One-off tasks

An issue is all a worker has to go on. Some work has a brief that is on no
issue: the reasoning from a conversation, a comparison to make before
anything is built, a job you want one worker in particular to do. Push it as
a task:

```sh
shed task linux-box brief.md               # the first worker that is free
shed task -worker app linux-box brief.md   # this worker
some-command | shed task linux-box -       # the brief on standard input
```

- **It goes ahead of the queue**, at the first point where the worker has
  nothing in progress. A worker is never interrupted for a task. A resting
  worker gets it within a tick, with no reviewer call.
- **The brief is the worker's whole instruction for it.** It lands in the
  worker's directory as `.shed/tasks/<id>.md`, and one line tells the worker
  to read it. Write it for a reader with no other context, and say where the
  result should go: a pull request, a comment on an issue, or only the
  report.
- **It may go beyond the worker's brief**, where it says so. The reviewer is
  shown the task's brief while the task is in hand and judges the worker
  against it. Standing orders still hold.
- **The worker writes a report** in `.shed/tasks/<id>.report.md`. Read it
  with `shed task -report <id> linux-box`.
- **Afterwards the worker goes back to its queue**, on a clear context when
  it has `fresh_per_issue`.
- A busy machine holds a task like any hand-over, for `max_wait` at most. A
  task that arrives while an issue is held goes ahead of that issue. A
  session that ends in the middle gives the task to that worker's next
  session.

`shed status` lists each task as queued, in hand or finished, and shows the
task in place of an issue for the worker that has it:

```
  supervised
    app  task 20261004-143210-512  nudge  8m ago  the owner pushed a one-off task
  one-off tasks
    20261004-143210-512  in hand  app
    20261004-150102-090  queued   any worker
```

A task that still waits can be cancelled, for example to push it again for a
worker that has since come free:

```sh
shed task -cancel 20261004-150102-090 linux-box
```

A task a worker was already handed is not cancelled: the worker has its
brief and may be halfway through.

A task has no issue behind it, so nothing in the queue rules applies to it:
no rework, no signals, no retries. If it leads to a pull request, that pull
request is reviewed like any other.

### Machine capacity

A machine often has other work: CI runners, builds, a database. Tell shed how
to see that the machine is busy, and it stops handing out new issues while it
is.

```yaml
machines:
  - name: linux-box
    capacity:
      max_load: 1.5        # 1-minute load average per core
      busy_when: '[ "$(pgrep -c -x "Runner\.Worker")" -ge 2 ]'   # exit 0 = busy
      max_wait: 20m
```

- **Scaling down means not starting more.** A worker that finishes an issue on
  a busy machine is not handed the next one. Work in progress is never
  stopped, and a nudge about the issue in hand still goes through. As workers
  finish, the machine sheds its load by itself.
- **Scaling up is automatic.** The held hand-over is delivered at the first
  30-second tick on which the machine has room. The reviewer is not asked a
  second time.
- **It resists; it does not refuse.** A hand-over is held for `max_wait` at
  most (default 20 minutes) and then delivered whatever the load. A machine
  that is never quiet still gets its work done, only more slowly.
- **Either test is enough to be busy.** `max_load` covers CPU. `busy_when` is
  any command of yours, run after the machine's `init` line, and it is the
  place for what load does not show. Two examples for Linux:

  Match a process by its exact name (`pgrep -x`), not by its command line
  (`pgrep -f`): `-f` also counts any shell or script whose arguments happen
  to contain the pattern, and the machine then looks busier than it is.

  ```sh
  # two or more GitHub Actions jobs are running on this machine's runners
  [ "$(pgrep -c -x "Runner\.Worker")" -ge 2 ]
  # disk or memory pressure: tasks stalled more than 20% of the last 10 seconds
  awk -F'[ =]' '/^some/ && $3 > 20 {busy=1} END {exit !busy}' /proc/pressure/io /proc/pressure/memory
  ```

- A test that cannot run (the command is missing, the load cannot be read)
  counts as room. A broken test must not stop a machine's work.

`shed status` shows a worker whose next issue is waiting as `held`, with the
reason. It needs nothing from you. With no `capacity` block a machine always
has room.

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

### State from the tool's own hooks

The supervisor reads a worker's screen to learn what it is doing. An agent
tool knows that itself: when a turn starts, when it ends, and when it stops
to ask a person. If the tool has hooks, let them tell shed:

```sh
shed mark working    # a turn started
shed mark idle       # the turn ended
shed mark waiting    # it stopped to ask a person
```

For Claude Code, in `~/.claude/settings.json` on each worker machine:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      { "hooks": [{ "type": "command", "command": "$HOME/.local/bin/shed mark working" }] }
    ],
    "Stop": [
      { "hooks": [{ "type": "command", "command": "$HOME/.local/bin/shed mark idle" }] }
    ],
    "StopFailure": [
      { "hooks": [{ "type": "command", "command": "$HOME/.local/bin/shed mark idle" }] }
    ],
    "Notification": [
      {
        "matcher": "permission_prompt|elicitation_dialog",
        "hooks": [{ "type": "command", "command": "$HOME/.local/bin/shed mark waiting" }]
      }
    ]
  }
}
```

What it changes:

- **A worker that waits for a person is `needs_owner` at once**, with the
  tool's own words as the reason ("Claude needs your permission to use
  Bash") and with no reviewer call. When the prompt is answered and the
  screen moves on, the worker is back on track by itself.
- **A turn that prints nothing is not taken for idle.** A long test run or a
  build is silent for minutes. Without a mark the worker is reviewed after
  ninety seconds of silence; with one it is busy and gets only its scope
  check.
- **`shed status` shows it.** Each supervised worker gets the tool's state
  and how long it has lasted:

  ```
    supervised
      app   #130      working 3m   on_track     12m ago  working on the brief
      docs  no issue  idle 41m     done         40m ago  the queue has nothing it can act on
      ops   #127      waiting 6m   needs_owner  6m ago   its tool waits for a person: Claude needs your permission to use Bash
  ```

`shed mark` knows the worker from the tmux window the hook runs in, so the
hooks can sit in the user's settings and serve every session on the machine:
outside a worker's window the command does nothing. It reads the message of
a notification from the hook's input. It never exits 2, which to Claude Code
would block the prompt or keep the session from stopping.

All of this is optional. A worker whose tool reports nothing is judged from
its screen, as before. Set all four hooks or none: a tool that says when a
turn starts but not when it ends looks busy until its next scope check.

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
| cannot read the release state | a `repos` commit command failed or did not print a commit, or GitHub could not be asked |
| eye checks are due on staging | merged work is on staging, owes a look, and no worker has `eye_checks` |
| the eye check is blocked | a worker could not do it and said why; clear the cause and comment `Eyes unblocked`, or look yourself |
| issues queued and no worker is running | there is a backlog and nobody is on it |
| worker has been quiet | an issue's window had no output for 30 minutes |

An issue counts as *working* when a tmux window on its machine has the issue
number in its name (`app-123`).

### Waiting on you: comment signals

A worker cannot ask you a question, so it writes one in its issue, in its
hand-back: a comment that contains the word `Hand-back` (configurable as
`handback`). In hand-back comments shed looks for these phrases
(configurable under `signals`):

| Signal | Opens when a comment has | Stays closed when followed by | Closes when a later comment has |
| --- | --- | --- | --- |
| decision | `Decisions needed:` | `none` | `Owner ruling` (and sends the issue back to a worker, unless it reads `Owner ruling: accepted`) |
| eyes | `Needs eyes:` | `nothing`, `none` | `Eyes checked`, or `Eyes failed` from a worker that looked on staging |
| review | `**PR:** #41` | | that pull request is merged or closed |

While that pull request is open but cannot merge (a conflict, a failed check),
the issue does not wait on you: it goes back to a worker. See *The queue*.

The review signal follows its pull request (`follows_pr`). An issue that takes
several pull requests comes back into the queue each time one is merged, and
the worker picks up the rest from what the issue says.

An ask counts only in a hand-back comment. The same words in a plan, a brief
or a discussion on the issue ask you nothing and hold nothing back. Your
answers (`Owner ruling`, `Eyes checked`) count in any comment.

An issue with an open signal is not handed to a worker.

An eye check that a worker will do on staging is not asked of you. Such an
issue shows as `eye check` and is left out of the count; see *Eye checks on
staging*.

Each `waits on you` line says how long ago the worker asked and, for a review,
which pull request: `(eyes 3h, review #41 3h)`. Under the report, one line
totals what waits on you by kind with the age of the oldest. When the workers
are idle, that line is where the work is.

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
shed status -json                           # for scripts: {"machines": [...], "repos": [...]}
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

One deploy of a machine runs at a time. A second one, from this watcher or
another, is refused while the first runs.

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

### Updating while workers are busy

`shed update && shed deploy` is safe in the middle of the night's work.

- **Workers are separate processes.** A restart of the agent does not touch a
  worker's session.
- **The agent stops between messages, never inside one.** It is asked to stop,
  finishes what it is typing to a worker, and exits. Only a call to the
  reviewer is cut short, and that is not recorded as a failure.
- **A restart costs a worker nothing.** The new agent rebuilds what the old one
  knew from the check-in log: the issue each worker has in hand, its model,
  what was last reviewed, how many nudges it has had, a usage limit it waits
  on. No extra review, no repeated message.
- **An edited brief does not interrupt.** The file is rewritten at once. A
  worker with an issue in hand is told with its next message; an idle worker
  is told straight away.
- **The preflight leaves a running worker alone.**

To get a worker onto new tooling (an integration you installed, a changed
agent definition), ask for a fresh session:

```sh
shed recycle linux-box app        # after the issue it has in hand
shed recycle -now linux-box app   # at once
```

The agent ends the session at the next point where nothing is in progress and
starts a new one, which reads the brief and is handed the next issue.
`shed status` shows the request while it waits.

A scheduled task that is running when the agent restarts is stopped and
recorded as stopped.

### Tidying up after merged work

A worker starts each issue on a new branch, and its tool makes worktrees for
sub-tasks. Nothing removes either when the pull request merges, and after a
few weeks a machine has dozens.

```sh
shed tidy linux-box          # list what is merged; change nothing
shed tidy -apply linux-box   # remove it
```

```
linux-box  /home/me/Projects/my-app/.git  (my-org/my-app)
  merged, would be removed
    worktree  /home/me/Projects/wt-123-retry-export  [task/123-retry-export]  merged in #131
    branch    task/118-audit-log-pages                                        merged in #125
  stays
    worktree  /home/me/Projects/my-app  [main]                    the clone itself
    worktree  /home/me/Projects/app-worker  [task/130-paginate]   a worker's directory
    worktree  /home/me/Projects/wt-127-totals  [task/127-totals]  2 uncommitted or untracked file(s)
    branch    task/140-spike                                      no merged pull request ends at its commit

linux-box: 1 worktree(s) and 1 branch(es) are merged. Nothing was changed. Run `shed tidy -apply linux-box` to remove them.
```

It looks at the clone behind each worker's `dir` and removes:

- a **worktree** that is on a branch, has nothing uncommitted or untracked,
  and whose last commit is the last commit of a merged pull request;
- a **local branch** whose last commit is the last commit of a merged pull
  request, unless a worktree that stays has it checked out.

It never removes the clone itself, a worker's directory, a detached
worktree, or anything with a commit that no merged pull request ends at.
"Merged" is asked of GitHub and not read from the default branch's history:
a squash merge leaves no trace there, and a branch that was only just made
would look merged.

With `-apply`, each removal checks again on the machine that the worktree or
branch is at the commit it was listed with. `git worktree remove` is run
without `--force`, so git refuses one that gained changes in the meantime.
What was left alone is listed with the reason, and the exit code is 1.

Remote branches are not touched. `shed tidy` runs on the watcher and needs
only `git` on the machine, so it works on a machine whose shed is older.

### A digest

```sh
shed digest              # the last 24 hours
shed digest -since 12h
```

One report of what merged in the window, how the eye checks went, and what
waits on you now, oldest first:

```
shed digest: the last 24h, since Sat 09:00

Merged: 3 pull request(s)
  my-org/my-app#131  20h ago  Retry the export when the API times out
  my-org/my-app#138  6h ago   Paginate the audit log
  my-org/my-app#140  2h ago   Show the total on the order page

Eye checks: my-org/my-app
  1 passed in this window; now 1 not done (1 on staging, 0 wait for a staging deploy), 1 failed
    #123  PR #131  passed  3h ago  Retry the export when the API times out
    #125  PR #138  FAILED  1h ago  Paginate the audit log

Waiting on you: 2
  30h  decision     my-org/my-app#124  Drop the legacy column
  2h   review #141  my-org/my-app#130  Split the importer

Also needs you: 1
  ! mac-mini: unreachable: me@mac-mini: exit status 255: Operation timed out
```

It changes nothing and exits 1 when something needs you. To get it every
morning, run it from a scheduler on the watcher and send the output wherever
you read. `-json` prints the same as data.

### An issue template

```sh
shed template issue > .github/ISSUE_TEMPLATE/worker-task.md
```

A worker reads its issue and nothing else, and it cannot ask. The template
has the sections such an issue needs: the goal, where the change belongs,
acceptance a worker can run, what is out of scope, what was decided
already, and what needs eyes. Edit it to your repository's vocabulary.

### Hearing about it

With the laptop closed, `shed status` tells nobody anything. Set a command and
the agent runs it, on the machine, when a worker starts to need you (it needs
a decision, it is stuck, it could not be checked) or a scheduled task starts
to fail:

```yaml
defaults:
  notify: curl -s -d "$SHED_MESSAGE" https://ntfy.sh/my-private-topic
```

The message is in `SHED_MESSAGE`; `SHED_MACHINE` and `SHED_WORKER` say where
it came from. It runs once when the state begins, not while it lasts. A usage
limit, a nudge and a finished queue do not notify.

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
  issue number on one machine share a match, and share a bump.
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
