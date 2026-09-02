/**
 * THE DATASET BROWSER — the lake, as a person addresses it.
 *
 * Nine routes in three groups, all of them about a Dataset's IDENTITY rather than its contents:
 *
 *   the listing, the preview, the provenance and the temp delete — what is in the lake;
 *   `GET /api/datasets/runs`                                     — actor@version + date → a Run;
 *   the four `/api/datasets/runs/:runId/{tags,name}` writes      — the record (ADR 0029 §1, §4).
 *
 * WHAT IT NEEDS, and every one of these is SHARED, which is why they are built in `buildServer`:
 *   - `store`           the {@link ObjectStore} the lake sits on. Also the archive's, the row
 *                       tail's, the workbench's and the explore manifest's — one store, so a count
 *                       on one surface cannot disagree with a count on another.
 *   - `lake`            DuckLake overrides; tests point this at a local directory.
 *   - `materialization` the ledger (ADR 0017) that names the Run behind an (actor, version, dt)
 *                       partition. Shared with the retention module and `RunLifecycle`.
 *   - `records`         the durable Dataset record — tags and renames. Shared with retention.
 *   - `runWorkflows`    the caller identity a derived name renders (ADR 0029 §2). Shared with the
 *                       run module, which stamps it, and with retention, which reports it.
 *
 * EVERY JOIN IN THE LISTING IS BEST-EFFORT AND NONE OF THEM CAN 502 IT. Three authorities can say
 * which Run is behind a row and each is caught separately, so a ledger outage costs the
 * dispatch-matched leg and nothing else. That is the rule to keep when editing this file.
 *
 * ADMISSION: the reads and the temp delete are ungated, like the rest of the lake browser — no
 * operator SQL crosses the wire, and no object-store URL leaves the process. The FOUR RECORD WRITES
 * are gated by the run token, and the reasoning that once left them open is quoted below because it
 * was wrong in a way worth keeping written down.
 */

import type { FastifyInstance } from 'fastify';

import { checkOptionalBearer } from '../auth';
import type { ObjectStore } from '../codec/objectStore';
import {
  type DatasetKind,
  NoSuchDatasetError,
  NotTemporaryDatasetError,
  datasetProvenance,
  datasetRunIds,
  deleteTemporaryDataset,
  listDatasets,
  previewDataset,
  withDatasetDeviations,
  withDatasetNames,
} from '../data/datasets';
import { type DatasetRecordStore, InvalidDeviationError } from '../data/datasetRecords';
import type { DispatchRef, MaterializationStore } from '../data/materializationStore';
import type { LakeConfig } from '../data/parquet';
import type { RunWorkflow, RunWorkflowStore } from '../data/runWorkflows';
import { RUN_TOKEN_VARS } from '../workflowControl';
import { errMessage } from './errors';
import { runIdOf } from './runId';

export interface DatasetRouteDeps {
  store: ObjectStore;
  lake: Partial<LakeConfig>;
  materialization: MaterializationStore;
  records: DatasetRecordStore;
  runWorkflows: RunWorkflowStore;
}

export function registerDatasetRoutes(app: FastifyInstance, deps: DatasetRouteDeps): void {
  const { store, lake, materialization, records, runWorkflows } = deps;

  // --- datasets (the query browser) ---
  //
  // EVERYTHING IS A DATASET: an actor's output for one dispatch, and an operator-loaded list,
  // listed together and addressed the same way. Both routes read the lake directly — no
  // Temporal, no run record — and both are bounded: the listing touches catalog metadata only,
  // and the preview is a clamped LIMIT.
  app.get('/api/datasets', async (req, reply) => {
    const { name, version, dt, kind } = req.query as Partial<{
      name: string;
      version: string;
      dt: string;
      kind: DatasetKind;
    }>;
    try {
      const infos = await listDatasets(store, { name, version, dt, kind }, lake);
      // Attach the DERIVED run-grain name (ADR 0029 §2). Three authorities can say which Run is
      // behind a row — the ledger, a temp's owner marker, and the row's own `run_id` statistics —
      // and `withDatasetNames` documents the order. They are joined HERE rather than inside the
      // catalog scan because the ledger and the run record are not the lake.
      //
      // EVERY LEG IS BEST-EFFORT AND NONE OF THEM CAN 502 THE LISTING. A ledger outage used to
      // return the rows bare; it now costs only the dispatch-matched leg, because a v2 Run's
      // Dataset names itself from the lake and the ledger has no record of it either way.
      // Scoped by the same selector so a named query does not group the whole ledger.
      let dispatches: DispatchRef[] = [];
      try {
        dispatches = await materialization.listDispatches({ actor: name, version, dt });
      } catch {
        /* no ledger: rows that know their own Run still name themselves */
      }
      // The identity the name renders is the CALLER WORKFLOW's, snapshotted at start (ADR 0029 §2)
      // and keyed by runId. Asked for the UNION the rows and the ledger between them can resolve,
      // in one round trip. Best-effort within the best-effort join: an unreachable store leaves the
      // rows named from the producing Actor — the documented fallback in `withDatasetNames`.
      let identities: RunWorkflow[] = [];
      try {
        identities = await runWorkflows.list(datasetRunIds(infos, dispatches));
      } catch {
        /* no identities: every row falls back to the Actor's name and version */
      }
      const named = withDatasetNames(infos, dispatches, identities);
      // Layer the stored tags and rename (ADR 0029 §1, §4) over the derived names, keyed by the
      // runId just stamped on each row. Best-effort on the same terms as the ledger join beside it:
      // the record store being unreachable leaves the rows derived-and-untagged, not a failed list.
      try {
        const runIds = named
          .map((i) => i.runId)
          .filter((id): id is string => typeof id === 'string');
        const deviations = await records.list(runIds);
        return withDatasetDeviations(named, deviations);
      } catch {
        return named;
      }
    } catch (err) {
      return reply.code(502).send({ error: `could not list datasets: ${errMessage(err)}` });
    }
  });

  // A bounded preview, rendered SERVER-SIDE.
  //
  // This replaces presigning every parquet file so the browser could range-read them with
  // DuckDB-WASM: 76 MB of WebAssembly, downloaded uncompressed on every visit, to show a few
  // hundred rows in a grid. The server already holds an attached read connection.
  //
  // Bounded on purpose — the answer to "I need more than this" is `kontra dataset query`, which
  // runs on the operator's own machine against the same catalog, not a bigger LIMIT here.
  app.get('/api/datasets/:name/preview', async (req, reply) => {
    const { name } = req.params as { name: string };
    const { kind, version, dt, limit } = req.query as Partial<{
      kind: DatasetKind;
      version: string;
      dt: string;
      limit: string;
    }>;
    const n = limit ? Number.parseInt(limit, 10) : undefined;
    try {
      return await previewDataset(
        store,
        {
          kind: kind === 'standalone' ? 'standalone' : 'output',
          name,
          version,
          dt,
          limit: Number.isFinite(n) ? n : undefined,
        },
        lake
      );
    } catch (err) {
      // An unknown name is the caller's mistake, not a lake failure — 404 so the page can say
      // "that dataset is gone" instead of showing a broker error. Matched by TYPE: this used to be
      // `msg.startsWith('no ')`, which made the 404/502 split a property of the sentence's first
      // word, and the sentence is one the console RENDERS. See `NoSuchDatasetError`.
      if (err instanceof NoSuchDatasetError) {
        return reply.code(404).send({ error: errMessage(err) });
      }
      return reply.code(502).send({ error: `could not preview dataset: ${errMessage(err)}` });
    }
  });

  // Which MACHINES wrote one Dataset, and how many rows each contributed.
  //
  // PER DATASET, ON OPEN — never on the listing poll. `/api/datasets` is a catalog scan measured
  // at 60–75 ms and the app already polls it every 2 s; a group-by is a real column scan, so it
  // is issued once, when an operator opens the Dataset, and answers about the same rows the
  // console's editor reads.
  //
  // UNGATED, like the preview beside it and for the same reason: the SQL is composed here, not
  // typed by an operator, the name is checked against the catalog, and no object-store URL
  // leaves the process — so no token has to be baked into the bundle for it. The bearer on
  // `/query`, `/schema` and `/export` guards operator-typed SQL, which this is not.
  app.get('/api/datasets/:name/provenance', async (req, reply) => {
    const { name } = req.params as { name: string };
    const { kind } = req.query as Partial<{ kind: DatasetKind }>;
    try {
      return await datasetProvenance(
        store,
        { kind: kind === 'standalone' ? 'standalone' : 'output', name },
        lake
      );
    } catch (err) {
      // 404 by TYPE, exactly as the preview above and for the same reason.
      if (err instanceof NoSuchDatasetError) {
        return reply.code(404).send({ error: errMessage(err) });
      }
      return reply.code(502).send({ error: `could not read dataset provenance: ${errMessage(err)}` });
    }
  });

  // Delete a TEMPORARY Dataset — the explicit half of the orphan policy (temp-datasets slice 03).
  //
  // THE ORPHAN POLICY IS EXPLICIT-ONLY: a temp is never swept on a clock and never dropped when its
  // owning Run closes — the whole point of a temp outliving its fleet is that triage happens later,
  // and a delete-at-Run-close would destroy exactly that case. So a temp goes only when a person, or
  // slice 04's UI button calling this route, says so, having seen its owner and age in the listing.
  //
  // TEMPORARIES ONLY, and the route ENFORCES it: `deleteTemporaryDataset` refuses anything with no
  // owner marker, so a durable Dataset — including one a temp was promoted INTO — cannot be destroyed
  // through here (409, not a silent no-op). Deleting a temp leaves such a promoted Dataset fully
  // intact, because promotion is a COPY into the target's own files (slice 02), not a re-registration
  // that shares them.
  //
  // Ungated, like the preview/provenance/list routes beside it and for the same reason: no operator
  // SQL crosses the wire — the name is checked against the owner marker and the catalog — and slice
  // 04's UI reaches it the same unauthenticated way it reaches the rest of this surface.
  app.delete('/api/datasets/:name', async (req, reply) => {
    const { name } = req.params as { name: string };
    try {
      const freed = await deleteTemporaryDataset(store, name, lake);
      return { deleted: true, ...freed };
    } catch (err) {
      if (err instanceof NotTemporaryDatasetError) {
        return reply.code(409).send({ error: errMessage(err) });
      }
      return reply.code(502).send({ error: `could not delete dataset: ${errMessage(err)}` });
    }
  });

  // --- run addressing: actor@version + date, never a UUID ---
  //
  // The plan's acceptance bar is "no generated query uses copied UUIDs or raw physical table
  // names". Physical partitioning stays keyed on `run_id` because an exact-run presigned file
  // is an authorization boundary — but that is a STORAGE decision, and it must not decide how
  // a person names what they want. This route is the translation: `crawl4ai@1.0.0` on the 2nd
  // resolves to the run, and the run id stays inside the tooling.
  app.get('/api/datasets/runs', async (req, reply) => {
    const { actor, version, dt, limit } = req.query as Partial<{
      actor: string;
      version: string;
      dt: string;
      limit: string;
    }>;
    const n = limit ? Number.parseInt(limit, 10) : 50;
    try {
      const runs = await materialization.listDispatches({
        actor,
        version,
        dt,
        limit: Number.isFinite(n) ? Math.min(Math.max(n, 1), 500) : 50,
      });
      return { runs };
    } catch (err) {
      return reply.code(502).send({ error: `could not resolve runs: ${errMessage(err)}` });
    }
  });

  // --- a Dataset's tags and its rename: the record, keyed by runId (ADR 0029 §1, §4) ---
  //
  // THE RECORD IS THE AUTHORITY, and NOTHING here touches a Temporal search attribute — that mirror
  // is issue 03, written from inside the live execution and never read as truth. These routes write
  // the durable record only, which is exactly what lets a Dataset be tagged after its Run has closed
  // (the common case): the store knows nothing about Temporal, so the execution's state is irrelevant.
  //
  // ADDRESSED BY runId — the record's key, which the listing stamps on every output row so a surface
  // holding one can act on it. Tags are a SET: `POST` adds (idempotent, concurrent adds converge),
  // `DELETE` removes. The rename is a single choice: `PUT` sets it, `DELETE` restores the derived
  // default. Every mutation returns the run's whole deviation so a caller updates in place.
  //
  // GATED BY THE RUN TOKEN, and the reasoning that left them open was wrong in a way worth keeping
  // written down. It read: "ungated, like the delete/preview/provenance routes beside it — no
  // operator SQL crosses the wire." SQL is the wrong test. §3 makes a tag the KEEP/COLLECT authority
  // the sweeper reads (`classifySweep` branches on `tagged`), so `DELETE …/tags/:tag` is not a
  // metadata edit — it is the delete button for a Dataset, one sweep tick later, and the inverse
  // pins the whole lake past its TTL forever. A route that can read every dataset (the workbench,
  // in `routes/query.ts`) was gated while the route that can DELETE one was not.
  //
  // `checkOptionalBearer` + RUN_TOKEN_VARS, matching `PUT /api/runs/:runId/workflow` exactly: these
  // are writes ABOUT A RUN, keyed by its id, and they travel with the same authority that started
  // it. Not `checkBearer` — fail-closed would 503 every tag on a box that deliberately leaves
  // `KONTRA_RUN_TOKEN` empty, which is this repo's stated posture for the Run surface. Not
  // INFRA_ROUTE_TOKEN_VARS: a credential a browser can reach must not be able to spend money.
  //
  // WHAT THIS DOES AND DOES NOT FIX. With no token configured these stay open, like the rest of the
  // Run surface — that posture is a separate decision. What it fixes is that they were previously
  // UNPROTECTABLE: setting `KONTRA_RUN_TOKEN` gated `start`, `cancel` and `terminate` and left the
  // one route that deletes data wide open, so an operator who had done everything right still had
  // no way to close it.
  //
  // A response of `{ runId, tags: [], renamedTo?: undefined }` means the record is now empty (removing
  // the last tag, clearing the rename) — the STORE keeps no such row, but the route reports the
  // post-state so the caller sees the emptied set rather than a 404.
  const deviationOf = async (runId: string) => (await records.get(runId)) ?? { runId, tags: [] };

  app.post('/api/datasets/runs/:runId/tags', async (req, reply) => {
    const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const runId = runIdOf(req);
    const { tag } = (req.body ?? {}) as { tag?: unknown };
    try {
      await records.addTag(runId, String(tag ?? ''));
      return await deviationOf(runId);
    } catch (err) {
      if (err instanceof InvalidDeviationError) return reply.code(400).send({ error: errMessage(err) });
      return reply.code(502).send({ error: `could not tag dataset: ${errMessage(err)}` });
    }
  });

  app.delete('/api/datasets/runs/:runId/tags/:tag', async (req, reply) => {
    const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const runId = runIdOf(req);
    const { tag } = req.params as { tag: string };
    try {
      await records.removeTag(runId, tag);
      return await deviationOf(runId);
    } catch (err) {
      if (err instanceof InvalidDeviationError) return reply.code(400).send({ error: errMessage(err) });
      return reply.code(502).send({ error: `could not untag dataset: ${errMessage(err)}` });
    }
  });

  app.put('/api/datasets/runs/:runId/name', async (req, reply) => {
    const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const runId = runIdOf(req);
    const { name } = (req.body ?? {}) as { name?: unknown };
    try {
      await records.setRename(runId, String(name ?? ''));
      return await deviationOf(runId);
    } catch (err) {
      if (err instanceof InvalidDeviationError) return reply.code(400).send({ error: errMessage(err) });
      return reply.code(502).send({ error: `could not rename dataset: ${errMessage(err)}` });
    }
  });

  app.delete('/api/datasets/runs/:runId/name', async (req, reply) => {
    const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const runId = runIdOf(req);
    try {
      await records.clearRename(runId);
      return await deviationOf(runId);
    } catch (err) {
      return reply.code(502).send({ error: `could not reset dataset name: ${errMessage(err)}` });
    }
  });
}
