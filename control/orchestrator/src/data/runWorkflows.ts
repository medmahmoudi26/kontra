/**
 * The **Run**'s own workflow identity — the caller workflow's manifest `name` and `version`,
 * SNAPSHOTTED at start and keyed by `runId`.
 *
 * WHY THIS EXISTS. ADR 0029 §2 says a Dataset's derived name renders the CALLER WORKFLOW's manifest
 * identity, and its Consequences say the name "labels the run's output, which may span several actor
 * tables" — one Run, one name. The Datasets seam cannot honour either sentence from the ledger alone:
 * a materialization record carries the ACTOR's name and version (`data/materialization.ts`), Temporal
 * holds the workflow type for 24h and then forgets it, and the run id is `<type>-<unixseconds>` —
 * a lowercased class name with no version and an `--id` override that can replace it entirely. So the
 * only reading of §2 that was implementable without this store rendered the ACTOR's identity, which
 * named `wf-bar-…` for a workflow `foo` dispatching an actor `bar`, and gave a single Run writing two
 * actor tables TWO different names. This is the fix, and it is ADR 0025's pattern verbatim: what must
 * outlive Temporal's retention is snapshotted into a durable record at the moment it is known.
 *
 * THIS IS PROVENANCE, NOT A NAME REGISTRY. The deleted `_kontra_dataset` table (see the header of
 * `data/datasets.ts`) mapped a NAME to a PATH — its only purpose was translating a hash back into a
 * name, and nothing may walk that back. Nothing here is keyed by a name, nothing here is consulted to
 * FIND a Dataset, and no name is stored: a row says "Run `nscheck-1755612727` was workflow `nscheck`
 * at `0.1.0`", and the name stays a rendering of that fact plus the run start and the run id. Delete
 * every row and every Dataset still lists, still resolves, and still renders a name — the fallback
 * below — which is the property a lookup table does not have.
 *
 * BOTH START PATHS WRITE IT, or the name would depend on which one an operator used:
 * `workflowControl.startRun()` behind `POST /api/runs`, and `cli/workflow.go` through
 * `PUT /api/runs/:runId/workflow` (the CLI has no database credentials — the orchestrator owns this
 * store, exactly as it owns the Dataset record the CLI mutates over HTTP).
 *
 * BEST-EFFORT ON BOTH SIDES. A Run whose identity was never recorded — every Dataset that existed
 * before this store did, a run started by a peer that predates it, a stamp that lost a race with a
 * restart — renders its name from the PRODUCING ACTOR's identity instead (`withDatasetNames` in
 * `data/datasets.ts`). That fallback is the compatibility contract, not an accident: a Dataset must
 * always say something it is called.
 *
 * TWO BACKENDS, ONE SCHEMA — the same split `data/datasetRecords.ts` and the materialization store
 * make: Postgres when the writer and reader are different hosts, SQLite for a single-host install and
 * the test suite. Same SQL, same assertions.
 */

import {
  chunkBinds,
  createDriver,
  memoizeInit,
  num,
  type DriverOptions,
  type SqlDriver,
} from './sql';

/**
 * One **Run**'s caller-workflow identity, as the name renderer wants it.
 *
 * Both fields are the MANIFEST's (`workflow.json` `name` / `version`), never the task-queue string
 * they are spelled into — ADR 0029 §2's first constraint. `--queue` cannot be overridden any more,
 * but the rule survives the flag: the queue is transport, the manifest is identity.
 */
export interface RunWorkflow {
  /** The **Run** — the caller workflow's id, which is what a Run IS (ADR 0023 §12). */
  runId: string;
  /** The caller workflow's manifest `name` (`nscheck`), NOT its `@workflow.defn` class. */
  workflow: string;
  /** The caller workflow's manifest `version` (`0.1.0`). */
  version: string;
}

/** The longest a recorded manifest name or version may be. A manifest field is an identifier, not a
 *  document; the bound stops a malformed `workflow.json` from becoming a column-wide row. */
export const RUN_WORKFLOW_MAX_LENGTH = 128;

/**
 * An identity this store refuses to snapshot — a blank or over-long `name`/`version`.
 *
 * Typed rather than string-matched so the route maps it to a 400 without parsing a message, the same
 * discipline {@link import('./datasetRecords').InvalidDeviationError} follows. A HALF identity is
 * worse than none: it would render `wf--0.1.0--…` or `wf-nscheck---…` where the fallback would have
 * rendered a perfectly good actor-grain name.
 */
export class InvalidRunWorkflowError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'InvalidRunWorkflowError';
  }
}

/** Trim and bound one manifest field. Returns the canonical form; throws on empty or over-long. */
function field(label: string, raw: unknown): string {
  const value = String(raw ?? '').trim();
  if (value === '') throw new InvalidRunWorkflowError(`a run's workflow ${label} cannot be empty`);
  if (value.length > RUN_WORKFLOW_MAX_LENGTH) {
    throw new InvalidRunWorkflowError(
      `a run's workflow ${label} is at most ${RUN_WORKFLOW_MAX_LENGTH} characters (got ${value.length})`
    );
  }
  return value;
}

/**
 * One table, in portable SQL.
 *
 * `run_workflow` — one row per **Run**, holding the identity its caller was started from. No index
 * beyond the primary key: every read is `run_id`-keyed (`get`) or an `IN (…)` over a page of runs the
 * listing already resolved (`list`), both of which the primary key serves.
 */
function ddl(t: (name: string) => string): string[] {
  return [
    `CREATE TABLE IF NOT EXISTS ${t('run_workflow')} (
       run_id      TEXT   PRIMARY KEY,
       workflow    TEXT   NOT NULL,
       version     TEXT   NOT NULL,
       recorded_at BIGINT NOT NULL DEFAULT 0
     )`,
  ];
}

export class RunWorkflowStore {
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
   * Snapshot a **Run**'s caller-workflow identity. Idempotent by the primary key — a re-stamp of the
   * same Run overwrites with the same values, so a start path that records twice (a retried HTTP
   * stamp, say) costs one row and no error.
   *
   * UPSERT, NOT INSERT-OR-IGNORE: Temporal refuses a duplicate workflow id while the first execution
   * is open, so a second stamp for one id is a re-statement of the same fact rather than a competing
   * one — and if the id ever is reused after a run closed, the LATEST start is the run whose Datasets
   * are being named.
   */
  async record(runId: string, workflow: string, version: string): Promise<void> {
    await this.init();
    const run = field('run id', runId);
    const name = field('name', workflow);
    const ver = field('version', version);
    await this.driver.run(
      `INSERT INTO ${this.t('run_workflow')} (run_id, workflow, version, recorded_at) VALUES (?, ?, ?, ?)
       ON CONFLICT (run_id) DO UPDATE SET workflow = excluded.workflow,
                                          version = excluded.version,
                                          recorded_at = excluded.recorded_at`,
      [run, name, ver, Date.now()]
    );
  }

  /** One **Run**'s identity, or undefined when none was ever recorded — the fallback case. */
  async get(runId: string): Promise<RunWorkflow | undefined> {
    await this.init();
    const rows = await this.driver.all(
      `SELECT run_id, workflow, version FROM ${this.t('run_workflow')} WHERE run_id = ?`,
      [runId]
    );
    return rows[0] ? rowTo(rows[0]) : undefined;
  }

  /**
   * Every recorded identity among a page of **Runs** — the read side of the `/api/datasets` name
   * join. A Run with no record is simply ABSENT, so the caller renders its row from the producing
   * Actor instead. An empty input asks for nothing and never opens the store.
   *
   * PAGED, BECAUSE THE ID LIST IS SIZED BY THE LEDGER AND NOT BY A PAGE OF ROWS. `GET /api/datasets`
   * calls `listDispatches` with no limit and fans every resolved `runId` into this call, so a 40k-Run
   * ledger asked for 40,000 bind variables in one statement and SQLite raised `too many SQL
   * variables` at 32,767 (Postgres raises its own past 65,535). The listing route SWALLOWS that
   * throw, so the symptom was not an error: every Dataset silently lost its run-grain name, its
   * rename and its tags, and fell back to the Actor-grain default that looks exactly like a correct
   * answer. Chunking is the fix, and it is what makes the retention sweep — which calls the same
   * `list` and does NOT swallow — stop aborting every tick at the same size.
   *
   * THE PAGES ARE MERGED, IN ORDER, AND SEQUENTIALLY: the Postgres pool is two connections wide
   * (`sql.ts`), so firing 45 statements at once would queue behind itself anyway while holding 45
   * result sets in memory.
   */
  async list(runIds: readonly string[]): Promise<RunWorkflow[]> {
    const ids = [...new Set(runIds)].filter((id) => id !== '');
    if (ids.length === 0) return [];
    await this.init();
    const out: RunWorkflow[] = [];
    for (const page of chunkBinds(ids)) {
      const holes = page.map(() => '?').join(', ');
      const rows = await this.driver.all(
        `SELECT run_id, workflow, version FROM ${this.t('run_workflow')} WHERE run_id IN (${holes})`,
        [...page]
      );
      for (const r of rows) out.push(rowTo(r));
    }
    return out;
  }

  /** Drop a **Run**'s identity — the retention arm, so a collected Run's provenance goes with the
   *  data it described rather than outliving it. Returns the number of rows removed (at most one). */
  async purgeRun(runId: string): Promise<number> {
    await this.init();
    return num(await this.driver.run(`DELETE FROM ${this.t('run_workflow')} WHERE run_id = ?`, [runId]));
  }

  async close(): Promise<void> {
    await this.driver.close();
  }
}

function rowTo(r: Record<string, unknown>): RunWorkflow {
  return { runId: String(r.run_id), workflow: String(r.workflow), version: String(r.version) };
}

/** The process-wide store, built on first use — the same lazy singleton the Dataset record and the
 *  materialization ledger use, so a single-host install needs no new configuration. */
let shared: RunWorkflowStore | null = null;

export function runWorkflowStore(opts: DriverOptions = {}): RunWorkflowStore {
  if (shared === null) shared = new RunWorkflowStore(opts);
  return shared;
}

/** Test seam: drop the process-wide store so the next call builds a fresh one. */
export function resetRunWorkflowStore(): void {
  shared = null;
}
