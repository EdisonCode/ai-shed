---
name: shed
description: Operate an ai-shed fleet of worker machines for the owner. Use when the owner wants to queue GitHub issues for a worker machine, label issues for a machine or a model, write or change a worker's brief, deploy the fleet file, check what the machines did ("shed status", "what did the workers do", "what needs me"), answer a worker's question, or set up a new machine. Also read this when you are a worker and your directory has .shed/BRIEF.md.
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
- A decision that belongs to the owner: write the question and your
  recommendation in an issue comment using the phrase the brief gives, then
  stop on that issue. Do not guess and do not wait.
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
3. **Model label**, only when the fleet file has `supervisor.models`. (With
   no `models` block shed switches nothing; a model label is then only what
   the standing orders say it is.) The
   labels under `supervisor.models.labels` are the choices; an issue with none
   of them gets `supervisor.models.default`. Pick the cheapest model that can
   do the work, by the owner's own rule if they gave one. When the rule does
   not settle it, ask the owner; do not default upward.
4. An issue is handed out oldest first, and never while it waits on the owner.
   To hold an issue back, remove its machine label.

Check the result: `shed status` lists each machine's issues.

## Write or change a brief

A worker's `brief` in the fleet file says what the worker is for and what it
must leave alone. The supervisor judges "is this worker on task" against it, so
a vague brief gives vague supervision.

- Say the theme and the boundary: which repo, which area, what is out of scope.
- Rules that hold for every worker belong in `defaults.standing_orders`, not in
  each brief.
- Never put a secret, customer data or a person's name in the fleet file.

After an edit: `shed validate`, then `shed deploy`. The agent picks the file up
within 30 seconds and tells a running worker to read its brief again.

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

1. `shed status`. Exit code 1 means something needs the owner.
2. For each `!` line, give the owner the fact and your recommendation:
   - `issue waits on you (decision)`: read the worker's comment
     (`gh issue view <n> --repo <r> --comments`), put the question to the
     owner, and post their answer as a comment that contains the
     `answered_by` phrase of the `decision` signal. That reopens the issue
     for a worker.
   - `issue waits on you (eyes)`: do the check the worker described, then
     post a comment that clears the signal: the `answered_by` phrase of the
     `eyes` signal if the fleet file has one, otherwise the ask phrase
     followed by a `clear` word.
   - `issue waits on you (review)`: a pull request is open. The signal closes
     by itself when it is merged or closed. Read the diff and
     the checks yourself before you say it is good. A worker's report is a
     claim. Merging is the owner's call.
   - `worker needs you` / `is stuck`: look at the terminal
     (`ssh <host> tmux capture-pane -p -t shed:<worker>`). A permission
     prompt or a menu needs the owner at the keyboard, or by `tmux attach`.
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
3. In the ai-shed checkout: `make dist`, then `shed deploy <machine>`.
4. Have the owner start each worker's command once by hand in its directory
   and answer the tool's first-run prompts (folder trust, integrations). A
   worker stopped at one shows as `needs_owner`.
5. Start the agent once on the machine. The README has the systemd and launchd
   commands.
6. `shed status` shows `agent ok` within a minute, and each worker under
   `supervised` within two.

## Do not

- Do not type into a `shed` tmux window yourself while the agent runs, except
  to answer a prompt for the owner. The supervisor reads that terminal.
- Do not edit the fleet file on a worker machine. Edit the owner's copy and
  deploy.
- Do not label an issue for a machine to "see what happens". A worker will
  start it.
