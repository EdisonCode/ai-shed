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

## A merge train, where there is no merge queue

GitHub offers a merge queue for public repositories and for private ones on
Enterprise Cloud. Without one, two pull requests that each pass against the
main they last saw can break main together. Where shed merges for you
(`merge_low`), it can run a train in place of the queue:

```yaml
repos:
  - repo: my-org/my-app
    verify:
      merge_low: true
      merge_train: true
```

shed then merges one queued pull request at a time per repository. It
brings the branch up to date with the base (a rebase when `merge_method` is
`rebase`, a merge of the base otherwise), waits for the checks on that new
head, and merges only at that head. If a check fails there, it takes the
pull request off the train with a comment that names the check, the
pull request goes back to a worker as rework, and the next one boards. A
pull request taken off boards again only after its next hand-back is
verified.

It costs one CI run per merge, and merges wait their turn. That is slower
than a merge queue, which batches, but main stays green. A repository with
no checks on its pull requests never merges on the train: there is nothing
to wait for, so there is nothing that passed.

### In batches

Where CI is slow and many pull requests wait, one run per merge falls
behind. A batch train tests several pull requests in one CI run:

```yaml
repos:
  - repo: my-org/my-app
    verify:
      merge_low: true
      merge_train: batch
      batch_size: 5                 # the default
      merge_train_alone:            # optional: a change here is tested by itself
        - db/migrations
        - package-lock.json
```

shed takes up to `batch_size` queued pull requests, the first verified
first. It makes a branch `shed-train/<machine>/<time>` from the base, merges
each pull request into it at its verified head, in order, and opens the
branch as a draft pull request, so your CI runs on the combination. Do not
merge that draft yourself.

- **Green.** shed merges each pull request of the batch, in order, at the
  head the batch tested (`--match-head-commit`), without updating its branch.
  Then it closes the draft and deletes its branch.
- **Red.** shed closes the draft and tests the first half of the batch, and
  so on, until a batch of one fails. That pull request comes off the train
  with a comment that names the failed check and the batch that failed
  first, with its pull requests, and goes back to a worker as rework; the
  rest merge.
- **A pull request that conflicts** with the ones before it comes off the
  train and goes back to a worker, and the batch is built again without it.
- **A pull request that gets new commits** while it is on the train (a fix
  after a red batch, say) comes off it too, but needs no worker: its
  verification was of the head that boarded, so a verifier looks at the new
  head, ahead of new work, and it boards again once that passes. `shed
  status` shows it as `re-verify`. The verifier is told the head it passed
  before, and judges only what changed since when that touches only tests
  or docs.
- **A batch whose base moved** under it (something merged to the base
  branch outside the train) goes on when the new base commits touch none of
  the files the batch changes: they cannot change what it tested, and it
  merges when it is green. When they touch its files, or are too many for
  GitHub to list, the batch is built again on the new base, its pull
  requests together, so that what merges is what was tested. A running
  batch is never thrown away for a pull request verified while it runs:
  that one waits for the next batch.
- **A red base branch** is not the pull requests' fault. Before shed takes
  anything off, it reads the checks of the base branch's latest commit.
  When every check that failed also fails there, nothing comes off and the
  batch is not split: the train holds, builds nothing while the base is
  red, and goes on once it is green. `shed status` shows `merge train:
  held: main is red for 12m (test)`, and after 30 minutes it puts a `!`
  line at the top: every merge waits on that fix. The one-at-a-time train
  holds the same way, and brings its pull request up to date with the fix.
- **One merger per base.** Each rebuild is counted on the batch, and
  `shed status` shows it (`built again 2 time(s) for the base moving outside
  the train`). From the second, the batch waits on you in `shed status` and
  the digest, with the base commit that moved it last: as long as people or
  other sessions merge to the base in the batch's files, a slow suite can
  keep it from ever landing. Route those merges through the train.
- **`merge_train_alone`** paths, named as in `areas`: a pull request that
  touches one is tested in a batch of its own.

The batch is a draft, and a CI that skips draft pull requests skips the
batch: every check comes back skipped, and nothing was tested. shed merges
nothing on a skipped run, and `shed status` shows the batch as `every check
was skipped`. Let such a CI run on the train's branches:

```yaml
if: ${{ !github.event.pull_request.draft || startsWith(github.head_ref, 'shed-train/') }}
```

A batch train merges each pull request as it stands, so the base must not
require branches to be up to date before merging (the branch protection or
ruleset option "Require branches to be up to date before merging"). Where it
does, shed boards nothing and logs why; turn the option off, or use
`merge_train: true`. One machine's batch runs at a time on a repository; one
another machine opened in the last hour holds the next back.
