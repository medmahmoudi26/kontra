/**
 * THE SHARED-DATASET GRANT — the durable record that one workspace's Dataset may be READ from
 * another (ADR 0053), keyed by the ADDRESS the read uses.
 *
 * ── WHY THIS IS NOT IN `datasetRecords.ts`, WHICH IS WHERE IT WAS MEANT TO GO ────────────────────
 *
 * The Dataset record (ADR 0029 §4) is the obvious home: it is already the durable authority for a
 * Dataset's deviation, it already stores only deviation, and a `shared` boolean follows that rule
 * exactly. It is keyed by the **Run** that wrote the Dataset — and that key cannot express the
 * thing being shared.
 *
 * MEASURED AGAINST THE CODE, not argued:
 *
 *   - `withDatasetNames` in `data/datasets.ts` returns early for `info.kind !== 'output'`, so a
 *     STANDALONE Dataset — an operator-loaded scope or seed list, `kontra dataset create` — never
 *     gets a `runId` at all, and never can: it has no Run. A standalone list is the single most
 *     shareable thing on this install (`scope_h1paid` is a list of programs, not a result), and a
 *     Run-keyed flag cannot mark one.
 *   - `DatasetInfo.runId` is "PRESENT EXACTLY WHEN THE ROW HAS ONE RUN" and is deliberately ABSENT
 *     the moment two Runs share a partition. A durable Dataset accumulates from many Runs — `lame`
 *     holds five on this box — so even for output, the grain of a Run-keyed record is a PARTITION,
 *     while the grain of a cross-workspace read is a TABLE: `SELECT … FROM shared_lake.output.lame`
 *     reads every Run's rows under that name whatever any one partition's record says.
 *
 * So a Run-keyed `shared` would be a grant whose stored grain is strictly finer than the access it
 * licenses — one Run's tag opening every Run's rows. That is the "surface that reports success over
 * something it never did" failure in its exact shape, so the key is the address instead:
 * **(workspace, schema, table)**, which is precisely what the reader ATTACHes and SELECTs.
 *
 * ── THE SHAPE: PRESENCE, NOT A COLUMN ───────────────────────────────────────────────────────────
 *
 * ADR 0029 §1 makes tags a SET because two writers must converge, and a rename a SCALAR because it
 * is one choice among many strings. `shared` is neither: its domain has two values and one of them
 * is the ABSENCE OF THE ROW. So it is stored as presence — share INSERTs, unshare DELETEs — which
 * keeps the deviation-only rule literally (an unshared Dataset has no row) and makes a concurrent
 * double-share idempotent by the primary key rather than by a read-modify-write that could race.
 * Two writers disagreeing (one shares, one revokes) is a real conflict with two legitimate
 * outcomes, and last-write-wins is the honest resolution — unlike a rename, nothing is lost that
 * the loser could not simply say again.
 *
 * ── IT IS CLUSTER-WIDE, AND ADR 0051 SAYS SO ────────────────────────────────────────────────────
 *
 * This table holds rows ABOUT several workspaces, which is exactly what §3 refuses for a Dataset's
 * contents. It is not the contents: it is the grant, the workspace is the KEY rather than a filter
 * over a shared pool, and a grant that lived inside the workspace it grants from could not be
 * enumerated without attaching every lake on the install. §1's list keeps "login, users, audit"
 * cluster-wide because they are the install's; a cross-workspace grant is the install's in the same
 * way. The thing the grant must never do is widen a READ, and it cannot: the reader derives its
 * address from the workspace NAME on the row it matched.
 *
 * TWO BACKENDS, ONE SCHEMA — Postgres when the writer and reader are different hosts, SQLite for a
 * single-host install and the test suite. The same split `datasetRecords.ts` makes.
 */

import type { DatasetKind } from './datasets';
import { safeName } from './parquet';
import {
  BIND_CHUNK,
  chunkBinds,
  createDriver,
  memoizeInit,
  type DriverOptions,
  type SqlDriver,
} from './sql';
import { assertWorkspaceName } from '../workspaces';

/**
 * One grant: this workspace's Dataset of this kind and name may be READ from another workspace.
 *
 * Every field is part of the ADDRESS the reader uses, which is the whole reason the record has this
 * shape — `workspace` derives the catalog and the bucket (`workspaceAddress`), `kind` selects the
 * DuckLake schema (`schemaOf`), and `name` is the table.
 */
export interface SharedDataset {
  /** The OWNING workspace — the lake the rows are in, never the reader's. */
  workspace: string;
  /** The Dataset name, exactly as the catalog holds it. See {@link normalizeSharedName}. */
  name: string;
  /** Which schema the table lives in: `output` for actor output, `standalone` for a loaded list. */
  kind: DatasetKind;
  /** When the grant was made (epoch ms) — what an operator auditing an exposure reads first. */
  sharedAt: number;
}

/** The longest Dataset name a grant may carry. A table name, not a document. */
export const SHARED_NAME_MAX_LENGTH = 128;

/**
 * A grant the store refuses to persist. Typed rather than string-matched so a route maps it to a
 * 400 without parsing the message — the discipline `InvalidDeviationError` and
 * `NotTemporaryDatasetError` already follow.
 */
export class InvalidShareError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'InvalidShareError';
  }
}

/**
 * The Dataset name a grant stores, and the one rule that makes it safe to address with.
 *
 * IT MUST ALREADY BE ITS OWN `safeName`. Every writer in this tree names its table through
 * `safeName` (`writeDatasetParquet` does `safeName(sel.actor)`), so a legitimate catalog name is
 * unchanged by it. A name that is NOT its own safe form is therefore a name no table has — and
 * storing it would mean the grant names one string while the reader interpolates another, so the
 * row an operator sees in the exposure listing would not be the row the read path admits. Refused
 * at the point a person types it, where it can be fixed by typing.
 */
export function normalizeSharedName(raw: string): string {
  const name = String(raw ?? '').trim();
  if (name === '') throw new InvalidShareError('a shared Dataset name cannot be empty');
  if (name.length > SHARED_NAME_MAX_LENGTH) {
    throw new InvalidShareError(
      `a shared Dataset name is at most ${SHARED_NAME_MAX_LENGTH} characters (got ${name.length})`
    );
  }
  if (safeName(name) !== name) {
    throw new InvalidShareError(
      `${JSON.stringify(name)} is not a Dataset name this lake can hold — a table name is ` +
        `[A-Za-z0-9_.-] and does not start with a digit, and the grant must name the table ` +
        `exactly as the read path will address it`
    );
  }
  return name;
}

/** `output` unless the caller said `standalone`. The same coercion the preview and provenance
 *  routes apply to their `kind` query parameter, so one spelling reaches the lake. */
export function sharedKind(raw: unknown): DatasetKind {
  return raw === 'standalone' ? 'standalone' : 'output';
}

/**
 * One table, keyed on the whole address.
 *
 * `kind` IS PART OF THE PRIMARY KEY because an output Dataset and a standalone list can share a
 * name and live in different schemas — the console's own row identity is `kind:name` for exactly
 * this reason. Sharing one must not share the other.
 */
function ddl(t: (name: string) => string): string[] {
  return [
    `CREATE TABLE IF NOT EXISTS ${t('dataset_shared')} (
       workspace TEXT   NOT NULL,
       name      TEXT   NOT NULL,
       kind      TEXT   NOT NULL,
       shared_at BIGINT NOT NULL DEFAULT 0,
       PRIMARY KEY (workspace, name, kind)
     )`,
    `CREATE INDEX IF NOT EXISTS idx_dataset_shared_ws ON ${t('dataset_shared')} (workspace)`,
  ];
}

export class SharedDatasetStore {
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
   * GRANT: this workspace's Dataset may be read from another.
   *
   * Idempotent by the primary key, so a double click and a retry are one grant. The workspace name
   * goes through `assertWorkspaceName` — the SAME validator `workspaceAddress` uses to derive the
   * catalog and bucket — so a grant can never be stored under a name the read path would refuse to
   * turn into an address. Storing one would be an exposure listed on the operator's screen that no
   * reader could ever use, which is the inverse lie but a lie either way.
   */
  async share(workspace: string, rawName: string, kind: DatasetKind): Promise<SharedDataset> {
    await this.init();
    assertWorkspaceName(workspace);
    const name = normalizeSharedName(rawName);
    const sharedAt = Date.now();
    await this.driver.run(
      `INSERT INTO ${this.t('dataset_shared')} (workspace, name, kind, shared_at)
       VALUES (?, ?, ?, ?)
       ON CONFLICT (workspace, name, kind) DO NOTHING`,
      [workspace, name, kind, sharedAt]
    );
    // The POST-STATE, re-read rather than assumed: on a repeat grant the stored `sharedAt` is the
    // FIRST one, and reporting `Date.now()` would date an exposure to the moment somebody looked
    // at it. An operator auditing "when was this opened up" needs the first answer.
    return (await this.get(workspace, name, kind)) ?? { workspace, name, kind, sharedAt };
  }

  /**
   * REVOKE. Revoking a grant that is not there is a no-op, not an error — the caller asked for the
   * Dataset not to be shared, and it is not. Returns whether a row was actually removed, so a route
   * can report "it was already closed" without the two cases being indistinguishable.
   */
  async unshare(workspace: string, rawName: string, kind: DatasetKind): Promise<boolean> {
    await this.init();
    const name = normalizeSharedName(rawName);
    const removed = await this.driver.run(
      `DELETE FROM ${this.t('dataset_shared')} WHERE workspace = ? AND name = ? AND kind = ?`,
      [workspace, name, kind]
    );
    return removed > 0;
  }

  /** One grant, or undefined when the Dataset is not shared. THE ADMISSION CHECK the read path
   *  runs before it attaches anything — see `data/sharedRead.ts`. */
  async get(
    workspace: string,
    rawName: string,
    kind: DatasetKind
  ): Promise<SharedDataset | undefined> {
    await this.init();
    const name = normalizeSharedName(rawName);
    const rows = await this.driver.all(
      `SELECT workspace, name, kind, shared_at FROM ${this.t('dataset_shared')}
        WHERE workspace = ? AND name = ? AND kind = ?`,
      [workspace, name, kind]
    );
    return rows[0] ? rowToShare(rows[0]) : undefined;
  }

  /**
   * EVERY grant, across every workspace — the cross-workspace enumeration ADR 0053 §4 calls the one
   * legitimate one. `workspace` narrows it to a single owner's exposures.
   *
   * Ordered (workspace, kind, name) so the listing is stable between polls and reads as a list of
   * exposures grouped by who is exposing them.
   */
  async list(workspace?: string): Promise<SharedDataset[]> {
    await this.init();
    const rows = workspace
      ? await this.driver.all(
          `SELECT workspace, name, kind, shared_at FROM ${this.t('dataset_shared')}
            WHERE workspace = ? ORDER BY workspace, kind, name`,
          [workspace]
        )
      : await this.driver.all(
          `SELECT workspace, name, kind, shared_at FROM ${this.t('dataset_shared')}
            ORDER BY workspace, kind, name`,
          []
        );
    return rows.map(rowToShare);
  }

  /**
   * The grants among a page of (kind, name) pairs IN ONE workspace — the read side of the
   * `/api/datasets` join.
   *
   * PAGED, for the reason `DatasetRecordStore.list` spells out: the caller hands over one pair per
   * listing row and the listing is sized by the lake, so a single `IN (?,?,…)` eventually raises
   * `too many SQL variables` — and the listing route swallows the error, so the symptom would be
   * Datasets quietly losing their SHARED badge rather than anything anybody saw. Two binds per row,
   * so the page size is halved against `chunkBinds`'s budget.
   */
  async listFor(
    workspace: string,
    names: ReadonlyArray<{ kind: DatasetKind; name: string }>
  ): Promise<SharedDataset[]> {
    if (!workspace || names.length === 0) return [];
    await this.init();
    const keys = [...new Map(names.map((n) => [`${n.kind}:${n.name}`, n])).values()];
    const out: SharedDataset[] = [];
    // Half the budget, because each row below binds TWO parameters (kind and name) plus the one
    // workspace — a page sized for one bind per item would overrun the cap it exists to respect.
    for (const page of chunkBinds(keys, Math.floor(BIND_CHUNK / 2))) {
      const holes = page.map(() => '(kind = ? AND name = ?)').join(' OR ');
      const binds: unknown[] = [workspace];
      for (const k of page) binds.push(k.kind, k.name);
      const rows = await this.driver.all(
        `SELECT workspace, name, kind, shared_at FROM ${this.t('dataset_shared')}
          WHERE workspace = ? AND (${holes})`,
        binds
      );
      for (const r of rows) out.push(rowToShare(r));
    }
    return out;
  }

  /**
   * Drop every grant a workspace owns — what a workspace deletion would have to call.
   *
   * NOTHING CALLS IT YET, and that is stated rather than implied: ADR 0051 puts workspace deletion
   * out of scope and warns it "must not be added casually". This exists so the grant table is not
   * the reason a future deletion leaves a dangling exposure, and returns the count so that deletion
   * can report it.
   */
  async purgeWorkspace(workspace: string): Promise<number> {
    await this.init();
    return this.driver.run(`DELETE FROM ${this.t('dataset_shared')} WHERE workspace = ?`, [
      workspace,
    ]);
  }

  async close(): Promise<void> {
    await this.driver.close();
  }
}

function rowToShare(r: Record<string, unknown>): SharedDataset {
  return {
    workspace: String(r.workspace),
    name: String(r.name),
    kind: sharedKind(r.kind),
    sharedAt: Number(r.shared_at ?? 0),
  };
}

/** The process-wide grant store, built on first use — the same lazy singleton the Dataset record,
 *  summary and materialization stores use, so a single-host install needs no new configuration. */
let shared: SharedDatasetStore | null = null;

export function sharedDatasetStore(opts: DriverOptions = {}): SharedDatasetStore {
  if (shared === null) shared = new SharedDatasetStore(opts);
  return shared;
}

/** Test seam: drop the process-wide store so the next call builds a fresh one. */
export function resetSharedDatasetStore(): void {
  shared = null;
}
