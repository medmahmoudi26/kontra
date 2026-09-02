/**
 * The read side of actor output: which tables exist, and which files belong to one dispatch.
 *
 * IDENTITY IS THE PATH NOW. `output/<actor>/version=<v>/dt=<dispatch>/…parquet` — the table
 * is named after the actor and the partition columns are the version and the dispatch time.
 * The previous scheme hashed `(actor, version, node)` into `ds_<sha1…>` and kept a
 * `_kontra_dataset` registry whose only purpose was translating that hash back into a name.
 * Both are gone: nothing here parses a path, and nothing needs a lookup table to answer
 * "whose output is this?".
 *
 * Operator lists (`kontra db`) live in a separate top-level directory, `standalone/`,
 * because they are inputs, not results.
 */

import type { DuckDBConnection } from '@duckdb/node-api';

import type { ObjectStore } from '../codec/objectStore';
import { datasetName } from './datasetName';
import type { DatasetDeviation } from './datasetRecords';
import type { MaterializationRecord } from './materialization';
import type { DispatchRef } from './materializationStore';
import type { RunWorkflow } from './runWorkflows';
import {
  LAKE,
  META,
  STANDALONE_SCHEMA,
  OUTPUT_SCHEMA,
  dtPartition,
  lakeConnection,
  lakeDataPath,
  lakeEnabled,
  parseDtPartition,
  resolveLakeConfig,
  safeName,
  type LakeConfig,
} from './parquet';

/** One column of a materialized table, as the CLI shows it before any query is written. */
export interface DatasetColumn {
  name: string;
  type: string;
}

/**
 * Everything is a dataset. An OUTPUT dataset is one actor's results for one dispatch,
 * addressed `<actor>` + `version` + `dt`; a STANDALONE dataset is an operator-loaded list
 * (`kontra dataset create`), addressed by name alone. They differ only in whether the
 * version/dt coordinates exist.
 */
export type DatasetKind = 'output' | 'standalone';

/**
 * A Dataset's lifecycle state (ADR 0023 §11).
 *
 * `open` is what a Dataset is until somebody declares otherwise — while its producer appends,
 * and equally after a producer died mid-append. `sealed` is a caller saying it is complete;
 * `abandoned` is a caller giving up on it. The distinction exists so a crashed Run leaves a
 * VISIBLY unfinished Dataset rather than a short one that reads as done.
 *
 * THE WORDS ARE THE CONTRACT, and they are spelled the same in four places with no shared code:
 * here, `frontend/src/datasets/state.ts` (the badge), `actorkit.catalog.DatasetWriter`
 * and `sdk/go/catalog`. A rename on one side does not fail — the reader falls back to
 * `open` — so a finished Dataset would silently render as one still being written.
 *
 * It lives as a small object beside the data rather than in the DuckLake catalog, because the
 * catalog's row count is a live SUM and cannot express "still appending".
 */
// THE LIFECYCLE WORDS ARE THE CONTRACT'S. Both TypeScript halves read the same union now — the
// browser used to declare its own as a bare `string`, so a new word compiled clean there and
// reached the code that interprets it unchecked. The two SDK spellings are a language boundary
// and are gated by a corpus instead (ADR 0035, rule two).
import type { DatasetState } from '../../contract/datasets';

export type { DatasetState };

/** Where one Dataset's lifecycle object lives in the object store. */
export function datasetStateKey(name: string): string {
  return `datasets/${name.replace(/[^A-Za-z0-9_.-]/g, '_')}/_state.json`;
}

/**
 * A temporary Dataset's ownership record (temp-datasets slice 01).
 *
 * A temporary Dataset is a durable Dataset in every mechanism — same per-chunk publish, same
 * `open`/`sealed`/`abandoned` lifecycle — except that it is framework-named, short-lived, and
 * OWNED by the Run that opened it. That ownership is the one fact the lifecycle work downstream
 * reads to answer "whose is this, and can it go", so it is recorded once at open and never derived
 * from the name.
 */
export interface DatasetOwner {
  /**
   * The owning Run's id — the caller workflow's id, which is what a **Run** IS (ADR 0023 §12), so
   * it addresses the Runs surface directly. PRESENCE of this record is also what marks a Dataset
   * temporary: a durable Dataset has none.
   */
  owner: string;
  /** When the temp was opened (epoch ms), for orphan accounting once the owning Run is long done. */
  createdAt: number;
}

/**
 * Where a temporary Dataset's ownership record lives — a SEPARATE object from the lifecycle state.
 *
 * Deliberately not folded into `_state.json`: `publishBatch` rewrites the state object to `open`
 * on every append, so an owner kept there would be clobbered by the first published Batch. Kept
 * apart, the marker is written once at open and untouched by publish and close, which is also what
 * lets it outlive a `sealed` temp so a later cleanup can still attribute an orphan to its Run.
 */
export function datasetOwnerKey(name: string): string {
  return `datasets/${name.replace(/[^A-Za-z0-9_.-]/g, '_')}/_owner.json`;
}

/** One dataset — the grain an operator addresses, and the grain the UI lists. */
export interface DatasetInfo {
  kind: DatasetKind;
  /** The actor's name for output; the list's name for standalone. Never a hash or node id. */
  name: string;
  /** Output only — a standalone list has no actor version. */
  version?: string;
  /** Output only. `YYYY-MM-DDTHH-MM-SS`, the dispatch and the directory it lives under. */
  dt?: string;
  rows: number;
  bytes: number;
  /** When this dataset's newest file was committed (epoch ms), from the catalog snapshot. */
  updatedAt?: number;
  /**
   * The lifecycle, when a caller's writer has recorded one. ABSENT means no writer ever touched
   * this Dataset — every pre-§11 output, and every operator-loaded list — which a reader must
   * not confuse with `open`: nothing is appending to it and nothing ever will.
   */
  state?: DatasetState;
  /**
   * TRUE for a temporary Dataset — framework-named, owned, short-lived (temp-datasets slice 01);
   * absent for a durable one. This is what lets `kontra dataset list` and the Datasets page
   * distinguish the two rather than show a flat list where `tmp_a7f3` reads like `lame`.
   */
  temporary?: boolean;
  /**
   * The owning Run's id, PRESENT only on a temporary Dataset. It is the address a reader follows
   * to "whose is this", and what every lifecycle decision downstream reads.
   */
  owner?: string;
  /**
   * The DERIVED, run-grain name (ADR 0029 §2) —
   * `wf-<workflow>-<version>--<dt>Z--<runFragment(runId)>`.
   *
   * ABSENT when nothing can say which single Run is behind the row (a standalone list; a partition
   * several Runs share), and absent too when the Run is known but its caller identity was never
   * recorded AND the Actor fallback does not apply — see {@link withDatasetNames}, which spells out
   * which authority resolved the Run and which of them licenses that fallback.
   *
   * It is NOT stored on the row — {@link withDatasetNames} renders it through the ONE
   * {@link datasetName} function, so the string is identical to the one the Datasets page and
   * `kontra dataset ls` show. See `data/datasetName.ts`.
   */
  datasetName?: string;
  /**
   * The ONE **Run** behind this row — the KEY a surface addresses to tag or rename this Dataset
   * (ADR 0029 §4, the record is keyed by `runId`). Set by {@link withDatasetNames}, and set on
   * strictly MORE rows than {@link DatasetInfo.datasetName}: knowing which Run wrote a Dataset does
   * not require knowing what that Run was CALLED. It is the caller workflow's id, which is what a
   * **Run** IS (ADR 0023 §12), so it is not a secret — `/api/datasets/runs` already returns it.
   *
   * PRESENT EXACTLY WHEN THE ROW HAS ONE RUN, and that is a structural guarantee rather than a
   * promise. A durable **Dataset** accumulates from many **Runs** — `lame` holds five **Runs**'
   * output on this box — so a single `runId` over a whole Dataset would become a lie the second
   * time anything promoted into it. The listing's grain saves it: a row is ONE `(name, version, dt)`
   * partition, and {@link DatasetInfo.contributingRuns} says which **Runs** wrote it. This field is
   * filled only when that set has exactly one member (or when the row is a temp, whose owner is the
   * answer before any row lands), and is ABSENT the moment two **Runs** share a partition. The
   * plural fact and the singular one therefore never contradict each other — ADR 0017's rule that
   * two authorities are never merged into one field, applied to a count rather than to a status.
   */
  runId?: string;
  /**
   * Every **Run** whose rows are in this partition, ascending — read from the CATALOG's own
   * per-file `run_id` statistics, never from the rows.
   *
   * WHY THE LAKE AND NOT THE LEDGER. The materialization ledger answers this for the graph
   * interpreter's dispatches and for nothing else: measured on this box, `/api/datasets/runs`
   * holds 50 dispatches and not one of them is a **Run** the actorkit path produced, because
   * `publishBatch` writes lake rows and no ledger record. Every **Dataset** a v2 **Run** writes —
   * every temp, and every durable **Dataset** promoted into — is therefore invisible to
   * {@link withDatasetNames}' ledger join, which is why `lame_demo` and `tmp_…` listed with no
   * `runId` and no name at all. The rows themselves have always known: `run_id` is stamped on
   * every materialized row (`data/parquet.ts`) and SURVIVES promotion, which copies it straight
   * off the source row rather than re-stamping the promoting workflow's.
   *
   * IT COSTS NO SCAN, which is the only reason it may sit on a listing poll. DuckLake keeps
   * `min_value`/`max_value` per data file per column in `ducklake_file_column_stats`, so the set
   * comes out of the SAME metadata query that already counts rows and bytes — no data file is
   * opened, and the "list of cards costs a lake scan" regression this function's header warns
   * about is not reintroduced. `datasetProvenance` remains the row-grain read, issued once when a
   * Dataset is opened, and is the only thing that counts rows per **Run**.
   *
   * ABSENT (not empty) when the table has no `run_id` column at all — an operator-loaded
   * standalone list — which is a different fact from "no **Run** wrote these rows".
   */
  contributingRuns?: string[];
  /**
   * TRUE when some data file in this partition spans MORE THAN ONE **Run**, so
   * {@link DatasetInfo.contributingRuns} is a LOWER BOUND rather than the whole set.
   *
   * A file's statistics are a min and a max, so a file holding two **Runs** contributes both
   * endpoints and hides anything between them. One publish writes one **Run**, and one promotion
   * out of a temp carries one **Run** (a temp is owned by exactly one), so every file on this box
   * measures `min = max`. A promotion whose SOURCE is a durable **Dataset** is the reachable way
   * to write a mixed file, and the flag exists so that case degrades into a stated bound instead
   * of into a set that quietly omits a **Run**. `runId` is refused whenever this is true.
   */
  contributingRunsPartial?: boolean;
  /**
   * The tag SET a **Run**'s Dataset carries (ADR 0029 §1), attached by {@link withDatasetDeviations}
   * from the Dataset record. ABSENT when the Dataset is untagged — the record stores only deviation,
   * so no tags means no field rather than an empty array to render. Sorted, so the surfaces show one
   * order.
   */
  tags?: string[];
  /**
   * The operator's (or author's) RENAME, when the record holds one (ADR 0029 §4). ABSENT means the
   * derived {@link datasetName} stands — a surface shows `renamedTo ?? datasetName`, which is how
   * "the derived default is used whenever no rename exists" (issue 01, issue 02) reaches the screen.
   */
  renamedTo?: string;
}

/** Files belonging to one actor's output for one dispatch. */
export interface DatasetFiles {
  actor: string;
  version: string;
  dt: string;
  table: string;
  keys: string[];
  columns: DatasetColumn[];
}

function lit(v: string): string {
  return `'${v.replace(/'/g, "''")}'`;
}

/** Column names and types of one dataset's table — actor output unless told otherwise. */
async function tableColumns(
  conn: DuckDBConnection,
  tbl: string,
  schema: string = OUTPUT_SCHEMA
): Promise<DatasetColumn[]> {
  const res = await conn.runAndReadAll(`DESCRIBE SELECT * FROM ${LAKE}.${schema}."${tbl}"`);
  return res.getRows().map((r) => ({ name: String(r[0]), type: String(r[1]) }));
}

/** Every actor that has output, from the catalog — no data file is opened. */
export async function listOutputActors(
  store: ObjectStore,
  override: Partial<LakeConfig> = {}
): Promise<string[]> {
  if (!lakeEnabled(store, override)) return [];
  const conn = await lakeConnection(store, resolveLakeConfig(store, override));
  const res = await conn.runAndReadAll(
    `SELECT name FROM (SHOW ALL TABLES) WHERE database = ${lit(LAKE)} ` +
      `AND schema = ${lit(OUTPUT_SCHEMA)} ORDER BY name`
  );
  return res.getRows().map((r) => String(r[0]));
}

/**
 * List every dataset — output dispatches and standalone lists — newest first.
 *
 * READS ONLY CATALOG METADATA. No data file is opened, so the cost is a handful of Postgres
 * rows regardless of how much output exists. The previous implementation ran
 * `count(*), list(DISTINCT node), max(run_started_at) … GROUP BY version, dt` against each
 * actor's TABLE: `count(*)` alone can come from Parquet footers, but the other two force full
 * reads of two columns across every file in the lake. On the 22-row dev dataset that is
 * invisible; on a real crawl it is the whole lake scanned to draw a list of cards.
 *
 * Row counts and byte sizes come from `ducklake_data_file`, and freshness from the snapshot
 * that committed each file — the same figures the Grafana `kontra_runs` view reports, so the
 * two surfaces cannot disagree.
 *
 * Dispatch ids are deliberately absent. Several Method calls of one Actor in one Run are ONE
 * dataset; `n1/n2/n3` is a per-call label that must not appear in an address. Explore still
 * surfaces them for diagnosis, sourced from the materialization ledger, not from a scan.
 */
export async function listDatasets(
  store: ObjectStore,
  sel: { name?: string; version?: string; dt?: string; kind?: DatasetKind } = {},
  override: Partial<LakeConfig> = {}
): Promise<DatasetInfo[]> {
  if (!lakeEnabled(store, override)) return [];
  const cfg = resolveLakeConfig(store, override);
  const conn = await lakeConnection(store, cfg);
  const meta = `${META}.${cfg.metaSchema}`;

  const where = [`df.end_snapshot IS NULL`, `t.end_snapshot IS NULL`];
  if (sel.name) where.push(`t.table_name = ${lit(safeName(sel.name))}`);
  if (sel.kind) where.push(`sc.schema_name = ${lit(schemaOf(sel.kind))}`);

  // Partition values arrive one ROW PER COLUMN, so they are pivoted on `partition_key_index`
  // (0 = version, 1 = dt — the PARTITIONED BY order). A standalone table has no partitions,
  // hence the LEFT JOIN and the NULL coordinates it yields.
  //
  // THE CONTRIBUTING RUNS RIDE THE SAME METADATA READ (see DatasetInfo.contributingRuns). DuckLake
  // keeps `min_value`/`max_value` per data file per column, so the set of **Runs** behind a
  // partition costs statistics rows and NOT a scan of the column. `end_snapshot IS NULL` keeps a
  // dropped-and-recreated column from matching twice, and `parent_column IS NULL` keeps a nested
  // field that happens to be spelled `run_id` out of it.
  //
  // AGGREGATED BEFORE IT IS JOINED, and that is not a style choice: MEASURED against the local
  // catalog (5,797 data files, 100,599 statistics rows), joining the raw statistics table into the
  // per-file scan took the metadata query from 34 ms to 280 ms, while narrowing it to the `run_id`
  // columns and folding it to one row per file FIRST costs 1.5 ms. DuckLake ships no index on
  // `ducklake_file_column_stats`, so anything that touches it unfiltered is a full scan of every
  // column's statistics — 8× the whole listing's cost, on a poll, to answer one question.
  const res = await conn.runAndReadAll(
    `WITH run_col AS (
       SELECT table_id, column_id
         FROM ${meta}.ducklake_column
        WHERE column_name = ${lit(RUN_COLUMN)} AND end_snapshot IS NULL AND parent_column IS NULL
     ),
     run_stats AS (
       SELECT rs.data_file_id,
              min(rs.min_value) AS run_lo,
              max(rs.max_value) AS run_hi
         FROM ${meta}.ducklake_file_column_stats rs
         JOIN run_col rc ON rc.table_id = rs.table_id AND rc.column_id = rs.column_id
        GROUP BY rs.data_file_id
     ),
     per_file AS (
       SELECT df.data_file_id,
              sc.schema_name AS schema_name,
              t.table_name   AS name,
              df.record_count     AS rows,
              df.file_size_bytes  AS bytes,
              s.snapshot_time     AS committed_at,
              -- Already one row per data file — run_stats groups by it, and a data file id is
              -- globally unique — but AGGREGATED anyway, like the partition pivot beside it, so
              -- per_file cannot split a file into two groups and DOUBLE the sum(rows) below. A
              -- wrong row count is a far worse failure than a coarse run set, and that sum is what
              -- deleteTemporaryDataset reports to an operator as the space it freed.
              min(rst.run_lo)     AS run_lo,
              max(rst.run_hi)     AS run_hi,
              max(CASE WHEN pv.partition_key_index = 0 THEN pv.partition_value END) AS version,
              max(CASE WHEN pv.partition_key_index = 1 THEN pv.partition_value END) AS dt
         FROM ${meta}.ducklake_data_file df
         JOIN ${meta}.ducklake_table t  ON t.table_id  = df.table_id
         JOIN ${meta}.ducklake_schema sc ON sc.schema_id = t.schema_id
         JOIN ${meta}.ducklake_snapshot s ON s.snapshot_id = df.begin_snapshot
         LEFT JOIN ${meta}.ducklake_file_partition_value pv ON pv.data_file_id = df.data_file_id
         LEFT JOIN run_stats rst ON rst.data_file_id = df.data_file_id
        WHERE ${where.join(' AND ')}
        GROUP BY ALL
     )
     SELECT schema_name, name, version, dt,
            sum(rows) AS rows, sum(bytes) AS bytes,
            epoch_ms(max(committed_at)) AS updated,
            to_json(list_sort(list_distinct(
              coalesce(list(run_lo) FILTER (WHERE run_lo IS NOT NULL), CAST([] AS VARCHAR[]))
              || coalesce(list(run_hi) FILTER (WHERE run_hi IS NOT NULL), CAST([] AS VARCHAR[]))
            ))) AS runs,
            coalesce(bool_or(run_lo IS DISTINCT FROM run_hi), false) AS run_span
       FROM per_file
      WHERE schema_name IN (${lit(OUTPUT_SCHEMA)}, ${lit(STANDALONE_SCHEMA)})
        ${sel.version ? `AND version = ${lit(sel.version)}` : ''}
        ${sel.dt ? `AND dt LIKE ${lit(sel.dt + '%')}` : ''}
      GROUP BY ALL
      ORDER BY dt DESC NULLS LAST, name`
  );

  const rows = res.getRows();

  // The lifecycle is keyed by NAME, so one read per distinct name rather than per row: an actor
  // with fifty dispatches is one object, not fifty. Read once here and hand it to every row of
  // that name — the alternative is a UI that lists a Dataset without saying whether anyone has
  // finished writing it, which is the single thing §11 exists to show.
  const names = [...new Set(rows.map((r) => String(r[1])))];
  const states = new Map<string, DatasetState>();
  const owners = new Map<string, string>();
  await Promise.all(
    names.map(async (name) => {
      // Two small object reads per distinct name, not per row: the lifecycle (§11) and the
      // temporary-ownership marker (slice 01) live in separate objects for the clobber reason
      // datasetOwnerKey documents, so both are read and handed to every row of that name.
      const [state, owner] = await Promise.all([
        readDatasetState(store, name),
        readDatasetOwner(store, name),
      ]);
      if (state) states.set(name, state);
      if (owner) owners.set(name, owner.owner);
    })
  );

  return rows.map((r) => {
    const kind: DatasetKind = String(r[0]) === STANDALONE_SCHEMA ? 'standalone' : 'output';
    const name = String(r[1]);
    const owner = owners.get(name);
    const contributingRuns = parseRunList(r[7]);
    return {
      kind,
      name,
      state: states.get(name),
      // Ownership is the marker, never the `tmp_` name prefix: a durable Dataset that happens to
      // start `tmp_` is not temporary, and a temp is temporary because its Run recorded it so.
      temporary: owner === undefined ? undefined : true,
      owner,
      // Undefined rather than null or '': a standalone list HAS no version, and a UI that
      // renders `v` + an empty string is how "unknown" becomes indistinguishable from "n/a".
      version: r[2] === null || r[2] === undefined ? undefined : String(r[2]),
      dt: r[3] === null || r[3] === undefined ? undefined : String(r[3]),
      rows: Number(r[4] ?? 0),
      bytes: Number(r[5] ?? 0),
      updatedAt: r[6] === null || r[6] === undefined ? undefined : Number(r[6]),
      // Absent, not empty, when the table carries no `run_id` at all — "this kind of Dataset
      // cannot say" is a different fact from "no Run wrote these rows" (see the field doc).
      ...(contributingRuns === undefined ? {} : { contributingRuns }),
      ...(r[8] === true ? { contributingRunsPartial: true } : {}),
    };
  });
}

/**
 * The `run_id` statistics of one partition, as the ordered set of **Runs** they name.
 *
 * The query hands them over as a JSON array rather than as a DuckDB LIST value, because a run id
 * is the caller's string — `--id` accepts anything Temporal accepts — so no separator is safe to
 * `string_agg` on and no client-side list shape is worth depending on. `undefined` means the
 * table has no `run_id` column; `[]` cannot occur (a file with the column always has statistics
 * for it, and a table with no files has no listing row).
 */
function parseRunList(raw: unknown): string[] | undefined {
  if (raw === null || raw === undefined) return undefined;
  try {
    const parsed: unknown = JSON.parse(String(raw));
    if (!Array.isArray(parsed)) return undefined;
    const runs = parsed.filter((v): v is string => typeof v === 'string' && v !== '');
    return runs.length === 0 ? undefined : runs;
  } catch {
    return undefined;
  }
}

/**
 * Attach the DERIVED run-grain name (ADR 0029 §2) to each output row it can, from the ledger's
 * DispatchRefs and the **Run**'s recorded workflow identity.
 *
 * WHY NOT IN {@link listDatasets}. That reads the lake and NOTHING else (no Temporal, no run
 * record — the `/api/datasets` contract), and the run behind a `(name, version, dt)` partition is a
 * ledger fact, a different authority. Keeping the join here leaves the listing a pure catalog scan
 * and makes this a pure, total function over three plain arrays — testable without a store, and
 * best-effort: a row whose Run does not resolve is returned UNNAMED rather than dropped.
 *
 * MATCHED ON `(actor, version, dt)`, the partition triple both sides already agree on: the ledger's
 * `dt` is `dtPartition(min(run_started_at))` and the catalog's is the partition value, the same
 * second-precision string. `safeName` is applied to both actor spellings because the catalog holds
 * the table name (already safe) while the ledger holds the raw Actor name. When two Runs share one
 * partition — the same Actor version started twice in one second — the newest wins; the run-id
 * fragment in the name is exactly what still tells two such Runs apart in {@link datasetName}.
 *
 * THE IDENTITY IN THE NAME IS THE CALLER WORKFLOW'S, and `identities` is where it comes from —
 * snapshotted at start into `data/runWorkflows.ts`, keyed by the same `runId` the ledger resolves.
 * This is what makes ONE Run writing TWO actor tables render ONE name, which is the Consequence ADR
 * 0029 states ("the name labels the run's output, which may span several actor tables") and which an
 * Actor-grain identity structurally cannot satisfy — two tables, two names, for one Run.
 *
 * THE FALLBACK IS DELIBERATE AND PERMANENT. A Run with no recorded identity — every Dataset written
 * before that store existed, and any Run whose stamp did not land — falls back to the PRODUCING
 * ACTOR's name and version, which is exactly what this join rendered before. A Dataset must always
 * say something it is called; an omitted name would blank the column on every historical row. The
 * cost of the fallback is the old behaviour (actor-grain, so one Run's two tables differ), which is
 * strictly better than nothing and is visible for what it is.
 *
 * THREE AUTHORITIES CAN NAME THE RUN, TRIED IN THIS ORDER, and they are not interchangeable —
 * each knows something the others structurally cannot:
 *
 *   1. THE LEDGER's DispatchRef, matched on the partition triple. The only one that resolves an
 *      Actor's output table, and the only one that exists for a Dataset written before `run_id`
 *      was stamped on rows.
 *   2. THE TEMP's OWNER MARKER, recorded at open (`datasetOwnerKey`). The only one that answers
 *      BEFORE any row lands, so an empty temp still says whose it is, and the only one that is an
 *      OWNERSHIP fact rather than a fact about rows.
 *   3. THE LAKE's own `run_id` statistics ({@link DatasetInfo.contributingRuns}), and ONLY when
 *      they name exactly one Run. This is the one that matters now: measured on this box, the
 *      ledger holds no dispatch for any Run the actorkit path started, so 1 resolves nothing for
 *      a v2 Run and `lame_demo` — 430 rows promoted out of one Run's temp — listed with no name
 *      and no run at all. The rows knew the whole time.
 *
 * A row whose statistics name TWO OR MORE Runs is deliberately left with no `runId` and no name:
 * that is a partition several Runs share, one name for it would be a lie, and the honest plural is
 * already on the row. See {@link DatasetInfo.runId}.
 *
 * THE NAME'S DATETIME COMES FROM THE ROW'S OWN `dt`, one rule for all three paths. A `dt=`
 * partition value IS `dtPartition(runStartedAt)` — that is how `writeDatasetParquet` computes it —
 * so feeding it back through `parseDtPartition` renders the byte-identical string the ledger path
 * rendered from `runStartedAt`, and authorities 2 and 3 (which have no run-start timestamp at all)
 * need no second source for it. The alternative was a name whose datetime came from the ledger for
 * one row and from somewhere else for the next.
 *
 * THE ACTOR FALLBACK BELONGS TO AUTHORITY 1 ONLY. When no caller identity was recorded, a
 * ledger-matched row still renders a name from the PRODUCING ACTOR — `info.name` IS that Actor's
 * table there, which is exactly what this join rendered before the identity store existed, and is
 * why every historical Dataset still says something it is called. On authorities 2 and 3 that same
 * field is a temp's storage name or a durable Dataset's own name, so the fallback is REFUSED: such
 * a row carries its `runId` and no name until its Run's identity is recorded. A name asserting a
 * workflow called `tmp_nscheck-…` would be worse than none, because it reads like one that existed.
 *
 * WHAT STAYS UNNAMED, on purpose: a standalone list has no Run and no `run_id` column; a partition
 * with no version or no dt has no name to render; a partition several Runs wrote is named by none
 * of them; and a row resolved from the lake or from an owner marker whose Run was never stamped.
 * All ABSENT rather than guessed.
 */
export function withDatasetNames(
  infos: readonly DatasetInfo[],
  dispatches: readonly DispatchRef[],
  identities: readonly RunWorkflow[] = []
): DatasetInfo[] {
  const runs = new Map<string, { runId: string; runStartedAt: number }>();
  for (const d of dispatches) {
    const key = `${safeName(d.actor)}\0${d.version}\0${d.dt}`;
    const prev = runs.get(key);
    if (!prev || d.runStartedAt > prev.runStartedAt) {
      runs.set(key, { runId: d.runId, runStartedAt: d.runStartedAt });
    }
  }
  const callers = new Map<string, RunWorkflow>();
  for (const i of identities) callers.set(i.runId, i);
  return infos.map((info) => {
    // A name needs a manifest identity (name + version) and a Run; a standalone list and a
    // version-less row have no Run to resolve against, so they are returned as they arrived.
    if (info.kind !== 'output' || !info.dt || !info.version) return info;
    // The ledger first, then the row itself. Which one answered is remembered, because only the
    // ledger's answer licenses the Actor fallback below.
    const dispatched = runs.get(`${safeName(info.name)}\0${info.version}\0${info.dt}`)?.runId;
    const runId = dispatched ?? soleRun(info);
    if (!runId) return info;
    // The caller workflow when it was recorded, the producing Actor when it was not. Read as ONE
    // choice rather than two `??`s so the two halves can never come from different sources — a
    // workflow name paired with an actor version would be a manifest identity that never existed.
    const caller = callers.get(runId);
    // THE ACTOR FALLBACK IS THE LEDGER'S ALONE. `info.name` is a producing Actor's table only when
    // a DispatchRef matched it; on the other two paths it is a TEMPORARY Dataset's storage name or
    // a durable Dataset's own name, and rendering `wf-tmp_nscheck-…-0.1.0--…` or `wf-lame_demo-…`
    // would state a workflow identity that never existed — worse than saying nothing, because it
    // reads exactly like one that did. Such a row keeps its `runId`, which is the traceability that
    // matters, and gets a name as soon as its Run's identity is recorded (both start paths do).
    if (!caller && !dispatched) return { ...info, runId };
    return {
      ...info,
      // The Run id travels onto the row too, not only its 6-char fragment inside the name: it is the
      // KEY a tag/rename mutation addresses (ADR 0029 §4), so a surface holding a listing row can act
      // on the record without a second lookup. {@link withDatasetDeviations} keys on exactly this.
      runId,
      datasetName: datasetName({
        workflow: caller ? caller.workflow : info.name,
        version: caller ? caller.version : info.version,
        runStartedAt: parseDtPartition(info.dt),
        runId,
      }),
    };
  });
}

/**
 * The ONE **Run** behind a listing row when the row itself knows — its owner marker, or its
 * `run_id` statistics naming exactly one.
 *
 * THE OWNER WINS OVER THE STATISTICS, and they can legitimately differ in one direction only: a
 * temp with no rows yet has an owner and no statistics. If they ever disagreed about a non-empty
 * temp it would mean a **Run** published into another **Run**'s temp, which nothing can do — the
 * temp's name is minted inside the owning workflow and handed to Method calls of that workflow.
 * The marker is preferred because it is the earlier and more direct fact.
 *
 * SEVERAL CONTRIBUTORS RESOLVE NOTHING. That is the whole discipline of {@link DatasetInfo.runId}:
 * the singular field exists only where a singular answer does.
 */
function soleRun(info: DatasetInfo): string | undefined {
  if (info.owner) return info.owner;
  if (info.contributingRunsPartial) return undefined;
  const runs = info.contributingRuns;
  return runs && runs.length === 1 ? runs[0] : undefined;
}

/**
 * Every **Run** a page of listing rows could resolve to — what to ask `runWorkflows` for.
 *
 * The name renders the CALLER WORKFLOW's identity, which lives in a store keyed by `runId`, so the
 * read side has to know the ids BEFORE {@link withDatasetNames} runs. The ledger's DispatchRefs
 * supply some of them and the rows themselves supply the rest (a temp's owner, a partition's sole
 * contributor); asking for the union in ONE round trip is what keeps a v2 Run's Dataset from
 * rendering the Actor-grain fallback purely because nobody asked about its Run.
 */
export function datasetRunIds(
  infos: readonly DatasetInfo[],
  dispatches: readonly DispatchRef[] = []
): string[] {
  const ids = new Set<string>();
  for (const d of dispatches) if (d.runId) ids.add(d.runId);
  for (const info of infos) {
    const sole = soleRun(info);
    if (sole) ids.add(sole);
  }
  return [...ids];
}

/**
 * Attach a Dataset's stored TAGS and RENAME (ADR 0029 §1, §4) to each row whose **Run** has one,
 * from the Dataset record — a SEPARATE authority from both the lake and the ledger.
 *
 * WHY A SECOND PURE JOIN. {@link withDatasetNames} rendered the DERIVED default and stamped the
 * `runId` on each row; this keys on that `runId` and layers the DEVIATION over it. Two functions, not
 * one, because they read two authorities: the derived name is a rendering of ledger facts, while the
 * deviation is what an operator chose and lives in `data/datasetRecords.ts`. Keeping this a pure,
 * total function over two plain arrays makes it testable without a store, and best-effort: a row
 * whose deviation is not supplied is returned as it arrived — derived name, no tags.
 *
 * ONLY DEVIATION IS ATTACHED. A Run with no record contributes nothing, so its row keeps the derived
 * name and carries no `tags` — which is what "an untagged, un-renamed Dataset has no row" looks like
 * from the read side. `renamedTo` is layered BESIDE `datasetName` rather than replacing it: the
 * derived string stays available (issue 01's invariant), and the surfaces render `renamedTo ??
 * datasetName` so the default is used whenever no rename exists.
 */
export function withDatasetDeviations(
  infos: readonly DatasetInfo[],
  deviations: readonly DatasetDeviation[]
): DatasetInfo[] {
  const byRun = new Map<string, DatasetDeviation>();
  for (const d of deviations) byRun.set(d.runId, d);
  return infos.map((info) => {
    if (!info.runId) return info;
    const dev = byRun.get(info.runId);
    if (!dev) return info;
    const next = { ...info };
    // Attach only the halves that exist — the deviation carries at least one, never neither.
    if (dev.tags.length > 0) next.tags = dev.tags;
    if (dev.renamedTo !== undefined) next.renamedTo = dev.renamedTo;
    return next;
  });
}

/**
 * One Dataset's recorded lifecycle, or undefined when nothing ever wrote one.
 *
 * Best-effort on purpose: an object store that is unreachable, or a state object written by a
 * newer peer in a spelling this build does not know, must not fail the LISTING. A Dataset with
 * no readable state is listed without one, which the UI renders as "no lifecycle recorded" —
 * strictly better than a 502 that hides every other Dataset too.
 */
async function readDatasetState(
  store: ObjectStore,
  name: string
): Promise<DatasetState | undefined> {
  try {
    const body = await store.get(datasetStateKey(name));
    if (!body) return undefined;
    const parsed: unknown = JSON.parse(Buffer.from(body).toString('utf8'));
    const state = (parsed as { state?: string })?.state;
    return state === 'open' || state === 'sealed' || state === 'abandoned' ? state : undefined;
  } catch {
    return undefined;
  }
}

/**
 * One temporary Dataset's ownership record, or undefined when it is a durable Dataset.
 *
 * Best-effort on the same terms as {@link readDatasetState}: an unreadable or unknown-shaped
 * marker leaves the Dataset listed WITHOUT an owner rather than failing the whole listing. A temp
 * that lost its owner reads as durable, which is the safe direction — it is never mistaken for
 * one whose Run can be reclaimed.
 */
async function readDatasetOwner(
  store: ObjectStore,
  name: string
): Promise<DatasetOwner | undefined> {
  try {
    const body = await store.get(datasetOwnerKey(name));
    if (!body) return undefined;
    const parsed: unknown = JSON.parse(Buffer.from(body).toString('utf8'));
    const owner = (parsed as { owner?: unknown })?.owner;
    if (typeof owner !== 'string' || owner === '') return undefined;
    const createdAt = Number((parsed as { createdAt?: unknown })?.createdAt) || 0;
    return { owner, createdAt };
  } catch {
    return undefined;
  }
}

/** The DuckLake schema a dataset kind lives in. */
export function schemaOf(kind: DatasetKind): string {
  return kind === 'standalone' ? STANDALONE_SCHEMA : OUTPUT_SCHEMA;
}

/**
 * Refusal to delete a Dataset that is not a **Run**'s temporary one (temp-datasets slice 03).
 *
 * The one guardrail deletion-by-name rests on: only a Dataset a **Run** recorded as its own (the
 * `_owner.json` marker slice 01 writes) may be dropped by name. A durable **Dataset** — including
 * one a temp was PROMOTED into — has no owner and is refused, so neither the CLI nor slice 04's UI
 * button can destroy the record by aiming a delete at the wrong name. A durable Dataset ages out by
 * retention, never by a delete-by-name. Typed rather than string-matched so the route can map it to
 * a 409 without parsing a message.
 */
export class NotTemporaryDatasetError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'NotTemporaryDatasetError';
  }
}

/**
 * THE CATALOG DOES NOT LIST THAT NAME — the caller's mistake, not the lake's failure.
 *
 * Typed for the reason stated two paragraphs up and for a sharper one. The preview and provenance
 * routes used to tell this apart from a broker outage with `msg.startsWith('no ')`, which made the
 * difference between a 404 and a 502 a property of the ENGLISH in the sentence below: rewording it
 * to `"<name>" is not in the catalog` turns every missing dataset into "kontra is broken", with
 * every test still green because the message is what they assert on.
 *
 * `isWorkflowNotFound` in `infraRoutes.ts` already says this out loud about the Temporal errors it
 * classifies — "matched by TYPE, not by message text, so a reworded SDK error cannot silently turn
 * 404s back into 502s". It cannot be reused here: it tests for `WorkflowNotFoundError`, and a
 * missing Dataset is not a missing workflow. This is the same rule applied to the other authority.
 *
 * THE MESSAGE IS UNCHANGED AND MUST STAY SO. The console prints the server's own sentence when it
 * has one (`frontend/src/datasets/provenance.ts`), so the wording is a rendered string as well as
 * a classification — which is exactly why the classification must not be the wording.
 */
export class NoSuchDatasetError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'NoSuchDatasetError';
  }
}

/** The one sentence both catalog lookups raise, so the two cannot drift apart. */
function noSuchDataset(sel: { kind: DatasetKind; name: string }): NoSuchDatasetError {
  return new NoSuchDatasetError(`no ${sel.kind} dataset named "${sel.name}"`);
}

/** What a temporary-Dataset deletion recovered, for the operator who asked for it. */
export interface DeletedDataset {
  name: string;
  /** Rows the catalog held for this name at the moment of deletion. */
  rows: number;
  /** Bytes those rows' data files occupied, from the catalog — what the drop freed. */
  bytes: number;
  /** The owning Run the deleted temp was attributed to. */
  owner: string;
}

/**
 * DELETE a temporary Dataset — the explicit half of the orphan policy (temp-datasets slice 03).
 *
 * THE ORPHAN POLICY IS EXPLICIT-ONLY, and this is the whole of the mechanism. A temp is never swept
 * on a clock and never dropped when its owning **Run** closes: the entire point of a temp outliving
 * its fleet is that triage — and the promotion that follows — happens LATER, and a delete-at-Run-
 * close would destroy exactly the case the feature exists for. So a temp goes only when a person (or
 * the slice 04 button that calls the route over this) says so, having seen its owner and age in
 * `kontra dataset list`. A TTL sweep and a promotion-implies-drop rule were both weighed and
 * rejected: a TTL races the operator's attention on work whose "later" is unbounded, and dropping on
 * promotion re-couples the two acts slice 02 spent its effort separating (and would take rows a
 * second, wider promotion still needs — promotion is deliberately non-idempotent).
 *
 * TEMPORARIES ONLY. The owner marker (never the `tmp_` name prefix) is the authority on temp-ness,
 * so a durable Dataset is refused with {@link NotTemporaryDatasetError}. Deleting a temp leaves any
 * Dataset PROMOTED FROM it fully intact, because promotion is a COPY into the target's own files
 * (slice 02), not a re-registration that shares them — dropping the source's files cannot reach the
 * target's rows.
 *
 * The drop is `DROP TABLE`, the same catalog-level removal `kontra db delete` performs on a
 * standalone list: the rows and the catalog entry go, and the physical parquet is reclaimed by
 * DuckLake's own file cleanup, not here. A temp whose Run opened it but never published a row has an
 * owner marker and no table — its marker is still cleaned, freeing zero rows rather than erroring.
 */
export async function deleteTemporaryDataset(
  store: ObjectStore,
  name: string,
  override: Partial<LakeConfig> = {}
): Promise<DeletedDataset> {
  const owner = await readDatasetOwner(store, name);
  if (!owner) {
    throw new NotTemporaryDatasetError(
      `"${name}" is not a temporary Dataset — only a Run's temporary Dataset is deleted by name. ` +
        `A durable Dataset, including one promoted from a temp, ages out by retention.`
    );
  }

  // Measure BEFORE the drop, from catalog metadata only (no data-file scan): the freed rows/bytes
  // are what the operator is told the deletion recovered. Summed across the name's partitions.
  const infos = await listDatasets(store, { name }, override);
  const rows = infos.reduce((sum, i) => sum + i.rows, 0);
  const bytes = infos.reduce((sum, i) => sum + i.bytes, 0);

  // Drop the table. A temp materializes in the OUTPUT schema exactly as a durable Dataset does
  // (slice 01: a temp is a DatasetWriter, not a new write path), so it is dropped from there. The
  // name is checked against the catalog before it is interpolated, the same discipline every read
  // here follows — an identifier cannot be parameterised.
  if (lakeEnabled(store, override)) {
    const conn = await lakeConnection(store, resolveLakeConfig(store, override));
    const table = safeName(name);
    if (await tableInCatalog(conn, OUTPUT_SCHEMA, table)) {
      await conn.run(`DROP TABLE ${LAKE}.${OUTPUT_SCHEMA}."${table}"`);
    }
  }

  // Remove BOTH markers so the next listing does not resurrect a phantom temp: the ownership record
  // (slice 01) and the lifecycle state (§11). Best-effort — a store hiccup cleaning these must not
  // read as the drop above having failed, which it did not.
  await store.delete(datasetOwnerKey(name)).catch(() => undefined);
  await store.delete(datasetStateKey(name)).catch(() => undefined);

  return { name, rows, bytes, owner: owner.owner };
}

/**
 * A bounded preview of one dataset, read SERVER-SIDE.
 *
 * The browser used to do this itself: presign every parquet file, download 36 MB of DuckDB-WASM,
 * and range-read them. For a capped `LIMIT` that is an enormous amount of machinery — and it put
 * object-store URLs in front of a page that only ever renders a grid. The server already holds
 * an attached read connection, so it answers directly and the browser receives JSON.
 *
 * BOUNDED BY CONSTRUCTION: `limit` is clamped, and `version`/`dt` are partition columns, so a
 * scoped preview prunes to one dispatch's directory instead of touching the actor's whole table.
 * Deep analysis is still `kontra dataset query`, on the operator's own machine.
 */
export async function previewDataset(
  store: ObjectStore,
  sel: { kind: DatasetKind; name: string; version?: string; dt?: string; limit?: number },
  override: Partial<LakeConfig> = {}
): Promise<{ columns: DatasetColumn[]; rows: unknown[][] }> {
  if (!lakeEnabled(store, override)) return { columns: [], rows: [] };
  const conn = await lakeConnection(store, resolveLakeConfig(store, override));
  const table = safeName(sel.name);
  const schema = schemaOf(sel.kind);
  if (!(await tableInCatalog(conn, schema, table))) {
    throw noSuchDataset(sel);
  }

  const where: string[] = [];
  if (sel.version) where.push(`version = ${lit(sel.version)}`);
  if (sel.dt) where.push(`dt LIKE ${lit(sel.dt + '%')}`);
  const limit = Math.min(Math.max(Math.trunc(sel.limit ?? PREVIEW_ROWS), 1), PREVIEW_MAX_ROWS);

  const res = await conn.runAndReadAll(
    `SELECT * FROM ${LAKE}.${schema}."${table}"
      ${where.length ? `WHERE ${where.join(' AND ')}` : ''} LIMIT ${limit}`
  );
  const columns = res.columnNames().map((name, i) => ({ name, type: String(res.columnTypes()[i]) }));
  return { columns, rows: res.getRows().map((row) => row.map(jsonSafe)) };
}

/** Default preview size, and the ceiling a caller may ask for. */
export const PREVIEW_ROWS = 500;
export const PREVIEW_MAX_ROWS = 5000;

/**
 * Whether the catalog lists this table.
 *
 * A dataset name reaches these routes from a URL path segment, and an identifier cannot be
 * parameterised — so the only safe identifier is one the catalog already lists. Checked rather
 * than escaped, in every read that interpolates a name.
 */
async function tableInCatalog(
  conn: DuckDBConnection,
  schema: string,
  table: string
): Promise<boolean> {
  const known = await conn.runAndReadAll(
    `SELECT count(*) FROM (SHOW ALL TABLES) WHERE database = ${lit(LAKE)} ` +
      `AND schema = ${lit(schema)} AND name = ${lit(table)}`
  );
  return Number(known.getRows()[0]?.[0] ?? 0) > 0;
}

/**
 * One bucket of a Dataset's provenance: the rows that carry this exact (Machine, Actor version,
 * Run) triple. VERBATIM — the values are whatever the lake holds, and nothing here reinterprets
 * them.
 */
export interface ProvenanceGroup {
  /**
   * The **Machine** that ran the **Method** which produced these rows, read straight out of the
   * `node` column — `MaterializeSelector.node` in `data/parquet.ts` carries the long form of why
   * that column is its home. `null` is UNRECORDED (nothing knew the Machine), which is a
   * different fact from the literal `'w'` the publish activity substituted before provenance
   * travelled with the **Batch**. Telling those apart is the reader's job, and
   * `datasets/provenance.ts` in the web app is the only thing that may.
   */
  machine: string | null;
  /** The **Actor** version that produced them. `null` is unrecorded, on the same terms. */
  version: string | null;
  /**
   * The **Run** that appended these rows, from the `run_id` column — the caller's workflow id,
   * which is what a **Run** IS (ADR 0023 §12), so it addresses the Runs surface directly.
   *
   * THIS IS WHAT MAKES A COUNT SAYABLE. A Dataset name spans **Runs**: `SELECT count(*) FROM lame`
   * returned 1,246 for two Runs of one workflow while the listing showed 623 for one of them.
   * Both correct, neither labelled. Bucketing by the Run is what lets one screen say "623 of
   * 1,246 rows, from this Run" out of a single statement instead of leaving the operator to
   * divide two numbers nobody told them the scope of. `null` is unrecorded, on the same terms as
   * the pair above.
   */
  run: string | null;
  rows: number;
}

/** What one Dataset's rows say about who wrote them. */
export interface DatasetProvenance {
  name: string;
  kind: DatasetKind;
  /**
   * Rows the group-by counted, summed by DuckDB IN THE SAME STATEMENT as the buckets.
   *
   * Not a second `SELECT count(*)`. A Dataset a **Run** is still appending to grows between two
   * queries, so a share computed from two separately-issued counts is wrong by however much
   * landed in between — this repo has shipped that bug before. One statement, one snapshot,
   * and `rows` is exactly the sum of {@link groups}.
   */
  rows: number;
  /**
   * One per distinct (Machine, version, Run) triple, largest first. Empty when the Dataset is
   * empty.
   */
  groups: ProvenanceGroup[];
  /**
   * Whether the table HAS the Machine/version columns at all.
   *
   * False for an operator-loaded list (`kontra dataset create`), which has neither — a fact
   * distinct from "every row is unrecorded", and one the console must not render as a Dataset
   * whose Machines were lost.
   */
  carriesProvenance: boolean;
  /**
   * Whether the table has `run_id`, and so whether a count here can be scoped to one **Run**.
   *
   * SEPARATE FROM {@link carriesProvenance} because the columns arrived separately: `run_id` has
   * been written since the first typed materialization, while `node`/`version` only became the
   * Machine and the Actor version in 2fcf4bf. A table with one and not the other is answerable
   * about the dimension it carries and silent about the other — which is not the same as being
   * silent about both.
   */
  carriesRun: boolean;
  /**
   * When the ONE statement behind {@link rows} and {@link groups} answered (epoch ms, server
   * clock).
   *
   * A count of a Dataset a **Run** is still appending to is true of a MOMENT and not of the
   * Dataset, so the moment travels with it and the console prints it. The alternative — a browser
   * stamping the response as it lands — dates the number by when it was drawn rather than by when
   * it was measured, which is the same class of lie as the unlabelled scope.
   */
  measuredAt: number;
}

/** The two columns that carry Machine and Actor version. Both, or the table carries neither. */
const PROVENANCE_COLUMNS = ['node', 'version'] as const;

/** The column that carries the **Run** — see {@link ProvenanceGroup.run}. */
const RUN_COLUMN = 'run_id';

/**
 * Which **Machines** wrote a Dataset, and how many rows each contributed.
 *
 * THE QUESTION THE RUN STATUS CANNOT ANSWER. A four-Machine `nscheck` run put 1,246 rows into
 * `lame` and reported `completed`; nothing in the console could say whether all four Machines
 * had produced any of them. "Four were asked, three produced rows" is the shape of every
 * silent-failure incident this repo has had, and this is the read that makes it sayable.
 *
 * SCOPED TO THE WHOLE NAMED DATASET, deliberately, because that is what the console's editor
 * reads: opening a Dataset runs `SELECT * FROM <name>`, over every dispatch, with no partition
 * filter. A panel that counted one dispatch while the grid below it showed another's rows would
 * be two numbers for one screen — the exact confusion the monitoring plane exists to remove.
 *
 * AND WHICH RUNS WROTE IT, in the same buckets and therefore in the same statement. A Dataset name
 * spans **Runs** — `lame` holds 1,246 rows from two Runs of one workflow, and the listing's 623 is
 * one of them — so every count over it has to say which of the two it is. Bucketing by `run_id`
 * is what lets the console form "623 of 1,246 rows, from this Run" as ONE fact rather than as a
 * division an operator performs across two numbers whose scopes nobody stated.
 *
 * ONE STATEMENT. The per-bucket counts and the total come out of one scan (`sum(count(*))
 * OVER ()`), so their ratio is a fact about one snapshot rather than about two moments in a
 * Dataset that may still be growing.
 *
 * COST: four columns over the Dataset's files — `node`, `version`, `run_id` and the count. A
 * group-by cannot be answered from parquet footers the way `count(*)` can, so this is a real scan,
 * which is why it is issued ONCE when a Dataset is opened and never from the listing poll. Adding
 * the Run made the buckets finer (Machines × Runs rather than Machines) but not the scan wider by
 * more than that one column, and the console folds the buckets back down per dimension.
 */
export async function datasetProvenance(
  store: ObjectStore,
  sel: { kind: DatasetKind; name: string },
  override: Partial<LakeConfig> = {}
): Promise<DatasetProvenance> {
  const base = {
    name: sel.name,
    kind: sel.kind,
    rows: 0,
    groups: [],
    carriesProvenance: false,
    carriesRun: false,
    measuredAt: Date.now(),
  };
  if (!lakeEnabled(store, override)) return base;
  const conn = await lakeConnection(store, resolveLakeConfig(store, override));
  const table = safeName(sel.name);
  const schema = schemaOf(sel.kind);
  if (!(await tableInCatalog(conn, schema, table))) {
    throw noSuchDataset(sel);
  }

  // DESCRIBE reads the catalog's column list and opens no data file, so a Dataset that carries
  // no provenance costs one metadata read rather than a scan that would count nothing.
  const columns = new Set(
    (await tableColumns(conn, table, schema)).map((c) => c.name)
  );
  const carriesProvenance = PROVENANCE_COLUMNS.every((c) => columns.has(c));
  const carriesRun = columns.has(RUN_COLUMN);
  if (!carriesProvenance && !carriesRun) return base;

  // A column this table does not have is selected as a typed NULL instead of being dropped, so a
  // bucket has the same shape whatever the Dataset carries and a reader never has to ask which
  // columns it happened to have. NULL is already how the lake spells "unrecorded".
  //
  // Only the columns that EXIST are grouped by: a constant in GROUP BY is at best a no-op and at
  // worst rejected by the engine, and the constant is the same value in every bucket regardless.
  const NONE = 'CAST(NULL AS VARCHAR)';
  const keys = [...(carriesProvenance ? PROVENANCE_COLUMNS : []), ...(carriesRun ? [RUN_COLUMN] : [])];
  const projected =
    `${carriesProvenance ? 'node' : NONE} AS node, ` +
    `${carriesProvenance ? 'version' : NONE} AS version, ` +
    `${carriesRun ? RUN_COLUMN : NONE} AS run_id`;

  // `n`, not `rows`: ROWS is a window-frame keyword, and an alias that has to be quoted to be
  // ordered by is an alias waiting to break.
  const res = await conn.runAndReadAll(
    `SELECT ${projected}, count(*) AS n, sum(count(*)) OVER () AS total
       FROM ${LAKE}.${schema}."${table}"
      GROUP BY ${keys.join(', ')}
      ORDER BY n DESC, node NULLS LAST, version NULLS LAST, run_id NULLS LAST`
  );
  const rows = res.getRows();
  const str = (v: unknown): string | null => (v === null || v === undefined ? null : String(v));
  return {
    name: sel.name,
    kind: sel.kind,
    // The window value repeats on every bucket; an empty Dataset has no bucket at all, and zero
    // rows is a true answer rather than a missing one.
    rows: Number(rows[0]?.[4] ?? 0),
    groups: rows.map((r) => ({
      machine: str(r[0]),
      version: str(r[1]),
      run: str(r[2]),
      rows: Number(r[3] ?? 0),
    })),
    carriesProvenance,
    carriesRun,
    // Stamped AFTER the statement answered, so it dates the numbers rather than the request.
    measuredAt: Date.now(),
  };
}

/**
 * Make a DuckDB cell JSON-serializable.
 *
 * The node-api returns integers as BigInt and LIST/STRUCT columns as wrapper objects; both
 * throw or stringify to `[object Object]` when handed to `JSON.stringify`. crawl4ai's output is
 * nested, so the conversion has to recurse — a shallow pass leaves BigInts inside structs and
 * the whole response fails to serialize.
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

/**
 * The object keys of one dispatch's parquet files, per actor.
 *
 * Driven by the run's MATERIALIZATION RECORDS rather than by a path guess: the ledger knows
 * which actor/version ran and when it was dispatched, which is exactly the partition. A run
 * with no records has no files, and says so by returning nothing.
 */
export async function runFiles(
  store: ObjectStore,
  records: readonly MaterializationRecord[],
  override: Partial<LakeConfig> = {}
): Promise<DatasetFiles[]> {
  if (!lakeEnabled(store, override) || records.length === 0) return [];
  const cfg = resolveLakeConfig(store, override);
  const conn = await lakeConnection(store, cfg);
  const dataPath = await lakeDataPath(conn, cfg.metaSchema);
  const existing = new Set(await listOutputActors(store, override));

  // One entry per (actor, version, dispatch) — nodes fold in, because a sharded dispatch of
  // one actor is one dataset.
  const wanted = new Map<string, { actor: string; version: string; dt: string }>();
  for (const r of records) {
    const actor = safeName(r.actor);
    if (!existing.has(actor)) continue;
    const dt = dtPartition(r.runStartedAt);
    wanted.set(`${actor} ${r.version} ${dt}`, { actor, version: r.version, dt });
  }

  const out: DatasetFiles[] = [];
  for (const w of wanted.values()) {
    const keys = await partitionFileKeys(conn, store, dataPath, w.actor, w.version, w.dt);
    if (keys.length === 0) continue;
    out.push({ ...w, table: w.actor, keys, columns: await tableColumns(conn, w.actor) });
  }
  out.sort((a, b) => a.actor.localeCompare(b.actor));
  return out;
}

/**
 * Object-store keys for one `version=…/dt=…` partition of an actor's table.
 *
 * `ducklake_list_files` resolves each data file's full path; we keep only this dispatch's
 * partition directories and re-root the rest under the store prefix so it can be presigned.
 * Matching on BOTH partition segments is what keeps one dispatch's presigned URLs from
 * reaching another's rows.
 */
async function partitionFileKeys(
  conn: DuckDBConnection,
  store: ObjectStore,
  dataPath: string,
  tbl: string,
  version: string,
  dt: string
): Promise<string[]> {
  const res = await conn.runAndReadAll(
    `SELECT data_file FROM ducklake_list_files(${lit(LAKE)}, ${lit(tbl)}, schema := ${lit(OUTPUT_SCHEMA)})`
  );
  const marker = `/version=${version}/dt=${dt}/`;
  const keys: string[] = [];
  for (const row of res.getRows()) {
    const full = String(row[0]);
    if (!full.includes(marker)) continue;
    const rel = full.startsWith(dataPath) ? full.slice(dataPath.length) : full;
    keys.push(store.key(rel));
  }
  return keys;
}
