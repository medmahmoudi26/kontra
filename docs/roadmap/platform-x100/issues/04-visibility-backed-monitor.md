# 04 — Visibility-backed monitor (Temporal Search Attributes)

Status: ready-for-agent
**Tier:** 2 | **Effort:** L | **Depends on:** —

## Problem
`kontra monitor list`/`--state` and `control/orchestrator/src/db/repo.ts` (`listRuns`) maintain a **SQLite**
run table that duplicates what Temporal already tracks for every workflow (status, timestamps,
type). Two sources of truth that drift; the SQLite one is the weaker copy.

## Native capability to use
Temporal **Visibility API** + custom **Search Attributes**. The dev server
(`temporalio/temporal start-dev`) ships SQLite visibility that supports custom search attributes —
no Elasticsearch required. Query with `ListWorkflowExecutions` + a SQL-like filter.

## Approach
- Register search attributes on the namespace at bootstrap: `KontraTenant`, `KontraActor`,
  `KontraGraph`, `KontraRunId` (+ rely on built-in `ExecutionStatus`, `StartTime`).
- Upsert them from the workflow (`runtime/handler/workflow.go` / interpreter) via
  `UpsertSearchAttributes` / `UpsertTypedSearchAttributes`.
- Add an orchestrator endpoint that proxies `ListWorkflowExecutions` (filter by tenant/actor/status)
  so the CLI/UI don't need Temporal creds directly.
- Rework `cli/monitor.go` execution views (`list`, `--state`, detail) to read that endpoint. Keep
  the **data** views (`--query`/`--export`/`--duckdb`) exactly as-is — those are kontra's value-add.
- Keep SQLite only for what Temporal does not model (e.g. idempotency keys), or retire it.

## Files
- `runtime/handler/workflow.go` (or interpreter) — upsert SAs
- orchestrator: bootstrap SA registration + a visibility proxy endpoint
- `cli/monitor.go` — exec views over the visibility endpoint
- `control/orchestrator/src/db/repo.ts` — trim to non-duplicated state

## Verify (local, no fleet)
- Dispatch runs across two tenants/actors.
- `temporal workflow list --query "KontraActor='cachebuster'"` returns them.
- `kontra monitor list` and `--state` match the Temporal UI exactly; kill/complete a run and see it
  reflected without a separate DB write.

## Risks
- Search-attribute registration differs slightly between `start-dev` and a full server — pin the
  path we use. Coordinate `runtime/handler/workflow.go` edits with issue 06 (same file).

## Comments
