/**
 * The dataset PAGER — the activity behind `workflows.dataset(name).batches(...)`.
 *
 * This is the read half of "a Dataset yields Batches". A caller's workflow cannot do I/O, so
 * paging is an activity; and it returns a REF rather than rows, so a 40k-unit dataset costs the
 * caller's history a handful of ~110-byte refs instead of the payload (ADR 0007).
 *
 * WHERE THE BYTES GO. The page is written to the SAME CAS the actors and the handler read —
 * one `KONTRA_S3_BUCKET` / `KONTRA_S3_PREFIX` across `objectstore.go`, `objectStore.ts`,
 * `casstore.py` and `unitstore.py` — so the ref this returns is directly dereferenceable by the
 * actor's own `kontra.fetch_blob`. That shared configuration is what closes the chain, and it is
 * an assumption worth stating because nothing type-checks it.
 *
 * WHY IT LIVES BESIDE THE MATERIALIZER. That worker already holds the DuckLake attach, an
 * ObjectStore and a bounded DuckDB budget, and it deliberately carries no claim-check converter
 * — correct here, because a ref is small and must stay un-offloaded. It runs on its own queue
 * (`DATASET_QUEUE`) so a caller's page read never queues behind a long decode.
 */

import { ApplicationFailure } from '@temporalio/common';

import { ObjectStore } from '../codec/objectStore';
// The lifecycle vocabulary lives with the READ side, because that is what has to agree with the
// UI's badge; this module only writes it (ADR 0023 §11).
import {
  datasetOwnerKey,
  datasetStateKey,
  type DatasetOwner,
  type DatasetState,
} from '../data/datasets';
// The durable authority for a Dataset's tags (ADR 0029 §1, §4). The in-workflow tag path writes
// THIS record — the same store the control-plane routes and the retention sweeper read — and only
// THEN mirrors to the `KontraTag` search attribute. The mirror is the projection; this is the truth.
import { datasetRecordStore, type DatasetRecordStore } from '../data/datasetRecords';
import {
  RESERVED_OUTPUT_COLUMNS,
  discardLakeConnection,
  promoteInto,
  writeDatasetParquet,
  type LakeConfig,
} from '../data/parquet';
import { pageDataset, type DatasetPage } from '../data/queryEngine';

/** What a caller asks for: one page of one dataset. */
export interface PageDatasetInput {
  /** The dataset's bare name, as `kontra dataset list` shows it. */
  name: string;
  /**
   * SQL over the dataset, referencing it by bare name. Omitted means `SELECT * FROM "<name>"`
   * — "the rows the query returns ARE the units", the same rule the CLI's `--query` follows.
   */
  sql?: string;
  /**
   * REQUIRED. A materialized dataset stamps no row id, so LIMIT/OFFSET over it has no defined
   * order and two pages may overlap or skip units with nothing raising.
   */
  orderBy: string;
  /** Units per page. */
  limit: number;
  offset: number;
  /** `--version` / `--dt` partition pruning, for an output dataset. */
  scope?: { name: string; version?: string; dt?: string };
}

export interface DatasetDeps {
  store?: ObjectStore;
  lake?: Partial<LakeConfig>;
  /**
   * The Dataset record store the `tagDataset` activity writes (ADR 0029 §4). Defaults to the
   * process-wide singleton — the same store the orchestrator's `/api/datasets/runs/:runId/tags`
   * route mutates, reached here because this activity runs in the materializer process, which
   * already holds the SQL config the status store uses.
   */
  records?: DatasetRecordStore;
}

/** Write the author's tag onto a Run's Dataset record. */
export interface TagDatasetInput {
  /** The caller workflow's id — what a Run IS (ADR 0023 §12), and the record's key. */
  runId: string;
  /** The tag to add. Trimmed and length-checked by the store's `normalizeTag`. */
  tag: string;
}

/** Append one Batch to a named Dataset. */
export interface PublishBatchInput {
  /** The Dataset's name — the table a caller will later page by that same bare name. */
  dataset: string;
  /** The Batch's ref: a CAS sha addressing a bare list of units. */
  sha256: string;
  /** Lineage stamped on every row. */
  runId: string;
  runStartedAt: number;
  /**
   * WHICH MACHINE ran the Method that produced these units — the actor host's own hostname,
   * carried on the Batch ref's meta since the host started stamping it. Omitted or empty means
   * UNRECORDED and is written as SQL NULL: a Batch paged straight out of a Dataset was produced
   * by the lake and not by a Machine, and an actor host older than the contract reports nothing.
   *
   * It lands in the `node` column — see {@link MaterializeSelector.node} for why that column is
   * the right home and what it used to mean.
   */
  machine?: string;
  /**
   * Which Actor version produced them. Omitted or empty means UNRECORDED, written as NULL —
   * never the `'0'` this used to substitute, which read back as a real version nobody deployed.
   */
  version?: string;
}

/** Promote rows out of one Dataset into a durable one. */
export interface PromoteDatasetInput {
  /** The durable Dataset the rows land in — created partitioned by (version, dt) if absent. */
  target: string;
  /** The Dataset the rows come from, by bare name — normally a temporary one. */
  source: string;
  /**
   * The SELECT deciding which rows promote, referencing the source by its bare name. The rows it
   * returns ARE the rows promoted. NO runId/machine ride here on purpose: promotion carries the
   * source row's own provenance rather than stamping this workflow over the producing one.
   */
  sql: string;
}

/** Re-page an existing Batch into smaller ones. */
export interface SplitBatchInput {
  /** The Batch's ref — a CAS sha addressing a bare list of units. */
  sha256: string;
  /** Maximum units per resulting Batch. */
  size: number;
}

/** Flatten a Method's result manifest into the records it points at. */
export interface ResolveBatchInput {
  /** The Batch's ref — a CAS sha addressing a list that may hold `$ref` entries. */
  sha256: string;
}

/** One entry of a result manifest: the record lives in its own blob. */
interface UnitRef {
  key?: string;
  sha256?: string;
}

function unitRefOf(entry: unknown): UnitRef | null {
  if (!entry || typeof entry !== 'object') return null;
  const ref = (entry as { $ref?: unknown }).$ref;
  if (!ref || typeof ref !== 'object') return null;
  const { key, sha256 } = ref as UnitRef;
  if (typeof key !== 'string' || key === '') return null;
  return typeof sha256 === 'string' ? { key, sha256 } : { key };
}

/**
 * DuckDB's DETERMINISTIC error classes — the ones that fail identically on every attempt.
 *
 * Each names a fault in the SQL or the catalog, not in the machine: a column that is not there, a
 * statement that does not parse, a table that does not exist, a value that cannot be cast. None of
 * those are fixed by waiting. Conservative BY CONSTRUCTION — anything unrecognised stays retryable,
 * because marking a transient failure non-retryable kills runs that would have recovered.
 */
function isDeterministicSqlError(err: unknown): boolean {
  const msg = err instanceof Error ? err.message : String(err);
  return /\b(Binder|Parser|Catalog|Conversion|Syntax) Error\b/i.test(msg);
}

/**
 * A promise-chain mutex for lake WRITES.
 *
 * One publish at a time, process-wide, because `lakeConnection` caches ONE DuckDB connection per
 * (catalog, dataPath) and a DuckDB connection has ONE transaction context. Two concurrent writers
 * on it do not race for throughput, they corrupt each other: the second gets `cannot start a
 * transaction within a transaction`, and from then on every statement — including unrelated
 * datasets' — gets `Current transaction is aborted (please ROLLBACK)`.
 *
 * THE CHAIN MUST SURVIVE A REJECTED PREDECESSOR. `publishing.then(run, run)` runs the next job on
 * either outcome, and the stored link swallows the rejection — without that, one bad publish
 * stalls every later one, which is the same failure in a different costume.
 *
 * The queue is not a throughput loss: DuckDB executes one transaction at a time whether or not
 * callers wait politely. See `.scratch/materializer-shared-connection/ISSUE.md`.
 */
let publishing: Promise<unknown> = Promise.resolve();

function publishSerially<T>(run: () => Promise<T>): Promise<T> {
  const mine = publishing.then(run, run);
  publishing = mine.catch(() => undefined);
  return mine;
}

export function createDatasetActivities(deps: DatasetDeps = {}) {
  const store = deps.store ?? new ObjectStore();
  const lake = deps.lake ?? {};
  const records = deps.records ?? datasetRecordStore();

  return {
    /**
     * Add the author's tag to a Run's Dataset RECORD (ADR 0029 §4) — the durable, authoritative
     * half of an in-workflow `publish(..., tag=…)`.
     *
     * ORDER IS THE WHOLE POINT. The caller SDK awaits THIS before it mirrors the tag to the
     * `KontraTag` search attribute, because the record is what the retention sweeper reads (§5).
     * Written the other way round — an `UpsertSearchAttributes` that also wrote the record — a
     * mirror failure would leave the record saying untagged while the run kept running, and the
     * sweeper would collect a Dataset the author asked to keep. So the record lands here, first,
     * over the SQL store that knows nothing about Temporal.
     *
     * IDEMPOTENT by the record's primary key: re-running (a workflow retry, a per-chunk publish
     * that reached this twice) adds no second row, and a concurrent add of a DIFFERENT tag by the
     * operator route survives — tags are a set (§1). Returns the run's whole tag set as the
     * post-state, the same shape the control-plane routes return.
     */
    async tagDataset(input: TagDatasetInput): Promise<{ tags: string[] }> {
      await records.addTag(input.runId, input.tag);
      const dev = await records.get(input.runId);
      return { tags: dev?.tags ?? [] };
    },

    /**
     * Split one Batch into Batches of at most `size`, returning their refs.
     *
     * THE PROBLEM THIS SOLVES: a caller controls its INPUT page size, but not what a Method
     * emits. A Method that fans out 1→50 turns a 200-unit page into a 10,000-unit result, and
     * feeding that to the next Actor is one activity on one worker with one oversized blob —
     * past the measured 200/1000 batch guard, with no parallelism and no isolation boundary
     * between 10,000 units.
     *
     * ONE activity call returns ALL the refs rather than one call per chunk: a ref is ~110
     * bytes, so 500 of them is ~55 KB, and the alternative is 500 round trips to slice a list
     * that is already in memory here.
     *
     * The chunks are content-addressed like everything else, so re-splitting the same Batch at
     * the same size writes nothing new.
     */
    /**
     * Turn a Method's result manifest into the records it addresses.
     *
     * THE BUG THIS FIXES, MEASURED ON A REAL FLEET. An actor host with `KONTRA_S3_ENDPOINT`
     * configured commits every emitted record to its OWN blob and puts a `{"$ref": {...}}` entry
     * in the result — that is ADR 0007's per-unit blob plane, and it is why a 10,000-unit result
     * costs a workflow history nothing. But the next Method's units are that list, verbatim:
     * `runtime/handler/workflow.go` fetches the ref and hands it to `RunBatch` without dereferencing,
     * and `Unit.Str("domain")` on a `{"$ref": …}` object returns `""`. So the second Method in
     * any chain sees every field empty.
     *
     * THE FAILURE IS SILENT AND EXPENSIVE, which is why this is worth an activity. An author's
     * "this unit is malformed" guard isolates each unit (ADR 0023 §13), so a four-Machine sweep
     * of 400 domains returned `{"pairs": 623, "checked": 0, "dropped": 623}` — a `completed`
     * run, an empty Dataset, and the only trace of the truth in the workflow's return value.
     * It cannot reproduce without an object store, so it does not happen locally: `unitStore ==
     * nil` makes the same emits ride inline and the chain works.
     *
     * WHY HERE AND NOT IN THE HANDLER. The handler is bundled into the Artifact placed on every
     * Machine, so fixing it there means republishing the bundle and redeploying every fleet
     * before any existing run works again. This runs on the CALLER's side, in the
     * orchestrator, so a fleet that is already up is fixed by fixing the control plane. The
     * handler-side dereference is still the better long-term home; this is the one that can be
     * shipped without a fleet-wide rebuild.
     *
     * IDEMPOTENT AND CHEAP WHEN THERE IS NOTHING TO DO: a batch whose entries are already
     * records returns the ref it was given, having read one blob. The resolved list is
     * content-addressed like everything else, so resolving the same batch twice writes nothing.
     */
    async resolveBatch(input: ResolveBatchInput): Promise<{ ref: DatasetPage['ref'] | null }> {
      const body = await store.get(store.casKey(input.sha256));
      if (!body) throw new Error(`batch ref ${input.sha256} not found in the CAS`);
      const parsed: unknown = JSON.parse(Buffer.from(body).toString('utf8'));
      const entries: unknown[] = Array.isArray(parsed)
        ? parsed
        : ((parsed as { results?: unknown[] })?.results ?? []);

      // Nothing to resolve. Returning `null` rather than the input ref keeps the caller's
      // "did anything change" check a null test rather than a string compare, and means the
      // common case adds no ref to the workflow's history.
      if (!entries.some((e) => unitRefOf(e) !== null)) return { ref: null };

      // Fetched with bounded concurrency: a 10,000-unit result is 10,000 small GETs, and firing
      // them all at once is what turns a resolve into a timeout against the object store.
      const records: unknown[] = [];
      const WIDTH = 32;
      for (let i = 0; i < entries.length; i += WIDTH) {
        const window = entries.slice(i, i + WIDTH);
        const fetched = await Promise.all(
          window.map(async (entry) => {
            const ref = unitRefOf(entry);
            // A plain record among refs is legal and stays where it is: a Method whose host had
            // no object store emits inline, and a batch can mix the two across a redeploy.
            if (!ref?.key) return [entry];
            const blob = await store.get(ref.key);
            if (!blob) {
              throw new Error(
                `unit blob ${ref.key} is missing — the Batch references a record the object ` +
                  'store does not have. This is data loss, not a shape problem, so it fails ' +
                  'the dispatch rather than silently dropping the unit.'
              );
            }
            const record: unknown = JSON.parse(Buffer.from(blob).toString('utf8'));
            // `unitstore.PutSubunit` writes the body as `[record]`, so the common shape is a
            // one-element list. A bare object is accepted too rather than assumed away.
            return Array.isArray(record) ? record : [record];
          })
        );
        for (const group of fetched) records.push(...group);
      }

      const bytes = Buffer.from(JSON.stringify(records), 'utf8');
      const sha = await store.putContentAddressed(bytes);
      return {
        ref: { sha256: sha, size: bytes.length, meta: { kind: 'units', n: String(records.length) } },
      };
    },

    async splitBatch(input: SplitBatchInput): Promise<{ refs: DatasetPage['ref'][] }> {
      const size = Math.max(Math.trunc(input.size), 1);
      const body = await store.get(store.casKey(input.sha256));
      if (!body) throw new Error(`batch ref ${input.sha256} not found in the CAS`);
      const parsed: unknown = JSON.parse(Buffer.from(body).toString('utf8'));
      // A Batch addresses a bare list. An envelope is tolerated for a ref minted before the
      // split, the same tolerance the handler's input path carries.
      const units: unknown[] = Array.isArray(parsed)
        ? parsed
        : ((parsed as { results?: unknown[] })?.results ?? []);

      const refs: DatasetPage['ref'][] = [];
      for (let i = 0; i < units.length; i += size) {
        const chunk = units.slice(i, i + size);
        const bytes = Buffer.from(JSON.stringify(chunk), 'utf8');
        const sha = await store.putContentAddressed(bytes);
        refs.push({
          sha256: sha,
          size: bytes.length,
          meta: { kind: 'units', n: String(chunk.length) },
        });
      }
      return { refs };
    },

    /**
     * Append one Batch to a named Dataset, and mark the Dataset `open`.
     *
     * This is ADR 0023 §1 — materialization is caller-invokable. It used to be the graph
     * interpreter's privilege, which is why deleting the interpreter left the model able to
     * CONSUME a Dataset but never produce one.
     *
     * The Batch's ref IS the manifest: `writeDatasetParquet` already accepts a bare-array
     * manifest and resolves `$ref` entries itself, so a Method's output needs no reshaping to
     * become rows. Appending is idempotent per batch by content address.
     *
     * NOTHING IS SUBSTITUTED FOR MISSING PROVENANCE. This used to write `node: input.nodeId ||
     * 'w'` and `version: input.version || '0'`, and since no caller ever sent either, EVERY row
     * in the lake carried that pair — measured on a four-Machine `nscheck` run,
     * `SELECT node, version, count(*) FROM lame GROUP BY 1,2` returned exactly one row,
     * `['w', '0', 1246]`. A default that is always taken is not a default, it is a fabricated
     * value that reads as measured. Unrecorded is now SQL NULL, which no producer can ever
     * emit, so a reader can tell "nothing wrote this" from "this is the value".
     */
    async publishBatch(input: PublishBatchInput): Promise<{ rows: number; dt: string }> {
      await store.put(
        datasetStateKey(input.dataset),
        Buffer.from(JSON.stringify({ state: 'open' satisfies DatasetState }), 'utf8')
      );
      // SERIALIZED, BECAUSE THE LAKE CONNECTION IS SHARED AND HAS ONE TRANSACTION CONTEXT.
      //
      // `lakeConnection` caches one connection per (catalog, dataPath) and hands the same object
      // to every caller, and Temporal runs activities concurrently by default. Two publishes in
      // flight therefore produce `TransactionContext Error: cannot start a transaction within a
      // transaction`, after which EVERY statement on that connection — including unrelated
      // datasets' — fails with `Current transaction is aborted (please ROLLBACK)`. Nothing rolls
      // back, so the activity retries forever against a connection that can never succeed, and the
      // run reads as a slow crawl rather than a stuck write.
      //
      // Measured on campaign-1790599185: four programs x four lanes, `publishBatch` at attempt 113
      // with the aborted-transaction message, row counts frozen for half an hour.
      //
      // Concurrency buys nothing here — DuckDB executes one transaction at a time regardless — so
      // queueing costs throughput nothing and turns corruption into a wait. See
      // `.scratch/materializer-shared-connection/ISSUE.md`.
      return publishSerially(async () => {
      try {
        const out = await writeDatasetParquet(
          store,
          {
            sha256: input.sha256,
            actor: input.dataset,
            version: input.version || null,
            runId: input.runId,
            node: input.machine || null,
            runStartedAt: input.runStartedAt,
          },
          lake
        );
        // `dt` RIDES BACK WITH THE ROW COUNT so a caller can read only its own partition.
        //
        // A **Dataset** that several **Runs** append to holds everybody's rows, and the natural
        // next thing a Method does is read back what it just wrote. Scoping that read needs the
        // partition string — and the ONE thing the caller must not do is compute it itself.
        // `dtPartition` is second-precision, colon-substituted and derived from the SERVER-minted
        // `run_started_at`; a second spelling that differs anywhere matches no partition and
        // returns zero rows with no error, which is indistinguishable from a run that produced
        // nothing. So the value comes from the process that owns the format, and the SDKs pass it
        // through rather than re-deriving it.
        return { rows: out.rows, dt: out.dt };
      } catch (err) {
        /*
         * A BINDER ERROR HERE IS THE AUTHOR'S SCHEMA, AND NO NUMBER OF RETRIES WILL FIX IT.
         *
         * `pageDataset` has had this guard since the query path burned two hours on attempt 22 of a
         * column that was never going to appear. `publishBatch` did not, and it is the one an author
         * actually hits: an `emits=` type that declares `node` fails with `Duplicate column name
         * "node" in INSERT`, eight attempts deep, while the run sits at RUNNING. Nothing reaches the
         * actor's log (the push succeeded) and nothing reaches the workflow's (it is still awaiting
         * the dispatch), so the only way to see it is `temporal workflow describe | jq
         * .pendingActivities`. GitHub #22.
         *
         * AND THE MESSAGE ANSWERS THE QUESTION THE ERROR RAISES. "Duplicate column name" tells an
         * author a name is taken; it does not tell them WHICH names are taken, and there was nowhere
         * to look it up. {@link RESERVED_OUTPUT_COLUMNS} is that list, so the failure carries it.
         */
        // THE CONNECTION IS DISCARDED, NOT RETURNED TO THE POOL. Serializing is only half the
        // fix: an aborted transaction or a DuckDB internal error leaves the shared connection
        // answering the same error to CALLERS THAT DID NOTHING WRONG, forever. Dropping it here
        // means the next publish re-ATTACHes a clean one — which costs real time, and is the
        // difference between a transient failure and a process that can never write again.
        discardLakeConnection(store, lake);
        const msg = err instanceof Error ? err.message : String(err);
        const dup = /Duplicate column name "([^"]+)"/i.exec(msg);
        if (dup) {
          const field = dup[1] as string;
          const reserved = RESERVED_OUTPUT_COLUMNS.includes(field);
          throw ApplicationFailure.nonRetryable(
            reserved
              ? `output field ${JSON.stringify(field)} collides with a column the framework stamps ` +
                `on every row — rename it. The reserved names are: ` +
                `${RESERVED_OUTPUT_COLUMNS.join(', ')}. (${msg})`
              : `output field ${JSON.stringify(field)} is declared twice in this Actor's emits ` +
                `type — rename one. (${msg})`,
            'DatasetSchemaRejected'
          );
        }
        if (isDeterministicSqlError(err)) {
          throw ApplicationFailure.nonRetryable(msg, 'DatasetWriteRejected');
        }
        throw err;
      }
      });
    },

    /**
     * Promote rows out of one Dataset into a durable one (temp-datasets slice 02) — the ACCEPTING
     * act, separate from production. A caller reads what a Run staged into a temporary Dataset,
     * decides what deserves to be the record, and promotes only that; the gap between producing and
     * accepting is where triage fits, and re-coupling them here would delete the feature.
     *
     * PROVENANCE SURVIVES because this is NOT `publishBatch`. `promoteInto` runs
     * `INSERT INTO target BY NAME SELECT … FROM source`, so `node`/`version`/`run_id`/
     * `run_started_at` come straight off the source row — a promoted row keeps the Machine and
     * Actor version that PRODUCED it, and a four-Machine run stays four Machines rather than
     * collapsing to the promoting workflow's one. See {@link promoteInto} for the measured cost
     * (a copy, ~170 ms/40k rows) and the deliberate non-idempotence.
     */
    async promoteDataset(input: PromoteDatasetInput): Promise<{ rows: number }> {
      return promoteInto(store, input, lake);
    },

    /**
     * Record ownership of a temporary Dataset (temp-datasets slice 01), at the moment its Run
     * opens it — before any Batch lands.
     *
     * This is the ONE temp-specific write. Everything after — the per-chunk publish, the
     * `open`/`sealed`/`abandoned` lifecycle — goes through the durable Dataset path unchanged,
     * which is the point: a temp is not a new write path, it is the same one against an
     * owned, framework-named Dataset. The owner is stored in `_owner.json` and NOT in the state
     * object, because `publishBatch` rewrites the state to `open` on every append and would
     * clobber an owner kept there (see `datasetOwnerKey`).
     *
     * Idempotent: re-opening writes the same marker. It sets no lifecycle state — the first
     * published Batch marks the temp `open` exactly as it does a durable Dataset, so a temp that
     * crashed after some rows stays `open` and reads as "the producer died", not "nothing found".
     */
    async openTempDataset(input: { dataset: string; owner: string }): Promise<void> {
      await store.put(
        datasetOwnerKey(input.dataset),
        Buffer.from(
          JSON.stringify({ owner: input.owner, createdAt: Date.now() } satisfies DatasetOwner),
          'utf8'
        )
      );
    },

    /**
     * Close a Dataset out. `sealed` means its producer finished on purpose.
     *
     * A crash never reaches this, which is the point: the Dataset stays `open` and a reader can
     * tell "the producer died" from "there was nothing to find". `abandoned` is the explicit
     * give-up, for a caller that caught its own failure and wants to say so.
     */
    async closeDataset(input: { dataset: string; state?: DatasetState }): Promise<void> {
      const state: DatasetState = input.state ?? 'sealed';
      await store.put(
        datasetStateKey(input.dataset),
        Buffer.from(JSON.stringify({ state }), 'utf8')
      );
    },

    /** A Dataset's state, or `null` when nothing ever wrote one. */
    async datasetState(input: { dataset: string }): Promise<{ state: DatasetState | null }> {
      const body = await store.get(datasetStateKey(input.dataset));
      if (!body) return { state: null };
      try {
        const parsed = JSON.parse(Buffer.from(body).toString('utf8'));
        return { state: (parsed?.state as DatasetState) ?? null };
      } catch {
        return { state: null };
      }
    },

    /**
     * One page, as a ref. `done` is derived by reading one row past the page, so end-of-dataset
     * is a fact rather than a guess from a full page — the termination condition Temporal's own
     * Batch Iterator pattern relies on.
     */
    async pageDataset(input: PageDatasetInput): Promise<DatasetPage> {
      const sql = input.sql?.trim() || `SELECT * FROM "${input.name.replace(/"/g, '""')}"`;
      try {
        return await pageDataset(store, {
          sql,
          orderBy: input.orderBy,
          limit: input.limit,
          offset: input.offset,
          scope: input.scope,
          lake,
        });
      } catch (err) {
        // A MALFORMED QUERY IS NOT A FLAKE, and retrying one is how a run spends hours looking
        // alive while doing nothing. MEASURED: `hunt` paged `scope_example` with
        // `WHERE kind NOT IN (…)` against a scope built without that column, and Temporal retried
        //
        //   Binder Error: Referenced column "kind" not found in FROM clause!
        //
        // 21 times against `MaximumAttempts: 0` — unbounded — while the workflow sat in `running`
        // for over two hours. The column was never going to appear on attempt 22.
        //
        // ONLY THE DETERMINISTIC CLASSES. DuckDB names them, and each is a statement about the SQL
        // or the catalog rather than about the machine: a Binder/Parser/Catalog/Conversion error
        // fails identically forever. Everything else — a closed connection, a lock, a spill, an
        // object store hiccup — stays retryable, because those are exactly the failures a retry is
        // for. Getting this backwards in the other direction would be worse: a non-retryable
        // network blip fails a run that would have succeeded on its own.
        if (isDeterministicSqlError(err)) {
          throw ApplicationFailure.nonRetryable(
            err instanceof Error ? err.message : String(err),
            'DatasetQueryRejected'
          );
        }
        throw err;
      }
    },
  };
}
