/**
 * READING ONE WORKSPACE'S SHARED DATASET FROM ANOTHER (ADR 0053) — the only path in this codebase
 * that crosses the isolation boundary ADR 0051 draws, and the whole of it.
 *
 * ── THE ADDRESS IS THE ADMISSION, AND IT IS CHECKED BEFORE ANYTHING IS ATTACHED ─────────────────
 *
 * A caller NAMES the source workspace, the Dataset and its kind. Those three are the address —
 * `workspaceAddress(workspace)` derives the catalog and the bucket, `schemaOf(kind)` picks the
 * DuckLake schema, and the name is the table. There is no `WHERE workspace = ?` anywhere in this
 * module and no query that could be widened by dropping a clause: get the workspace wrong and the
 * attach lands on a catalog that does not hold the table, and the read says so (ADR 0051 §3).
 *
 * The GRANT is looked up first, on the same three fields, and a missing grant throws
 * {@link NotSharedError} before a DuckDB instance is created. So the failure for an ungranted
 * Dataset costs nothing and is indistinguishable from the failure for one that does not exist —
 * which is deliberate: this surface must not become a way to ask whether another workspace holds a
 * table of a given name.
 *
 * ── THE SQL IS COMPOSED HERE. THAT IS NOT A STYLE CHOICE, IT IS THE BOUNDARY ─────────────────────
 *
 * The obvious implementation was a parameter on the query workbench (`routes/query.ts` +
 * `data/queryEngine.ts`): it already attaches a lake READ_ONLY, already hardens the connection,
 * already caches per (catalog, dataPath), and `resolveLakeConfig(store, { workspace })` already
 * names another workspace's address. Every one of those is true and the design still does not work,
 * for one reason that cannot be patched:
 *
 *     THE WORKBENCH TAKES SQL A PERSON WROTE, AND AN ATTACHED CATALOG IS REACHABLE BY QUALIFIED
 *     NAME. `refreshViews` exposes the shared Dataset as a bare name, but nothing stops the same
 *     statement from writing `FROM shared_lake.output.<anything>` — so naming ONE shared Dataset
 *     would hand over the whole of the owning workspace's lake. Filtering the view layer does not
 *     help; the views are a convenience over a catalog the statement can already address.
 *
 * So the cross-workspace read composes its own statement, from a name checked against the grant and
 * then against the catalog — the trust model `routes/datasets.ts` already draws the line on
 * ("Everything in `routes/datasets.ts` composes its own SQL from a name checked against the
 * catalog; these take a statement a person wrote"). No caller SQL reaches this connection, so there
 * is no qualified name to escape through and the grant is the entire boundary.
 *
 * `orderBy` is the one exception and is held to a GRAMMAR rather than trusted — see
 * {@link assertSimpleOrderBy}, which exists because `ORDER BY (SELECT … FROM shared_lake.output.x)`
 * is a perfectly ordinary ORDER BY.
 *
 * ── READ_ONLY IS DUCKDB'S, NOT THIS MODULE'S ────────────────────────────────────────────────────
 *
 * `sharedLakeConnection` puts READ_ONLY on the ATTACH, so DuckDB refuses a write by statement type.
 * A later function in this file that composed an INSERT would be refused by the engine rather than
 * by a reviewer, which is what makes the claim in ADR 0053 checkable.
 *
 * ── AND THE CONNECTION IS CLOSED ────────────────────────────────────────────────────────────────
 *
 * Every function here opens its own attach and closes it in a `finally`. It is not pooled, for the
 * reason `attachedLakeConnection` gives, and the cost is real: `queryEngine.ts` measures ~260 ms for
 * a DuckDB instance plus three extension loads. A clone workflow pays that per page, which is the
 * right trade for a boundary crossing and the wrong one for a poll — nothing here is polled.
 */

import type { DuckDBConnection } from '@duckdb/node-api';

import type { ObjectStore } from '../codec/objectStore';
import { workspaceAddress, type WorkspaceAddress } from '../workspaces';
import {
  NoSuchDatasetError,
  PREVIEW_MAX_ROWS,
  PREVIEW_ROWS,
  schemaOf,
  type DatasetColumn,
  type DatasetKind,
} from './datasets';
import {
  SHARED_SRC,
  lakeEnabled,
  safeName,
  sharedLakeConnection,
  type LakeConfig,
} from './parquet';
import type { SharedDataset, SharedDatasetStore } from './sharedDatasets';
import type { DatasetPage } from './queryEngine';

/**
 * THE DATASET IS NOT SHARED FROM THAT WORKSPACE — the caller's mistake, and the one refusal this
 * module exists to make.
 *
 * Typed, not string-matched, for the reason `NoSuchDatasetError` states at length: the difference
 * between a 404 and a 502 must not be a property of the English in the message, because the console
 * prints the message. It is RAISED IDENTICALLY whether the Dataset is ungranted or absent, so this
 * surface cannot be used to enumerate another workspace's tables — the message says "not shared",
 * which is true in both cases.
 */
export class NotSharedError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'NotSharedError';
  }
}

/** An `ORDER BY` term this module refuses. Separate from {@link NotSharedError} because it is an
 *  input fault rather than an admission one, and a route maps it to a 400. */
export class UnsupportedOrderByError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'UnsupportedOrderByError';
  }
}

/** Where the rows are: the three fields that ARE the address. */
export interface SharedAddress {
  /** The OWNING workspace, named by the caller. Never defaulted — see {@link assertNamed}. */
  workspace: string;
  name: string;
  kind: DatasetKind;
}

/** What one exposure looks like to an operator auditing the install — the grant, plus where it
 *  points, derived rather than stored. */
export interface SharedExposure extends SharedDataset {
  /**
   * The owning workspace's three addresses, derived by `workspaceAddress`.
   *
   * ON THE LISTING BECAUSE AN EXPOSURE IS A POINTER AND A POINTER'S VALUE IS THE POINT. "`lame` is
   * shared from `bugbounty`" does not tell an operator which bucket and catalog a reader will
   * actually open; this does, from the same derivation the read path uses, so the two cannot
   * disagree.
   */
  address: WorkspaceAddress;
}

/** A caller must NAME the source workspace. Defaulting it to the active one would make a
 *  cross-workspace read expressible by omission, which is the opposite of an address. */
function assertNamed(sel: SharedAddress): void {
  if (!sel.workspace || sel.workspace.trim() === '') {
    throw new NotSharedError(
      'a cross-workspace read must NAME the source workspace — there is no default, because a ' +
        'default would make reading another workspace possible by leaving a field out'
    );
  }
  if (!sel.name || sel.name.trim() === '') {
    throw new NotSharedError('a cross-workspace read must name the Dataset');
  }
}

/**
 * `ORDER BY` terms this module will compose, and nothing else.
 *
 * AN ORDER BY IS AN EXPRESSION, which is the part that is easy to miss. `pageDataset` REQUIRES an
 * `orderBy` because a materialized Dataset stamps no row id, so LIMIT/OFFSET over it has no defined
 * order — and the same requirement, pointed at another workspace's lake, would accept
 * `ORDER BY (SELECT count(*) FROM shared_lake.output.secrets)`, which reads a table no grant
 * mentioned. Everything else in this module is composed from a checked name; this one field comes
 * from the caller, so it is held to a grammar:
 *
 *     <column> [ASC|DESC] [NULLS FIRST|NULLS LAST] (, …)
 *
 * A column is a bare or double-quoted identifier. That is enough to page a Dataset deterministically
 * — `ORDER BY run_id, dt` is the realistic case — and admits no subquery, no function call, no
 * operator and no comment. Refused loudly rather than silently narrowed, because a caller whose sort
 * was quietly dropped would get pages that overlap or skip rows with nothing raising, which is the
 * exact failure `pageDataset`'s requirement exists to prevent.
 */
export function assertSimpleOrderBy(orderBy: string): string {
  const text = String(orderBy ?? '').trim();
  if (text === '') {
    throw new UnsupportedOrderByError(
      'paging a shared Dataset needs an explicit order_by: a materialized Dataset stamps no row ' +
        'id, so LIMIT/OFFSET over it has no defined row order and two pages may overlap or skip rows'
    );
  }
  const TERM = /^(?:"[A-Za-z_][A-Za-z0-9_]*"|[A-Za-z_][A-Za-z0-9_]*)(?:\s+(?:asc|desc))?(?:\s+nulls\s+(?:first|last))?$/i;
  const terms = text.split(',').map((t) => t.trim());
  for (const term of terms) {
    if (!TERM.test(term)) {
      throw new UnsupportedOrderByError(
        `order_by term ${JSON.stringify(term)} is not a plain column sort. A cross-workspace read ` +
          'composes its own SQL, so the only caller-supplied fragment is this one and it is held to ' +
          '`<column> [ASC|DESC] [NULLS FIRST|NULLS LAST]` — an expression here could address a ' +
          'table no grant named'
      );
    }
  }
  // Re-emitted from the parsed terms rather than passed through, so what reaches the SQL is what
  // was validated and not a string that merely contained it.
  return terms.join(', ');
}

/** The grant, or a refusal. The admission check, run before anything is attached. */
async function admit(shares: SharedDatasetStore, sel: SharedAddress): Promise<SharedDataset> {
  assertNamed(sel);
  const grant = await shares.get(sel.workspace, sel.name, sel.kind);
  if (!grant) {
    throw new NotSharedError(
      `no ${sel.kind} Dataset named "${sel.name}" is shared from workspace "${sel.workspace}"`
    );
  }
  return grant;
}

/** Whether the SOURCE catalog lists the granted table. Checked for the reason every read in
 *  `data/datasets.ts` checks it: an identifier cannot be parameterised, so the only safe one is a
 *  name the catalog already holds. */
async function tableInSharedCatalog(
  conn: DuckDBConnection,
  schema: string,
  table: string
): Promise<boolean> {
  const r = await conn.runAndReadAll(
    `SELECT count(*) FROM (SHOW ALL TABLES) WHERE database = '${SHARED_SRC}' ` +
      `AND schema = '${schema}' AND name = '${table.replace(/'/g, "''")}'`
  );
  return Number(r.getRows()[0]?.[0] ?? 0) > 0;
}

/** A single-quoted SQL literal. The only values interpolated here are a version and a `dt`. */
function lit(v: string): string {
  return `'${v.replace(/'/g, "''")}'`;
}

/** The partition-pruning predicate, identical in form to the one `previewDataset` composes, so a
 *  shared read of one dispatch prunes directories rather than filtering rows. */
function partitionWhere(sel: { version?: string; dt?: string }): string {
  const where: string[] = [];
  if (sel.version) where.push(`version = ${lit(sel.version)}`);
  if (sel.dt) where.push(`dt LIKE ${lit(sel.dt + '%')}`);
  return where.length > 0 ? ` WHERE ${where.join(' AND ')}` : '';
}

/**
 * Open the granted Dataset: admit, attach READ_ONLY, verify the table, hand back the reference.
 *
 * THE CALLER CLOSES THE CONNECTION. Returned rather than wrapped in a callback so the two public
 * reads below can each shape their own statement; both close in a `finally`.
 *
 * `override` IS THE PROCESS'S `lake` AND IS EMPTY IN PRODUCTION, which is the only reason threading
 * it here is safe. `resolveLakeConfig` lets an explicit `catalog`/`dataPath` WIN over the derived
 * workspace address, so an override naming a lake would send a cross-workspace read to that lake
 * whatever workspace was asked for. `server.ts` passes `{}` outside the suite ("tests point this at
 * a local directory"), and the tests rely on exactly that precedence to stand two real DuckLakes up
 * in a temp directory. Said here because a future caller that starts passing a real override would
 * break the address silently, and an address that can be overridden is not one.
 */
async function openShared(
  store: ObjectStore,
  shares: SharedDatasetStore,
  sel: SharedAddress,
  override: Partial<LakeConfig>
): Promise<{
  conn: DuckDBConnection;
  cfg: LakeConfig;
  grant: SharedDataset;
  /** `shared_lake.<schema>."<table>"` — the fully qualified, catalog-verified reference. */
  ref: string;
}> {
  const grant = await admit(shares, sel);
  // NO LAKE IS A THROW, NEVER AN EMPTY RESULT. Every read in `data/datasets.ts` opens with
  // `if (!lakeEnabled(…)) return []`, which is right for a LISTING — "this install has no lake" and
  // "this install has no datasets" are the same answer to "what is in the lake". For a
  // cross-workspace read they are not: zero rows from a granted Dataset would read as "the owner's
  // Dataset is empty", which is a statement about another workspace's data that this process is in
  // no position to make. So it says what is actually wrong.
  if (!lakeEnabled(store, override)) {
    throw new Error(
      `cannot read workspace "${grant.workspace}": no lake is configured on this install ` +
        '(no object-store endpoint and no KONTRA_DUCKLAKE_DATA_PATH), so there is nothing to attach'
    );
  }
  const { conn, cfg } = await sharedLakeConnection(store, grant.workspace, override);
  try {
    const schema = schemaOf(grant.kind);
    const table = safeName(grant.name);
    if (!(await tableInSharedCatalog(conn, schema, table))) {
      // THE GRANT EXISTS AND THE TABLE DOES NOT — a real and reachable state, because a grant is a
      // record and a Dataset can be dropped or aged out from under it. Reported as the lake's
      // answer ("no such dataset") rather than as the grant's, because the exposure IS open; it
      // points at nothing. `listSharedDatasets` says the same thing from the other side.
      throw new NoSuchDatasetError(
        `no ${grant.kind} dataset named "${grant.name}" in workspace "${grant.workspace}" — ` +
          `the share is recorded but the Dataset is not in that workspace's catalog ` +
          `(${cfg.catalog})`
      );
    }
    return { conn, cfg, grant, ref: `${SHARED_SRC}.${schema}."${table}"` };
  } catch (err) {
    conn.closeSync();
    throw err;
  }
}

/** What a bounded shared read answers — the rows, and the address they came from. */
export interface SharedPreview {
  columns: DatasetColumn[];
  rows: unknown[][];
  /** Echoed back so a reader can see which lake answered, not only that something did. */
  address: SharedAddress & { catalog: string; dataPath: string };
}

/**
 * A BOUNDED read of a shared Dataset — the operator's and the console's path.
 *
 * Bounded by construction on the same terms as `previewDataset`: the limit is clamped to
 * {@link PREVIEW_MAX_ROWS}, and `version`/`dt` are partition columns so a scoped read prunes to one
 * dispatch's directories instead of touching the whole table. The answer to "I need more than this"
 * is the paging read below, which is what a clone uses.
 */
export async function previewSharedDataset(
  store: ObjectStore,
  shares: SharedDatasetStore,
  sel: SharedAddress & { version?: string; dt?: string; limit?: number },
  override: Partial<LakeConfig> = {}
): Promise<SharedPreview> {
  const { conn, cfg, grant, ref } = await openShared(store, shares, sel, override);
  try {
    const limit = Math.min(Math.max(Math.trunc(sel.limit ?? PREVIEW_ROWS), 1), PREVIEW_MAX_ROWS);
    const res = await conn.runAndReadAll(
      `SELECT * FROM ${ref}${partitionWhere(sel)} LIMIT ${limit}`
    );
    const types = res.columnTypes();
    return {
      columns: res.columnNames().map((name, i) => ({ name, type: String(types[i]) })),
      rows: res.getRows().map((row) => row.map(jsonSafe)),
      address: {
        workspace: grant.workspace,
        name: grant.name,
        kind: grant.kind,
        catalog: cfg.catalog,
        dataPath: cfg.dataPath,
      },
    };
  } finally {
    conn.closeSync();
  }
}

/**
 * ONE PAGE OF A SHARED DATASET, AS A CLAIM-CHECKED REF — what a clone workflow in another workspace
 * calls, through the `pageSharedDataset` activity.
 *
 * THE SAME SHAPE `pageDataset` RETURNS, deliberately: `{ ref, n, done }`, the units written to the
 * reader's OWN CAS as a `kind: units` object. So a clone workflow consumes a cross-workspace page
 * with exactly the code it uses for its own Datasets, and the workflow's history holds ~110-byte
 * refs rather than rows — which is what makes cloning a large Dataset possible at all.
 *
 * THE BYTES LAND IN THE READER'S STORE, NOT THE OWNER'S. `store` is the calling process's own
 * object store, so the page object is written where the reader can dereference it. That is the
 * copy ADR 0053 hands to userland, begun: this function produces the ref, and the reader's ordinary
 * publish path puts the rows in its own lake. Nothing here writes to either lake.
 *
 * `done` is derived by reading ONE ROW PAST the page, so end-of-Dataset is a fact rather than a
 * guess from a full page — the termination condition Temporal's Batch Iterator pattern relies on,
 * and the same trick `pageDataset` uses.
 */
export async function pageSharedDataset(
  store: ObjectStore,
  shares: SharedDatasetStore,
  sel: SharedAddress & {
    orderBy: string;
    limit: number;
    offset?: number;
    version?: string;
    dt?: string;
  },
  override: Partial<LakeConfig> = {}
): Promise<DatasetPage & { address: SharedAddress }> {
  // VALIDATED BEFORE THE ATTACH, so a malformed sort costs no DuckDB instance and no grant lookup
  // is wasted on a request that cannot be served.
  const orderBy = assertSimpleOrderBy(sel.orderBy);
  const { conn, grant, ref } = await openShared(store, shares, sel, override);
  try {
    const limit = Math.max(Math.trunc(sel.limit), 1);
    const offset = Math.max(Math.trunc(sel.offset ?? 0), 0);
    const res = await conn.runAndReadAll(
      `SELECT * FROM ${ref}${partitionWhere(sel)} ORDER BY ${orderBy} ` +
        `LIMIT ${limit + 1} OFFSET ${offset}`
    );
    const names = res.columnNames();
    const raw = res.getRows();
    const done = raw.length <= limit;
    const rows = done ? raw : raw.slice(0, limit);
    const units = rows.map((r) => {
      const o: Record<string, unknown> = {};
      names.forEach((name, i) => {
        o[name] = jsonSafe(r[i]);
      });
      return o;
    });
    const body = Buffer.from(JSON.stringify(units), 'utf8');
    const sha = await store.putContentAddressed(body);
    return {
      ref: {
        sha256: sha,
        size: body.length,
        // The same discriminator the handler stamps, so a cross-workspace page and a Method's
        // output are indistinguishable downstream — which is the point.
        meta: { kind: 'units', n: String(units.length) },
      },
      n: units.length,
      done,
      address: { workspace: grant.workspace, name: grant.name, kind: grant.kind },
    };
  } finally {
    conn.closeSync();
  }
}

/**
 * EVERY EXPOSURE ON THE INSTALL — the one cross-workspace listing ADR 0053 admits, and its scope is
 * stated rather than implied.
 *
 * WHAT IT READS: the grant table and nothing else. No lake is attached and no workspace's catalog
 * is opened, so this costs one indexed SQL read however many workspaces exist and cannot fail
 * because one workspace's lake is down.
 *
 * WHAT IT THEREFORE DOES **NOT** SAY: whether the named Dataset still exists, how many rows it
 * holds, or whether anybody has ever read it. It is the list of things MARKED shared. A grant whose
 * Dataset was dropped lists here and fails the read with "the share is recorded but the Dataset is
 * not in that workspace's catalog", which is the loud direction; a listing that silently omitted it
 * would leave an exposure nobody could see in order to revoke.
 *
 * WHY A CROSS-WORKSPACE READ IS LEGITIMATE HERE AND NOWHERE ELSE: the rows are not any workspace's
 * DATA, they are the install's record of which boundaries have been opened. An operator who cannot
 * see every exposure in one place cannot audit the boundary at all, and an exposure that is only
 * visible from inside the workspace that granted it is one a reader has to already know about.
 */
export async function listSharedDatasets(
  shares: SharedDatasetStore,
  opts: { workspace?: string } = {}
): Promise<SharedExposure[]> {
  const grants = await shares.list(opts.workspace);
  return grants.map((grant) => ({
    ...grant,
    // DERIVED, NEVER STORED — `workspaceAddress` is the same function the read path calls, so the
    // bucket and catalog printed here are the ones a reader will actually open. A stored copy is a
    // lookup that must be kept in sync; a function cannot drift (`workspaces.ts`).
    address: workspaceAddress(grant.workspace),
  }));
}

/**
 * Make a DuckDB cell JSON-serializable.
 *
 * THE SAME SHALLOW-RECURSIVE CONVERSION `data/datasets.ts` uses, and deliberately that one rather
 * than `queryEngine.ts`'s richer version: this module answers a preview and a page of units, which
 * are the two shapes `datasets.ts` and `pageDataset` already produce, and a cross-workspace page
 * that spelled a timestamp differently from a same-workspace one would make a clone's rows differ
 * from its source's by where they were read. Integers arrive as BigInt and LIST/STRUCT columns as
 * wrapper objects; both throw or stringify to `[object Object]` through `JSON.stringify`.
 */
function jsonSafe(v: unknown): unknown {
  if (typeof v === 'bigint') return Number(v);
  if (Array.isArray(v)) return v.map(jsonSafe);
  if (v && typeof v === 'object') {
    const items = (v as { items?: unknown[] }).items;
    if (Array.isArray(items)) return items.map(jsonSafe);
    const entries = (v as { entries?: unknown }).entries;
    if (entries && typeof entries === 'object') return jsonSafe(entries);
    const out: Record<string, unknown> = {};
    for (const [k, val] of Object.entries(v as Record<string, unknown>)) out[k] = jsonSafe(val);
    return out;
  }
  return v;
}
