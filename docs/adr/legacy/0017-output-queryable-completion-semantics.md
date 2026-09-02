# 17. A public run is not `completed` until its typed output is queryable

## Status

**Accepted — decision only; not built.** Supersedes, in part, ADR 0014's best-effort materialization
clause and the completion-reporting implication of ADR 0005. Execution independence from analytics
is preserved unchanged. The two-dimensional status model and the isolated materializer it depends on
are the first implementation slice (`.scratch/query-surface/PLAN.md`, delivery stages 1-3).

## Context

Today a run reports `completed` while its output is not queryable, and the two facts are unrelated
by design:

- `interpreter.ts:272` (and `:471` on the streaming path) sets `status[node.id] = 'completed'`
  **before** calling `linkDataset` at `:280` / `:474`.
- Both call sites wrap that call in `try { … } catch { /* dataset mirror is best-effort */ }`.
- ADR 0014's Decision states it outright: "**Best-effort: a conversion failure is logged, never
  fails the run.**" `parquet.ts:176` repeats it.
- Worse than best-effort in practice: the failure path swallows the error and returns null with **no
  log line at all**, despite the comment claiming otherwise. Absent output is indistinguishable from
  empty output.

Separately, what does get materialized is not the output. Every DuckLake table holds two columns,
`run_id` and `$ref` — claim-check pointers, because `linkDataset` materializes the node's output
*envelope* and that envelope is a claim-check. A row is
`{"$ref":{"key":"units/…/u0/a5ccb888….json","sha256":"…","size":156}}`. `SELECT host, status_code`
cannot be written at all: those columns exist in the blob, not the table. This is not a size
threshold — the refs point at 156-byte payloads.

The combination is the actual defect. A run says `completed`; the dataset for it may be absent
(silently), or present and unqueryable (pointers). An operator cannot distinguish "found nothing",
"materialization failed", and "materialization succeeded but stores pointers" from any status this
system reports. Three failures wearing one green label.

This matters more here than it would elsewhere because this platform's recurring failure mode is
precisely a confident success report over lost work — the same shape as a run that discards every
unit and still reports `completed` (ADR 0015 §7a, and the `isolated` counter that exists to break
that tie).

## Decision

**A public run lifecycle does not report `completed` until that run's typed output is queryable.**

### 1. Two status dimensions, two authorities, never merged in storage

- **Execution status** — authority: Temporal. The existing `RunStatus`
  (`pending|running|completed|failed|cancelled`, `backend/types.ts:147`) is **unchanged**, keeps
  its current meaning ("did the actor graph finish"), and keeps its terminal set
  (`runs.ts:32`). Nothing about how it is reconciled from Temporal (`runs.ts:83`) changes.
- **Materialization status** — authority: an application-owned PostgreSQL table, keyed by
  `(run_id, actor, version, node, materialization_schema_version)`, with states
  `pending|running|complete|failed`. DuckLake's own catalog tables are never written directly.

Neither dimension is derived from the other, and neither is stored as a mutation of the other.

### 2. The public lifecycle is a projection, not a new `RunStatus` member

The API derives and exposes `executing | finalizing | completed | output_failed` **from both
dimensions**, alongside both raw dimensions. This is a new field, not a new member of `RunStatus`.

That distinction is load-bearing. Adding a non-terminal state to `RunStatus` would silently hang
every existing poller — `TERMINAL` at `runs.ts:32` and the CLI loop at `cli/dispatch.ts:184-185`
both break on `completed` — and would make Temporal reconciliation write a state Temporal has no
concept of. Keeping the projection separate means existing consumers keep working and opt in.

Mapping: execution terminal + all nodes materialized `complete` → `completed`; execution terminal +
materialization outstanding → `finalizing`; execution terminal + any materialization exhausted →
`output_failed`; otherwise `executing`.

### 3. What "queryable" means, precisely

Typed rows in DuckLake — actual actor output columns, not `$ref` pointers — committed in one
transaction per node, with a row count recorded. Row count zero is a **successful empty result** and
is not a failure; a node materialization is `complete` or `failed`, with no `partial` state.

### 4. Analytics still never block execution

ADR 0005's invariant is preserved verbatim: "Analytics never sit on the critical path; a stale or
unavailable query store (browser **or** Iceberg) cannot block execution."

What changes is only the **label**, not the execution. Execution reaches its terminal outcome
independently; every ref is durable in CAS the moment the actor emits it; no actor waits on
materialization; no unit is re-run because a query store is unavailable. A run whose materialization
exhausts its retries has still *executed*, its refs are still readable, and `kontra explore` still
opens it through the raw-object compatibility path. `output_failed` is a reporting outcome, not a
rollback.

The narrow claim being superseded is ADR 0005's implication — carried in "an async sink **off
run-completion**" — that materialization is necessarily *after and outside* the reported lifecycle.
It is now inside the reported lifecycle and still outside the execution path.

### 5. Refs remain the sole workflow currency

ADR 0007 is untouched. Typed rows never enter workflow memory. The materialization child workflow
carries paged **refs**, counts and scalar status only; payload bytes flow SeaweedFS → embedded
DuckDB → Parquet inside an isolated materializer process that is not the orchestrator worker and
not on the controller. The dataset is a second **witness** of completion, never a payload.

CAS addressing is unchanged: no run-scoped component enters the content hash (ADR 0001). The
completion gate reads the run-scoped **dataset** grain `(actor, version, run_id)`, never the
tenant- and run-blind CAS key.

### 6. The gate cannot deadlock on a cross-worker retry

ADR 0016 notes a Temporal retry may land on a different worker. Re-materialization is therefore
required to be idempotent: the write key above is checked before insert, so a retry that finds a
committed key **repairs status rather than appending rows**. If the DuckLake commit succeeded but
the status write failed, reconciliation by that key writes `complete`. The gate resolves in both
orderings.

## Consequences

- **Supersedes** ADR 0014's Decision §1 clause "Best-effort: a conversion failure is logged, never
  fails the run." A materialization failure now surfaces as public `output_failed`. It still does
  not fail the *execution*, and still does not roll anything back.
- **Supersedes** the completion-reporting implication of ADR 0005's "an async sink **off
  run-completion**". ADR 0005's critical-path invariant is explicitly retained.
- `interpreter.ts:272` / `:471` marking nodes `completed` before `linkDataset` is no longer the
  whole story: node execution status stays as-is, and node *materialization* status becomes a
  separate record. Both call sites and their `catch { /* best-effort */ }` blocks are the concrete
  edit sites.
- `runs.ts:88`'s contract — "A run's materialized output by workflow id (null until completed)" —
  inverts. Output availability becomes an input to the public lifecycle rather than a consequence of
  it. `/api/runs/:workflowId/output` returning 409 until `completed` must key off the projection.
- The silent-swallow at `parquet.ts` must be fixed as part of this: a gate that reads a status which
  can be wrong without logging is not a gate. This is a prerequisite, not a follow-up.
- Existing pollers are unaffected because `RunStatus` is unchanged. The CLI should move to the
  projection so `kontra actor … dispatch` stops returning before output exists, but that is an
  opt-in change, not a forced migration.
- `docs/wiki/Orchestrator.md:75` ("every **completed** run's per-node outputs") becomes accurate
  rather than aspirational, and should be re-read once the projection ships.
- Historical pointer-only runs are **not** retroactively `output_failed`. They predate the gate;
  they report their recorded execution status and are opened through the compatibility path. No
  mandatory backfill.

## Alternatives considered

- **Add a `finalizing` member to `RunStatus`.** Rejected: it would hang `TERMINAL` (`runs.ts:32`)
  and the CLI poll (`dispatch.ts:184-185`), and Temporal — the authority for that enum — has no such
  state to reconcile from.
- **Make `linkDataset` failure fail the workflow.** Rejected: it puts analytics on the execution
  path, directly violating ADR 0005's retained invariant, and would re-run actor work because a
  Parquet write failed.
- **Leave completion alone and document the caveat.** Rejected: this is the status quo, and the
  status quo is three distinct failures sharing one green label. A caveat in a wiki page does not
  reach the operator reading a status field.
- **Gate on the raw blobs existing rather than typed rows.** Rejected: blobs already exist at
  `completed` today, so it would gate on something always true and change nothing.

Supersedes in part **ADR 0014** (the best-effort materialization clause of its Decision §1) and
**ADR 0005** (the completion-reporting implication of its P4 "async sink off run-completion";
its critical-path invariant is retained verbatim). Refines **ADR 0016** (names re-materialization
idempotency as the reason a cross-worker retry cannot deadlock the gate). Relates to **ADR 0007**
and **ADR 0001** (the gate reads the run-scoped dataset grain, never the CAS key, so refs remain the
sole workflow currency and CAS stays content-only).
