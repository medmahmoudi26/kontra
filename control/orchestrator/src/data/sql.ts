/**
 * The two-backend SQL driver the application-owned tables share.
 *
 * The orchestrator owns three durable tables outside DuckLake — materialization status and
 * the operational summaries — and they must work in two very different deployments:
 *
 *   - **Postgres**, when the materializer runs OFF the controller (the production shape).
 *     The status writer and the API reader are then different hosts, and only a shared
 *     database can carry that.
 *   - **SQLite**, for a single-host install and the test suite, where standing up a
 *     Postgres to record four counters would be absurd.
 *
 * One schema, one set of statements, one set of assertions. Statements are written with
 * `?` placeholders and the Postgres driver rewrites them to `$1…$n`, so no caller ever
 * holds two dialects of the same query. Every construct used here (`ON CONFLICT … DO
 * UPDATE`, upsert `WHERE` clauses, `BIGINT`) exists in both engines with the same meaning.
 */

import { createRequire } from 'node:module';
import type { DatabaseSync } from 'node:sqlite';

// `node:sqlite` via runtime require — the Vite bundler vitest runs under does not list it
// as a builtin and tries (and fails) to bundle a static import. Same indirection as db/repo.ts.
const { DatabaseSync: DatabaseSyncCtor } = createRequire(__filename)(
  'node:sqlite'
) as typeof import('node:sqlite');

/** A row as either engine returns it: column names, values loosely typed. */
export type SqlRow = Record<string, unknown>;

/** The minimum SQL surface both engines provide. */
/** The SQLSTATEs a concurrent `CREATE … IF NOT EXISTS` loser gets instead of a no-op: a unique
 *  violation on a catalog index, or the plain duplicate_schema/table/object codes. */
const CREATION_RACE_CODES = new Set(['23505', '42P06', '42P07', '42710']);

/** Whether `err` is a lost creation race on an idempotent DDL statement. Exported for its test. */
export function isCreationRace(sql: string, err: unknown): boolean {
  const code = (err as { code?: unknown } | null)?.code;
  return typeof code === 'string' && CREATION_RACE_CODES.has(code) && /\bIF\s+NOT\s+EXISTS\b/i.test(sql);
}

export interface SqlDriver {
  exec(sql: string): Promise<void>;
  /** Rows changed. */
  run(sql: string, params: unknown[]): Promise<number>;
  all(sql: string, params: unknown[]): Promise<SqlRow[]>;
  close(): Promise<void>;
}

/** `?` → `$1, $2, …`. Every `?` in our SQL is a bind, never a literal, so this is total. */
export function toPositional(sql: string): string {
  let i = 0;
  return sql.replace(/\?/g, () => `$${(i += 1)}`);
}

/**
 * Postgres returns BIGINT as a STRING — it does not fit a JS number in general. Silently
 * treating that string as a number is how a byte counter becomes `"12" + 1 === "121"`.
 */
export function num(v: unknown): number {
  if (v === null || v === undefined) return 0;
  const n = Number(v);
  return Number.isFinite(n) ? n : 0;
}

export function str(v: unknown): string | null {
  return v === null || v === undefined ? null : String(v);
}

class SqliteDriver implements SqlDriver {
  constructor(private readonly db: DatabaseSync) {}

  async exec(sql: string): Promise<void> {
    this.db.exec(sql);
  }

  async run(sql: string, params: unknown[]): Promise<number> {
    return Number(this.db.prepare(sql).run(...(params as never[])).changes);
  }

  async all(sql: string, params: unknown[]): Promise<SqlRow[]> {
    return this.db.prepare(sql).all(...(params as never[])) as unknown as SqlRow[];
  }

  async close(): Promise<void> {
    this.db.close();
  }
}

/** Minimal shape of the `pg` Pool we use — keeps `pg` a runtime-only dependency. */
export interface PgPoolLike {
  query(text: string, values?: unknown[]): Promise<{ rows: unknown[]; rowCount: number | null }>;
  end(): Promise<void>;
}

class PostgresDriver implements SqlDriver {
  constructor(private readonly pool: PgPoolLike) {}

  async exec(sql: string): Promise<void> {
    try {
      await this.pool.query(sql);
    } catch (err) {
      // TWO CREATORS, ONE WINNER, AND THE LOSER IS NOT TOLD "ALREADY EXISTS". `CREATE … IF NOT EXISTS`
      // checks the catalog and then inserts into it, and two sessions doing that at once both pass
      // the check: one inserts, the other hits the catalog's unique index and gets a 23505 on
      // `pg_namespace_nspname_index` (or `pg_type_typname_nsp_index` for a table) instead of a
      // no-op. Five stores create the `kontra` schema at boot, in two processes, so this was a race
      // a fresh install lost at random: `[orchestrator] fatal: duplicate key value violates unique
      // constraint "pg_namespace_nspname_index"`, the API never healthy, the install never up.
      //
      // ONE RETRY, NOT A SWALLOW. By the time the loser sees the error the winner has committed, so
      // the same statement now finds the object and does nothing. If it fails again, that is a real
      // error and it propagates. Only for IF NOT EXISTS DDL, where "it exists" is the success case.
      if (!isCreationRace(sql, err)) throw err;
      await this.pool.query(sql);
    }
  }

  async run(sql: string, params: unknown[]): Promise<number> {
    const res = await this.pool.query(toPositional(sql), params);
    return res.rowCount ?? 0;
  }

  async all(sql: string, params: unknown[]): Promise<SqlRow[]> {
    const res = await this.pool.query(toPositional(sql), params);
    return res.rows as SqlRow[];
  }

  async close(): Promise<void> {
    await this.pool.end();
  }
}

export function isPostgresUrl(url: string): boolean {
  return url.startsWith('postgres://') || url.startsWith('postgresql://');
}

/**
 * Where the application-owned tables live, resolved from options + env.
 *
 * Falls back to `KONTRA_ORCHESTRATOR_DB` so a single-host install needs NO new
 * configuration at all — the tables simply appear beside the run records.
 */
export function resolveStoreUrl(url?: string): string {
  return (
    url ??
    process.env.KONTRA_MATERIALIZATION_DB ??
    process.env.KONTRA_ORCHESTRATOR_DB ??
    'orchestrator.db'
  );
}

/**
 * Application-owned schema name on Postgres. Its own schema, never `public`: the DuckLake
 * catalog shares this database, and an unqualified table would sit among the `ducklake_*`
 * tables where catalog maintenance would happily drop it.
 */
export const APP_SCHEMA = 'kontra';

export interface DriverOptions {
  url?: string;
  /** Inject an already-open SQLite handle. */
  sqlite?: DatabaseSync;
  /** Inject a `pg`-compatible pool (tests, or a caller that owns pooling). */
  pool?: PgPoolLike;
  /**
   * Postgres schema to place the tables in. Defaults to {@link APP_SCHEMA}.
   *
   * Exists so a test run against a REAL Postgres lands in its own schema. Without it the
   * suite writes into the schema production reads — which is not hypothetical: a summary
   * test asserting `pending = 9` put exactly that number on the live materialization-health
   * dashboard until it was noticed.
   */
  schema?: string;
}

export interface ResolvedDriver {
  driver: SqlDriver;
  backend: 'postgres' | 'sqlite';
  /** Qualify a table name for the backend (`kontra.x` on Postgres, `x` on SQLite). */
  table(name: string): string;
  /** The schema to create, or null when the backend has none. */
  schema: string | null;
}

export function createDriver(opts: DriverOptions = {}): ResolvedDriver {
  const url = resolveStoreUrl(opts.url);
  const backend = isPostgresUrl(url) || opts.pool !== undefined ? 'postgres' : 'sqlite';
  if (backend === 'postgres') {
    const schema = opts.schema ?? APP_SCHEMA;
    return {
      driver: new PostgresDriver(opts.pool ?? createPool(url)),
      backend,
      table: (name) => `${schema}.${name}`,
      schema,
    };
  }
  return {
    driver: new SqliteDriver(opts.sqlite ?? new DatabaseSyncCtor(url)),
    backend,
    table: (name) => name,
    schema: null,
  };
}

/** Build a `pg` Pool. Required lazily so a SQLite-only install never loads the driver. */
function createPool(url: string): PgPoolLike {
  const { Pool } = createRequire(__filename)('pg') as typeof import('pg');
  // Small on purpose: these are status ledgers on the hot path of a constrained worker,
  // not an analytics database.
  return new Pool({ connectionString: url, max: 2 }) as unknown as PgPoolLike;
}

/**
 * Memoize an async initializer, CLEARING THE CACHE ON FAILURE.
 *
 * A memoized rejected promise is a live bug this codebase has already paid for: the
 * dataset connection cache stored one, so a single transient catalog error disabled writes
 * for the rest of the process's life while runs kept reporting success. Every lazy
 * initializer here goes through this helper so that cannot recur.
 */
export function memoizeInit(fn: () => Promise<void>): () => Promise<void> {
  let ready: Promise<void> | null = null;
  return () => {
    if (ready) return ready;
    const p = fn().catch((err) => {
      ready = null;
      throw err;
    });
    ready = p;
    return p;
  };
}

/**
 * The most bind parameters ONE statement may carry.
 *
 * BOTH ENGINES CAP IT, AT DIFFERENT NUMBERS, AND NEITHER CAP IS A CONFIGURATION THIS PROCESS SEES.
 * `node:sqlite` on Node 22 is compiled with `SQLITE_MAX_VARIABLE_NUMBER = 32766` and raises `too
 * many SQL variables` at 32,767 — measured, not read off a doc page. Postgres's wire protocol
 * numbers a Bind message's parameters in an int16, so 65,535 is a protocol limit and `bind message
 * has too many parameters` is what it answers past it. A store picks its backend from a URL at
 * runtime, so a page size that is safe on one and not the other is a bug that only appears in the
 * deployment that matters.
 *
 * 900 IS UNDER EVERY LIMIT INCLUDING THE HISTORICAL ONE — SQLite before 3.32 shipped a default of
 * 999, and an `IN (…)` sized just under a current limit is a statement that breaks when the engine
 * underneath it is older than the box it was measured on. The cost is round trips over an indexed
 * primary key, which is the trade this makes on purpose: a listing that is slower at 40k Runs is a
 * listing; one that throws is a Dataset silently missing its name and its tags.
 */
export const BIND_CHUNK = 900;

/**
 * Split a bind list into statement-sized pages. Total: an empty input yields no pages, so a caller
 * loops zero times rather than issuing `IN ()`.
 *
 * Callers must MERGE the pages' rows, never assume one round trip — see `RunWorkflowStore.list` and
 * `DatasetRecordStore.list`, where a result assembled from the last page alone would lose every Run
 * whose id sorted into an earlier one.
 */
export function chunkBinds<T>(values: readonly T[], size: number = BIND_CHUNK): T[][] {
  const pages: T[][] = [];
  for (let i = 0; i < values.length; i += size) pages.push(values.slice(i, i + size));
  return pages;
}
