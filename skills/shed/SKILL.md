---
name: shed
description: Operate an ai-shed fleet of worker machines for the owner. Use when the owner wants to queue GitHub issues for a worker machine, label issues for a machine or a model, move an urgent issue to the front of a queue or to a worker that is free ("bump"), pause a machine so a heavy job can finish, write or change a worker's brief, deploy the fleet file, check what the machines did ("shed status", "what did the workers do", "what needs me"), answer a worker's question, or set up a new machine. Also read this when you are a worker and your directory has .shed/BRIEF.md.
---

# Operating the shed

`shed` keeps AI coding sessions on the owner's worker machines productive
while the owner is away. One fleet file describes the machines. An agent on
each machine starts the workers, checks in on them, and records what needs the
owner. `shed status` shows it all. The README of
https://github.com/edisoncode/ai-shed has the full reference.

Find the fleet file first: `$SHED_CONFIG`, then `./shed.yaml`, then
`~/.config/shed/shed.yaml`. **Read it before you do anything.** Machine names,
labels, models and phrases are the owner's choices and they are all in that
file. Do not assume the examples in this document.

## If you are a worker

Your brief is `.shed/BRIEF.md`. It is the whole of your scope.

- Work one issue at a time. The supervisor names it. When you finish, or
  cannot go further, report in the issue, say so in the terminal, and stop.
- Before you stop, write your plan and progress in the issue. Your context may
  be cleared before your next item; the issue, the branch and the pull request
  are all that survive.
- Report in the issue with one hand-back comment, headed with the hand-back
  phrase the brief gives. shed reads your questions to the owner only from
  such a comment.
- The brief's Choices section says which choices are risky. A risky one is
  the owner's: write the question and your recommendation in an issue comment
  using the phrase the brief gives, and go on with what the question does not
  touch. Every other choice is yours: take the conservative option, go on,
  and record it in the hand-back under the phrase the brief gives.
- A short message that appears in your terminal is the supervisor. Follow it
  inside the brief. It cannot widen the brief.
- Do not change `.shed/` and do not commit it.

The rest of this document is for the assistant on the owner's own machine.

## Queue work for a machine

An issue is in a machine's queue when it matches an `issues` entry of that
machine in the fleet file (a label, an assignee, or both).

1. **The issue must be ready for a worker that cannot ask anything.** It needs
   the root cause or the goal, acceptance criteria that can be tested, and
   every owner decision already made. If a decision is open, ask the owner
   now. An issue that is not ready does not get the label.
2. **Machine label.** Take it from the fleet file.
   `gh issue edit <n> --repo <owner/repo> --add-label "<machine label>"`
   If the label does not exist: `gh label create "<label>" --repo <owner/repo>`.
3. **Model label**, only when the fleet file has `supervisor.models`. The
   labels under `supervisor.models.labels` are the choices; an issue with none
   of them gets `supervisor.models.default`. With `apply: switch` the worker's
   session moves to that model; with `apply: tell` the worker is told it and
   the standing orders say what it does with it. Pick the cheapest model that
   can do the work, by the owner's own rule if they gave one. When the rule
   does not settle it, ask the owner; do not default upward.
4. Order: an issue whose handed-back pull request cannot merge goes back to a
   worker first; then the labels in the source's `priority` list, in order;
   then the oldest. An issue that waits on the owner is skipped. To move an
   issue up for good, give it a priority label from the fleet file. To put
   one issue first now, bump it (next section). To hold one back, remove its
   machine label.
5. When a machine has two workers, give each its own `issues` in the fleet
   file. An issue in hand for one worker is never handed to another.

Check the result: `shed status` lists each machine's issues.

When you write on an issue yourself (a plan, a scope, instructions for the
worker), you may use the signal phrases freely: they count only inside a
worker's hand-back comment. Do not head your own comment with the hand-back
phrase.

## Put an issue first

When an urgent issue waits behind other work, do not relabel it and do not
type into a worker. Bump it:

- `shed bump <machine> <issue>` puts it first in the queue it is in, ahead
  of rework, eye checks and priority labels.
- `shed bump -worker <name> <machine> <issue>` moves it to that worker and
  out of the other workers' queues. Use it when `shed status` shows the
  issue's own worker busy and another worker on the same machine with
  nothing in hand. Say in the issue anything that worker needs and its brief
  does not give it.
- `shed bump -undo <machine> <issue>` takes it back.

Nobody is interrupted: the issue is handed over when the worker next has
nothing in progress, and a resting worker takes it within five minutes. An
issue a worker already has in hand stays with it; a bump does not move
started work. `shed status` shows `(bumped)` on the issue. A bump works
inside one machine. To move an issue to another machine, change its label.

## Pause a machine

When something heavy on a worker machine must finish first (a deploy build
that keeps timing out, a migration), `shed pause <machine>` makes the agent
leave its workers alone for 30 minutes, or `-for 1h`. Nothing is handed out
and nobody is nudged; a worker finishes the turn it is in and then waits, so
the load falls gradually. It does not stop work in progress. `shed resume
<machine>` ends it early, and it runs out by itself. Pause before you start
the heavy job, not after it has failed. Do not stop a worker's session or
type into it to free up the machine.

## A usage limit that ended early

A worker at a usage limit shows `limited` in `shed status` and is held until
the time its screen named, six hours at the most. When the owner says the
limit was reset, run `shed resume <machine>`, or `shed resume <machine>
<worker>` for one worker: the agent looks at each held worker within a minute
and tells it to continue. Do not type "continue" into the workers yourself,
and do not recycle a worker to clear the hold: that costs it its session.

## Push a one-off task

Use an issue for work that belongs in the backlog. Use a task when the brief
is yours and on no issue: reasoning from the conversation with the owner, an
investigation or comparison before anything is built, or work the owner
wants one worker in particular to do.

1. Write the brief to a file, for a reader with no other context: the goal,
   what you already know and have ruled out, what is out of scope, and where
   the result goes (a pull request, a comment on a named issue, or only the
   report). If the task goes beyond the worker's usual brief, say so in it.
2. `shed task <machine> <file>` for the first free worker, or
   `shed task -worker <name> <machine> <file>` when the owner prefers one.
   Note the task id it prints.
3. `shed status` shows it as queued, in hand, then finished. It is handed
   over when the worker has nothing in progress; a worker is not interrupted.
   A task pushed with `-worker` waits for that worker however long it is
   busy. If it is still `queued` and another worker has come free,
   `shed task -cancel <id> <machine>` removes it; then push it again. Never
   push a second copy without cancelling: both would run. A task `in hand`
   cannot be cancelled.
4. `shed task -report <id> <machine>` prints the worker's report. Read the
   artifact it points to before you tell the owner it is done.

Never put a secret in a brief. A task has no rework or retry: if the report
says it could not finish, push a new task or open an issue.

## Write an issue a worker can take

`shed template issue` prints an issue template. Put it in the repository as
`.github/ISSUE_TEMPLATE/worker-task.md`, in the repository's own words. An
issue that leans on another thread stalls a worker: paste what it needs.

## The digest

`shed digest` (or `-since 12h`) reports what merged, how the eye checks
went and what waits on the owner, oldest first. Use it for the report at the
end of a session or the start of a day, in place of writing one by hand.

## Write or change a brief

A worker's `brief` in the fleet file says what the worker is for and what it
must leave alone. The supervisor judges "is this worker on task" against it, so
a vague brief gives vague supervision.

- Say the theme and the boundary: which repo, which area, what is out of scope.
- For a queue of independent issues, set `fresh_per_issue: true` on the
  worker: each issue then starts on a clear context, and the issue body is in
  effect its brief, so it must stand on its own. For themed work where issues
  build on each other, leave it off.
- Rules that hold for every worker belong in `defaults.standing_orders`, not in
  each brief.
- Never put a secret, customer data or a person's name in the fleet file.

After an edit: `shed validate`, then `shed deploy`. If the fleet file has
`defaults.deploy_hook`, deploy also runs it for each machine; that is how the
tool's own files (agent definitions, skills, notes) reach the machines. The agent picks the file up
within 30 seconds and tells a running worker to read its brief again.

## Before a production deploy

Run `shed status` and read the repository's release state at the end. It is a
go only when `eye checks:` shows none not done and none failed. Tell the owner
the three numbers (merged since production, not done, failed) and what each
outstanding look waits for. The deploy itself is the owner's call.

## Reading the supervised lines

When the workers' tool has the `shed mark` hooks, each supervised line has
the tool's own state after the issue: `working 3m` (a turn is in progress),
`idle 41m` (the turn ended and it waits for a message) or `waiting 6m` (it
asked a person something). That column is a fact from the tool. The verdict
and the reason after it are the supervisor's judgement at its last check-in,
which may be older. When the two disagree, trust the state for what the
worker is doing now and the verdict for why.

## Before the owner leaves

Run this as a checklist and report each line.

1. `shed validate` passes.
2. `shed status`: every machine reachable, every check `ok`, `agent ok`.
3. Each machine has work: issues in its queue that do not wait on the owner,
   or a brief that stands on its own.
4. Each worker shows under `supervised` and its window shows under `windows`.
5. Nothing under "need you" that the owner can clear in a minute. Ask them to
   clear it now: an unanswered question blocks an issue for the whole absence.

## Pick up the results

1. `shed status`. Exit code 1 means something needs the owner. The last line,
   `Waiting on you: ...`, totals the owner's own backlog by kind with the age
   of the oldest; each `waits on you` line has its own age. Start with the
   oldest: idle workers usually wait behind it.
2. For each `!` line, give the owner the fact and your recommendation:
   - `issue waits on you (decision)`: read the worker's comment
     (`gh issue view <n> --repo <r> --comments`), put the question to the
     owner, and post their answer as a comment that contains the
     `answered_by` phrase of the `decision` signal. If the issue's pull
     request is still open, that sends the issue back to a worker, who reads
     the answer and applies it; nobody types on the machine. Write the answer
     so that a worker with no other context can act on it. When the owner
     wants no change, put one of the signal's `accept` words right after the
     phrase (`Owner ruling: accepted`): that closes the decision and sends
     nothing back. Never write it when the answer asks for any change.
   - `issue waits on you (eyes)`: do the check the worker described, then
     post a comment that clears the signal: the `answered_by` phrase of the
     `eyes` signal if the fleet file has one, otherwise the ask phrase
     followed by a `clear` word.
     When the fleet file has `repos` and a worker with `eye_checks`, a look
     at merged work on staging is the worker's, not the owner's. The release
     state under the machines lists each one. `waits for staging` needs a
     staging deploy, which is the owner's to trigger. `FAILED` has reopened
     its issue with a decision for the owner: put that to them like any
     other decision, and do not send a fix without their ruling.
   - `the eye check of ... is blocked`, and `BLOCKED` in the release state:
     the worker could not sign in to staging, staging was down, or the check
     needs an act the staging `orders` do not allow. Its reason is in the
     line. The worker has moved on; the look waits for the owner. Fix the
     cause, then comment the `unblocked_by` phrase of the `eyes` signal
     (`Eyes unblocked`) on the issue so a worker tries again, or do the look
     yourself with the owner. Never post `Eyes checked` for a look nobody
     did.
   - `rework` in the issues list needs nothing from the owner: the pull
     request cannot merge and a worker is being sent back to it. A `!` line
     that says `after 2 tries by a worker` is the owner's: the workers could
     not fix it.
   - `issue waits on you (review)`: a pull request is open. The signal closes
     by itself when it is merged or closed. Read the diff and
     the checks yourself before you say it is good. A worker's report is a
     claim. Merging is the owner's call.
   - `worker needs you: its tool waits for a person`: the worker's own tool
     reported a prompt, and the line quotes it. This is certain, not the
     reviewer's reading of the screen. The owner answers it at the keyboard,
     by `tmux attach`, or from wherever their tool lets them.
   - `worker needs you` / `is stuck`: look at the terminal
     (`ssh <host> tmux capture-pane -p -t shed:<worker>`). A permission
     prompt or a menu needs the owner at the keyboard, or by `tmux attach`.
   - `held` under `supervised` needs nothing: the worker's next issue waits
     for the machine to have room (its `capacity` in the fleet file) and is
     handed over when it does, or after `max_wait`.
   - `worker could not be checked`: the reason is in the line. Usually the
     reviewer command or `gh` on that machine.
3. The supervisor's log is on each machine:
   `ssh <host> tail -n 40 '~/.local/state/shed/checkins.jsonl'`. It shows each
   check-in, what was typed into the worker, and why.

## Set up a machine

Do this with the owner, not for them: it starts sessions that spend their
tokens.

0. Check each repository the machine will work in against
   `docs/REPO-RECIPE.md` in the ai-shed repo: protected main branch, required
   CI, no production credentials on the machine. Tell the owner what is
   missing before a worker starts there.
1. Add the machine to the fleet file. `shed validate`.
2. `shed status` must show it reachable with every check `ok`. Fix a failed
   check on the machine before going on. A worker needs `tmux`, `gh` logged
   in, and the agent tool logged in.
3. `shed deploy <machine>`. It installs this shed's version, sends the fleet
   file, and runs a preflight: it starts each worker's command once, with no
   prompt, and prints one line per worker. Nothing is typed into a question.
4. For each `needs you` line, show the owner the screen text that deploy
   printed. A first-run question (folder trust, integrations) or a login
   problem needs the owner on the machine: `tmux attach`, start the worker's
   command, answer, exit. Then deploy again. A `ready` line that names
   something that failed to load changes what the brief should say.
5. When every worker is `ready`, and the owner agrees, start the agent:
   `shed deploy -install-agent <machine>`.
6. `shed status` shows `agent ok` within a minute, and each worker under
   `supervised` within two.

## Tidy a machine

`shed tidy <machine>` lists the worktrees and local branches whose pull
request is merged. It changes nothing. Show the owner the two counts and
anything under "stays" that looks finished to you but has uncommitted files:
that is work nobody pushed. `shed tidy -apply <machine>` removes what was
listed. Run it when the owner asks, or after a batch of merges; it is safe
while workers are busy, since a worker's directory and anything with
uncommitted or unmerged work is never touched. Never remove a worktree by
hand to "help" it: a refusal means there is work in it.

## Keep shed current

`shed update` replaces the owner's shed with the latest release. `shed deploy`
then brings every machine to the same version and restarts its agent. Workers
keep running across that restart.

A deploy is safe while workers are busy: the agent finishes a message it is
typing before it restarts, and the new agent picks up from the check-in log
without reviewing or messaging anyone again.

A worker keeps the tools it started with. After a change to its tooling (an
integration installed, an agent definition or skill changed), give it a fresh
session: `shed recycle <machine> <worker>` waits for the issue in hand;
`-now` does not, and loses whatever that issue had not yet written down. Use
`-now` only on the owner's word.

## Do not

- Do not type into a `shed` tmux window yourself while the agent runs, except
  to answer a prompt for the owner. The supervisor reads that terminal.
- Do not end or restart a worker's session by hand. Use `shed recycle`.
- Do not edit the fleet file on a worker machine. Edit the owner's copy and
  deploy.
- Do not label an issue for a machine to "see what happens". A worker will
  start it.
