/**
 * Operational SUMMARIES — the bounded, scalar aggregates Grafana reads (plan §4).
 *
 * The plane split this enforces:
 *
 *   Grafana answers "IS IT MOVING?"  — rates, counts, freshness, failures.
 *   DuckDB  answers "WHAT DID IT FIND?" — the detailed and nested actor output.
 *
 * Grafana must not become the findings explorer. Its data-frame model has no struct type,
 * so nested output either gets flattened (losing shape) or serialized to a string (losing
 * queryability) — and pointing a dashboard at the findings files means every panel refresh
 * scans object storage. These tables exist so the dashboards can be complete AND cheap: a
 * panel reads four scalar columns from Postgres instead of opening a Parquet file.
 *
 * Refreshed after a DuckLake commit, from the materialization status ledger, which is the
 * only thing that knows both what was attempted and what landed.
 *
 * CARDINALITY IS THE DESIGN CONSTRAINT. `run_id` appears here as a COLUMN — a row key in a
 * relational table, which is fine and bounded by retention. It must never become a METRIC
 * LABEL, where each distinct value is a new time series and a run's worth of runs
 * quietly becomes an unbounded series count. Exact identifiers belong in traces and logs;
 * see `metricSafeLabels` below for the guard.
 */

import {
  summarize,
  type MaterializationRecord,
  type MaterializationState,
} from './materialization';
import { createDriver, memoizeInit, num, type DriverOptions, type SqlDriver } from './sql';

/**
 * The four summary tables, in portable SQL.
 *
 * `run_summary`        — one row per run: both status dimensions plus totals.
 * `node_summary`       — one row per materialized node: rows, bytes, attempts, error.
 * `output_type_hourly` — output volume bucketed by actor and hour, for rate panels.
 * `materialization_health` — a single fleet-wide row: the "is decoding keeping up?" panel.
 */
function ddl(t: (name: string) => string): string[] {
  return [
    `CREATE TABLE IF NOT EXISTS ${t('run_summary')} (
       run_id           TEXT PRIMARY KEY,
       execution        TEXT NOT NULL,
       lifecycle        TEXT NOT NULL,
       nodes_total      INTEGER NOT NULL DEFAULT 0,
       nodes_complete   INTEGER NOT NULL DEFAULT 0,
       nodes_failed     INTEGER NOT NULL DEFAULT 0,
       nodes_pending    INTEGER NOT NULL DEFAULT 0,
       rows_total       BIGINT  NOT NULL DEFAULT 0,
       bytes_total      BIGINT  NOT NULL DEFAULT 0,
       run_started_at   BIGINT  NOT NULL DEFAULT 0,
       updated_at       BIGINT  NOT NULL DEFAULT 0
     )`,
    `CREATE TABLE IF NOT EXISTS ${t('node_summary')} (
       run_id         TEXT    NOT NULL,
       actor          TEXT    NOT NULL,
       version        TEXT    NOT NULL,
       node           TEXT    NOT NULL,
       schema_version INTEGER NOT NULL,
       state          TEXT    NOT NULL,
       attempt        INTEGER NOT NULL DEFAULT 0,
       row_count      BIGINT  NOT NULL DEFAULT 0,
       byte_count     BIGINT  NOT NULL DEFAULT 0,
       error          TEXT,
       updated_at     BIGINT  NOT NULL DEFAULT 0,
       PRIMARY KEY (run_id, actor, version, node, schema_version)
     )`,
    `CREATE TABLE IF NOT EXISTS ${t('output_type_hourly')} (
       hour_start BIGINT  NOT NULL,
       actor      TEXT    NOT NULL,
       version    TEXT    NOT NULL,
       rows_total BIGINT  NOT NULL DEFAULT 0,
       runs_total INTEGER NOT NULL DEFAULT 0,
       PRIMARY KEY (hour_start, actor, version)
     )`,
    `CREATE TABLE IF NOT EXISTS ${t('materialization_health')} (
       id            INTEGER PRIMARY KEY,
       pending       INTEGER NOT NULL DEFAULT 0,
       running       INTEGER NOT NULL DEFAULT 0,
       complete      INTEGER NOT NULL DEFAULT 0,
       failed        INTEGER NOT NULL DEFAULT 0,
       stale         INTEGER NOT NULL DEFAULT 0,
       refreshed_at  BIGINT  NOT NULL DEFAULT 0
     )`,
    `CREATE INDEX IF NOT EXISTS idx_node_summary_state ON ${t('node_summary')} (state, updated_at)`,
    `CREATE INDEX IF NOT EXISTS idx_run_summary_lifecycle ON ${t('run_summary')} (lifecycle, updated_at)`,
  ];
}

const HOUR_MS = 3_600_000;

/** Truncate an epoch-ms instant to its hour bucket — the grain the rate panels chart. */
export function hourStart(ms: number): number {
  return Math.floor(ms / HOUR_MS) * HOUR_MS;
}

/**
 * Label keys a metric may legitimately carry. Everything else is high-cardinality by
 * nature: `run_id` is one series per run, a URL is one per target, an actor instance id
 * is one per process. This is not a style preference — it is the difference between a
 * bounded time-series database and one that falls over mid-run.
 */
export const ALLOWED_METRIC_LABELS: readonly string[] = ['actor', 'version', 'node', 'state', 'queue'];

/**
 * Drop every label that is not on the allowlist, returning the safe subset. Callers use
 * this before exporting a metric so a new dimension cannot be added by accident; the
 * dropped identifiers belong in traces and logs, which are sampled and searchable.
 */
export function metricSafeLabels(labels: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(labels)) {
    if (ALLOWED_METRIC_LABELS.includes(k)) out[k] = v;
  }
  return out;
}

export interface SummaryRefreshInput {
  runId: string;
  execution: string;
  lifecycle: string;
  records: readonly MaterializationRecord[];
}

/** How long a non-terminal record may sit untouched before it counts as stale. */
export const DEFAULT_STALE_MS = 30 * 60 * 1000;

export class SummaryStore {
  private readonly driver: SqlDriver;
  readonly backend: 'postgres' | 'sqlite';
  private readonly t: (name: string) => string;
  private readonly init: () => Promise<void>;

  constructor(opts: DriverOptions = {}) {
    const resolved = createDriver(opts);
    this.driver = resolved.driver;
    this.backend = resolved.backend;
    this.t = resolved.table;
    this.init = memoizeInit(async () => {
      if (resolved.schema) await this.driver.exec(`CREATE SCHEMA IF NOT EXISTS ${resolved.schema}`);
      for (const stmt of ddl(this.t)) await this.driver.exec(stmt);
    });
  }

  async ensureSchema(): Promise<void> {
    await this.init();
  }

  /**
   * Recompute one run's summary rows from its materialization records.
   *
   * Idempotent by construction — every write is an upsert keyed on identity, so a refresh
   * that runs twice produces the same rows. That matters because this is called after a
   * commit whose activity may itself be retried; a summary that double-counted on retry
   * would make the dashboards drift away from the ledger they are supposed to summarize.
   */
  async refreshRun(input: SummaryRefreshInput): Promise<void> {
    await this.init();
    const now = Date.now();
    const s = summarize(input.records);
    const runStartedAt = input.records.length > 0 ? Math.min(...input.records.map((r) => r.runStartedAt)) : 0;

    await this.driver.run(
      `INSERT INTO ${this.t('run_summary')}
         (run_id, execution, lifecycle, nodes_total, nodes_complete, nodes_failed, nodes_pending,
          rows_total, bytes_total, run_started_at, updated_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT (run_id) DO UPDATE SET
         execution = excluded.execution, lifecycle = excluded.lifecycle,
         nodes_total = excluded.nodes_total, nodes_complete = excluded.nodes_complete,
         nodes_failed = excluded.nodes_failed, nodes_pending = excluded.nodes_pending,
         rows_total = excluded.rows_total, bytes_total = excluded.bytes_total,
         run_started_at = excluded.run_started_at, updated_at = excluded.updated_at`,
      [
        input.runId,
        input.execution,
        input.lifecycle,
        s.total,
        s.complete,
        s.failed,
        s.pending + s.running,
        s.rows,
        s.bytes,
        runStartedAt,
        now,
      ]
    );

    for (const r of input.records) {
      await this.driver.run(
        `INSERT INTO ${this.t('node_summary')}
           (run_id, actor, version, node, schema_version, state, attempt, row_count, byte_count,
            error, updated_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT (run_id, actor, version, node, schema_version) DO UPDATE SET
           state = excluded.state, attempt = excluded.attempt, row_count = excluded.row_count,
           byte_count = excluded.byte_count, error = excluded.error, updated_at = excluded.updated_at`,
        [
          r.runId,
          r.actor,
          r.version,
          r.node,
          r.schemaVersion,
          r.state,
          r.attempt,
          r.rows,
          r.bytes,
          r.error,
          now,
        ]
      );
    }

    // Hourly output volume, bucketed by the run's SERVER-MINTED start rather than by now:
    // a run that materializes an hour after it executed must land in the hour it ran, or
    // the rate panel shows a spike that never happened.
    const byActor = new Map<string, { rows: number; hour: number; actor: string; version: string }>();
    for (const r of input.records) {
      if (r.state !== 'complete') continue;
      const hour = hourStart(r.runStartedAt || now);
      const key = `${hour} ${r.actor} ${r.version}`;
      const cur = byActor.get(key) ?? { rows: 0, hour, actor: r.actor, version: r.version };
      cur.rows += r.rows;
      byActor.set(key, cur);
    }
    for (const b of byActor.values()) {
      await this.driver.run(
        `INSERT INTO ${this.t('output_type_hourly')} (hour_start, actor, version, rows_total, runs_total)
         VALUES (?, ?, ?, ?, 1)
         ON CONFLICT (hour_start, actor, version) DO UPDATE SET
           rows_total = excluded.rows_total, runs_total = ${this.t('output_type_hourly')}.runs_total`,
        [b.hour, b.actor, b.version, b.rows]
      );
    }
  }

  /**
   * Refresh the single fleet-wide health row.
   *
   * `stale` is the number the operator actually needs: records sitting non-terminal past
   * the threshold, i.e. materialization that nothing is going to finish. Without it, a
   * worker that died mid-decode leaves runs reading `finalizing` forever and no panel
   * names the cause.
   */
  async refreshHealth(
    counts: Record<MaterializationState, number>,
    stale: number
  ): Promise<void> {
    await this.init();
    await this.driver.run(
      `INSERT INTO ${this.t('materialization_health')}
         (id, pending, running, complete, failed, stale, refreshed_at)
       VALUES (1, ?, ?, ?, ?, ?, ?)
       ON CONFLICT (id) DO UPDATE SET
         pending = excluded.pending, running = excluded.running, complete = excluded.complete,
         failed = excluded.failed, stale = excluded.stale, refreshed_at = excluded.refreshed_at`,
      [counts.pending, counts.running, counts.complete, counts.failed, stale, Date.now()]
    );
  }

  /** One run's summary row, or null. */
  async getRun(runId: string): Promise<Record<string, number | string | null> | null> {
    await this.init();
    const rows = await this.driver.all(
      `SELECT * FROM ${this.t('run_summary')} WHERE run_id = ?`,
      [runId]
    );
    return (rows[0] as Record<string, number | string | null>) ?? null;
  }

  async getHealth(): Promise<Record<MaterializationState | 'stale' | 'refreshedAt', number>> {
    await this.init();
    const rows = await this.driver.all(`SELECT * FROM ${this.t('materialization_health')} WHERE id = 1`, []);
    const r = rows[0] ?? {};
    return {
      pending: num(r.pending),
      running: num(r.running),
      complete: num(r.complete),
      failed: num(r.failed),
      stale: num(r.stale),
      refreshedAt: num(r.refreshed_at),
    };
  }

  async hourly(sinceMs: number): Promise<Array<{ hour: number; actor: string; version: string; rows: number }>> {
    await this.init();
    const rows = await this.driver.all(
      `SELECT hour_start, actor, version, rows_total FROM ${this.t('output_type_hourly')}
        WHERE hour_start >= ? ORDER BY hour_start, actor, version`,
      [hourStart(sinceMs)]
    );
    return rows.map((r) => ({
      hour: num(r.hour_start),
      actor: String(r.actor),
      version: String(r.version),
      rows: num(r.rows_total),
    }));
  }

  /** Retention's arm here: drop a purged run's summary rows alongside its status rows. */
  async purgeRun(runId: string): Promise<number> {
    await this.init();
    const a = await this.driver.run(`DELETE FROM ${this.t('run_summary')} WHERE run_id = ?`, [runId]);
    await this.driver.run(`DELETE FROM ${this.t('node_summary')} WHERE run_id = ?`, [runId]);
    return a;
  }

  async close(): Promise<void> {
    await this.driver.close();
  }
}

/** The process-wide summary store, built on first use. */
let shared: SummaryStore | null = null;

export function summaryStore(opts: DriverOptions = {}): SummaryStore {
  if (shared === null) shared = new SummaryStore(opts);
  return shared;
}

/** Test seam: drop the process-wide store so the next call builds a fresh one. */
export function resetSummaryStore(): void {
  shared = null;
}
