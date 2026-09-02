/**
 * The materializer's activities — the ONLY code that writes typed output into DuckLake,
 * and the only place the materialization status dimension is mutated (ADR 0017).
 *
 * These run on their OWN task queue (`kontra-materializer`), hosted by a separate worker
 * process on a non-controller host with its own cgroup. That isolation is the point: the
 * embedded DuckDB that decodes and writes parquet is the single largest memory consumer in
 * this system, and it must not share a heap — or a 4 GB host — with anything else.
 *
 * WHO CALLS IT. Nothing in this repository does, at the moment: the graph interpreter was its
 * only caller and materialization has stopped being interpreter-driven and privileged (ADR
 * 0023 §1). The caller's own workflow publishes a Dataset, and these are the activities that
 * write one. The identity below is still spelled the way the interpreter spelled it — see
 * `MaterializeNodeInput` — because the row shape is pinned by MATERIALIZATION_SCHEMA_VERSION
 * and renaming it is a new table version, not a rename.
 *
 * WHY AN ACTIVITY ON A SEPARATE QUEUE, NOT A CHILD WORKFLOW
 *
 * An activity dispatched to a dedicated task queue gives the isolation that matters —
 * different process, different host, different resource budget — without adding a second
 * class of pending operation to the caller's workflow. That distinction is not theoretical: a
 * fan-out that exceeded the server's pending-operation limit wedged a real run for ~15
 * hours while every surface still read healthy.
 *
 * WHAT IT MUST NEVER DO: fail the run. ADR 0005's invariant is retained verbatim —
 * analytics never sit on the critical path. A materialization that exhausts its retries
 * records `failed`, and the API's public projection reports `output_failed`. The
 * EXECUTION status is untouched, because `RunStatus` means "did the caller's workflow finish".
 */

import { Context } from '@temporalio/activity';
import { ApplicationFailure } from '@temporalio/common';

import { ObjectStore } from '../codec/objectStore';
import {
  MATERIALIZATION_SCHEMA_VERSION,
  boundedError,
  type MaterializationKey,
} from '../data/materialization';
import { MaterializationStore, materializationStore } from '../data/materializationStore';
import {
  MaterializationIntegrityError,
  lakeEnabled,
  writeDatasetParquet,
  type LakeConfig,
} from '../data/parquet';
import { DEFAULT_STALE_MS, SummaryStore, summaryStore } from '../data/summaries';

/** Identity + provenance for one node's materialization. */
export interface MaterializeNodeInput {
  runId: string;
  actor: string;
  version: string;
  nodeId: string;
  /** The node output manifest's CAS content address. */
  sha256: string;
  /** Server-minted run start (ms) — stamped on every row, preserved across retries. */
  runStartedAt: number;
}

/** What the workflow learns about one node's output. Scalars only — never payload. */
export interface MaterializeNodeResult {
  tbl: string | null;
  rows: number;
  bytes: number;
  objectGets: number;
  /** True when the key was already committed and this attempt did no work. */
  alreadyComplete: boolean;
}

export interface MaterializeFailureInput {
  runId: string;
  actor: string;
  version: string;
  nodeId: string;
  /** Bounded reason, as Temporal surfaced it to the workflow. */
  reason: string;
}

function keyOf(i: { runId: string; actor: string; version: string; nodeId: string }): MaterializationKey {
  return {
    runId: i.runId,
    actor: i.actor,
    version: i.version,
    node: i.nodeId,
    schemaVersion: MATERIALIZATION_SCHEMA_VERSION,
  };
}

export interface MaterializerDeps {
  store?: ObjectStore;
  status?: MaterializationStore;
  summaries?: SummaryStore;
  lake?: Partial<LakeConfig>;
}

/**
 * Build the activities the materializer worker registers. `store` and `status` are
 * injectable so the suite drives real DuckLake + SQLite without an S3 or a Postgres.
 */
export function createMaterializerActivities(deps: MaterializerDeps = {}) {
  const store = deps.store ?? new ObjectStore();
  const status = deps.status ?? materializationStore();
  const summaries = deps.summaries ?? summaryStore();
  const lake = deps.lake ?? {};

  /**
   * Refresh the operational summaries after a commit. BEST-EFFORT AND SAY SO: these tables
   * feed dashboards, not correctness, and the status ledger remains the authority. A
   * summary write that failed the activity would put a Grafana panel on the critical path
   * of a scan — precisely the coupling ADR 0005 forbids. The failure is logged, not
   * swallowed silently, so a persistently stale dashboard is diagnosable.
   */
  async function refreshSummaries(runId: string, execution: string, lifecycle: string): Promise<void> {
    try {
      const records = await status.listForRun(runId);
      await summaries.refreshRun({ runId, execution, lifecycle, records });
      await summaries.refreshHealth(await status.health(), (await status.listStale(DEFAULT_STALE_MS)).length);
    } catch (err) {
      // eslint-disable-next-line no-console
      console.warn(`summary refresh failed for run ${runId}: ${boundedError(err)}`);
    }
  }

  return {
    /**
     * Declare that output exists and will be materialized. Called the moment its envelope
     * ref is durable, BEFORE any writing starts.
     *
     * This is what makes the record set self-describing: `finalizing` (work outstanding)
     * is distinguishable from "no output was ever expected" without a separate list of
     * expected output that could drift from reality. Idempotent.
     */
    async declareMaterialization(input: MaterializeNodeInput): Promise<void> {
      // No lake configured (a storeless unit run): declare nothing. An empty record set
      // projects as `completed`, which is correct — no output was ever expected. Declaring
      // a `pending` that nothing will ever pick up would strand the run in `finalizing`.
      if (!lakeEnabled(store, lake)) return;
      await status.declare(keyOf(input), input.runStartedAt);
    },

    /**
     * Decode one node's output into typed DuckLake rows.
     *
     * Idempotent by the ADR 0017 §6 rule: a retry that finds the key already `complete`
     * does NO WORK and appends no rows — it reports the committed numbers back. That is
     * what lets a cross-worker retry (ADR 0016) be safe rather than a duplicate-row bug.
     *
     * Throws on failure so Temporal retries it. The status stays `running` between
     * attempts, because a retry is still coming; only the workflow, which is the one thing
     * that knows retries are exhausted, records `failed`.
     */
    async materializeNode(input: MaterializeNodeInput): Promise<MaterializeNodeResult> {
      if (!lakeEnabled(store, lake)) {
        return { tbl: null, rows: 0, bytes: 0, objectGets: 0, alreadyComplete: true };
      }
      const key = keyOf(input);
      const claimed = await status.claim(key, input.runStartedAt);
      if (claimed === null) {
        const rec = await status.get(key);
        return {
          tbl: rec?.tbl ?? null,
          rows: rec?.rows ?? 0,
          bytes: rec?.bytes ?? 0,
          objectGets: 0,
          alreadyComplete: true,
        };
      }

      // Heartbeat once up front so a long node materialization is visibly alive rather
      // than indistinguishable from a wedged worker.
      //
      // Guarded: `Context.current()` throws outside a Temporal activity, and this function
      // is deliberately callable without one — that is how the pipeline is verified against
      // a live stack without dispatching a real actor run. An unguarded call would make the
      // whole module untestable outside the worker, which is precisely the code that most
      // needs to be exercisable on its own.
      try {
        Context.current().heartbeat({ node: input.nodeId, attempt: claimed.attempt });
      } catch {
        /* not running inside a Temporal activity — nothing to beat to */
      }

      let out;
      try {
        out = await writeDatasetParquet(
          store,
          {
            sha256: input.sha256,
            actor: input.actor,
            version: input.version,
            runId: input.runId,
            node: input.nodeId,
            runStartedAt: input.runStartedAt,
            schemaVersion: MATERIALIZATION_SCHEMA_VERSION,
          },
          lake
        );
      } catch (err) {
        // An integrity failure is DETERMINISTIC: the object is absent, or its bytes are
        // not the bytes the manifest committed to. Three more attempts would re-read the
        // same bytes, re-derive the same digest and burn three more rounds of GETs to
        // reach the same verdict. Fail non-retryably so the run reaches `output_failed`
        // now, with the offending key named. Everything else — an S3 blip, a catalog
        // conflict, a worker restart — stays retryable.
        if (err instanceof MaterializationIntegrityError) {
          throw ApplicationFailure.create({
            type: 'MaterializationIntegrity',
            nonRetryable: true,
            message: err.message,
            details: [err.keys],
          });
        }
        throw err;
      }

      await status.complete(key, {
        rows: out.rows,
        bytes: out.bytes,
        snapshotId: out.snapshotId,
        // A node that legitimately produced nothing has no table; record the key as
        // complete with zero rows rather than inventing a table name for it.
        tbl: out.tbl ?? '',
      });
      // Refreshed AFTER the DuckLake commit and the status write, so a dashboard can never
      // show rows the ledger has not yet accepted.
      await refreshSummaries(input.runId, 'running', 'finalizing');

      return {
        tbl: out.tbl,
        rows: out.rows,
        bytes: out.bytes,
        objectGets: out.objectGets,
        alreadyComplete: false,
      };
    },

    /**
     * Record an EXHAUSTED materialization. Called by the workflow only after Temporal has
     * stopped retrying, which is the one place that fact is known.
     *
     * `complete` is absorbing in the store, so a straggler cannot un-commit a landed write.
     */
    async recordMaterializationFailure(input: MaterializeFailureInput): Promise<void> {
      if (!lakeEnabled(store, lake)) return;
      await status.fail(keyOf(input), boundedError(input.reason));
      // A failure must reach the dashboards as promptly as a success, or "output stopped
      // arriving" becomes the only signal this failure mode gives an operator.
      await refreshSummaries(input.runId, 'running', 'output_failed');
    },
  };
}

/** The activities interface a caller's workflow proxies (`import type` only). */
export type MaterializerActivities = ReturnType<typeof createMaterializerActivities>;
