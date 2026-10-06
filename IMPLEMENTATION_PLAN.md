# Workers must never stall (#43)

The aim: with the owner away, work goes on. A worker stops only for a
decision that is really the owner's, and even then it takes other work.

## Stage 1: A tool error is retried before the owner is asked
**Goal**: A worker whose tool stopped on an API error (403, 429, 5xx, overloaded) is told to continue after a backoff of 5, 15 and 45 minutes. Only when the last retry fails does it need the owner. Proof that the account works (another worker seen working after the error) retries at once.
**Success Criteria**: A worker stopped by an error that clears is working again with no person involved.
**Tests**: retry is sent after the backoff and not before; three failed retries end in `needs_owner`; another worker on track retries at once; a restart of the agent keeps the count and the due time.
**Status**: Complete

## Stage 2: Three kinds of choice
**Goal**: A safe default is taken and recorded under `Decided without you:` and never holds an issue. A risky decision starts a clock (`signals.decision.grace`); after it the issue is parked and the worker takes the next. A ruling brings it back ahead of new work. The fleet file says what "risky" means.
**Success Criteria**: A hand-back with only `Decided without you:` entries does not hold its issue. A risky decision with no ruling parks its issue after `grace`, and a later ruling brings it back first.
**Tests**: signal parsing, queue order after a ruling, brief and reviewer wording, status and digest lines.
**Status**: Complete

## Stage 3: A queue that cannot run dry
**Goal**: A lower-priority `backfill` source per machine, taken from only when the first sources have nothing workable; a readiness check; a mark so that two machines do not take the same issue.
**Success Criteria**: With every labelled issue waiting on the owner and a backfill source set, no worker is idle for more than one check-in.
**Tests**: config validation, queue order, claim, not-ready pass and its cap.
**Status**: Complete

## Stage 4: Waits that do not hold a worker, and no finished work handed out
**Goal**: A draft that says what it waits for is not a review for the owner; a pull request that conflicts reads `rework`, not `eyes`; an issue whose pull request merged after its last hand-back is not handed out as new work.
**Success Criteria**: Each case has a test in backlog or supervisor and shows correctly in `shed status`.
**Tests**: backlog signal states, queue contents, status rendering.
**Status**: Not Started

## Stage 5: Say that the fleet is starved
**Goal**: One `!` line per machine that says how many workers have no work and why; a notification when a worker has had no work for a set time or the workable queue is short; idle worker-hours by cause in `shed digest`.
**Success Criteria**: `shed status` states in one line how many workers have no work, and why.
**Tests**: status assessment and rendering, supervisor notifications, digest totals.
**Status**: Not Started
