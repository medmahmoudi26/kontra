/**
 * Materialization identity and the two-dimensional status model (ADR 0017).
 *
 * PURE — no I/O, no DuckDB, no database. Everything here is a total function over
 * plain values, so the rules that decide "is this run's output queryable?" can be
 * tested without a lake, a Postgres or a Temporal cluster. The durable side lives in
 * `materializationStore.ts`; the writer lives in `parquet.ts`.
 *
 * Two dimensions, two authorities, never merged in storage:
 *
 *   - EXECUTION status — authority Temporal, the existing `RunStatus`. Unchanged.
 *   - MATERIALIZATION status — authority an application-owned table, keyed by
 *     `(run_id, actor, version, node, materialization_schema_version)`.
 *
 * They are combined only here, in {@link publicLifecycle}, and only to derive a
 * REPORTING projection. The projection is a separate field, never a new `RunStatus`
 * member: adding a non-terminal member would hang `TERMINAL` (runs.ts) and the CLI
 * poll (cli/dispatch.ts), and Temporal has no such state to reconcile from.
 */

import type { RunStatus } from '../../contract/types';

/**
 * Identity version of the materialized physical layout. It is part of the table name,
 * the registry key and the idempotency key, so a writer change that alters the row
 * shape lands in a NEW table instead of evolving a table readers already understand.
 *
 *   1 — legacy claim-check layout: two columns, `run_id` + `$ref` (pointers, not output).
 *   2 — typed layout: the unit's own fields as columns, plus `run_id` and
 *       `run_started_at`. This is what {@link writeDatasetParquet} writes today.
 *
 * Bumping this is a deliberate act: old tables stay readable at their own version and
 * are never rewritten (ADR 0017 — no mandatory backfill).
 */
export const MATERIALIZATION_SCHEMA_VERSION = 2;

/** The pre-ADR-0017 pointer layout. Registered, readable, never written again. */
export const LEGACY_SCHEMA_VERSION = 1;

/**
 * A node materialization is `complete` or `failed` — there is no `partial`. Row count
 * zero is a SUCCESSFUL EMPTY RESULT, not a failure; that distinction is the whole point
 * of recording a count, and collapsing it back into a status would recreate the
 * three-failures-one-label defect ADR 0017 exists to fix.
 */
export type MaterializationState = 'pending' | 'running' | 'complete' | 'failed';

export const MATERIALIZATION_STATES: readonly MaterializationState[] = [
  'pending',
  'running',
  'complete',
  'failed',
];

/** Terminal materialization states — nothing further will be attempted. */
const MATERIALIZATION_TERMINAL: ReadonlySet<MaterializationState> = new Set<MaterializationState>([
  'complete',
  'failed',
]);

export function isMaterializationTerminal(s: MaterializationState): boolean {
  return MATERIALIZATION_TERMINAL.has(s);
}

/**
 * The idempotency key of one node's materialization — and, at
 * {@link MATERIALIZATION_SCHEMA_VERSION}, the primary key of the durable record.
 *
 * `schemaVersion` is in the key so a writer upgrade cannot silently append rows of a
 * new shape into a table a reader already resolved at the old shape.
 */
export interface MaterializationKey {
  runId: string;
  actor: string;
  version: string;
  node: string;
  schemaVersion: number;
}

/** One node's durable materialization record. */
export interface MaterializationRecord extends MaterializationKey {
  state: MaterializationState;
  /** How many times a materializer has claimed this key (1 on the first claim). */
  attempt: number;
  /** Rows committed to DuckLake. 0 with `complete` = a successful empty result. */
  rows: number;
  /** Bytes of the parquet data files written for this key. */
  bytes: number;
  /** The DuckLake snapshot the commit landed in, when known. */
  snapshotId: number | null;
  /** Physical DuckLake table name — resolved for callers so they never derive it. */
  tbl: string | null;
  /** Bounded failure summary; null unless `state` is `failed`. */
  error: string | null;
  /** Server-minted run start (ms). Stamped on every typed row as `run_started_at`. */
  runStartedAt: number;
  createdAt: number;
  updatedAt: number;
}

/** Stable string form of a {@link MaterializationKey} — for logs, maps and error text. */
export function materializationKeyOf(k: MaterializationKey): string {
  return `${k.runId}\0${k.actor}\0${k.version}\0${k.node}\0${k.schemaVersion}`;
}

/**
 * Whether a materialization state transition is allowed. The store applies this as a
 * compare-and-set, so a stale writer cannot walk a record backwards.
 *
 * `undefined` means "no record yet". The notable edges:
 *
 *   - `running` → `running` is ALLOWED. A Temporal retry may land on a different
 *     worker (ADR 0016) after the previous attempt died mid-write; refusing it would
 *     deadlock the gate on exactly the failure it exists to survive. Temporal's
 *     start-to-close timeout, not this table, bounds concurrent attempts.
 *   - `complete` is ABSORBING. A retry that finds a committed key repairs status
 *     instead of appending rows — the idempotency rule of ADR 0017 §6.
 *   - `pending`/`running` → `complete` are both allowed, so a commit whose status
 *     write was lost can be reconciled by key without replaying the write.
 */
export function canTransition(
  from: MaterializationState | undefined,
  to: MaterializationState
): boolean {
  if (from === undefined) return to === 'pending' || to === 'running';
  if (from === 'complete') return to === 'complete'; // absorbing; a repeat is a no-op
  if (to === 'pending') return false; // never walks backwards once claimed
  if (from === 'failed') return to === 'running' || to === 'failed' || to === 'complete';
  return true; // pending|running -> running|complete|failed
}

/**
 * The PUBLIC run lifecycle — a projection over both dimensions, exposed alongside
 * them, never stored as a mutation of either.
 *
 *   executing     — the actor graph has not reached a terminal outcome yet.
 *   finalizing    — execution is done; typed output is still being written.
 *   completed     — execution succeeded AND every node's typed output is queryable.
 *   output_failed — the run has no trustworthy queryable output.
 */
export type PublicLifecycle = 'executing' | 'finalizing' | 'completed' | 'output_failed';

export const PUBLIC_LIFECYCLES: readonly PublicLifecycle[] = [
  'executing',
  'finalizing',
  'completed',
  'output_failed',
];

/** Execution statuses Temporal considers terminal (mirrors `TERMINAL` in runs.ts). */
const EXECUTION_TERMINAL: ReadonlySet<RunStatus> = new Set<RunStatus>([
  'completed',
  'failed',
  'cancelled',
]);

/**
 * Derive the public lifecycle from the two dimensions.
 *
 * `states` is every materialization record that EXISTS for the run. Whatever publishes output
 * declares a `pending` record before writing any of it, so the record set is self-describing:
 * there is no separate "expected output" list to drift from it, and a run that produced none
 * simply has no records.
 *
 * Order matters. A failed materialization outranks an outstanding one, because
 * "some output is missing and will not arrive" is the fact an operator must act on.
 *
 * A run whose EXECUTION failed or was cancelled projects `output_failed` rather than
 * `completed`, even when every record it did produce committed: the four-value
 * vocabulary is about output trustworthiness, and a run that did not finish has no
 * trustworthy complete output. The raw execution status is always returned next to
 * this field, so the operator still sees *which* dimension failed — that pairing is
 * what stops this from becoming another single green label over three failures.
 */
export function publicLifecycle(
  execution: RunStatus,
  states: readonly MaterializationState[]
): PublicLifecycle {
  if (!EXECUTION_TERMINAL.has(execution)) return 'executing';
  if (states.some((s) => s === 'failed')) return 'output_failed';
  if (states.some((s) => !isMaterializationTerminal(s))) return 'finalizing';
  return execution === 'completed' ? 'completed' : 'output_failed';
}

/** Counts by state — what the API returns beside the projection, and Grafana charts. */
export interface MaterializationSummary {
  total: number;
  pending: number;
  running: number;
  complete: number;
  failed: number;
  /** Total rows committed across every `complete` record. */
  rows: number;
  /** Total parquet bytes committed across every `complete` record. */
  bytes: number;
}

export function summarize(records: readonly MaterializationRecord[]): MaterializationSummary {
  const out: MaterializationSummary = {
    total: records.length,
    pending: 0,
    running: 0,
    complete: 0,
    failed: 0,
    rows: 0,
    bytes: 0,
  };
  for (const r of records) {
    out[r.state] += 1;
    if (r.state === 'complete') {
      out.rows += r.rows;
      out.bytes += r.bytes;
    }
  }
  return out;
}

/**
 * Cap a failure summary before it is persisted. An unbounded error string is a durable
 * store's memory leak: a DuckDB parse error can carry the offending row, and a row can
 * be megabytes.
 */
export const MAX_ERROR_CHARS = 2000;

export function boundedError(err: unknown): string {
  const msg = err instanceof Error ? err.message : String(err);
  const flat = msg.replace(/\s+/g, ' ').trim();
  return flat.length <= MAX_ERROR_CHARS ? flat : `${flat.slice(0, MAX_ERROR_CHARS)}… (truncated)`;
}
