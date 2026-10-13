# A repo recipe for unattended workers

shed keeps workers moving. It does not make their work safe to accept. That
comes from the repository they work in. This is the recipe the author uses;
it is an opinion, and shed works without it.

The rule behind every line: **a worker may be wrong, a supervisor may be
wrong, and neither may be able to do damage by being wrong.**

| Guardrail | What it stops |
| --- | --- |
| The main branch is protected. Nobody pushes to it, including the owner. | A worker that "just fixes it on main". |
| All work merges through a pull request. | Work that nobody looked at. |
| A pre-push hook runs the local gates (format, lint, unit tests, build), and a push needs all of them green. Nobody bypasses it with `--no-verify`. | CI minutes spent on a commit that already fails on the machine that made it. |
| Every pull request runs exhaustive CI: static analysis, lint, unit tests, integration tests against real dependencies. Merging needs all of it green. | A worker that reports "tests pass" for the tests it chose to run. |
| A merge deploys to staging, not to production. | A bad merge reaching users. |
| Staging gets a smoke test in a real browser, with screenshots at each step. | Changes that pass every test and still look or behave wrong. |
| Production deploys need the owner's sign-off. | Everything else. |

## How it meets shed

- **The worker's side of the recipe goes in `defaults.standing_orders`**:
  open a pull request, never merge, never deploy, never weaken a test,
  never push past a failing hook. The
  supervisor judges every check-in against those orders.
- **The repository enforces what the orders ask.** Orders are a request.
  Branch protection and required checks are the guarantee. Give the worker
  machines credentials that cannot bypass them, and no production access.
- **A pull request is a hand-back.** A worker that comments `**PR:** #41` on
  its issue opens the `review` signal: `shed status` shows the issue as
  waiting on you, and the supervisor gives the worker its next issue.
- **Screenshots are for you.** A check the worker could not make itself goes
  in the issue after `Needs eyes:` with the URL, the steps, and what a pass
  looks like. That opens the `eyes` signal.
- **Sign-off stays with you.** The supervisor is told never to make a decision
  that belongs to the owner, and merging and deploying are on that list.

## Before you point a worker at a repository

1. Try to push to main from the worker machine. It must fail.
2. Open a pull request with a failing test. Merging must be blocked.
3. Push a commit that fails lint from the worker machine. The pre-push hook must stop it.
4. Confirm the worker machine has no production credentials.

## When the merge stops being yours

`repos[].verify.merge_low` moves your approval of a low-risk pull request
from the merge to the promotion: a pull request a verifier passed at low
risk, with green checks, is queued for merge by shed, and you approve what
reaches production with `shed promote`. The guardrails above still hold:
the merge queue runs the full suite on what lands, staging gets its eye
checks, and production needs your sign-off. Turn it on only after the
digest has shown the escape rate of `low` near zero for some weeks, and
give the worker machines a GitHub login that may enable auto-merge and
nothing more.

## A merge queue

Per-pull-request CI tests a branch against the main branch as it was when
the branch was made. With several workers merging in a day, what lands is a
combination that nobody tested, and each merge sends the other open pull
requests through a rebase and another full run.

A merge queue tests what will land: each pull request on top of the ones
queued before it. Run the full suite there, and only there:

1. Turn on the merge queue in the main branch's protection rules, and make
   the full suite a required check.
2. Run the full suite's workflow on `merge_group`. A required check must
   also report on the pull request, so give the pull request a light job of
   the same name, or run there only what the gates do not cover.
3. Give the repository `gates` in the fleet file. The machine that built the
   pull request runs them on every push, in minutes, on warm caches.

The gates catch most failures before the queue; the queue catches the rest
on the real combination, where no worker can post a status for it.
