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
  ! my-org/my-app#124 waits on you (decision 2h; returns to app, which is idle): Drop the legacy column
  ! my-org/my-app#123 waits on you (review #131 5h): Retry the export when the API times out

mac-mini  me@mac-mini  UNREACHABLE
  ! unreachable: me@mac-mini: exit status 255: Operation timed out

4 item(s) need you.
Waiting on you: 1 return to a worker (oldest 2h), 1 to read (oldest 5h).
```

The exit code is 1 when anything needs you, so a script or a notifier can act on it.

What waits on you is in the order to answer it, oldest first inside each group:

- **returns**: answering hands the issue back to a worker, ahead of new
  work. A decision whose grace period is over, an issue that was not ready,
  each named with the worker it goes to, and `which is idle` when that
  worker has nothing else.
- **to read**: a review, a page to look at. The worker has moved on.
- **to merge**: reviews a verifier passed at low risk with green checks,
  in one line. Merging is still yours; the tier is an order, not an approval.
- **in its grace period**: a decision the worker still goes on beside.

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
| `limited` | it reached a usage limit and must wait for the reset | types nothing, looks again just after the reset (every 15 minutes when the screen names no time), then tells it to continue. The time on the screen is the latest: see below for a limit that ends sooner |
| `tool_error` | its tool stopped on an API error (a 403, a 429, a 5xx, "overloaded"), not on its work | tells it to continue after 5 minutes, then 15, then 45; `needs_owner` only when the third retry fails |
| `unsent` | a message the supervisor sent sits typed in the worker's input and was not sent | presses Enter once (a carriage return, `C-m`) and looks again; `needs_owner` when the message is still there. The supervisor sees this itself, without a reviewer, when the input box between the screen's last two rules starts with the message it typed |

The terminal is captured with its styles. Text that the worker's tool draws
faint, such as a placeholder or a suggestion of what to type next in an empty
input line, reaches the reviewer marked `[greyed out: ...]`, so it is not
taken for something that was typed or sent. The reviewer is also told which
issue the supervisor's own log says the worker has in hand; the screen is not
the record of what was handed over.

The reviewer may not make your decisions, and it sends a worker on that
stopped for one that was not yours to make; see
[Choices](#choices-yours-and-the-workers). It never tells a worker to merge,
deploy, or weaken a test.

Guards against wasted tokens:

- Three nudges in 30 minutes about the same work that do not get a worker moving end with `stuck`, not a fourth nudge. A hand-over of the next issue starts the count again, and so does a new session: a worker that finishes short issues quickly is not stuck.
- A session that exits three times in 30 minutes is not started a fourth time.
- A failed check-in (`gh` is logged out, the reviewer command is broken) is retried after 10 minutes, not every tick.
- A usage limit costs no nudges. A limited worker is left alone until its reset. A reviewer that is itself at its limit is tried again after 15 minutes and is not reported as a failure.
- A limited worker's line names the limit its screen shows and when it resets: `weekly limit, resets Wed 18:00 (in 46h), held for up to another 6h`. A 5-hour limit means wait. A weekly limit can idle a machine for days, so when every worker of a machine is at its weekly limit, `shed status` and the digest list the machine under what needs you: `switch the machine's account or wait until Wed 18:00`.
- A limit may end before the time the screen names: you reset the usage, or bought more. The screen does not change when that happens, so the hold on a limited worker ends early in three ways:
  - `shed resume <machine>` ends it for every worker of the machine, and `shed resume <machine> <worker>` for one. The worker is looked at within a minute and told to continue. A worker that was at no limit is left as it is.
  - The worker's screen moves. A worker at its limit prints nothing, so one that prints is reviewed like any other.
  - The reviewer answers again after its own limit. It runs on the workers' account, so every held worker is looked at and told to continue.
- `shed status` shows on a `limited` line how long the hold lasts at the most, when the worker last printed anything, and the `shed resume` command that ends it.
- An API error is not yours to fix until it has outlasted three retries. Such an error often clears by itself, and a worker that sits at one for hours has lost them for nothing. When the reviewer saw another worker of the machine on track after the error, the account works, and the retry is sent at once. A sub-agent that died with the error is named in the retry, so the worker dispatches it again. A retry is not counted as a nudge. It keeps the worker's context while the cache is warm, and clears it once the cache has expired.
- An idle worker is reviewed once per silence, not once per tick.
- A resting worker is reviewed again only when its queue has something new it can act on. A label, a comment or a hand-back that leaves an issue waiting on you costs no review.

Guards against a worker that rests beside work:

- One wrong answer of the reviewer does not hold a worker. A worker that has rested 10 minutes after an `on_track` or a `done`, with nothing in hand that it can act on and an item in its queue that it can, is handed that item by the agent itself. No reviewer is asked: the agent already knows both facts. Nothing in hand means no issue yet, an issue that left the queue, or one that waits on you with no grace period left. `shed status` shows the hand-over with "handed over without the reviewer".
- A worker with an issue in hand that it can act on is never handed another this way. If it rests there after an `on_track`, it may wait on something it started, so it is reviewed again every `scope_every`, like a busy worker. With `no_work_after` set, your `notify` command hears of it once when the rest has lasted that long.
- A wait on its own work has a bound. A hung background command looks on the screen like a healthy one, so a worker found `on_track` while idle that then prints nothing for `own_wait_max` (default `45m`) is told by the agent to check what it waits on: its output file, its process, and to end it if it is stuck. The second time in one issue it is `stuck` instead, and nothing more is typed. A new issue or a new session starts the count again. `own_wait_max: "0"` turns the bound off.
- The reviewer sees the shells the worker's tool started, each with how long it has run and its CPU time, marked `unchanged since 45m ago` when that time has not moved. A shell that should have finished long ago, or that does nothing, shows as what it is. The message of the bound lists them too.
- A worker that needs you, is at a limit, sits at an API error or was found stuck is left as that verdict says.

### The queue

A worker's queue is the open issues of the machine's `issues` sources, or of
the worker's own `issues` when it has them. The supervisor hands them out one
at a time, in this order:

1. **Rework.** An issue that was handed back and needs a worker again while
   its pull request is still open. Finishing started work comes before
   starting more. There are two causes:
   - *You answered a question.* An issue that was parked on a decision
     comes back when you rule on it, with or without a pull request.
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

### Work that waits for other work

Two workers that build on the same code at once make work for each other:
the second pull request goes stale when the first merges, and costs a
rebase, a full run of the checks, and sometimes a conflict resolved wrong.
An issue that needs another issue's change first cannot be built at all.
The supervisor holds both kinds back, and hands out what can go ahead.

- **Dependencies.** An issue whose body or comments say `Depends on #12`
  (or `Blocked by #12, org/api#7`), or that GitHub's blocked-by relation
  links to an open issue, is not handed out until those issues are closed.
  The number may be a pull request, which counts as closed once it is merged
  or closed. A number that is neither holds only the issue that names it,
  and `shed status` says so: `depends on #12, which is not an issue or pull
  request`.
  A worker that finds a dependency writes the line itself and hands back.
- **With `stack: true`**, an issue with one open dependency whose pull
  request is ready and waits for review goes ahead: the worker builds it on
  a new branch from that pull request's branch and opens its pull request
  against it.
- **Overlap.** A worker ends its plan with `**Paths:**` and the files and
  directories it expects to change. An issue whose `Paths:` line, in its
  body or a comment, overlaps the paths of an issue another worker of the
  machine has in hand is held until that worker hands it back. So is an
  issue that shares a label with it when the label starts with
  `overlap_labels`:

  ```yaml
  supervisor:
    overlap_labels: "area:"   # area:billing and area:billing overlap; area:auth does not
  ```

  Labels suit code with clear boundaries: a module, a service, a bounded
  context. Paths catch the rest.
- **Started work is never held**: the issue a worker has in hand, rework,
  stacked work and eye checks.
- The reviewer sees `[held: waits for #12 to close]` or `[held: overlaps
  #12, which docs has in hand: both are area:billing]`, and `shed status`
  shows the issue as `held` with the same reason.
- Overlap is checked against the issues held by the workers of the same
  machine, as far as that machine's queues list them. Work in flight on
  another machine is not seen.

### A queue that cannot run dry

A queue is what somebody labelled. When each of those issues waits on you,
the workers have nothing, however long the backlog is. A `backfill` source is
a reserve for that case:

```yaml
machines:
  - name: linux-box
    issues:
      - repo: my-org/my-app
        label: "machine:linux-box"
      - repo: my-org/my-app
        query: 'label:"priority: P1" no:assignee'
        backfill: true
        priority: ["urgent"]
```

- `query` is a GitHub issue search. A source may have one in place of a
  label, with or without `backfill`.
- The reserve is listed only when the sources beside it have nothing the
  worker can act on, counting what the weekly pace holds: ahead of pace, the
  reserve's own `priority` work is handed out. Its issues come after everything else, in its own
  `priority` order and then oldest first. `shed status` does not list them;
  `shed queue` does, with what the reserve left out and why.
- An issue that is handed over gets the label of the first labelled source
  of the same repository in that list, here `machine:linux-box`. That is the
  one thing shed changes on GitHub. From then on the issue is in the
  machine's queue like one you labelled, and it shows in `shed status`. An
  issue that carries the queue label of any machine in the fleet file is
  never taken, so two machines can share one `query`. A backfill source
  needs such a labelled source beside it.
- Nobody prepared a backlog issue for a worker. The worker is told to check
  first that it says the goal or the root cause and has acceptance it can
  test. If not, it changes nothing and posts a hand-back with `Not ready:`
  and what is missing. That opens the `ready` signal: the issue waits on
  you, and no worker is handed it until you fill it in and comment
  `Ready now`.
- A worker takes at most four backlog issues in an hour. Real work takes
  longer than that allows; a run of issues that are not ready must not cost
  a review each, all night.
- The worker's brief still names what it works on. A backlog issue is in
  scope for the worker that is handed it even where the brief does not cover
  it, as a bumped issue is; your standing orders hold.

### Choices: yours and the worker's

Nobody answers while a worker works, so a choice must not stop it unless the
choice is really yours. The `decision` signal says which those are:

```yaml
signals:
  - name: decision
    ask: "Decisions needed:"
    answered_by: "Owner ruling"
    sends_back: true
    accept: ["accepted"]
    risky: anything that changes what is merged or deployed, how money or sign-in behaves, a data migration, or a product choice
    grace: 15m
    decided: "Decided without you:"
```

- **A safe default.** Every choice that `risky` does not name. The worker
  takes the conservative option and goes on. It records the choice in its
  hand-back after `Decided without you:`, with the alternative and how to
  reverse it. `shed status` and `shed digest` show these as information.
  They hold nothing and put no `!` line anywhere. To overrule one, write
  `Owner ruling: ...` on the issue; while its pull request is open the issue
  goes back to a worker.
- **A risky choice.** The worker writes the question and its recommendation
  after `Decisions needed:`, and a clock starts. For `grace` the worker
  stays on the issue and does the parts the question does not touch. An
  answer in that time reaches a worker that still has the issue in its
  context. After it the issue is parked, and the worker is handed the next
  one. Your ruling un-parks it, ahead of new work.

A worker that stops on a choice outside your `risky` list is told by the
reviewer that it is a safe default, to take it and to record it.

`shed digest` counts, over the issues closed in its window, the hand-backs
whose safe defaults held and the ones you overruled with an `Owner ruling`
that is not an acceptance, before the next hand-back. Silence counts as
held: a default nobody looked at shipped as the worker chose it. The count
is by the issue's `overlap_labels` label, or by repository:

```
Safe defaults on the issues closed in this window:
  area:billing  8 held  3 overruled (27%)
  my-org/my-app 4 held  0 overruled (0%)
  overruled: my-org/my-app#124  Kept the old column. — drop it
```

A high rate in one area is a line of yours that workers keep crossing: the
evidence to name that area in `risky`. The digest changes nothing itself. The words
of `risky` are in each worker's brief and in front of the reviewer, so write
them for both.

### Putting an issue first

### Why a worker has no work

`shed status` reads the issues from where you run it. The agent on the
machine knows more: its weekly pace, which issue each worker has in hand,
what the reserve offered. Each time it reads a worker's queue, it keeps that
queue with why it holds each item, and `shed queue` prints it:

```sh
$ shed queue linux-box
linux-box worker app: 0 of 3 item(s) workable, read 2m ago
  my-org/my-app#41          Add export         paced: the account is ahead of its weekly pace (...); only priority work is handed out
  my-org/my-app#44  rework  Fix login          rework that waits: it overlaps #40, which docs has in hand: both are area:auth
  my-org/my-app#47          Retry the import   waits on the owner: decision
  reserve: listed: 0 issue(s) offered; left out: 12 labelled machine:mac-mini, a queue of the fleet (#50, #52, #53, ...)
```

- `shed queue <machine> <worker>` prints one worker's queue, and `-json`
  prints the queues as the agent wrote them.
- The reserve line says why it was not listed (`not listed: #41 can be acted
  on`, or the four-an-hour limit), or what it offered and what it left out,
  by reason.
- `shed status` takes the agent's word: an issue that every worker's queue
  holds is `held` with `agent:` and the agent's reason, and a machine with
  idle workers beside such issues says so, with what the reserve did.

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
- The issue must be open in one of the machine's queues, or in its reserve.
  A bumped reserve issue is handed over like any bumped issue, and gets the
  worker's queue label as a reserve issue does. One with the queue label of
  another machine is that machine's: change the label to move it.
- To send an issue to
  another machine, change its label; a bump right after it works, as the
  issue is read by number when GitHub's issue lists have not caught up with
  the label yet. A refusal names the labels the issue has now.

`shed status` marks the issue `(bumped)` or `(bumped to docs)`.

### Gates on the worker machine

A worker writes a change in minutes; a hosted runner then queues and runs
the whole suite on every push, while the worker waits or moves on and comes
back cold. The worker machines are mostly idle while their agents think.
Give a repository `gates`, its fast checks, and the agent of the machine
that built a pull request runs them on the commit it pushed:

```yaml
repos:
  - repo: my-org/my-app
    gates:
      command: make lint test   # runs after the machine's init line; exit 0 passes
      timeout: 30m              # the default
```

- **What is gated.** Each open pull request that is not a draft, at its
  head commit, when that commit is in the checkout of one of the machine's
  workers: the work this machine built. Each commit is gated once; a new
  push is a new commit. One run at a time per machine.
- **Where.** In a gate tree of the worker's checkout, a detached worktree
  under `~/.local/state/shed/gates/<worker>`. The worker's own checkout is
  never touched. Files git ignores, such as installed dependencies and build
  caches, stay between runs, so a run is fast. `shed tidy` keeps the tree.
- **The machine's room.** A run waits while the machine's `capacity` says
  it is busy, up to `max_wait`, as a hand-over does.
- **The result** is the `shed/gates` commit status: pending while it runs,
  then success or failure with how long it took. A failure also leaves one
  comment on the pull request with the last 60 lines of output, and where the
  whole log is on the machine. A later pass replaces that comment.
- **The worker hears of a failure as rework.** A failed status is a failed
  check, so a handed-back pull request whose gates failed goes back to a
  worker, which is told to fix that and nothing else.
- **A checkout that fails** (a locked worktree, a commit not yet fetched)
  posts nothing and is tried again after 10 minutes. A worker cannot fix it.
- `shed status` lists the last run of each pull request under `gates`.

The status is fast feedback, not proof: a worker shares the machine and its
GitHub login, and could post one too. Keep the full suite where a worker
cannot reach it, in a merge queue; see
[`docs/REPO-RECIPE.md`](docs/REPO-RECIPE.md#a-merge-queue). With both, a
pull request's own CI can shrink to what the gates do not cover.

A `repos` entry may have gates alone. The release state and eye checks need
`staging.commit`, and leave out a repository that has none.

### A limit on open pull requests

Workers that start issues faster than their pull requests merge leave a
pile for the morning: each one waits for review, goes stale as main moves,
and costs another full CI run when it is brought up to date. Give a
repository a limit, and new issues wait while it has that many open pull
requests or more:

```yaml
repos:
  - repo: my-org/my-app
    max_open_prs: 8
```

- **Started work goes on.** The issue a worker has in hand, rework of an
  open pull request, verification, an issue whose question you answered, and
  an issue you bumped are handed out at any count: each brings an open pull
  request closer to its merge, or is your own word.
- **The count is shed's**, read from the repository's open pull requests
  each time the queue is read, drafts and pull requests nobody's worker
  opened included. No worker counts for itself.
- **It says so.** A held issue shows
  `held: my-org/my-app has 9 open pull requests, at or over its limit of 8`
  in `shed status` and to the reviewer, and a worker left with no work for
  that reason is an `!` line: `1 of 3 workers have no work: 4 queued
  issue(s) are held: ...`.

When your CI is the bottleneck and not the workers, hold the CI and not the
work. A draft runs no CI, so count only the pull requests that are ready:

```yaml
repos:
  - repo: my-org/my-app
    max_open_prs: 3
    max_open_prs_count: ready   # the default is all
```

- **At the limit, work goes on.** New issues are still handed out. The
  worker is told `my-org/my-app has 3 ready pull requests, at or over its
  limit of 3: open the pull request of #12 as a draft and do not mark it
  ready`, and names it in its hand-back as `#41 (draft, parked)`.
- **A parked draft is nobody's yet.** It is not a review that waits on you,
  and `shed status` lists it as `draft #41 (parked)`.
- **shed unparks it.** Each time a machine reads its queue and the ready
  count is under the limit, its agent marks its oldest parked draft ready
  (`gh pr ready`), which starts its CI, and says so on the issue:
  `Unparked: pull request #41 is marked ready, so its CI runs`. Only the
  machine that built the work unparks it; two machines with room at the same
  moment may each unpark one.

### Verification before your review

A second reader catches work that does not do what its issue asked. A
strong model told to break every pull request costs as much on a typo as on
a refund, so the depth of the reading follows the risk of the change.
Give a repository `verify`, and a worker `verifies: true`:

```yaml
repos:
  - repo: my-org/my-app
    verify:
      areas:                              # changes here are high risk
        - name: billing
          paths: [internal/billing/, migrations/]
        - name: auth
          paths: [internal/auth/]
      high_lines: 400                     # the default: a code change this large is high risk

machines:
  - name: mac-mini
    workers:
      - name: rev
        verifies: true

supervisor:
  models:
    default: sonnet
    verify: { low: haiku, high: opus }    # optional: the model each tier runs on
```

- **Deterministic first.** A handed-back pull request waits until its checks
  have finished. One that fails a check, `shed/gates` included, goes back to
  a worker as rework and no model reads it.
- **The tier** is rated with no model, from the diff. A pull request is
  `high` when it touches an area, when it changes more than `high_lines`
  lines of code, or when its code changed and no test did. Otherwise it is
  `low`. Documentation alone is never high.
- **The verifier** is handed the pull request ahead of new work, with an
  empty context and on the tier's model. It reads the issue and its
  acceptance, checks the branch out in its own directory, and goes once
  through a `low` pull request; a `high` one it tries to break. It changes
  no code.
- **Its verdict** is a comment on the issue. `Verified (high):` with its
  evidence passes the work, and only then does the review wait on you.
  `Verification failed (high):` with its findings sends the pull request
  back to a worker as rework, as your own ruling would.
- **A second reader.** Where two workers verify the queue, neither is handed
  what it built. A verifier alone verifies its own work in a fresh context.
- While it waits, the issue is `verifying` in `shed status`, not waiting on
  you, and no worker is handed it to build. The digest counts the spend
  under `verification`.
- A repository with `verify` needs a worker that verifies and takes its
  issues, and the fleet file says so when it has none.
- **The escape rate.** `shed digest` counts, over the issues closed in its
  window, each tier's verifications: how many passed, how many sent the
  work back, and how many passes a fault got through. A pass escaped when,
  before the next verification, you sent the work back with a ruling or a
  look on staging failed. Each escape is named.

  ```
  Verification of the issues closed in this window:
    low   18 passed  1 escaped (6%)  3 sent back
    high  5 passed   0 escaped (0%)  2 sent back
    escaped: my-org/my-app#123: the owner sent it back after it was verified
  ```

  It is the evidence for the tiers: widen `low` (fewer `areas`, a larger
  `high_lines`) while its passes do not escape, and tighten it when they do.

### Approval at promotion

Off unless you turn it on. With verification you read every high-risk pull
request and every low-risk one a verifier already passed. Where the escape
rate of `low` has stayed near zero for a while, the merge of a low-risk pull
request can stop being yours, and the promotion to production stays yours:

```yaml
repos:
  - repo: my-org/my-app
    verify:
      merge_low: true
      merge_method: squash      # where the repository has no merge queue; the default is merge
      merge_train: true         # optional, where it cannot have a merge queue: true or batch, see below
```

- **What is queued.** A pull request a verifier passed at low risk, with
  its checks green, that is not a draft, and whose issue asks nothing else
  of you. The machine whose worker built it turns on GitHub's auto-merge:
  the pull request goes into the merge queue, or merges by itself once its
  required checks pass. It comments `Queued for merge by shed:` on the
  issue with the verifier's words. A high-risk pull request waits for you,
  as before. A `Needs eyes:` ask does not hold it back when a worker with
  `eye_checks` looks at the repository on staging: that look can only
  happen after the merge, it shows under `eye checks:`, and a failed one
  reopens the issue for you. Where nobody looks for you, the ask waits on
  you as before.
- **Without a merge queue.** With `merge_train: true`, shed does not turn
  on auto-merge. It merges one pull request at a time: it brings the branch
  up to date with the base, waits for its checks on that head and merges at
  that head. One whose checks fail there is taken off with a comment
  `Off the merge train by shed:` and goes back to a worker. `shed status`
  shows the train in the repository's release state:
  `merge train: PR #41 (#12) in hand 8m, waiting for its checks; next PR #44 (#13)`.
  With `merge_train: batch`, shed tests up to `batch_size` (5) pull requests
  together on one draft pull request, merges each when its checks pass, and
  splits a batch that fails until it finds the pull request that fails. A
  pull request alone on the train whose own green checks already tested the
  base merges directly, without a batch:
  `merge train: batch PR #101 of PR #41 (#12), PR #44 (#13) 25m, checks running`. See
  [the repository recipe](docs/REPO-RECIPE.md#a-merge-train-where-there-is-no-merge-queue).
- **It leaves your list.** A pull request on its way to the merge is not in
  `waits on you`. It reaches staging, and the eye checks run there as
  before.
- **The promotion is the batch you approve.**

  ```sh
  shed promote my-org/my-app
  ```

  ```
  my-org/my-app: 6 pull request(s) merged since production (production 9b1e0d4, 2d ago; staging 3f2a1c9)
  No go yet: see below.

  Queued by shed on the verifier's word, low risk: 4
    #131  #123  on staging  low  Retry the export when the API times out
                                 verified: ran the export tests, tried an empty file
    ...
  Merged by you: 2
    #138  #125  NOT ON STAGING  high  Paginate the audit log

  Looks on staging: 1 not done, 0 failed or blocked
  ```

  It is read-only and exits 1 while something should stop a deploy:
  merged work that staging does not serve, a look not done, failed or
  blocked. The deploy to production is still yours.
- **When it is safe.** Start with a narrow `low`: many `areas`, a small
  `high_lines`. Widen it only while the digest's escape rate for `low`
  stays near zero, and turn `merge_low` off when a low pass escapes.
- **High risk too.** `merge_high: true` (it needs `merge_low`) queues a
  high-risk pass the same way, on the train where there is one, so nothing
  merges behind it with its own auto-merge. A verifier keeps a merge for
  you by starting its pass with `Verified (high, owner merges):` and saying
  why, which its brief asks for when the merge cannot be undone: a dropped
  column or table, rows rewritten for good, data sent to another system.
  That pass waits for you as before. `shed promote` lists the high-risk
  merges shed queued in a group of their own, first. Turn it on only where
  the digest's escape rate for `high` has stayed near zero.

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
      viewport: 1440x900   # optional: the size to look at when a hand-back names none
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
  production's commit. With no `production` command, or when production is
  older than `look_back`, the window is `look_back`. A look nobody did of
  work that production already serves, within `look_back`, is still owed:
  too late to gate, not too late to find a defect. It goes to a worker
  marked `production already serves it`, and a failure is a defect in
  production.
- **Before the merge.** A `Needs eyes:` ask of a pull request that has not
  merged is nobody's yet where a worker does eye checks: it is not in
  `waits on you`, and it holds neither the verification nor the merge.
- **When it is due.** When staging's commit has the pull request's merge
  commit in its history. Until then the look waits for a staging deploy, and
  nothing is asked of anyone.
- **Who does it.** A worker with `eye_checks: true`, ahead of new issues.
  Where another such worker shares the queue, not the worker that built the
  work: a second reader catches a hand-back that is too vague to check.
- **What the worker may do.** Its brief gets the staging address and your
  `orders`. An act the orders do not name is not allowed. Name the writes a
  check needs and the ones that must never happen.
- **One identity per worker.** Workers on one machine that sign in as the
  same test user move each other's state: one ends the other's session,
  closes its drawer, empties its cart. Each then blames the other's
  leftovers, and both checks are wrong. Give each worker its own user:
  `{worker}` in `orders` is replaced with the name of the worker that reads
  them, and a worker's `env` is set in its session.

  ```yaml
  repos:
    - repo: my-org/my-app
      staging:
        orders: |
          Sign in as qa-{worker}@example.com with the password in $QA_PASSWORD.
          Never sign in as another worker's user.
  machines:
    - name: mac-mini
      workers:
        - name: app
          eye_checks: true
          env: { QA_PASSWORD: "..." }
  ```

  The users themselves are your repository's business: a fixture script on
  staging that creates one per worker.
- **The viewport.** shed cannot resize a browser. The worker looks at the
  size the hand-back names, or at `viewport` when it names none, and its
  comment always says the size the browser really had, as in
  `viewport 1440x900`. When it cannot get the size and what is to be judged
  depends on it, the look is blocked and the comment says both sizes. If
  your worker's browser tool can resize, say how in `orders`.
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

#### Deploying staging when looks pile up

A look waits until staging serves the merged work. When staging deploys
only when you start it, looks pile up in `waits for staging` while the
workers that do them have nothing. Let the agent of one machine start it:

```yaml
repos:
  - repo: my-org/my-app
    staging:
      commit: "curl -fsS https://staging.example.com/healthz | jq -r .commit"
      deploy: "gh workflow run deploy.yml --ref main -f environment=staging"
      deploy_from: mac-mini          # the machine whose agent deploys
      deploy_when:
        waiting_looks: 5             # the default
        or_after: 4h                 # the default: since the oldest waiting look was asked for
```

- **When.** As soon as that many looks wait for staging, or the oldest has
  waited that long, and only while no worker of `deploy_from` has a look in
  hand that it has not posted. A deploy restarts the app under a look.
- **During it** no look of the repository is handed out. The agent reads
  `staging.commit` until staging serves a new commit, then hands them out
  again. An agent restarted in the middle goes on waiting for that deploy.
- **A failure** (the command fails, or staging still serves the old commit
  an hour later) goes to your `notify` command once, puts a `!` line in
  `shed status`, and is not tried again before `or_after` has passed.
- `shed status` shows each repository's last deploy by shed under `staging
  deploys by shed`. A deploy you start yourself is not in it; the release
  state shows the commit staging serves.
- **It never touches production**: `production` takes only `commit`.
- The agent sees the looks of its own machine only. Keep the workers with
  `eye_checks` on `deploy_from`, or a look on another machine may be under
  way when staging restarts.

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

### A worker to ask

A worker with `ask: true` builds nothing. It is a session on the machine that
you ask about the code while you are away from your desk: what does this do,
where is that decided, what changed last week.

```yaml
    workers:
      - name: ask
        dir: ~/Projects/my-app-ask        # a checkout of its own
        command: claude --remote-control linux-box-ask --permission-mode default
        ask: true
        brief: |
          Answer questions about my-app. The other checkouts under
          ~/Projects belong to workers.
```

- **The agent keeps it running.** Its session is started with the machine and
  started again when it ends. Its brief is written like any worker's, and it
  is told when you edit it. `shed recycle` gives it a fresh session.
- **Nothing else is typed into it.** It has no queue, takes no one-off task,
  and no reviewer reads its screen. Your conversation with it is never
  interrupted, and it costs no check-in. A machine's `issues` are not its
  queue, and `shed bump` and `shed task -worker` refuse it.
- **Its brief tells it to change nothing.** It answers from the code, the git
  history, the issues and the pull requests, and names what an answer rests
  on. It does not edit, commit, push, merge, deploy or comment. A reply meant
  for someone else is a draft for you to send. The brief is an instruction
  and not a lock: for a lock, give its `command` a permission mode that asks
  before a write, and give its directory a checkout of its own.
- **How you reach it is its `command`.** shed only keeps the session alive.
  With Claude Code, `--remote-control <name>` makes the session reachable
  from claude.ai/code and the Claude mobile app under that name; the machine
  must be signed in to a claude.ai account for it. Any other way to reach a
  terminal session works the same.
- `shed status` lists it under `supervised` with `answers questions`. It is
  not counted among the workers that have no work.

### Pausing a machine

Sometimes one heavy job on the machine has to finish first: a deploy build,
a migration, a CI run that keeps timing out beside four busy workers. Tell
the agent to leave the workers alone for a while:

```sh
shed pause linux-box            # for 30 minutes
shed pause -for 1h linux-box
shed resume linux-box           # end it early
```

`shed resume` also ends the hold on workers that wait for a usage limit you
know is over; `shed resume linux-box app` does only that, for one worker.

- **Nothing is typed into any worker while it lasts.** No issue or task is
  handed over, nobody is nudged, and a session that ended is not started.
  The reviewer is not asked anything either.
- **Work in progress is not stopped.** A worker goes on with the turn it is
  in and then waits, so the load falls as the workers come to rest, not at
  once. shed does not freeze or kill a worker's processes.
- **A pause always runs out.** One you forgot must not idle the machine for
  the whole of an absence. When it ends, or on `shed resume`, the next tick
  finds each worker as it is and carries on: an idle one is reviewed and
  given its next item.
- Scheduled `tasks` still run. `shed status` shows how long the pause has
  left.

For load that comes and goes by itself, use the machine's `capacity`
instead: it holds new work automatically and needs nobody to say so.

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
  that is never quiet still gets its work done, only more slowly. An issue
  from a backfill source is the exception: any machine of the fleet may take
  it, so a busy machine waits for room however long that takes and leaves it
  to a machine that has some.
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

#### Giving work to a machine with room

A machine schedules only its own queue. When one machine is loaded and
another has idle workers, the queued work stays where it is. With
`release_after`, a machine that stays busy gives the work nobody has started
to the fleet:

```yaml
accounts:
  - name: max-1
    machines: [mini]
    overflow: "fleet:ready"   # see Sharing work across machines
machines:
  - name: mini
    capacity:
      max_load: 1.5
      release_after: 30m
```

- After the machine has been busy for `release_after` without a break, it
  releases one issue from its queue onto the account's `overflow` label,
  then one more each `release_after` while it stays busy. A machine that
  lists the label as a backfill source takes it when it has room, as it
  takes the work of a spent machine.
- Only work nobody has started moves: no worker of the machine was ever
  handed it, its hand-back names no pull request, the owner did not bump it,
  and it is not rework, a look or a verification. An issue that was
  released once is never released again, so two busy machines do not pass
  it back and forth.
- The first such issue in queue order goes first: what is most urgent goes
  where it can start soonest.
- The comment says `Released: mini, busy for 30m: the load is 3.10 per core,
  above the limit of 1.50.` `shed status` lists the issue under the released
  issues until a machine takes it.

`release_after` needs `max_load` or `busy_when`, and an account with
`overflow`.

`shed status` ends with a `Balance` line when the fleet is out of step: a
machine's workers are idle while another machine on the same repository has
queued work nobody has started and no room for it:

```
Balance:
  pc: 3 worker(s) idle, the longest for 7h, while mini has 4 queued issue(s) nobody has started (load 3.10 per core, above 1.50)
```

`shed digest` shows each machine's idle worker-hours by cause, `held for
room` included, and its peak load per core and swap in the window. It names
a machine that idled with no work while another held work for room:

```
By machine:
  mini  held for room 4.0h; peak load 3.33 per core, swap 6.0G
  pc  no work 7.0h; peak load 0.40 per core, swap none
  pc idled 7.0 worker-hours with no work while mini held work 4.0h for room, peak load 3.33 per core, swap 6.0G
```

The agent reads the load and swap each minute and keeps the peak of each
hour, for a week, in `~/.local/state/shed/loads.jsonl`.

A machine whose 1-minute load has stayed at or above its core count for 10
minutes is full: tests time out and builds slow, for the workers and for
anything else on it, such as CI runners. `shed status` says so on the
machine line, `load 11.69/12 high 14m`, and as an `!` line. A shorter spell,
such as one build, is not flagged. The agent records when the load reached
the cores in `~/.local/state/shed/high-load` while it stays there.

#### Sharing the machine with what else runs on it

`capacity` holds new work back, but a test run that has already started goes
on. Three things pace work in progress.

- **Priority.** `nice: 10` under `capacity` starts each worker's command at a
  lower CPU priority (1 to 19). Everything the worker starts, its gates
  included, inherits it, so CI runners and databases on the machine get the
  cores first. The agent and its checks keep their own priority, which an
  `init` line with `renice` cannot do.

  ```yaml
      capacity:
        max_load: 1.5
        nice: 10
  ```

- **A lock for heavy commands.** `shed lock <name> -- <command>` runs the
  command once no other command holds the same lock on the machine, and exits
  with the command's own code. Wrap what is heavy, in the repository's
  pre-push hook or in the standing orders, so that one full test run happens
  at a time and the others wait in line:

  ```sh
  shed lock test -- npm test
  shed lock typecheck -- npx tsc --noEmit
  ```

  A command that waits says which one holds the lock, its worker and since
  when. The lock is flock(2), which macOS has too, and the system gives it up
  when its holder exits, however it exits: a holder that crashed leaves
  nothing to clean up. Locks with different names do not wait for each
  other.

- **What each worker uses.** `shed status` shows, for each supervised worker,
  the CPU and memory of its whole process tree now, and its largest process:

  ```
    supervised
      app  #7   cpu 326% 7.0G, most node vitest 6.0G  nudge     20m ago  ...
      web  #12  cpu 2% 310M, most claude 300M         on track  4m ago   ...
  ```

  CPU is in percent of one core: 326% is more than three cores. The JSON
  report has it under `probe.usage`.

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
- A usage limit follows the same rule. The session stays up and nothing is
  typed while it waits. A resume that is still inside `cache_ttl` continues
  in the same conversation. A resume after the cache has expired clears the
  context first: the transcript would be read again at full price, and the
  branch and the issue hold the work. An API error retried after the cache
  has expired is cleared the same way. The brief tells the worker to write
  its plan when it chooses an approach and its progress when a step lands,
  so a limit can cut a turn off without taking the work with it.

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

An orchestrator told "sonnet" may still run its implementers on its own
model. To run a plain implementation issue's whole session on its model and
tell the rest, set `apply` per label:

```yaml
supervisor:
  models:
    default: opus
    apply: tell
    labels: { "model:sonnet": sonnet }
    apply_labels: { "model:sonnet": switch }   # the session switches to sonnet
    session: opus                               # and back to opus for a told issue
```

An eye check is a browser look: read the hand-back, sign in, click,
compare. It rarely needs the model the hardest issues do. Give it one:

```yaml
supervisor:
  models:
    default: opus
    eye_checks: sonnet
```

When the agent hands over an eye check it switches the session to that
model, clearing the context as any switch does, and the next issue switches
back to its own. Every worker with `eye_checks: true` stays eligible, so a
pile of looks spreads across the machine without workers set aside for
them. With `apply: tell` the look still switches, and `session` is the
model the session goes back to; in switch mode that is the next issue's
model, so `default` is needed.

`session` is needed whenever some labels switch and others are told. The
cost comment names any sub-agent that ran on another model than the issue's
(`Ran on another model than the issue's (sonnet): implementer (opus-5-5)`),
and the digest shows each worker's spend by model.

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

### Clear a large context inside an issue

```yaml
supervisor:
  recycle_context: 300000   # tokens
```

`fresh_per_issue` clears between issues, not inside a long one, where each
turn re-reads a context that grew through planning, building, review and
fixes. With `recycle_context`, once the last call of the worker's own
session carried more context than this, the agent clears it at the next
hand-off where the worker is idle: the message that sends it on starts with
"Your context was cleared", and the worker picks the work up from the
issue, the branch and the pull request. The check-in records the size it
was cleared at. The cost comment gives the ratio of cache reads to output,
so an issue that carried a large context through many turns stands out.

### Standing orders

`defaults.standing_orders` is text that goes into every worker's brief on
every machine, and the reviewer judges scope against it. Use it for the rules
that do not change from task to task: your architecture principles, your
definition of done, what a worker must never do.

### One home per rule

A worker reads several sources before its first item: the repository's
instructions (`CLAUDE.md`, `AGENTS.md`), your global ones, a memory index,
the standing orders and its brief. Each is read again on every turn of a
long session, so a rule written twice costs twice, and two copies drift
until they disagree and the worker guesses. Give each rule one home:

- **Standing orders:** how the fleet works. Hand-backs, choices, what a
  worker never does unattended. Nothing about the code.
- **The repository's instructions:** how the work in that repository is
  done. Its architecture, its tests, its tools.
- **Memory:** lessons, each with the reason it holds.

Workers do not write any of them. A worker that learned something the next
one should know writes it in its hand-back after `**Lessons:**`, and
`shed digest` lists the lessons that wait for you, once each however many
workers wrote them:

```
Lessons to promote: 2 (promote, reject or fold each, then comment "Lessons promoted" or "Lesson rejected" on its issue)
  30h  my-org/my-app#6             Staging logs are in /var/log/app.
  3h   my-org/my-app#5, my-org/my-app#6  The suite needs `make db-reset` first.
```

To see what a worker loads, and what in it is said twice or out of date:

```sh
shed context mac-mini app                   # read-only; runs on the machine over SSH
shed context -contradictions mac-mini app   # also asks the reviewer model for contradictions
```

```
mac-mini app: about 13.2k tokens before its first item, read again on every turn
  global      ~370   /home/me/.claude/CLAUDE.md
  parent      ~1k    /home/me/AGENTS.md
  repository  ~6k    /home/me/Projects/my-app/CLAUDE.md
  memory      ~190   /home/me/.claude/projects/-home-me-Projects-my-app/memory/MEMORY.md
  brief       ~5.6k  /home/me/Projects/my-app/.shed/BRIEF.md

Said twice: 2 (one home per rule; every copy costs tokens on every turn)
  /home/me/.claude/CLAUDE.md and /home/me/AGENTS.md: 2
    lines 7 and 7: **Incremental progress.** Small changes that compile and pass tests.

Stale references: 1
  /home/me/Projects/my-app/CLAUDE.md:41  internal/legacy/: no such path in the checkout
```

- **The sources** are what Claude Code reads at session start: your global
  `CLAUDE.md`, the `CLAUDE.md`, `CLAUDE.local.md`, `.claude/CLAUDE.md` and
  `AGENTS.md` of the worker's directory and of each directory above it, what
  those import with `@path`, the memory index, and the worker's brief. The
  tokens are estimated at four characters each.
- **Said twice** compares paragraphs and single statements across sources:
  nearly the same words, or the same rule inside a longer one.
- **Stale** are paths in backticks that the worker's checkout does not have,
  and issues the text calls open that are closed. Only the repository's own
  sources and memory are checked; the global and parent files span other
  repositories.
- **Memory is per directory.** Claude Code keys it by the working
  directory, under `~/.claude/projects/<the directory, with / as ->`. A
  fleet usually syncs one memory folder for a repository, under a main
  checkout's path, and every worker runs in a checkout of its own: no
  worker loads it, and nothing else says so. When a worker has no memory
  index and another checkout of the same repository has one, `shed context`
  says where it is. Link each worker's folder to the synced one in the
  deploy hook:

  ```sh
  ln -sfn ~/.claude/projects/-home-me-Projects-my-app/memory \
          ~/.claude/projects/-home-me-Projects-my-app-wt-app/memory
  ```
- Branch names in backticks (`origin/main`, `feat/…`, a branch the
  checkout has) are git refs, not stale paths.
- **Contradictions** are read by the reviewer model, on request, and are
  for you to settle. shed changes no source.
- It exits 1 when it finds anything; the half that runs on the machine
  exits 0 and puts its findings in the report. `shed status` gives each worker's total
  under `supervised`, measured when its session starts, and with
  `supervisor.context_budget: 20000` names a worker that loads more.

You put each one where it belongs, or drop it, and say so on its issue:
`Lessons promoted`, `Lessons folded` or `Lesson rejected` settles the
lessons written before it. shed never writes the memory itself, so there is
one source of truth and it is yours.

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

### Cost on the pull request

When a hand-back names a pull request, shed comments on that pull request
with the tokens the worker's session spent on the issue: uncached input,
cache write, cache read and output, by model. A later pass, such as rework,
updates the same comment with this pass and the total.

Work that ends with no pull request gets its cost too, when the worker
moves on from it: an eye check, a verification, or a pass whose hand-back
names none (`**PR:** none`, a plan) is costed in a comment on its issue,
one per worker, so the builder's cost and the looker's stand side by side.
A one-off task's report ends with `## Cost (from shed)`.

Each model's line ends with what its tokens would cost at Anthropic's
published API rates, and the comment totals them: `At API rates: $2.26`.
A subscription does not pay that price. The rates are the weights that make
a cache read, a cache write and an output token of different models
comparable, as a subscription's limits also count them differently. shed
carries the rates of the pricing page; the fleet file can replace one or add
a model it has no rate for, in US dollars per million tokens:

```yaml
pricing:
  my-local-model: {input: 0, cache_write_5m: 0, cache_write_1h: 0, cache_read: 0, output: 0}
```

A model with no rate shows `no published rate` and is left out of the total.

The numbers are read from the session transcript the tool's hooks point at.
They are not the worker's own estimate. A few calls the tool keeps only in
memory are missing, and the comment says so. The reviewer's own calls are
not included: the digest counts them on a line of their own.

`shed mark` records the transcript whenever the hook passes one, and shed
reads it again when it posts, so a turn that was still being written is
counted. Stop and StopFailure already do this, because they run
`shed mark idle`. A sub-agent has its own transcript. Without a hook for it,
those tokens are left out:

```json
"SubagentStop": [
  { "hooks": [{ "type": "command", "command": "$HOME/.local/bin/shed mark usage" }] }
]
```

`shed mark usage` records tokens and does not change the worker's state, so
it can run while the parent turn is still going.
It reads the sub-agent's own transcript, which the hook names, and tags each
of its calls with the sub-agent's type. The comment then splits the cost:
`By agent: worker session $2.00 (opus-5-5); implementer $2.00 (opus-5-5,
sonnet-5-5); Explore $0.50 (haiku-5-5)`, so you can see whether the work ran
on the model its label named. When the worker started sub-agents and none of
their tokens were recorded, the comment says the count is partial.
`shed preflight`, and so `shed deploy`, reads the Claude Code settings each
worker's session loads (`~/.claude/settings.json` and the worker directory's
`.claude/settings.json` and `settings.local.json`). When the state hooks are
set and this one is not, the worker's line carries
`note: cost comments leave out sub-agents`.

### Settings

```yaml
supervisor:
  command: claude -p --model sonnet --output-format json   # the reviewer: prompt on stdin, answer on stdout
  cache_ttl: 5m
  scope_every: 30m
  when_cold: clear                    # or: resume
  clear_command: /clear               # typed into a worker to empty its context
  model_command: "/model {model}"     # typed into a worker to switch model
  no_work_after: 30m                  # optional: notify when a worker has had no work this long
  low_queue: 3                        # optional: notify when a queue has fewer workable issues
  stack: true                         # optional: go on with an issue on a branch from its pull request while it waits for review
  context_budget: 20000               # optional: tokens a worker may load before its first item
  overlap_labels: "area:"             # optional: issues sharing a label with this prefix are not worked at once
  own_wait_max: 45m                   # optional: how long an idle worker may wait on its own work before it is told to check it; "0" for no bound
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
| workers have no work | workers rest with nothing they can act on while issues of the queue wait on you: `2 of 3 workers have no work: every queued issue waits on you (oldest 27h)`. It is the first line of the machine |
| worker needs you | the last check-in ended with `needs_owner` |
| worker is stuck | nudges or restarts did not get it moving |
| worker could not be checked | the check-in itself failed |
| task failed | the last run exited non-zero |
| task missed its run | a scheduled time passed with no run |
| issue waits on you | a comment asks for a decision, a look or a review, and nothing answers it |
| cannot read the release state | a `repos` commit command failed or did not print a commit, or GitHub could not be asked |
| cannot tell which merged work staging serves | GitHub could not say whether staging has some merge commits; the counts stand, but a look of that work shows as waiting for staging |
| every worker is at its weekly limit | the whole machine waits days for a reset; log it in to an account with room, or wait |
| eye checks are due on staging | merged work is on staging, owes a look, and no worker has `eye_checks` |
| the eye check is blocked | a worker could not do it and said why; clear the cause and comment `Eyes unblocked`, or look yourself |
| issues queued and no worker is running | there is a backlog and nobody is on it |
| worker has been quiet | an issue's window had no output for 30 minutes |

An issue counts as *working* when a tmux window on its machine has the issue
number in its name (`app-123`).

On a machine with supervised workers, a window that runs an agent tool and
is not a worker's own is marked `unsupervised` under `windows`, with one
line that counts them: a manual session, or one left from another workflow.
It is not something shed asks of you, but it spends the same account limit
and CPU as the workers. An agent tool is any tool the machine's workers run,
or a process titled with a version number, as Claude Code titles its own.

`shed tidy -sessions <machine>` lists those sessions with how long each has
been idle. With `-apply` it closes the ones idle longer than `-idle` (three
days by default), each by its exact session and window name. Nothing else on
the machine is touched.

### Waiting on you: comment signals

A worker cannot ask you a question, so it writes one in its issue, in its
hand-back: a comment that contains the word `Hand-back` (configurable as
`handback`). In hand-back comments shed looks for these phrases
(configurable under `signals`):

| Signal | Opens when a comment has | Stays closed when followed by | Closes when a later comment has |
| --- | --- | --- | --- |
| decision | `Decisions needed:` | `none` | `Owner ruling` (and sends the issue back to a worker, unless it reads `Owner ruling: accepted`) |
| eyes | `Needs eyes:` | `nothing`, `none` | `Eyes checked`, or `Eyes failed` from a worker that looked on staging |
| review | `**PR:** #41` | `none` | that pull request is merged or closed |
| ready | `Not ready:` | `none` | `Ready now` |

`Decided without you:` in a hand-back is no signal: it tells you what a
worker chose and asks nothing.

While that pull request is open but cannot merge (a conflict, a failed check),
the issue does not wait on you: it goes back to a worker. See *The queue*.

The review signal follows its pull request (`follows_pr`). An issue that takes
several pull requests lists what is still to do in each hand-back but the
last, after `Still to do:` (the review signal's `remaining`):

```markdown
**PR:** #41 (ready)
**Still to do:**
- the frontend
- the docs
```

It comes back into the queue each time one of its pull requests is merged,
and the worker picks up the rest. A hand-back whose pull request merged and
that lists nothing still to do is the issue's last. The issue is not handed
out again, even while it stays open (a merge into a branch that does not
close issues, or a pull request that only refers to it), and `shed status`
shows it as `merged`. A comment after that hand-back that is not another
hand-back brings it back, as does a pull request that was closed without
merging. shed asks GitHub for the last 200 merged pull requests of the
repository, so a hand-back that names an older one is handed out as before.

With `supervisor.stack: true`, a pull request that waits for your review no
longer leaves its issue idle when the hand-back lists what is still to do.
The worker is sent on with the issue: it builds the rest on a new branch from
that pull request's branch and opens the next pull request against it. The
review still waits on you. A draft, or a pull request that cannot merge, is
not stacked on. Without `stack`, the worker takes another issue while the
review waits.

A hand-back may name two pull requests on its `**PR:**` line, the part that
merged and the part that is still open: `#41 (backend, merged) and #42
(frontend, ready)`. The review, and a conflict or a failed check, follow the
first one that is still open. The eye check is of the first one named.

A draft that is a draft on purpose says what it waits for, in the brackets
after its number: `**PR:** #41 (draft: waits for #40 to merge)`. While that
pull request is a draft on GitHub it is not yours to review: the issue shows
as `draft` with those words, puts no `!` line on the machine and is not
counted in `Waiting on you`. A draft that gives no reason is a review like
any other, and so is one that was marked ready since.

A hand-back with no pull request, such as a plan for you to rule on, writes
`**PR:** none`: there is nothing to review, so the review signal stays closed.
Your `Owner ruling` then closes the decision and the issue is in the queue
again like any other. It is not rework, which needs an open pull request, so
it takes its turn by the queue order. A `**PR:**` line that says anything else
without a number opens a review that no merge can close; `shed status` and
`shed digest` show it as `review (no pull request named)`.

An ask counts only in a hand-back comment. The same words in a plan, a brief
or a discussion on the issue ask you nothing and hold nothing back. In a
hand-back, the ask counts only where it begins a line (after a list mark,
a quote or emphasis at most): "the others were not ready: #12" in a sentence
asks nothing. Your answers (`Owner ruling`, `Eyes checked`, `Eyes failed`)
count in any comment, and also only where they begin a line: a ruling that
tells a worker to "post `Eyes checked` or `Eyes failed`" is no verdict.

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

**macOS and worker dirs on an external drive.** macOS asks before a program
reads a removable volume, and on a machine with no one at it nobody answers.
A read over SSH fails with `Operation not permitted`; a worker's tools, which
all run under the tmux server, and the agent, which runs git and gates in the
dirs, wait on a dialog on the machine's screen. `shed status` and the deploy
preflight read each worker's dir over SSH and through tmux, the agent reads
each one itself every 5 minutes, and `shed status` prints a `!` line for any
that fails, naming the program to grant. To set a machine up:

- Grant **Full Disk Access** (System Settings → Privacy & Security → Full Disk
  Access) to the `tmux` binary the agent starts, with symlinks resolved
  (`readlink -f "$(command -v tmux)"`), and to the `shed` binary. The file
  picker hides dot-folders such as `~/.local`: press ⌘⇧. to show them, or drag
  the binary in from Finder.
- For SSH, turn on System Settings → General → Sharing → Remote Login → ⓘ →
  "Allow full disk access for remote users".
- Turn on file ownership on the volume (`sudo diskutil enableOwnership <mount
  point>`), so files have owners as they do on the internal disk.
- A grant to a binary that is not signed by a developer is kept for that
  exact build. Homebrew's `tmux` is such a binary, so `brew upgrade tmux`
  drops its grant; so can a macOS update. shed's release binaries are signed,
  so `shed update` keeps theirs, as long as shed stays at the same path. `shed status` says so the next time
  it runs. `tccutil` can only reset grants, not add them.

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

### Pacing a week

Every worker on an account runs at full speed until the account reaches its
weekly limit, and then all of them idle until the reset, with the week's
budget spent on whatever came first. With `accounts`, shed paces the week:

```yaml
accounts:
  - name: max-1
    machines: [mac-mini, linux-box]   # the machines logged in to this account
    weekly_budget: 400                # dollars at API rates, as on the cost comments
    resets: Wed 18:00                 # when the weekly limit starts again, machine time
    hold_all_but: 1                   # optional: workers that keep taking work far ahead of pace
```

Each machine's agent sees only its own spend, so each is paced to an equal
share of the budget: here $200. The pace line is that share times the part
of the week gone since the reset. When the machine has spent more:

- **ahead**: only issues with a label from their source's `priority` list
  are handed out;
- **far ahead** (more than a quarter of the share over the line): new work
  is held while `hold_all_but` other workers of the machine have some.

Work already started is never held: the issue in hand, rework, an answered
question, stacked work and eye checks. The reviewer sees why an issue is
held. `shed status` shows each machine's spend against its line and each
account's total:

```
mac-mini  me@mac-mini  load 1.20/8  disk 41%  agent ok
  account  me@example.com  (one usage limit with linux-box)
  pace     $120.00 of a $200.00 share this week, pace line $100.00: ahead, only priority work is handed out

Account max-1: $180.00 of $400.00 this week, pace line $200.00 (mac-mini, linux-box)
```

Pick the budget from what a week of your plan has bought: the digest's
`Usage at API rates` over `-since 168h` for a week that ended at the limit.

### Sharing work across machines

A weekly limit idles every worker on an account for days, while a machine
on another account has room. The work does not need the session that
started it: the plan is in the issue and the work is on its branch, which is
what a cleared context already resumes from. Let a spent machine hand its
issue on:

```yaml
accounts:
  - name: max-1
    machines: [mac-mini]
    overflow: "fleet:ready"          # where a spent worker releases its issue

machines:
  - name: linux-box                  # another account
    issues:
      - repo: my-org/my-app
        label: "machine:linux-box"
      - repo: my-org/my-app
        label: "fleet:ready"         # takes released issues when its queue is empty
        backfill: true
```

- **The holder releases; nobody takes.** A worker of `max-1` that is at its
  weekly limit, or stuck, releases the issue in hand when the work lives
  outside its session (its hand-back names a pull request, or a comment
  landed after the hand-over) and its tool is not in a turn. A five-hour
  limit is waited out. A bumped issue, an eye check, a verification and a
  task are never released. A machine that cannot be reached cannot release,
  so two workers never build on one branch.
- **Releasing** takes the machine's queue label off the issue, puts
  `fleet:ready` on, and comments `Released: mac-mini/app, weekly limit
  (resets Wed 18:00)`. The worker's context is cleared, so when its limit
  resets it takes its next issue instead of going on with this one.
- **Taking.** A machine that lists `fleet:ready` as a backfill source takes
  a released issue when its own queue has nothing it can act on, puts its
  own label on and takes `fleet:ready` off. Its worker is told the issue was
  released and to go on from the plan, the branch and the pull request.
  Two machines that look at the same moment both see `fleet:ready`; it is
  the same window backfill has.
- **Overlap across machines.** With `overflow` set, an issue that carries
  another machine's queue label, has a `**Paths:**` plan comment and no
  hand-back after it is work in flight there, and holds an overlapping
  issue here.
- `shed status` lists the released issues no machine has taken yet, and
  `shed digest` counts the releases on the issues it closed. The weekly
  limit line stays, so switching the account still works when nothing takes
  the work.
- An account may have `overflow` without `weekly_budget` and `resets`: it is
  then not paced. The fleet file asks that some machine take `overflow` as a
  backfill source, and that no machine has it as its own queue.

### Switching a machine's account

When a machine's Claude account reaches its weekly limit, it can move to an
account with room without an ssh session:

```sh
shed login mac-mini                    # prints the sign-in URL
shed login -code 'aBc123...#xyz' mac-mini
```

The first step starts the machine's tool in a throwaway `login-account`
window, in the folder of the machine's first worker (one the tool already
trusts, so it does not stop to ask), with the PATH the machine's `init`
sets. It types `/login`, takes the subscription login, and prints the URL as
one line. If the tool asks whether to trust the folder anyway, the step
stops and says so instead of answering: start the tool there once and trust
it. Open it, sign in with the account the machine should use, and give
the code the page shows to the second step, which types it into that window.
It then prints the account the machine is logged in as, ends the usage-limit
holds of its workers (as `shed resume` does), and types `/remote-control`
into each idle worker whose screen says its Remote Control connection
dropped. A worker in the middle of a turn is named instead: a command typed
into a turn would be read as a message. A code that the tool did not take
leaves the window open for another.

`shed status` names each machine's account, read from Claude Code's own
`~/.claude.json`, and which machines share one usage limit:

```
mac-mini  me@mac-mini  load 1.20/8  disk 41%  agent ok
  account  me@example.com  (one usage limit with linux-box)
```

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

Where the time went: 3 merged pull request(s), median and worst
  build   42m  3h   my-org/my-app#140  first commit to ready for review
  checks  11m  28m  my-org/my-app#138  the merged head's checks, first started to last finished
  waits   5h   20h  my-org/my-app#131  checks finished or ready, whichever is later, to merged

Eye checks: my-org/my-app
  1 passed in this window; now 1 not done (1 on staging, 0 wait for a staging deploy), 1 failed
    #123  PR #131  passed  3h ago  Retry the export when the API times out
    #125  PR #138  FAILED  1h ago  Paginate the audit log

Idle worker-hours: no work 6.5h, tool error 0.3h, waits on you 2.0h

Waiting on you: 2
  returns to a worker when you answer
    30h  decision  my-org/my-app#124  Drop the legacy column
  to read
    2h  review #141  my-org/my-app#130  Split the importer

Also needs you: 1
  ! mac-mini: unreachable: me@mac-mini: exit status 255: Operation timed out
```

`Where the time went` splits each merged pull request into three stages,
read from GitHub, and names the pull request that was slowest at each:

- **build**: from the first commit to ready for review, the time the work
  took to write.
- **checks**: the checks on the merged head, from the first started to the
  last finished. GitHub keeps no time for a push; the first check's start
  is the nearest record.
- **waits**: from the checks finishing, or the pull request being marked
  ready if that was later, to the merge. The time it sat in your hands or
  in a merge queue.

A stage GitHub has no record of, such as checks on a pull request that has
none, is left out of that stage's median. `-json` carries each pull
request's stages under `stages`.

`Idle worker-hours` totals the time workers spent not working in the window,
by cause: no work in the queue, an error of the tool, waiting on you, a usage
limit. It is read from the last 500 check-ins of each machine, so on a busy
fleet it may reach back less far than the window.

For a week's usage, `shed digest -since 168h` adds what the workers spent,
read from each machine with `shed usage -json`:

```
Usage at API rates: $41.80 (a subscription does not pay this price; it weighs the kinds of token as the published rates do)
  mac-mini  app   opus-5-5    in 1.2M  write 300k  read 15.7M  out 52k  $12.25
  mac-mini  app   sonnet-5-5  in 4.1M  write 900k  read 61M    out 410k $18.70
  mac-mini  docs  haiku-5-5   in 2M    write 0     read 8M     out 300k $10.85
  by model: sonnet-5-5 $18.70, opus-5-5 $12.25, haiku-5-5 $10.85

Spend by kind of work:
  pull request            $24.10  58%  6 item(s), median $3.10
  issue, no pull request  $2.20   5%   2 item(s), median $1.10
  eye check               $6.40   15%  5 item(s), median $1.20
  no item                 $2.10   5%   nothing in hand: before the first item, at rest, after a hand-back
  reviewer                $9.90   24%  the supervisor's check-ins

Cache reads per output token: app 310 (high), docs 120
  Above 300, each turn re-reads a context larger than its work needs: fresh_per_issue or recycle_context would cut it.

Merged pull requests with a cost: 6 of 7, median $3.10
  my-org/my-app#140  $9.20  Show the total on the order page
  ...
  mac-mini  (reviewer)  sonnet-5-5  in 3.1M  write 1.2M  read 22M  out 190k  $9.90

Idle at a usage limit: mac-mini 40.1h
```

`Spend by kind of work` puts each call under what the worker had in hand
when it made it, read from the check-in log: an issue built into a pull
request (all its passes), an issue with no pull request (a plan, work not
finished), an eye check, a one-off task, or nothing at all. The cost
comments on pull requests show only the first kind; this shows the rest,
and `no item` is the share that no work carries. The median is per item.

`Cache reads per output token` is each worker's prompt-cache reads divided
by what it wrote. A long session re-reads its whole context on every turn,
so a high ratio is the clearest sign that a context has grown past what its
work needs.

The reviewer's calls have a line of their own, `(reviewer)`, when its
command prints Claude Code's JSON result (`--output-format json`, the
default): shed takes the answer from it and records the tokens of each
model. A command that prints plain text still works; the digest then says
the reviewer's calls are not counted.

The dollars are the published API rates, as on the cost comments: the
weights that make tokens of different kinds and models comparable. A merged
pull request's cost adds every worker and pass that worked on it; the top
five are named. One with no cost comment, such as one merged by hand, is
counted in `of 7` only. `shed usage` on a machine prints the same totals
for one machine.

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

Idle workers stop nothing, so by default nobody hears of them. Two settings
under `supervisor` change that: `no_work_after: 30m` notifies when a worker
has had nothing it can act on for that long, and `low_queue: 3` when a queue
has fewer issues than that for a worker to act on. Each says how many issues
of the queue wait on you. `no_work_after` also notifies, once per silence, when
a worker has rested that long beside an issue in hand that it can act on.

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
- The log files are not rotated, and neither are the gate logs under `~/.local/state/shed/gates`, one per commit gated.
- An `ask` worker is only as reachable as its tool makes it. shed restarts a
  session that exited; it does not see a session that still runs and has
  lost its connection. Remote Control on a session that nobody attends for
  days has not been tested by this project.
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
binaries for Linux and macOS, signs and notarizes the macOS ones with a
Developer ID (identifier `com.edisoncode.shed`), and publishes them with
checksums on the GitHub release. A build from `make dist` is not signed: on a
Mac, a privacy grant to it lasts only until the next build.
