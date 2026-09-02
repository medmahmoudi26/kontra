# 41. The shared kernel as a published package

## Status

**Accepted.** Extends **0038**, which split the code into repositories and left the console inside `kontra` as `frontend/`. This is the decision that lets the console leave.

## Context

`kontra-console` is a third repository, and the console had been reaching into the orchestrator's source directly:

```ts
'@core':     path.resolve(here, '../backend/src'),
'@contract': path.resolve(here, '../backend/contract'),
```

That is not a dependency, it is a path. It typechecks only on a machine that has the orchestrator checked out at the right relative location with its `node_modules` installed, and it says nothing about what the console is allowed to reach.

Measured before the split: **10 orchestrator modules, 72 import sites, 40 files**. Not a seam that can be cut by copying a few functions.

Four facts shaped the answer.

1. **Some of it must not be copied.** `vocabulary.test.ts` reads `cli/warden_workflow.go`'s **bytes** to pin the Warden's five kind spellings — a rename in Go is a red test in TypeScript. `sharedQueue` is one of four arms of `conformance/queues.json`, executed by Go, Python, the CLI and the orchestrator; the console's `actorSession` is a fifth. Duplicating any of it is what those tests exist to prevent.

2. **The duplication had already started, and nothing failed.** `backend/contract/types.ts` and a copy of it were **byte-identical** — one edit from an API whose two halves disagree with nothing going red. `SECRET_NAME_RE` was declared twice, kept honest by a test that read the server's source and compared two regex literals as strings. `DatasetState` was declared in the contract *and* in the console, under a test asserting "widening it is a compile error on both sides" that checked a file which never declared it — passing vacuously for its whole life.

3. **Most of the kernel is pure and some of it is emphatically not.** `panels/pollers.ts` reaches Pulumi and a filesystem; `actorControl.ts` reaches DuckDB. Each had a pure half worth extracting and a server half that had to stay. A browser bundle that transitively imported the server half got `node:sqlite` with it, which surfaced as a vitest pool failure naming no file in either repository.

4. **A package is resolved by four tools that disagree.** `tsc`, vite, rollup and Temporal's Workflow bundler each answer "where is `@kontra/core`" differently, and three of them are silent about it until late.

## Decision

- **`core/` is a package in `kontra`, published as `@kontra/core`,** Apache-2.0 like the SDKs — a client's console build links it.

- **The rule for what belongs is narrow and stated in `core/src/index.ts`:** code that answers *how is a Run read* and depends on nothing but its arguments. Anything reaching a filesystem, a Pulumi stack, a secret store or a Temporal client stays in the orchestrator. Where a module had both, the pure half moved and the server half re-exports it, so its own callers did not have to move.

- **The orchestrator keeps one-line re-export shims** at the old paths. 81 import sites did not change in the commit that moved the code, and each shim says what moved and why.

- **The conformance corpora ship inside the package.** `core/dist/conformance/` is generated from the repository's single `conformance/` directory at build time and exported as `@kontra/core/conformance/*`. The console's browser arm resolves the corpus through the dependency rather than through a sibling checkout, so the corpus stays one file with five arms rather than becoming two files.

- **It emits JavaScript, dual CJS + ESM, and carries a `typesVersions` map.** Each of those three is load-bearing and none is stylistic — see below.

- **`vocabulary` stays in `kontra` whatever else moves,** because its test reads Go source in this repository. That contract cannot cross a repository boundary.

## Considered options

**Leave the path aliases and require a sibling checkout.** Rejected: it is the status quo that made `frontend/` unmovable, and it gives the console unbounded reach into server code — which is how `node:sqlite` got into a browser test in the first place.

**Copy the shared modules into the console.** Rejected on fact (1) and demonstrated by fact (2): the two duplications already present were both invisible, and one of the tests guarding against a third was passing without checking anything.

**A third repository for the kernel.** Rejected: `vocabulary`'s test reads `cli/warden_workflow.go`, so the kernel would need kontra checked out beside it — the same problem, moved.

**Publish TypeScript source and let consumers transpile.** Rejected on measurement, not taste. Temporal's Workflow bundler registers its loader as `{ test: /\.ts$/, exclude: /node_modules/ }`, so a package with a `.ts` entry point resolves and is then never transpiled. It typechecks, passes vitest, and fails five suites the moment anything builds a Workflow bundle — which is to say, it fails in the orchestrator at runtime.

**CommonJS only.** Rejected: rollup reads a CJS module as one default export, and the console's build fails with `"workflowScratchId" is not exported by dist/scratch.js` — an error naming a symbol that is plainly exported. Hence dual output, with `dist/esm/package.json` generated to mark it.

**ESM only.** Rejected: the orchestrator is CommonJS and cannot `require` it.

## Consequences

- **The package must be built before anything consumes it.** `tsc --noEmit` alone is not evidence: the extraction went green on typecheck and red on **54 test files**, then green again and red on **five more** that build a Workflow bundle. The workspace root's `build`, `typecheck` and `test` scripts all build `core` first, and there is deliberately **no** `paths` entry and **no** vitest alias pointing at `core/src` — an alias would let the suite pass against source the orchestrator does not run, and hide a stale `dist`.

- **`moduleResolution: "Node"` cannot read `exports`.** The orchestrator is on node10 resolution, so `@kontra/core/panels/ids` has no types without the `typesVersions` map. Removing it is a silent breakage in one consumer only.

- **The console no longer needs kontra to build or test** — 120 files and 2,147 tests run against the package alone. Its Playwright harness still constructs a real `PanelServer` from the orchestrator's source and cannot be made not to, so that is fenced into `tsconfig.e2e.json`, which names the sibling-checkout requirement instead of letting `pnpm typecheck` fail obscurely.

- **Two cross-repo tests were split and one lost coverage, stated where it happened.** The retention-window sweep and the `DatasetState` check now assert only this repository's half. The console asserts its own. **No single test sees both halves any more** — if the console vendors a literal and deletes its test in one commit, nothing here goes red. The mitigation is that both values are now importable, so copying one is a choice rather than the default.

- **The console's workflow fixtures are copies.** `workflowSource.test.ts` read the canonical examples from `examples/python/workflows/`; those are in `kontra-workflows` now and a unit suite cannot require two sibling checkouts. They are vendored into `testdata/`, and nothing checks that they are current.

- **`link:` until it is published.** The console depends on `link:../kontra/core` — a symlink, so an edit is live. A `file:` dependency was tried first and pnpm **copied** it, which is a stale-dependency trap of exactly the kind this ADR is otherwise about. Publishing to npm needs a token; that is the one credential this decision adds.
