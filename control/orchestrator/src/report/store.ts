/**
 * WHERE A REPORT RESTS: four tables, keyed by Run, in portable SQL.
 *
 * ── WHY NOT `db/repo.ts`, WHICH THE SPECIFICATION NAMES ────────────────────────────────────────
 *
 * §7.1 says to use the orchestrator's own store. That store's header records a decision this would
 * reverse — it holds NO run records at all — and reversing it would be defensible on its own. What is
 * not defensible is the process boundary.
 *
 *   • `repo.ts` is `node:sqlite` over a FILE. `KONTRA_ORCHESTRATOR_DB` lives on the `orchestrator-db`
 *     volume, which `docker-compose.yml:703` mounts on the **kontra-api** service only.
 *   • A report is WRITTEN where a finished run is noticed and READ by the API. Run deletion is a third
 *     process again: `collectRun` runs inside the retention activity on the dataset queue, which the
 *     `materializer` role polls (`roles.ts:91`).
 *   • Those are the same container today only because compose runs `api,materializer` together
 *     (`docker-compose.yml:671`). Split the roles — which is the entire point of having roles — and a
 *     `repo.ts`-backed report silently finds no file, or creates an empty one at the relative default
 *     `orchestrator.db` and reports nothing wrong.
 *
 * So the tables join the family built for exactly this: `data/sql.ts`, Postgres when the writer and
 * the reader are different hosts, SQLite for a single-host install and the test suite — the same split
 * `runWorkflows`, `summaries`, `datasetRecords` and the materialization ledger already make. §7.1
 * offers this as the escape hatch for blobs that are too large; the real reason is the boundary, and
 * it is recorded here rather than borrowed.
 *
 * ── THE COST OF THAT CHOICE, STATED ────────────────────────────────────────────────────────────
 *
 * §7.1 also says a deleted run's four tables are "cleaned for that `run_id` in the same transaction".
 * **THIS FAMILY HAS NO TRANSACTION PRIMITIVE.** `SqlDriver` is `exec`/`run`/`all`/`close`
 * (`data/sql.ts:32`); the only `tx()` in the orchestrator's TypeScript is `Repo.tx()`, over the handle
 * this store deliberately does not use — and even on a single-host SQLite install the two families are
 * two separate `DatabaseSync` handles over one file, which cannot share a transaction and will take
 * `SQLITE_BUSY` from each other if one holds a long `BEGIN`.
 *
 * {@link ReportStore.purgeRun} therefore deletes in a STATED ORDER rather than atomically, and the
 * order is the mitigation: **the unredacted bytes go first.** A purge that dies halfway has removed
 * the only rows that hold a credential; what survives is a redacted snapshot and some feedback, which
 * the next sweep removes. The reverse order would leave `report_secret` rows reachable by a reveal
 * route whose version row had already gone, which is the one outcome worth engineering against.
 *
 * ── TABLE NAMES ARE SINGULAR, WHICH THE SPEC'S ARE NOT ─────────────────────────────────────────
 *
 * `report_template`, not `report_templates`. The family is `run_workflow`, `run_summary`,
 * `node_summary`, `dataset_record`; matching the four neighbours a reader will see in the same
 * database beats matching the prose that commissioned them.
 */

import { randomUUID } from 'node:crypto';

import { chunkBinds, createDriver, memoizeInit, num, type DriverOptions, type SqlDriver } from '../data/sql';
import { ADDRESS_PREFIX, activeNamespace } from '../workspaces';

/** Where a pinned template came from. `default` is the built-in, and carries the kontra version. */
export type TemplateSource = 'workspace' | 'default';

/** A pinned template: the TEXT as it was when the run started, and its digest. */
export interface PinnedTemplate {
  runId: string;
  templateHash: string;
  templateText: string;
  source: TemplateSource;
  /**
   * The workspace the folder was in when the Run started.
   *
   * PINNED HERE BECAUSE IT IS ONLY KNOWN HERE. §2.4's contract gives a template `workflow.workspace`,
   * and at render time the folder may have moved, been renamed, or stopped existing — a Run reports on
   * where it ran, not on where its code is now. The start path holds the absolute folder and therefore
   * the answer; nothing later does.
   */
  workspace: string;
  capturedAt: number;
}

/** Whether a render produced a document or an explanation. */
export type VersionStatus = 'ok' | 'error';

/** One immutable render. */
export interface ReportVersion {
  runId: string;
  version: number;
  status: VersionStatus;
  templateHash: string;
  /** The stored snapshot — mdast plus blocks — as JSON text. Absent on an `error` version. */
  snapshotJson?: string;
  /** Why the render failed. Absent on an `ok` version. */
  errorText?: string;
  renderedAt: number;
  /** `sweep` for the automatic render, or the console user who asked for a re-render. */
  renderedBy: string;
}

/** A version without its snapshot — what the version list serves. */
export type VersionSummary = Omit<ReportVersion, 'snapshotJson'>;

/** One Run's report, as the Reports surface lists it: the newest version and how many there are. */
export interface ReportListRow {
  runId: string;
  version: number;
  status: VersionStatus;
  templateHash: string;
  renderedAt: number;
  renderedBy: string;
  /** How many versions this Run's report has. Filled by {@link ReportStore.listDetail}. */
  versions: number;
  /** The workspace the Run ran in, when a template was pinned. Absent for an unpinned Run. */
  workspace?: string;
  /** The caller workflow's manifest name, filled by the route from `run_workflow`. */
  workflow?: string;
}

export interface FeedbackNote {
  id: string;
  runId: string;
  /** The workflow name, denormalised so `list_feedback` can filter by workflow without a join. */
  workflow: string;
  author: string;
  authorKind: 'user' | 'token';
  body: string;
  createdAt: number;
  editedAt?: number;
  deletedAt?: number;
}

/** The longest a feedback note may be. A note is a remark, not a document. */
export const FEEDBACK_MAX_BYTES = 16 * 1024;

export class InvalidFeedbackError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'InvalidFeedbackError';
  }
}

/**
 * Four tables.
 *
 * `BIGINT` for every timestamp and byte count, never `INTEGER`: the family's rule, because Postgres
 * `INTEGER` is 32-bit and an epoch-millisecond value is not.
 *
 * `report_version.render_key` is what makes a render idempotent — see {@link ReportStore.declareVersion}.
 * It is UNIQUE per run rather than per row, so re-rendering the same run from the same template over
 * the same inputs cannot produce a second version.
 */
function ddl(t: (name: string) => string, idx: (name: string) => string = (n) => n): string[] {
  return [
    `CREATE TABLE IF NOT EXISTS ${t('report_template')} (
       run_id        TEXT   PRIMARY KEY,
       template_hash TEXT   NOT NULL,
       template_text TEXT   NOT NULL,
       source        TEXT   NOT NULL CHECK (source IN ('workspace','default')),
       workspace     TEXT   NOT NULL DEFAULT '',
       captured_at   BIGINT NOT NULL DEFAULT 0
     )`,
    `CREATE TABLE IF NOT EXISTS ${t('report_version')} (
       run_id        TEXT   NOT NULL,
       version       BIGINT NOT NULL,
       status        TEXT   NOT NULL CHECK (status IN ('ok','error')),
       template_hash TEXT   NOT NULL,
       snapshot_json TEXT,
       error_text    TEXT,
       rendered_at   BIGINT NOT NULL DEFAULT 0,
       rendered_by   TEXT   NOT NULL DEFAULT '',
       render_key    TEXT   NOT NULL DEFAULT '',
       PRIMARY KEY (run_id, version)
     )`,
    // AN INDEX NAME IS NEVER SCHEMA-QUALIFIED. Postgres puts an index in its table's schema and
    // refuses `CREATE INDEX kontra.x ON kontra.y` with `syntax error at or near "."` — which is
    // what every compose install got from this line, for every Run, so no Run there ever had a
    // report. SQLite has no schema, `t()` is the identity there, and the default suite is SQLite,
    // which is why nothing failed until `KONTRA_TEST_PG` was set.
    `CREATE UNIQUE INDEX IF NOT EXISTS ${idx('report_version_key')} ON ${t('report_version')} (run_id, render_key)`,
    // The unredacted bytes. A separate table and not a column on the version, so that the reveal
    // route's read touches nothing else and a purge can drop these FIRST on their own.
    `CREATE TABLE IF NOT EXISTS ${t('report_secret')} (
       run_id   TEXT NOT NULL,
       version  BIGINT NOT NULL,
       block_id TEXT NOT NULL,
       raw_b64  TEXT NOT NULL,
       PRIMARY KEY (run_id, version, block_id)
     )`,
    `CREATE TABLE IF NOT EXISTS ${t('run_feedback')} (
       id          TEXT   PRIMARY KEY,
       run_id      TEXT   NOT NULL,
       workflow    TEXT   NOT NULL DEFAULT '',
       author      TEXT   NOT NULL,
       author_kind TEXT   NOT NULL CHECK (author_kind IN ('user','token')),
       body        TEXT   NOT NULL,
       created_at  BIGINT NOT NULL DEFAULT 0,
       edited_at   BIGINT,
       deleted_at  BIGINT
     )`,
    `CREATE INDEX IF NOT EXISTS ${idx('run_feedback_run')} ON ${t('run_feedback')} (run_id, created_at)`,
  ];
}

/**
 * Where one namespace's report tables live (ADR 0051: a workspace is addressed, never filtered).
 *
 * The legacy namespace keeps the tables every existing report is already in, so those stay readable
 * from the `default` workspace and from nowhere else. A workspace namespace gets tables of its own:
 * its own Postgres schema (`kontra_ws_hello`), or on SQLite, which has no schemas, its own table and
 * index names (`ws_hello__report_version`). A report is therefore not visible from another
 * workspace because no query there can name its table, not because a WHERE clause leaves it out.
 */
export function reportScope(
  namespace: string,
  base: { schema: string | null }
): { schema: string | null; table: (name: string) => string; index: (name: string) => string } {
  const ws = namespace.startsWith(ADDRESS_PREFIX) ? namespace.replace(/[^0-9a-z]/g, '_') : '';
  if (base.schema !== null) {
    const schema = ws ? `${base.schema}_${ws}` : base.schema;
    return { schema, table: (name) => `${schema}.${name}`, index: (name) => name };
  }
  const prefix = ws ? `${ws}__` : '';
  return { schema: null, table: (name) => `${prefix}${name}`, index: (name) => `${prefix}${name}` };
}

export class ReportStore {
  private readonly driver: SqlDriver;
  readonly backend: 'postgres' | 'sqlite';
  private readonly baseSchema: string | null;
  private readonly inits = new Map<string, () => Promise<void>>();

  constructor(opts: DriverOptions = {}) {
    const resolved = createDriver(opts);
    this.driver = resolved.driver;
    this.backend = resolved.backend;
    this.baseSchema = resolved.schema;
  }

  /** The tables of the namespace this call is addressed to — the request's workspace, or the run's
   *  own inside a background pass (`inNamespace`). Read per call, never cached on the instance. */
  private get t(): (name: string) => string {
    return reportScope(activeNamespace(), { schema: this.baseSchema }).table;
  }

  /** Create the addressed namespace's tables once per process, and again after a failure. */
  private get init(): () => Promise<void> {
    const scope = reportScope(activeNamespace(), { schema: this.baseSchema });
    const id = scope.table('');
    let init = this.inits.get(id);
    if (!init) {
      init = memoizeInit(async () => {
        if (scope.schema) await this.driver.exec(`CREATE SCHEMA IF NOT EXISTS ${scope.schema}`);
        for (const stmt of ddl(scope.table, scope.index)) await this.driver.exec(stmt);
      });
      this.inits.set(id, init);
    }
    return init;
  }

  async ensureSchema(): Promise<void> {
    await this.init();
  }

  // --- the pinned template -----------------------------------------------------------------------

  /**
   * Pin the template a Run will be reported through, at the moment the Run starts.
   *
   * UPSERT BY RUN ID, for the reason `runWorkflows.record` upserts: Temporal refuses a duplicate
   * workflow id while the first execution is open, so a second pin for one id is a re-statement of the
   * same fact rather than a competing one.
   *
   * THE TEXT IS STORED, NOT A PATH. The whole point of pinning is that editing `report.md` after a Run
   * starts does not change that Run's report (acceptance test 12), and a path would be re-read.
   */
  async pinTemplate(input: {
    runId: string;
    templateHash: string;
    templateText: string;
    source: TemplateSource;
    workspace?: string;
    at?: number;
  }): Promise<void> {
    await this.init();
    await this.driver.run(
      `INSERT INTO ${this.t('report_template')}
         (run_id, template_hash, template_text, source, workspace, captured_at)
       VALUES (?, ?, ?, ?, ?, ?)
       ON CONFLICT (run_id) DO UPDATE SET template_hash = excluded.template_hash,
                                          template_text = excluded.template_text,
                                          source        = excluded.source,
                                          workspace     = excluded.workspace,
                                          captured_at   = excluded.captured_at`,
      [
        input.runId,
        input.templateHash,
        input.templateText,
        input.source,
        input.workspace ?? '',
        input.at ?? Date.now(),
      ]
    );
  }

  /** The template pinned for a Run, or undefined when nothing pinned one — §4.6's fallback case. */
  async template(runId: string): Promise<PinnedTemplate | undefined> {
    await this.init();
    const rows = await this.driver.all(
      `SELECT run_id, template_hash, template_text, source, workspace, captured_at
         FROM ${this.t('report_template')} WHERE run_id = ?`,
      [runId]
    );
    const r = rows[0];
    if (!r) return undefined;
    return {
      runId: String(r.run_id),
      templateHash: String(r.template_hash),
      templateText: String(r.template_text),
      source: String(r.source) as TemplateSource,
      workspace: String(r.workspace ?? ''),
      capturedAt: num(r.captured_at),
    };
  }

  // --- versions ----------------------------------------------------------------------------------

  /**
   * Store one render as a NEW VERSION, or report that this exact render already exists.
   *
   * ── WHAT MAKES IT IDEMPOTENT (acceptance test 13) ──────────────────────────────────────────────
   *
   * `renderKey` is the caller's statement of what this render was OF — the template hash and a digest
   * of the inputs. It is UNIQUE per run, so the completion hook firing twice for one Run produces one
   * version and the second call answers `{ created: false }` with the version that already exists.
   *
   * ── THE VERSION NUMBER IS ALLOCATED IN THE STATEMENT ───────────────────────────────────────────
   *
   * `INSERT … SELECT COALESCE(MAX(version), 0) + 1` reads and writes in ONE statement, which both
   * backends execute atomically even though this family has no transaction. Two orchestrators racing
   * can still compute the same next number and collide on the primary key; `ON CONFLICT DO NOTHING`
   * absorbs that and the caller retries, which is the same shape as `MaterializationStore.declare`'s
   * conditional claim. A bounded retry rather than a lock, because the loser's work is one render and
   * the alternative is a lost report.
   */
  async declareVersion(input: {
    runId: string;
    status: VersionStatus;
    templateHash: string;
    renderKey: string;
    snapshotJson?: string;
    errorText?: string;
    renderedBy: string;
    at?: number;
  }): Promise<{ created: boolean; version: number }> {
    await this.init();
    const table = this.t('report_version');
    for (let attempt = 0; attempt < 3; attempt += 1) {
      const existing = await this.driver.all(
        `SELECT version FROM ${table} WHERE run_id = ? AND render_key = ?`,
        [input.runId, input.renderKey]
      );
      if (existing[0]) return { created: false, version: num(existing[0].version) };

      const changed = await this.driver.run(
        `INSERT INTO ${table}
           (run_id, version, status, template_hash, snapshot_json, error_text, rendered_at, rendered_by, render_key)
         SELECT ?, COALESCE(MAX(version), 0) + 1, ?, ?, ?, ?, ?, ?, ?
           FROM ${table} WHERE run_id = ?
         ON CONFLICT DO NOTHING`,
        [
          input.runId,
          input.status,
          input.templateHash,
          input.snapshotJson ?? null,
          input.errorText ?? null,
          input.at ?? Date.now(),
          input.renderedBy,
          input.renderKey,
          input.runId,
        ]
      );
      if (num(changed) > 0) {
        const mine = await this.driver.all(
          `SELECT version FROM ${table} WHERE run_id = ? AND render_key = ?`,
          [input.runId, input.renderKey]
        );
        return { created: true, version: mine[0] ? num(mine[0].version) : 1 };
      }
    }
    throw new Error(
      `could not allocate a report version for ${input.runId} after 3 attempts — another orchestrator ` +
        'is rendering the same run'
    );
  }

  /**
   * The version already stored for one render key, if any.
   *
   * THE CHEAP CHECK THAT COMES BEFORE A RENDER. The sweep asks this first so a steady-state pass over
   * a full retention window costs one indexed read per run and no rendering at all — the same shape as
   * `HistoryArchive.has`, which is what keeps that sweep affordable.
   */
  async versionByKey(runId: string, renderKey: string): Promise<number | undefined> {
    await this.init();
    const rows = await this.driver.all(
      `SELECT version FROM ${this.t('report_version')} WHERE run_id = ? AND render_key = ?`,
      [runId, renderKey]
    );
    return rows[0] ? num(rows[0].version) : undefined;
  }

  /**
   * What the next version number will be.
   *
   * A PREDICTION, NOT A RESERVATION — §2.4 promises a template `report.version`, and the number is not
   * actually allocated until {@link declareVersion}'s insert. They agree unless another orchestrator
   * inserts in between, in which case the rendered number is one behind the stored one. That is a
   * cosmetic divergence in a document, and the alternative — reserving a number before rendering —
   * would leave a gap in the sequence for every render that failed.
   */
  async nextVersion(runId: string): Promise<number> {
    await this.init();
    const rows = await this.driver.all(
      `SELECT COALESCE(MAX(version), 0) AS top FROM ${this.t('report_version')} WHERE run_id = ?`,
      [runId]
    );
    return (rows[0] ? num(rows[0].top) : 0) + 1;
  }

  /** One version in full, snapshot included. `version` omitted means the latest. */
  async version(runId: string, version?: number): Promise<ReportVersion | undefined> {
    await this.init();
    const rows =
      version === undefined
        ? await this.driver.all(
            `SELECT * FROM ${this.t('report_version')} WHERE run_id = ? ORDER BY version DESC LIMIT 1`,
            [runId]
          )
        : await this.driver.all(
            `SELECT * FROM ${this.t('report_version')} WHERE run_id = ? AND version = ?`,
            [runId, version]
          );
    return rows[0] ? rowToVersion(rows[0]) : undefined;
  }

  /**
   * Every version of one Run's report, newest first, WITHOUT the snapshots.
   *
   * The projection is the point: a run with ten versions of a 5 MiB snapshot is 50 MiB, and the
   * version selector needs none of it. `listGraphs` in `db/repo.ts` leaves its blob behind for the
   * same reason.
   */
  async versions(runId: string): Promise<VersionSummary[]> {
    await this.init();
    const rows = await this.driver.all(
      `SELECT run_id, version, status, template_hash, error_text, rendered_at, rendered_by
         FROM ${this.t('report_version')} WHERE run_id = ? ORDER BY version DESC`,
      [runId]
    );
    return rows.map((r) => {
      const out: VersionSummary = {
        runId: String(r.run_id),
        version: num(r.version),
        status: String(r.status) as VersionStatus,
        templateHash: String(r.template_hash),
        renderedAt: num(r.rendered_at),
        renderedBy: String(r.rendered_by ?? ''),
      };
      if (r.error_text !== null && r.error_text !== undefined) out.errorText = String(r.error_text);
      return out;
    });
  }

  /**
   * The newest version of every Run's report, newest first — what the Reports surface lists.
   *
   * ONE ROW PER RUN, via a correlated subquery that both backends support. The alternative, pulling
   * every version and reducing in TypeScript, would read N versions to show one and would make the
   * `limit` mean something different from what a caller asked for.
   *
   * NO WORKFLOW NAME HERE, deliberately. It lives in `run_workflow`, a different store in this same
   * family, and a join across two stores' tables would couple them in SQL where they are only coupled
   * by a run id. The route enriches instead — `withDatasetNames` in `data/datasets.ts` is the same
   * shape for the same reason, and it degrades to "no name" rather than to no row.
   */
  async listReports(opts: { limit?: number } = {}): Promise<ReportListRow[]> {
    await this.init();
    const limit = opts.limit && opts.limit > 0 ? Math.min(opts.limit, 500) : 200;
    const t = this.t('report_version');
    const rows = await this.driver.all(
      `SELECT v.run_id, v.version, v.status, v.template_hash, v.rendered_at, v.rendered_by
         FROM ${t} v
        WHERE v.version = (SELECT MAX(w.version) FROM ${t} w WHERE w.run_id = v.run_id)
        ORDER BY v.rendered_at DESC
        LIMIT ${limit}`,
      []
    );
    return rows.map((r) => ({
      runId: String(r.run_id),
      version: num(r.version),
      status: String(r.status) as VersionStatus,
      templateHash: String(r.template_hash),
      renderedAt: num(r.rendered_at),
      renderedBy: String(r.rendered_by ?? ''),
      versions: 0,
    }));
  }

  /**
   * How many versions each of these Runs has, and which workspace it ran in.
   *
   * A SECOND STATEMENT RATHER THAN A WIDER FIRST ONE: the listing above is one row per Run by
   * construction, and counting versions in the same query would need a second aggregate over the same
   * table. Two cheap statements beat one clever one, and this one is skipped entirely for an empty page.
   */
  async listDetail(runIds: readonly string[]): Promise<Map<string, { versions: number; workspace: string }>> {
    const out = new Map<string, { versions: number; workspace: string }>();
    const ids = [...new Set(runIds)].filter((id) => id !== '');
    if (ids.length === 0) return out;
    await this.init();
    for (const page of chunkBinds(ids)) {
      const holes = page.map(() => '?').join(', ');
      const counts = await this.driver.all(
        `SELECT run_id, COUNT(*) AS n FROM ${this.t('report_version')}
          WHERE run_id IN (${holes}) GROUP BY run_id`,
        [...page]
      );
      for (const r of counts) {
        out.set(String(r.run_id), { versions: num(r.n), workspace: '' });
      }
      const spaces = await this.driver.all(
        `SELECT run_id, workspace FROM ${this.t('report_template')} WHERE run_id IN (${holes})`,
        [...page]
      );
      for (const r of spaces) {
        const id = String(r.run_id);
        const found = out.get(id);
        if (found) found.workspace = String(r.workspace ?? '');
      }
    }
    return out;
  }

  /** Which of these Runs have a report at all — one statement per page of ids, never one per Run. */
  async haveReports(runIds: readonly string[]): Promise<Set<string>> {
    const ids = [...new Set(runIds)].filter((id) => id !== '');
    if (ids.length === 0) return new Set();
    await this.init();
    const out = new Set<string>();
    // chunkBinds, because the caller is a page of runs sized by a ledger and not by this table:
    // `runWorkflows.list` documents the 32,767-variable failure that taught the family this.
    for (const page of chunkBinds(ids)) {
      const holes = page.map(() => '?').join(', ');
      const rows = await this.driver.all(
        `SELECT DISTINCT run_id FROM ${this.t('report_version')} WHERE run_id IN (${holes})`,
        [...page]
      );
      for (const r of rows) out.add(String(r.run_id));
    }
    return out;
  }

  // --- the unredacted bytes ----------------------------------------------------------------------

  /**
   * Keep the originals of the blocks redaction changed, for the audited reveal path.
   *
   * ONLY THE BLOCKS THAT WERE ACTUALLY REDACTED reach here — `codeTag.ts` sets `raw` only when
   * redaction changed something — because an unredacted copy of bytes nobody redacted is a second
   * place for the same data to live and no second reader.
   */
  async putSecrets(
    runId: string,
    version: number,
    blocks: ReadonlyArray<{ blockId: string; rawB64: string }>
  ): Promise<void> {
    if (blocks.length === 0) return;
    await this.init();
    for (const b of blocks) {
      await this.driver.run(
        `INSERT INTO ${this.t('report_secret')} (run_id, version, block_id, raw_b64) VALUES (?, ?, ?, ?)
         ON CONFLICT DO NOTHING`,
        [runId, version, b.blockId, b.rawB64]
      );
    }
  }

  /** One block's original bytes, base64, or undefined. The reveal route's only read. */
  async secret(runId: string, version: number, blockId: string): Promise<string | undefined> {
    await this.init();
    const rows = await this.driver.all(
      `SELECT raw_b64 FROM ${this.t('report_secret')} WHERE run_id = ? AND version = ? AND block_id = ?`,
      [runId, version, blockId]
    );
    return rows[0] ? String(rows[0].raw_b64) : undefined;
  }

  // --- feedback ----------------------------------------------------------------------------------

  /**
   * Add one note.
   *
   * THE AUTHOR IS NOT A PARAMETER A CLIENT CAN SET. The route resolves it from the credential and
   * passes it here; §7.2 requires that and acceptance test 19 asserts it. This signature cannot
   * express "author from the body" because the route never has a body field for it.
   */
  async addFeedback(input: {
    runId: string;
    workflow?: string;
    author: string;
    authorKind: 'user' | 'token';
    body: string;
    at?: number;
  }): Promise<FeedbackNote> {
    const body = input.body.trim();
    if (body === '') throw new InvalidFeedbackError('a note cannot be empty');
    if (Buffer.byteLength(body, 'utf8') > FEEDBACK_MAX_BYTES) {
      throw new InvalidFeedbackError(`a note is at most ${FEEDBACK_MAX_BYTES} bytes`);
    }
    await this.init();
    const note: FeedbackNote = {
      id: randomUUID(),
      runId: input.runId,
      workflow: input.workflow ?? '',
      author: input.author,
      authorKind: input.authorKind,
      body,
      createdAt: input.at ?? Date.now(),
    };
    await this.driver.run(
      `INSERT INTO ${this.t('run_feedback')} (id, run_id, workflow, author, author_kind, body, created_at)
       VALUES (?, ?, ?, ?, ?, ?, ?)`,
      [note.id, note.runId, note.workflow, note.author, note.authorKind, note.body, note.createdAt]
    );
    return note;
  }

  /** Notes, newest first, excluding deleted. Filterable by run or by workflow, per the MCP tool. */
  async feedback(filter: { runId?: string; workflow?: string; limit?: number } = {}): Promise<FeedbackNote[]> {
    await this.init();
    const where: string[] = ['deleted_at IS NULL'];
    const binds: unknown[] = [];
    if (filter.runId) {
      where.push('run_id = ?');
      binds.push(filter.runId);
    }
    if (filter.workflow) {
      where.push('workflow = ?');
      binds.push(filter.workflow);
    }
    const limit = filter.limit && filter.limit > 0 ? Math.min(filter.limit, 500) : 200;
    const rows = await this.driver.all(
      `SELECT * FROM ${this.t('run_feedback')} WHERE ${where.join(' AND ')}
       ORDER BY created_at DESC LIMIT ${limit}`,
      binds
    );
    return rows.map(rowToNote);
  }

  /** One note by id, deleted ones included — the authorship check needs to see a deleted row. */
  async note(id: string): Promise<FeedbackNote | undefined> {
    await this.init();
    const rows = await this.driver.all(`SELECT * FROM ${this.t('run_feedback')} WHERE id = ?`, [id]);
    return rows[0] ? rowToNote(rows[0]) : undefined;
  }

  /**
   * Edit a note's body. The caller has already checked authorship.
   *
   * THE AUTHOR IS IN THE `WHERE` CLAUSE ANYWAY. The route checks and refuses with 403, which is the
   * answer a client needs; this is the second lock, so a future caller that forgets the check cannot
   * rewrite somebody else's note — a 0-rowcount is a refusal rather than a silent success.
   */
  async editFeedback(id: string, author: string, body: string, at = Date.now()): Promise<boolean> {
    const next = body.trim();
    if (next === '') throw new InvalidFeedbackError('a note cannot be empty');
    if (Buffer.byteLength(next, 'utf8') > FEEDBACK_MAX_BYTES) {
      throw new InvalidFeedbackError(`a note is at most ${FEEDBACK_MAX_BYTES} bytes`);
    }
    await this.init();
    const changed = await this.driver.run(
      `UPDATE ${this.t('run_feedback')} SET body = ?, edited_at = ?
         WHERE id = ? AND author = ? AND deleted_at IS NULL`,
      [next, at, id, author]
    );
    return num(changed) > 0;
  }

  /** Soft-delete. The row stays so an edit cannot resurrect it and an audit can still see it. */
  async deleteFeedback(id: string, author: string, at = Date.now()): Promise<boolean> {
    await this.init();
    const changed = await this.driver.run(
      `UPDATE ${this.t('run_feedback')} SET deleted_at = ?
         WHERE id = ? AND author = ? AND deleted_at IS NULL`,
      [at, id, author]
    );
    return num(changed) > 0;
  }

  // --- retention ---------------------------------------------------------------------------------

  /**
   * Everything this Run left behind — the fifth arm of `collectRun`.
   *
   * THE ORDER IS THE MITIGATION, because this family cannot do it in one transaction (see the file
   * header). `report_secret` first: a purge that dies halfway has removed the only rows that hold a
   * credential, and what survives is a redacted snapshot the next sweep will take. The reverse order
   * would leave unredacted bytes behind a version row that no longer exists — reachable by a reveal
   * route that would have no version to authorise against, which is the one outcome worth engineering
   * against.
   *
   * Returns the number of VERSION rows removed, which is what the sweep's counter means by "a report
   * was deleted"; secrets and feedback are consequences of that, not separate events.
   */
  /**
   * Drop version rows that are the render of the EMPTY template rather than of the template they name.
   *
   * ── WHY DELETING A STORED REPORT IS NOT A CONTRADICTION HERE ───────────────────────────────────
   *
   * A report is immutable and versioned, and that protects a record of what a Run returned. These rows
   * are not one. `contextForRun` read `pinned` as a boolean, and `pinTemplate` stores `templateText:
   * ''` for `source: 'default'` on purpose — the text ships with the orchestrator, so a copy per Run
   * would be megabytes of identical rows — so `'' ?? DEFAULT_TEMPLATE` was `''` and every Run started
   * without a `report.md` rendered an EMPTY document. The row says `default@<digest>` and holds the
   * render of a different template, the empty one. It lies about its own provenance, and removing it
   * is not discarding a report: it is clearing the way for the one the label already promises.
   *
   * ── WHY THIS CANNOT BE LEFT TO CONVERGE ON ITS OWN ─────────────────────────────────────────────
   *
   * `renderKey` covers the template hash and the context, and the FIX CHANGES NEITHER — the hash was
   * always `defaultTemplateId()`, which is what makes the fix safe for every other Run. So the sweep's
   * key check finds the bad row and skips, and `POST /report/render` answers 200 `reproduced: true`
   * with that same row, because a pinned template over unchanged metadata is exactly what it holds a
   * version for. Both correct, both unable to help. Nothing writes a second version under that key,
   * so without this the rows are permanent.
   *
   * ── EMPTINESS IS READ FROM THE TREE, NEVER FROM THE TEXT ───────────────────────────────────────
   *
   * A snapshot whose root has no children is structurally empty and could not have come from a
   * template with content in it. Matching on the markdown instead — a short document, no headings —
   * is a heuristic, and a heuristic that deletes reports eventually deletes a real one.
   *
   * Filtered to `source = 'default'` as well, though the structural test alone would be nearly as
   * tight: the bug could not reach a workspace template, so a Run that pinned its own `report.md` and
   * legitimately rendered nothing is somebody's template doing what they wrote, not this.
   *
   * SECRETS GO FIRST, on {@link ReportStore.purgeRun}'s grounds exactly: a half-finished pass must not
   * leave `report_secret` rows reachable by a reveal route whose version row has already gone.
   * An empty render has no blocks and so no secrets, but that is a fact about today's bug rather than
   * a property of the method, and ordering it correctly costs one statement.
   */
  async dropEmptyDefaultVersions(): Promise<Array<{ runId: string; version: number }>> {
    await this.init();
    const rows = await this.driver.all(
      `SELECT v.run_id, v.version, v.snapshot_json
         FROM ${this.t('report_version')} v
         JOIN ${this.t('report_template')} t ON t.run_id = v.run_id
        WHERE t.source = ? AND v.status = ?`,
      ['default', 'ok']
    );

    const removed: Array<{ runId: string; version: number }> = [];
    for (const r of rows) {
      const raw = r.snapshot_json;
      if (typeof raw !== 'string') continue;
      let snapshot: { root?: { children?: unknown[] } };
      try {
        snapshot = JSON.parse(raw) as { root?: { children?: unknown[] } };
      } catch {
        // UNPARSEABLE IS NOT EMPTY. It is a different defect and this method has no opinion on it;
        // deleting it here would destroy the only evidence of whatever wrote it.
        continue;
      }
      const children = snapshot.root?.children;
      if (!Array.isArray(children) || children.length > 0) continue;

      const runId = String(r.run_id);
      const version = num(r.version);
      await this.driver.run(
        `DELETE FROM ${this.t('report_secret')} WHERE run_id = ? AND version = ?`,
        [runId, version]
      );
      await this.driver.run(
        `DELETE FROM ${this.t('report_version')} WHERE run_id = ? AND version = ?`,
        [runId, version]
      );
      removed.push({ runId, version });
    }
    return removed;
  }

  async purgeRun(runId: string): Promise<number> {
    await this.init();
    await this.driver.run(`DELETE FROM ${this.t('report_secret')} WHERE run_id = ?`, [runId]);
    const versions = await this.driver.run(`DELETE FROM ${this.t('report_version')} WHERE run_id = ?`, [runId]);
    await this.driver.run(`DELETE FROM ${this.t('report_template')} WHERE run_id = ?`, [runId]);
    await this.driver.run(`DELETE FROM ${this.t('run_feedback')} WHERE run_id = ?`, [runId]);
    return num(versions);
  }

  async close(): Promise<void> {
    await this.driver.close();
  }
}

function rowToVersion(r: Record<string, unknown>): ReportVersion {
  const out: ReportVersion = {
    runId: String(r.run_id),
    version: num(r.version),
    status: String(r.status) as VersionStatus,
    templateHash: String(r.template_hash),
    renderedAt: num(r.rendered_at),
    renderedBy: String(r.rendered_by ?? ''),
  };
  // NULL becomes ABSENT, not `undefined` — the discipline `db/repo.ts` asserts key-for-key.
  if (r.snapshot_json !== null && r.snapshot_json !== undefined) out.snapshotJson = String(r.snapshot_json);
  if (r.error_text !== null && r.error_text !== undefined) out.errorText = String(r.error_text);
  return out;
}

function rowToNote(r: Record<string, unknown>): FeedbackNote {
  const out: FeedbackNote = {
    id: String(r.id),
    runId: String(r.run_id),
    workflow: String(r.workflow ?? ''),
    author: String(r.author),
    authorKind: String(r.author_kind) as 'user' | 'token',
    body: String(r.body),
    createdAt: num(r.created_at),
  };
  if (r.edited_at !== null && r.edited_at !== undefined) out.editedAt = num(r.edited_at);
  if (r.deleted_at !== null && r.deleted_at !== undefined) out.deletedAt = num(r.deleted_at);
  return out;
}

/** The process-wide store, built on first use — the family's lazy singleton. */
let shared: ReportStore | null = null;

export function reportStore(opts: DriverOptions = {}): ReportStore {
  if (shared === null) shared = new ReportStore(opts);
  return shared;
}

/** Test seam: drop the process-wide store so the next call builds a fresh one. */
export function resetReportStore(): void {
  shared = null;
}
