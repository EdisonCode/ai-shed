# Implementation Plan

One binary, `shed`. The watcher (a laptop) reads one YAML file that describes
every machine and asks each machine for its state over SSH. A small agent on
each machine runs that machine's scheduled tasks.

## Stage 1: Fleet config
**Goal**: `shed.yaml` schema, loader and validation; `shed validate`.
**Success Criteria**: A bad config fails with every problem listed; the example config loads.
**Tests**: valid example loads; duplicate names, bad schedule, unknown field, missing host are rejected.
**Status**: Complete

## Stage 2: Machine probe
**Goal**: One SSH round trip per machine returns load, disk, check results, tmux workers, agent state.
**Success Criteria**: Probe output parses on Linux and macOS formats; a local machine can be probed without SSH.
**Tests**: script generation; parsing of both `uptime` formats, checks, windows, run records; truncated output is an error.
**Status**: Not Started

## Stage 3: Issue backlog
**Goal**: List each machine's open GitHub issues (through `gh`) and detect the ones that wait on the owner.
**Success Criteria**: A hand-back that asks for a decision marks the issue as waiting until an owner ruling follows.
**Tests**: signal open, cleared by "none", answered by a later ruling, re-opened by a later ask.
**Status**: Not Started

## Stage 4: Status report
**Goal**: `shed status` combines probe and backlog into one report with an attention list; exit code 1 when something needs the owner.
**Success Criteria**: Unreachable machine, failed check, full disk, waiting issue, idle backlog, failed or missed task each produce an attention line.
**Tests**: one test per attention rule with fake runner and lister.
**Status**: Not Started

## Stage 5: Agent and deploy
**Goal**: `shed agent` runs a machine's scheduled tasks and records each run; `shed deploy` copies binary and config to a machine.
**Success Criteria**: A task run writes start and end records; timeout kills the task; config edits are picked up without a restart.
**Tests**: run record for success, failure, timeout; platform mapping for deploy.
**Status**: Not Started
