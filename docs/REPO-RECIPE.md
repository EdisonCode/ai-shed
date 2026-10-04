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
| Every pull request runs exhaustive CI: static analysis, lint, unit tests, integration tests against real dependencies. Merging needs all of it green. | A worker that reports "tests pass" for the tests it chose to run. |
| A merge deploys to staging, not to production. | A bad merge reaching users. |
| Staging gets a smoke test in a real browser, with screenshots at each step. | Changes that pass every test and still look or behave wrong. |
| Production deploys need the owner's sign-off. | Everything else. |

## How it meets shed

- **The worker's side of the recipe goes in `defaults.standing_orders`**:
  open a pull request, never merge, never deploy, never weaken a test. The
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
3. Confirm the worker machine has no production credentials.
