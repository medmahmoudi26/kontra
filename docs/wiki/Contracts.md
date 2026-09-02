# Contracts

Kontra binds a TypeScript orchestrator, a Python SDK, and a Go SDK. Two kinds of artifact keep them from drifting: **proto type-of-record** (shapes) and **conformance corpora** (behavior). The wire stays JSON (ADR 0002) — proto is the single type definition each seam is congruence-tested against, not a binary converter.

## Layout rule

Contracts live **only** in the root `contracts/` directory. `buf generate` writes the committed, drift-checked stubs into the *consuming* seam (`sdk/python/_gen/`, `control/orchestrator/_gen/`, `handler/_gen/`) — generated code never lives under a contract dir.

## The protos

| Proto | Defines | Consumed by |
|---|---|---|
| `entry.proto` | `EntryInput` (the actor's `run` input) + `BareRef` (the claim-check ref) — the two genuinely cross-language types | a caller's workflow ↔ the handler ↔ the actor hosts |
| `actor.proto` | `ActorRunInput` / `ActorRunResult` / `PerUnitFailure` / `ErrorInfo` — the first-class per-batch actor contract (ADR 0012) | both SDKs |
| `step.proto` | *Deleted 2026-08-13 with ADRs 0009/0013 — `StepOptions` was read by no SDK in any language.* | — |
| `catalog.proto` | `ActorDescriptor` — identity + operation schemas for catalog registration | orchestrator ↔ workers |
| `run.proto` | `RunEnvelope` — the future Nexus envelope (cross-namespace routing metadata); unused today | — |

**Congruence tests** hold each hand-written wire struct to its proto field set: `tests/test_workflows_client.py` (Python), `handler/internal/wire/wire_congruence_test.go` (Go), `backend/entry.contract.ts` (a compile-time TS guard). Change the proto → the tests fail until every seam matches.

### `parallel_sessions` and the reserved field

`entry.proto` reserves field 4 (`parallel_sessions`) — removed as a *wire* field in the ADR 0012 cleanup (the actor runs one batch; the dispatcher shards). The rule stands: never re-add sharding knobs **to the wire**; a proto comment records the distinction.

## Conformance corpus

Shapes agreeing isn't enough — behavior has to match too. A JSON corpus is run against every SDK:

- **`shared/conformance/codec/`** — the claim-check codec: same marker (`binary/claim-check-v1`), same `cas/<sha[:2]>/<sha>` layout, same `{sha256, size, meta}` ref, same sha256 integrity check. Three implementations run the one `fixtures.json`: the handler's Go codec, the TS orchestrator's, and Python actorkit's (`internals/codec.py`). Run: `just conformance` / `pnpm test` (`src/codec/conformance.test.ts`) / `go test ./internal/codec` in `handler/`.
- **`shared/conformance/blobkey.json`** — the per-unit blob key, asserted by each SDK's own suite. Both hosts must produce the same key for the same inputs or the reader sees two layouts; they have drifted once already, which is why the fixture exists.

## Compat ownership (ADR 0027, superseding 0008)

Two owners, cleanly split — both inside this repo:

- **buf** owns the **envelope** — the shared proto types above; `buf lint` + `buf breaking` against the fork point, run by `make proto-check` and by CI.
- **the catalog's registration gate** owns **author I/O** — each actor's derived input/output/params JSON Schemas, checked at `POST /api/actors` (`control/orchestrator/src/catalog.ts`). A re-registration of a `(name, version)` the catalog already holds whose schema differs is refused with **409**, naming the Method that moved. This is why identity is content-pinned (ADR 0004/0011): `(name, version)` is immutable, so a rebuilt-but-not-bumped image is caught (the worker self-verifies its OCI digest against the catalog's pin, and the call fails rather than running unknown code).

**What is NOT checked, and used to be claimed:** compatibility *across* versions. Nothing compares `myactor@0.2.0`'s input schema to `0.1.0`'s, so a bump whose new schema an existing caller cannot satisfy registers cleanly. `legacy/0008` assigned that check to an external schema registry that never ran, so the check was never run either — the gap is real and is now written down (ADR 0027) rather than covered by a claim.

## Identity strings (one derivation per side)

The Go handler derives the Temporal routing strings from `(name, version)` in one place (`handler/internal/identity`); each actor host derives the queue it polls the same way (`runtime/python/internals/temporal/host.py` `task_queue`, `runtime/go/temporalhost` `TaskQueue`), so no caller invents a third encoding:

| String | Value |
|---|---|
| handler (shared) queue | `{name}-{version}` (or `{name}-shared` when version is empty) |
| actor (sessions) queue | the shared queue + `-sessions` |
| Nexus endpoint | `kontra-{name}-{version}` (non-alphanumerics → `-`) |
| catalog key | `{name}@{version}` |

| per-Session queue | the shared queue + `-s-{sessionId}` (ADR 0023 §6) |
| tmux session | `{actor}-{version}`, `.` and `:` rewritten to `_` |

The sessions queue is the one derivation with no room to drift: the workflow builds it at runtime from its OWN task queue (workflow code cannot read env and stay deterministic), so a mismatch on the actor side does not error — the actor registers, polls a queue nobody schedules onto, and looks like a healthy idle worker while every run hangs.

**Every row above is pinned by [`shared/conformance/queues.json`](../../conformance/queues.json), which Python, three Go modules, both TypeScript halves and the CLI all execute.** It replaced three hand-copied golden tables and, with them, three comments that each told the reader how many peer derivations there were: they said "a fourth", "THE FIFTH" and "a SIXTH", no two counting the same set, and one named a file that had been renamed out of the tree. The corpus carries the fallbacks (an absent version, an absent session id, an unnameable tmux session) and the inputs that break — a space and a non-ASCII rune, which a queue name keeps and an endpoint name does not. ADR 0018 deleted two rows from this table — an actor TYPE string and an app id — which had been derived three times over, in the handler and in both SDKs, with three hand-synced test tables.
