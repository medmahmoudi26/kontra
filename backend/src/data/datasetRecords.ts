/**
 * The Dataset record — the durable authority for a Dataset's TAGS and its optional RENAME
 * (ADR 0029 §1 and §4), keyed by the **Run** that wrote it.
 *
 * ONLY DEVIATION IS STORED. A Dataset's name is DERIVED (`data/datasetName.ts`) and its tags are
 * empty until somebody says otherwise, so an untagged, un-renamed Dataset has NO row here at all —
 * `get` returns undefined and the surfaces fall back to the derived default. The record exists to
 * hold the two things the derivation cannot: what an operator (or an author) chose to KEEP, and what
 * they chose to CALL it.
 *
 * TAGS ARE A SET, NEVER A SCALAR (ADR 0029 §1). Two writers exist — the author tagging at publish
 * time ("this kind of run always matters", §4), and the operator tagging afterwards ("this run
 * turned out to matter") — so a tag is `add`/`remove`, one row per (run, tag), and a concurrent add
 * of a different tag survives instead of clobbering. A scalar column would make that
 * last-write-wins; the set makes it converge. Adding a tag already present is idempotent by the
 * primary key, not by a read-modify-write that could race.
 *
 * THIS RECORD IS AUTHORITATIVE; `KontraTag` IS A PROJECTION (ADR 0029 §4). Nothing here reads or
 * writes a Temporal search attribute — that is issue 03, a MIRROR of this record over the live
 * window, and it is never read as truth. The record can be written after the execution has closed,
 * which is the common case (an operator tags at hour 23), and that is exactly the window a search
 * attribute cannot reach. So tagging works whether or not the Run is still alive, because this store
 * knows nothing about Temporal.
 *
 * TWO BACKENDS, ONE SCHEMA — the same split `data/summaries.ts` and the materialization store make:
 * Postgres when the writer and reader are different hosts, SQLite for a single-host install and the
 * test suite. Same SQL, same assertions.
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
 * A Dataset's stored deviation from its derived identity, keyed by `runId` (the caller workflow's
 * id, which is what a **Run** IS — ADR 0023 §12).
 *
 * Returned ONLY when there is a deviation: a run with no tags and no rename has no record, so `get`
 * and `list` omit it rather than returning an empty one. That is what keeps "an untagged, un-renamed
 * Dataset has no row" observable from the read side, not just true in storage.
 */
export interface DatasetDeviation {
  runId: string;
  /**
   * The tag SET, sorted for a stable projection so the API and the tests see one order. A returned
   * deviation always has at least one tag OR a {@link renamedTo} — an all-empty deviation is not a
   * deviation and is not returned.
   */
  tags: string[];
  /**
   * The operator's (or author's) rename. ABSENT means the derived default (ADR 0029 §2, issue 01)
   * stands — the record never stores the derived name, only a departure from it.
   */
  renamedTo?: string;
}

/** The longest a tag may be. A tag is a label an operator scans, not a document; a bound keeps one
 *  fat-fingered paste from becoming a column-wide row. */
export const TAG_MAX_LENGTH = 64;

/** The longest a rename may be. Generous — a person may want a sentence — but bounded. */
export const DATASET_NAME_MAX_LENGTH = 200;

/**
 * A tag or a name the caller supplied that this store refuses to persist — empty after trimming, or
 * past its length bound. Typed rather than string-matched so a route can map it to a 400 without
 * parsing the message, the same discipline {@link import('./datasets').NotTemporaryDatasetError}
 * follows for the DELETE route.
 */
export class InvalidDeviationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'InvalidDeviationError';
  }
}

/**
 * Trim and validate a tag before it becomes a set member. Returns the canonical (trimmed) form —
 * leading/trailing whitespace never distinguishes two tags, so `"prod "` and `"prod"` are the same
 * member rather than two. Throws {@link InvalidDeviationError} on empty or over-long input.
 */
export function normalizeTag(raw: string): string {
  const tag = String(raw ?? '').trim();
  if (tag === '') throw new InvalidDeviationError('a tag cannot be empty');
  if (tag.length > TAG_MAX_LENGTH) {
    throw new InvalidDeviationError(`a tag is at most ${TAG_MAX_LENGTH} characters (got ${tag.length})`);
  }
  return tag;
}

/** Trim and validate a rename. Same rules as a tag, its own length bound. */
export function normalizeName(raw: string): string {
  const name = String(raw ?? '').trim();
  if (name === '') throw new InvalidDeviationError('a Dataset name cannot be empty');
  if (name.length > DATASET_NAME_MAX_LENGTH) {
    throw new InvalidDeviationError(
      `a Dataset name is at most ${DATASET_NAME_MAX_LENGTH} characters (got ${name.length})`
    );
  }
  return name;
}

/**
 * The two tables, in portable SQL.
 *
 * `dataset_tag`    — one row per (run, tag). The SET. Add is an idempotent insert; remove a delete.
 * `dataset_rename` — one row per run with a rename. The optional scalar, absent when the default
 *                    stands.
 *
 * Both keyed on `run_id` and indexed for the list join — the read side fetches every deviation for a
 * page of runs in one round trip per table.
 */
function ddl(t: (name: string) => string): string[] {
  return [
    `CREATE TABLE IF NOT EXISTS ${t('dataset_tag')} (
       run_id     TEXT   NOT NULL,
       tag        TEXT   NOT NULL,
       created_at BIGINT NOT NULL DEFAULT 0,
       PRIMARY KEY (run_id, tag)
     )`,
    `CREATE TABLE IF NOT EXISTS ${t('dataset_rename')} (
       run_id     TEXT   PRIMARY KEY,
       name       TEXT   NOT NULL,
       updated_at BIGINT NOT NULL DEFAULT 0
     )`,
    `CREATE INDEX IF NOT EXISTS idx_dataset_tag_run ON ${t('dataset_tag')} (run_id)`,
  ];
}

export class DatasetRecordStore {
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
   * Add a tag to a Run's Dataset. IDEMPOTENT by the primary key — adding a tag already present is a
   * no-op, and two concurrent adds of DIFFERENT tags both land, because each is its own row rather
   * than a write of one scalar. This is the whole reason tags are a set (ADR 0029 §1).
   */
  async addTag(runId: string, rawTag: string): Promise<void> {
    await this.init();
    const tag = normalizeTag(rawTag);
    await this.driver.run(
      `INSERT INTO ${this.t('dataset_tag')} (run_id, tag, created_at) VALUES (?, ?, ?)
       ON CONFLICT (run_id, tag) DO NOTHING`,
      [runId, tag, Date.now()]
    );
  }

  /** Remove a tag. Removing one that is not present is a no-op, not an error — the caller asked for
   *  it to be gone, and it is. */
  async removeTag(runId: string, rawTag: string): Promise<void> {
    await this.init();
    const tag = normalizeTag(rawTag);
    await this.driver.run(
      `DELETE FROM ${this.t('dataset_tag')} WHERE run_id = ? AND tag = ?`,
      [runId, tag]
    );
  }

  /**
   * Rename a Run's Dataset — store an explicit name that overrides the derived default. Upsert, so
   * renaming twice keeps the latest, and a rename is a single authoritative choice, not a set.
   */
  async setRename(runId: string, rawName: string): Promise<void> {
    await this.init();
    const name = normalizeName(rawName);
    await this.driver.run(
      `INSERT INTO ${this.t('dataset_rename')} (run_id, name, updated_at) VALUES (?, ?, ?)
       ON CONFLICT (run_id) DO UPDATE SET name = excluded.name, updated_at = excluded.updated_at`,
      [runId, name, Date.now()]
    );
  }

  /** Drop the rename, so the derived default (ADR 0029 §2) stands again. */
  async clearRename(runId: string): Promise<void> {
    await this.init();
    await this.driver.run(`DELETE FROM ${this.t('dataset_rename')} WHERE run_id = ?`, [runId]);
  }

  /**
   * One Run's deviation, or undefined when it has none.
   *
   * Undefined is the un-deviated case made observable: no tags AND no rename means no record, which
   * is what the surfaces read as "use the derived name, show no tags". A Run with only a rename, or
   * only tags, returns a deviation carrying exactly the half that exists.
   */
  async get(runId: string): Promise<DatasetDeviation | undefined> {
    await this.init();
    const [tags, rename] = await Promise.all([
      this.driver.all(
        `SELECT tag FROM ${this.t('dataset_tag')} WHERE run_id = ? ORDER BY tag`,
        [runId]
      ),
      this.driver.all(`SELECT name FROM ${this.t('dataset_rename')} WHERE run_id = ?`, [runId]),
    ]);
    return buildDeviation(
      runId,
      tags.map((r) => String(r.tag)),
      rename[0] ? String(rename[0].name) : undefined
    );
  }

  /**
   * Every deviation among a page of Runs — the read side of the `/api/datasets` join. Runs with no
   * deviation are simply absent from the result, so the caller attaches nothing to their rows and
   * they render as derived+untagged. An empty input asks for nothing and returns nothing without
   * touching the database.
   *
   * PAGED, FOR THE REASON `RunWorkflowStore.list` SPELLS OUT: the caller hands over one id per
   * listing row and the listing is sized by the lake, so past ~32k Runs a single `IN (?,?,…)` raised
   * `too many SQL variables` — swallowed by the listing route, which is why the symptom was Datasets
   * quietly losing their tags and their rename rather than an error anybody saw. The two statements
   * are still issued as a pair per page: they answer about the same page of Runs and are merged by
   * `runId` below, so the pairing is what keeps a tag and a rename of one Run in one deviation.
   */
  async list(runIds: readonly string[]): Promise<DatasetDeviation[]> {
    const ids = [...new Set(runIds)].filter((id) => id !== '');
    if (ids.length === 0) return []; // nothing to look up — don't even open the store
    await this.init();
    const tagsByRun = new Map<string, string[]>();
    const renameByRun = new Map<string, string>();
    for (const page of chunkBinds(ids)) {
      const holes = page.map(() => '?').join(', ');
      const [tags, renames] = await Promise.all([
        this.driver.all(
          `SELECT run_id, tag FROM ${this.t('dataset_tag')} WHERE run_id IN (${holes}) ORDER BY tag`,
          [...page]
        ),
        this.driver.all(
          `SELECT run_id, name FROM ${this.t('dataset_rename')} WHERE run_id IN (${holes})`,
          [...page]
        ),
      ]);
      for (const r of tags) {
        const runId = String(r.run_id);
        const list = tagsByRun.get(runId) ?? [];
        list.push(String(r.tag));
        tagsByRun.set(runId, list);
      }
      for (const r of renames) renameByRun.set(String(r.run_id), String(r.name));
    }

    const out: DatasetDeviation[] = [];
    for (const runId of new Set([...tagsByRun.keys(), ...renameByRun.keys()])) {
      const dev = buildDeviation(runId, tagsByRun.get(runId) ?? [], renameByRun.get(runId));
      if (dev) out.push(dev);
    }
    return out;
  }

  /**
   * Drop a Run's whole record — the retention arm, so a purged Run's tags and rename go with its
   * status rows rather than outliving the data they described. Returns the number of tag rows
   * removed (the rename is at most one).
   */
  async purgeRun(runId: string): Promise<number> {
    await this.init();
    const removed = await this.driver.run(
      `DELETE FROM ${this.t('dataset_tag')} WHERE run_id = ?`,
      [runId]
    );
    await this.driver.run(`DELETE FROM ${this.t('dataset_rename')} WHERE run_id = ?`, [runId]);
    return num(removed);
  }

  async close(): Promise<void> {
    await this.driver.close();
  }
}

/** Assemble a deviation, or undefined when there is nothing to deviate. Keeps the "no row means no
 *  record" rule in ONE place so `get` and `list` cannot disagree about what an empty deviation is. */
function buildDeviation(
  runId: string,
  tags: string[],
  renamedTo: string | undefined
): DatasetDeviation | undefined {
  if (tags.length === 0 && renamedTo === undefined) return undefined;
  const dev: DatasetDeviation = { runId, tags };
  if (renamedTo !== undefined) dev.renamedTo = renamedTo;
  return dev;
}

/** The process-wide record store, built on first use — the same lazy singleton the summary and
 *  materialization stores use, so a single-host install needs no new configuration. */
let shared: DatasetRecordStore | null = null;

export function datasetRecordStore(opts: DriverOptions = {}): DatasetRecordStore {
  if (shared === null) shared = new DatasetRecordStore(opts);
  return shared;
}

/** Test seam: drop the process-wide store so the next call builds a fresh one. */
export function resetDatasetRecordStore(): void {
  shared = null;
}
