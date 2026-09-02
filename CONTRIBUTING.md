# Contributing

Issues, questions and discussion are very welcome — including "this documentation is wrong" and
"this error message did not help me", which are real bugs here.

## Before you open a pull request

kontra is **AGPL-3.0** with commercial licences sold by the maintainer. That model only works
while the maintainer holds the copyright, so contribution terms are not yet settled and a policy
will be published before the repository accepts outside changes.

**Please open an issue first.** For anything beyond a typo, agreement on the approach before code
is written saves everyone the awkward conversation afterwards.

## What good looks like here

This codebase has some unusual habits, and they are load-bearing rather than decorative:

- **Comments explain WHY, and name what was measured.** "Retries three times" is visible in the
  code. "Retries three times because a hold arriving while the Fleet is tearing down is refused on
  purpose, and that resolves in seconds" is not.
- **A new test must be proved red.** Break the thing it guards, watch it fail with its own
  message, restore. A test that has only ever been seen to pass is not evidence — this repo has
  shipped several guards that could never have fired.
- **A guard that sweeps must assert it found something.** A walk over zero files reports success.
- **A contract with two implementations gets a corpus**, not two string literals. See
  `conformance/README.md`.
- **Measure, don't assert.** If a change claims a cost, put the number in the commit message and
  the test that produced it in the tree.

## Running the suites

```bash
go test ./cli/...              # ~10 minutes; pass -timeout 40m
cd backend && pnpm vitest run
pytest -m "not e2e"
```

`scripts/parity-gate.sh` is the end-to-end check that the single binary really runs an actor.
