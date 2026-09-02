/**
 * The durable materialization-status store — the MATERIALIZATION authority of ADR 0017's
 * two-dimensional status model. Temporal remains the execution authority; this table is
 * never derived from it and never stored inside it.
 *
 * DuckLake's own `ducklake_*` catalog tables are NEVER written here. They describe files
 * and snapshots; they cannot express "this node's materialization is running, on attempt
 * 3, and here is why the last one failed", and writing into another system's catalog is
 * how a lake becomes unreadable to the tool that owns it.
 *
 * TWO BACKENDS, ONE SCHEMA:
 *
 *   - **Postgres** (`KONTRA_MATERIALIZATION_DB=postgres://…`) — production. The
 *     materializer runs OFF the controller (plan §1), so the status writer and the API
 *     reader are different hosts; only a shared database can carry that. Lives in its own
 *     application-owned schema, beside — never inside — the DuckLake catalog schema.
 *   - **SQLite** (default) — single-host dev, `kontra infra up`, and the test suite. Same
 *     SQL, same transitions, same assertions.
 *
 * Every transition is a COMPARE-AND-SET against {@link canTransition}: the `WHERE` clause
 * on the upsert names the states the write may overwrite, so a stale attempt cannot walk a
 * record backwards and a retry that finds a committed key repairs status instead of
 * appending rows.
 */

import type { DatabaseSync } from 'node:sqlite';

import {
  MATERIALIZATION_SCHEMA_VERSION,
  boundedError,
  type MaterializationKey,
  type MaterializationRecord,
  type MaterializationState,
} from './materialization';
import {
  APP_SCHEMA,
  createDriver,
  isPostgresUrl,
  memoizeInit,
  num,
  resolveStoreUrl,
  toPositional,
  type PgPoolLike,
  type SqlDriver,
  type SqlRow,
} from './sql';

/**
 * The one schema, in portable SQL. `BIGINT` and `TEXT` mean the same thing in both
 * engines; the primary key is the ADR 0017 idempotency key verbatim.
 *
 * `run_started_at` is server-minted ONCE per run and preserved across retries — it is
 * stamped onto every typed row, so a retry that re-minted it would give two attempts of
 * the same node two different run timestamps and break `dt=` bucketing after the fact.
 */
function ddl(table: string): string[] {
  return [
    `CREATE TABLE IF NOT EXISTS ${table} (
       run_id         TEXT    NOT NULL,
       actor          TEXT    NOT NULL,
       version        TEXT    NOT NULL,
       node           TEXT    NOT NULL,
       schema_version INTEGER NOT NULL,
       state          TEXT    NOT NULL,
       attempt        INTEGER NOT NULL DEFAULT 0,
       row_count      BIGINT  NOT NULL DEFAULT 0,
       byte_count     BIGINT  NOT NULL DEFAULT 0,
       snapshot_id    BIGINT,
       tbl            TEXT,
       error          TEXT,
       run_started_at BIGINT  NOT NULL,
       created_at     BIGINT  NOT NULL,
       updated_at     BIGINT  NOT NULL,
       PRIMARY KEY (run_id, actor, version, node, schema_version)
     )`,
    `CREATE INDEX IF NOT EXISTS idx_matnode_run ON ${table} (run_id)`,
    `CREATE INDEX IF NOT EXISTS idx_matnode_state ON ${table} (state, updated_at)`,
  ];
}

/** Column list shared by every read, in the order {@link rowToRecord} expects. */
const COLS =
  'run_id, actor, version, node, schema_version, state, attempt, row_count, byte_count, ' +
  'snapshot_id, tbl, error, run_started_at, created_at, updated_at';

/** Column shape of `materialization_node`, as either engine returns it. */
type Row = SqlRow;

function rowToRecord(r: Row): MaterializationRecord {
  return {
    runId: String(r.run_id),
    actor: String(r.actor),
    version: String(r.version),
    node: String(r.node),
    schemaVersion: num(r.schema_version),
    state: String(r.state) as MaterializationState,
    attempt: num(r.attempt),
    rows: num(r.row_count),
    bytes: num(r.byte_count),
    snapshotId: r.snapshot_id === null || r.snapshot_id === undefined ? null : num(r.snapshot_id),
    tbl: r.tbl === null || r.tbl === undefined ? null : String(r.tbl),
    error: r.error === null || r.error === undefined ? null : String(r.error),
    runStartedAt: num(r.run_started_at),
    createdAt: num(r.created_at),
    updatedAt: num(r.updated_at),
  };
}

export interface MaterializationStoreOptions {
  /**
   * `postgres://…` / `postgresql://…` for the shared production store; anything else is a
   * SQLite path (`:memory:` for tests). Defaults to `KONTRA_MATERIALIZATION_DB`, then to
   * `KONTRA_ORCHESTRATOR_DB`, so a single-host install needs no new configuration at all.
   */
  url?: string;
  /** Table name, qualified when the backend wants a schema. Defaults per backend. */
  table?: string;
  /** Inject an already-open SQLite handle (the API shares the Repo's database). */
  sqlite?: DatabaseSync;
  /** Inject a `pg`-compatible pool (tests, or a caller that owns pooling). */
  pool?: PgPoolLike;
}

/**
 * Default table name. Postgres gets its own application-owned schema so the status table
 * can never be confused with — or dropped alongside — DuckLake's catalog tables, which
 * share the same database.
 */
export function defaultTable(url: string): string {
  return isPostgresUrl(url) ? `${APP_SCHEMA}.materialization_node` : 'materialization_node';
}

export class MaterializationStore {
  private readonly driver: SqlDriver;
  readonly table: string;
  readonly backend: 'postgres' | 'sqlite';
  /** Memoized schema creation that CLEARS ON FAILURE — see `memoizeInit` for why. */
  private readonly init: () => Promise<void>;

  constructor(opts: MaterializationStoreOptions = {}) {
    const url = resolveStoreUrl(opts.url);
    const resolved = createDriver({ url, sqlite: opts.sqlite, pool: opts.pool });
    this.driver = resolved.driver;
    this.backend = resolved.backend;
    this.table = opts.table ?? defaultTable(url);
    this.init = memoizeInit(async () => {
      if (resolved.schema) {
        const schema = this.table.includes('.') ? this.table.split('.')[0]! : resolved.schema;
        await this.driver.exec(`CREATE SCHEMA IF NOT EXISTS ${schema}`);
      }
      for (const stmt of ddl(this.table)) await this.driver.exec(stmt);
    });
  }

  /** Explicit init — lets a caller surface a misconfigured store at boot, not mid-run. */
  async ensureSchema(): Promise<void> {
    await this.init();
  }

  /**
   * Insert the `pending` record for a node whose output ref just became durable. This is
   * what makes the record set self-describing: the interpreter declares the work exists
   * before anything tries to do it, so `finalizing` can be distinguished from "no output
   * was ever expected" without a separate expected-node list to drift.
   *
   * Idempotent: an existing record (any state) is left exactly as it is.
   */
  async declare(key: MaterializationKey, runStartedAt: number): Promise<void> {
    await this.init();
    const now = Date.now();
    await this.driver.run(
      `INSERT INTO ${this.table}
         (run_id, actor, version, node, schema_version, state, attempt, row_count, byte_count,
          snapshot_id, tbl, error, run_started_at, created_at, updated_at)
       VALUES (?, ?, ?, ?, ?, 'pending', 0, 0, 0, NULL, NULL, NULL, ?, ?, ?)
       ON CONFLICT (run_id, actor, version, node, schema_version) DO NOTHING`,
      [key.runId, key.actor, key.version, key.node, key.schemaVersion, runStartedAt, now, now]
    );
  }

  /**
   * Claim the key for a materialization attempt.
   *
   * Returns the record if the claim succeeded, or `null` when the key is already
   * `complete` — the idempotent short-circuit of ADR 0017 §6: a retry that finds a
   * committed key must repair status, never append rows. The caller MUST treat `null` as
   * "already done" and do no work.
   *
   * `running` → `running` is deliberately allowed (see {@link canTransition}).
   */
  async claim(key: MaterializationKey, runStartedAt: number): Promise<MaterializationRecord | null> {
    await this.declare(key, runStartedAt);
    const now = Date.now();
    const changed = await this.driver.run(
      `UPDATE ${this.table}
          SET state = 'running', attempt = attempt + 1, error = NULL, updated_at = ?
        WHERE run_id = ? AND actor = ? AND version = ? AND node = ? AND schema_version = ?
          AND state IN ('pending', 'running', 'failed')`,
      [now, key.runId, key.actor, key.version, key.node, key.schemaVersion]
    );
    if (changed === 0) return null; // already complete — absorbing
    return this.get(key);
  }

  /**
   * Record a successful commit. `rows = 0` is a SUCCESSFUL EMPTY RESULT and is stored as
   * `complete`, not as a failure — the count is what tells the two apart.
   *
   * Accepts `pending`/`running`/`failed` as the prior state so a commit whose status write
   * was lost can be reconciled by key on the next attempt.
   */
  async complete(
    key: MaterializationKey,
    result: { rows: number; bytes: number; snapshotId: number | null; tbl: string }
  ): Promise<void> {
    await this.init();
    const now = Date.now();
    await this.driver.run(
      `UPDATE ${this.table}
          SET state = 'complete', row_count = ?, byte_count = ?, snapshot_id = ?, tbl = ?,
              error = NULL, updated_at = ?
        WHERE run_id = ? AND actor = ? AND version = ? AND node = ? AND schema_version = ?
          AND state IN ('pending', 'running', 'failed')`,
      [
        result.rows,
        result.bytes,
        result.snapshotId,
        result.tbl,
        now,
        key.runId,
        key.actor,
        key.version,
        key.node,
        key.schemaVersion,
      ]
    );
  }

  /**
   * Record an exhausted materialization. Only called once Temporal has stopped retrying —
   * an in-flight failure leaves the record `running`, because a retry is still coming.
   *
   * `complete` is absorbing, so a late failure report cannot un-commit a landed write.
   */
  async fail(key: MaterializationKey, err: unknown): Promise<void> {
    await this.init();
    const now = Date.now();
    await this.driver.run(
      `UPDATE ${this.table}
          SET state = 'failed', error = ?, updated_at = ?
        WHERE run_id = ? AND actor = ? AND version = ? AND node = ? AND schema_version = ?
          AND state IN ('pending', 'running', 'failed')`,
      [boundedError(err), now, key.runId, key.actor, key.version, key.node, key.schemaVersion]
    );
  }

  async get(key: MaterializationKey): Promise<MaterializationRecord | null> {
    await this.init();
    const rows = await this.driver.all(
      `SELECT ${COLS} FROM ${this.table}
        WHERE run_id = ? AND actor = ? AND version = ? AND node = ? AND schema_version = ?`,
      [key.runId, key.actor, key.version, key.node, key.schemaVersion]
    );
    return rows.length > 0 ? rowToRecord(rows[0]!) : null;
  }

  /** Every materialization record for one run, in node order. */
  async listForRun(runId: string): Promise<MaterializationRecord[]> {
    await this.init();
    const rows = await this.driver.all(
      `SELECT ${COLS} FROM ${this.table} WHERE run_id = ? ORDER BY node, actor, version`,
      [runId]
    );
    return rows.map(rowToRecord);
  }

  /**
   * Records stuck in a non-terminal state past `olderThanMs` — the materializer's own
   * dead-letter view. A run whose worker died between `claim` and `complete` sits here;
   * without this query it would sit in `finalizing` forever with nothing naming it.
   */
  async listStale(olderThanMs: number, limit = 200): Promise<MaterializationRecord[]> {
    await this.init();
    const rows = await this.driver.all(
      `SELECT ${COLS} FROM ${this.table}
        WHERE state IN ('pending', 'running') AND updated_at < ?
        ORDER BY updated_at LIMIT ?`,
      [Date.now() - olderThanMs, limit]
    );
    return rows.map(rowToRecord);
  }

  /** Dispatches matching an actor selector, newest first. See {@link DispatchRef}. */
  async listDispatches(
    sel: { actor?: string; version?: string; dt?: string; limit?: number } = {}
  ): Promise<DispatchRef[]> {
    await this.init();
    const where: string[] = [];
    const params: unknown[] = [];
    if (sel.actor) {
      where.push('actor = ?');
      params.push(sel.actor);
    }
    if (sel.version) {
      where.push('version = ?');
      params.push(sel.version);
    }
    const rows = await this.driver.all(
      `SELECT run_id, actor, version, min(run_started_at) AS started, count(*) AS nodes,
              sum(CASE WHEN state = 'complete' THEN row_count ELSE 0 END) AS rows_total,
              max(CASE WHEN state = 'failed' THEN 3
                       WHEN state IN ('pending','running') THEN 2 ELSE 1 END) AS worst
         FROM ${this.table}
        ${where.length ? `WHERE ${where.join(' AND ')}` : ''}
        GROUP BY run_id, actor, version
        ORDER BY started DESC`,
      params
    );
    let out: DispatchRef[] = rows.map((r) => ({
      actor: String(r.actor),
      version: String(r.version),
      runId: String(r.run_id),
      runStartedAt: num(r.started),
      dt: dtOf(num(r.started)),
      nodes: num(r.nodes),
      rows: num(r.rows_total),
      state: (num(r.worst) === 3 ? 'failed' : num(r.worst) === 2 ? 'running' : 'complete') as MaterializationState,
    }));
    // `--dt` is a PREFIX match: `2026-08-03` selects a day, `2026-08-03T19` an hour,
    // `2026-08-03T19-42-07` one dispatch.
    const dt = sel.dt;
    if (dt) out = out.filter((r) => r.dt.startsWith(dt));
    return typeof sel.limit === 'number' ? out.slice(0, Math.max(1, sel.limit)) : out;
  }

  /**
   * Drop every materialization record for a run. The status ledger's arm of retention:
   * when a run's data files are expired (stage 6 maintenance) its status rows must go
   * too, or the table grows forever and `listStale` fills with records whose files no
   * longer exist. Returns the number of rows removed.
   */
  async purgeRun(runId: string): Promise<number> {
    await this.init();
    return this.driver.run(`DELETE FROM ${this.table} WHERE run_id = ?`, [runId]);
  }

  /** Fleet-wide health, for the summary tables and the materialization dashboard. */
  async health(): Promise<Record<MaterializationState, number>> {
    await this.init();
    const rows = (await this.driver.all(
      `SELECT state, count(*) AS row_count FROM ${this.table} GROUP BY state`,
      []
    )) as unknown as Array<{ state: string; row_count: number | string }>;
    const out: Record<MaterializationState, number> = {
      pending: 0,
      running: 0,
      complete: 0,
      failed: 0,
    };
    for (const r of rows) out[r.state as MaterializationState] = num(r.row_count);
    return out;
  }

  async close(): Promise<void> {
    await this.driver.close();
  }
}

/**
 * The process-wide store, built on first use. Returns `null` only if construction throws
 * (a malformed URL); a database that is merely unreachable surfaces per-call, so the API
 * boots with no Postgres exactly as it boots with no Temporal.
 */
let shared: MaterializationStore | null = null;

export function materializationStore(opts: MaterializationStoreOptions = {}): MaterializationStore {
  if (shared === null) shared = new MaterializationStore(opts);
  return shared;
}

/** Test seam: drop the process-wide store so the next call builds a fresh one. */
export function resetMaterializationStore(): void {
  shared = null;
}

export { MATERIALIZATION_SCHEMA_VERSION, isPostgresUrl, resolveStoreUrl, toPositional };

/**
 * Resolve `actor[@version]` (+ an optional `dt` prefix) to dispatches, newest first.
 *
 * THE ADDRESSING SURFACE, and it reads the LEDGER rather than the lake: the ledger already
 * knows which actor and version ran and exactly when it was dispatched, which is precisely
 * what an operator names. It also knows about dispatches whose output is still being
 * written, so a run in progress is addressable before it finishes.
 */
export interface DispatchRef {
  actor: string;
  version: string;
  runId: string;
  runStartedAt: number;
  /** `YYYY-MM-DDTHH-MM-SS` — matches the `dt=` directory the output lives under. */
  dt: string;
  nodes: number;
  rows: number;
  /** Worst node state: `failed` > non-terminal > `complete`. */
  state: MaterializationState;
}

function dtOf(ms: number): string {
  const v = Number.isFinite(ms) && ms > 0 ? ms : 0;
  return new Date(v).toISOString().slice(0, 19).replace(/:/g, '-');
}
