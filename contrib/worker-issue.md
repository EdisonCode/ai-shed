---
name: Worker task
about: An issue an unattended worker can take with no other context
title: ""
labels: []
---

<!--
A worker reads this issue and nothing else. It cannot ask you a question and
it has not seen any other thread. If it needs something from another issue,
a chat or your head, write it here.
-->

## Goal

What is true when this is done, in one or two sentences. Say who notices the
difference and where.

## Where

The module, files or screen the change belongs in, and the boundary it must
not cross.

## Acceptance

- [ ] One check per line that a worker can run or show.
- [ ] For each, what must fail when the behaviour is broken: the test that
      goes red, or the change that must make it go red.

## Out of scope

What a worker might reasonably do while it is here, and must not.

## Decided already

Each decision that bears on this, with the answer quoted. A link is not
enough.

## Needs eyes

What someone should look at once it runs, the steps to get there, and what a
pass looks like. Write "nothing" when a test covers it all.
