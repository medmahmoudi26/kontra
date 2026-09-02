/**
 * THE RETENTION PREVIEW — one route, and the dry-run half of a sweep that otherwise only ever runs
 * on a schedule (ADR 0029 §3, §5).
 *
 * It is not a corner of the dataset browser, even though it sits on that prefix and reads the same
 * lake. What it answers is a question about POLICY — which Datasets the untagged TTL would collect
 * — and it runs the exact same `sweepDatasets` classification the scheduled
 * `sweepDatasetsWorkflow` runs, with `dryRun` forced on. Keeping it beside the listing would put
 * the sweep's dependency set (six authorities, including the summaries table nothing else on the
 * dataset surface touches) into a module whose other nine routes need five of them.
 *
 * WHAT IT NEEDS: the whole {@link RetentionDeps} set — `store`, `lake`, `materialization`,
 * `records`, `runWorkflows` (as `workflows`) and `summaries`. Every one is SHARED and built in
 * `buildServer`; this module holds none of its own.
 *
 * COLLECTION IS NEVER EXPOSED HERE. `dryRun: true` is set by this file and is not a parameter — a
 * delete happens on the schedule, so there is no route that ages out durable output on demand. A
 * caller may narrow the horizon with `?ttlMs=`/`?graceMs=` to preview a shorter one without
 * touching the real policy.
 *
 * Ungated, like the list/preview/provenance routes it sits beside: no operator SQL crosses the
 * wire, and it only ever reads.
 */

import type { FastifyInstance } from 'fastify';

import type { ObjectStore } from '../codec/objectStore';
import type { DatasetRecordStore } from '../data/datasetRecords';
import type { MaterializationStore } from '../data/materializationStore';
import type { LakeConfig } from '../data/parquet';
import { sweepDatasets } from '../data/retention';
import type { RunWorkflowStore } from '../data/runWorkflows';
import type { SummaryStore } from '../data/summaries';
import { errMessage } from './errors';

export interface RetentionRouteDeps {
  store: ObjectStore;
  lake: Partial<LakeConfig>;
  materialization: MaterializationStore;
  records: DatasetRecordStore;
  /** Passed to the sweep as `workflows` — the caller identity the report's names render. */
  runWorkflows: RunWorkflowStore;
  summaries: SummaryStore;
}

export function registerRetentionRoutes(app: FastifyInstance, deps: RetentionRouteDeps): void {
  const { store, lake, materialization, records, runWorkflows, summaries } = deps;

  // Retention PREVIEW — what the untagged-TTL sweep WOULD collect, deleting nothing (ADR 0029 §3, §5).
  //
  // This is the dry-run half of the sweep, over HTTP: it runs the exact same classification the
  // scheduled `sweepDatasetsWorkflow` runs — reads the record (never `KontraTag`), the clock is last
  // write, an `open` or tagged or temporary Dataset is kept — but with `dryRun` forced on, so an
  // operator (or the Datasets page) can see the blast radius before anything ages out. The TTL is the
  // repo constant, never the namespace config; a caller may narrow it with `?ttlMs=` to preview a
  // shorter horizon without touching the real policy.
  //
  // Ungated, like the list/preview/provenance routes beside it: no operator SQL crosses the wire, and
  // it only ever reads. Collection itself is never exposed here — a delete happens on the schedule, so
  // there is no route that ages out durable output on demand.
  app.get('/api/datasets/retention/preview', async (req, reply) => {
    const { ttlMs, graceMs } = req.query as Partial<{ ttlMs: string; graceMs: string }>;
    const num = (v: string | undefined): number | undefined => {
      const n = v ? Number.parseInt(v, 10) : NaN;
      return Number.isFinite(n) && n >= 0 ? n : undefined;
    };
    try {
      return await sweepDatasets(
        { store, lake, materialization, records, workflows: runWorkflows, summaries },
        { dryRun: true, ...(num(ttlMs) !== undefined ? { ttlMs: num(ttlMs) } : {}), ...(num(graceMs) !== undefined ? { graceMs: num(graceMs) } : {}) }
      );
    } catch (err) {
      return reply.code(502).send({ error: `could not preview retention: ${errMessage(err)}` });
    }
  });
}
