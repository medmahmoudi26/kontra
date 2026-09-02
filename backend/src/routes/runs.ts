/**
 * THE RUN SURFACE — one execution of a caller's workflow, from starting it to watching it move.
 *
 * A Run is identified by that workflow's id (ADR 0023 §12); there is no second identifier, so
 * `:runId` IS the workflow id everywhere below. Eight routes: the list, the start, the one-run
 * read across both status dimensions (ADR 0017), the caller-identity stamp, the stop, the two
 * halves of the host healthcheck beat, and the Temporal-native heartbeat read.
 *
 * IT STARTS A WORKFLOW; IT DOES NOT EXECUTE A BATCH. `POST /api/runs` makes the same
 * `client.workflow.start` the CLI makes, on a queue derived from the caller's folder, refusing
 * outright when no worker polls it — which is why this module needs the queue describer beside the
 * lifecycle. Nothing here runs an Actor in this process.
 *
 * WHAT IT NEEDS:
 *   - `runs`          the {@link RunLifecycle}: the two-authority read (Temporal + the lake).
 *                     SHARED with the hitl and explore modules, which is why it is built in
 *                     `buildServer` rather than here.
 *   - `runWorkflows`  the caller-identity snapshot (ADR 0029 §2) `PUT …/workflow` upserts.
 *                     SHARED with the datasets and retention modules, which render the name it
 *                     decides.
 *   - `queueDescriber` who is POLLING a task queue — a lazily-built Temporal connection, SHARED
 *                     with the pollers module so the page's polling does not open one per request.
 *
 * THE LIVE PROGRESS MAP IS THIS MODULE'S OWN, and deliberately not injectable. It is ephemeral by
 * design — a visibility aid, not durable run state — so nothing outside these two routes may read
 * it, and a test that wanted to assert on it would be asserting on a cache rather than on a Run.
 *
 * ADMISSION: every WRITE takes `checkOptionalBearer` + `RUN_TOKEN_VARS` — open when
 * `KONTRA_RUN_TOKEN` is unset, which is the operator's choice, and closed by setting it with no
 * other change. The reads are open like the rest of this API. See `workflowControl.ts`.
 *
 * WITH ONE EXCEPTION, NAMED HERE BECAUSE A HEADER THAT SAYS "EVERY" AND MEANS "ALL BUT ONE" IS
 * WORSE THAN NO HEADER. `POST /api/runs/:runId/progress` takes no token: its only caller is a
 * worker container that is never given one. What that costs, and why the cap below had to change
 * instead, is written at the handler.
 */

import type { FastifyInstance } from 'fastify';

import { checkOptionalBearer } from '../auth';
import { InvalidRunWorkflowError, type RunWorkflowStore } from '../data/runWorkflows';
import type { QueueDescriber } from '../panels/pollers';
import type { RunLifecycle } from '../runs';
import { describeRunHeartbeats } from '../temporalClient';
import {
  CANCEL_REPORT_MS,
  ControlRefused,
  RUN_TOKEN_VARS,
  clampGrace,
  startRun,
  stopRun,
} from '../workflowControl';
import { errMessage } from './errors';
import { runIdOf } from './runId';

export interface RunRouteDeps {
  /** The two-authority read behind `GET /api/runs` and `GET /api/runs/:runId`. */
  runs: RunLifecycle;
  /** The Run's caller-workflow identity, keyed by runId (ADR 0029 §2). */
  runWorkflows: RunWorkflowStore;
  /** Who is polling a queue — called only when a start needs it, so no connection opens at boot. */
  queueDescriber: () => QueueDescriber;
}

export function registerRunRoutes(app: FastifyInstance, deps: RunRouteDeps): void {
  const { runs, runWorkflows, queueDescriber } = deps;

  // Live progress: the actor host beats @actor.healthcheck output here while a Batch runs;
  // the CLI (and, later, the UI) polls it. Ephemeral by design — a visibility aid, not durable
  // run state — and bounded so long-lived servers don't leak.
  const progress = new Map<string, { nodes: Record<string, unknown>; beatAt: number }>();
  const PROGRESS_CAP = 500;
  /** No beat for this long and a run is not live, so its entry is not what the cap is for. */
  const PROGRESS_TTL_MS = 60 * 60 * 1000;

  // --- runs (READ ONLY) ---
  //
  // A Run is one execution of a caller's workflow, identified by that workflow's id (ADR 0023
  // §12). Nothing here starts one: the caller does, on the caller's own task queue. There is
  // also no second identifier — `:runId` IS the workflow id, so a route that used to take a
  // `workflowId` takes the same string under its real name.
  app.get('/api/runs', async (req, reply) => {
    const { tenant, status, limit } = req.query as Partial<{
      tenant: string;
      status: string;
      limit: string;
    }>;
    const n = limit ? Number.parseInt(limit, 10) : undefined;
    try {
      return await runs.list({ tenant, status, limit: Number.isFinite(n) ? n : undefined });
    } catch (err) {
      return reply.code(502).send({ error: `could not list runs: ${errMessage(err)}` });
    }
  });

  // --- start ---
  //
  // One of the two verbs behind the Playground's buttons (the other, `serve`, is on the workflow
  // surface). It does not move execution back into this process: it is the same
  // `client.workflow.start` the CLI makes. See `workflowControl.ts` — including why it is open by
  // default and what KONTRA_RUN_TOKEN closes.
  //
  // `POST /api/runs` and not `/api/runs/start`: starting a Run is creating one, and the read
  // route for the collection is already `GET /api/runs`.
  app.post('/api/runs', async (req, reply) => {
    const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    // NO QUEUE in the body — it is derived from the folder server-side (GitHub #15). `file` is the
    // registered folder; `type` is the class the page is showing (a folder can declare several), and
    // the queue comes from the folder, never from either string a client sends.
    const body = (req.body ?? {}) as { file?: string; type?: string; input?: unknown };
    try {
      const started = await startRun(
        {
          file: body.file ?? '',
          ...(body.type === undefined ? {} : { type: body.type }),
          ...(body.input === undefined ? {} : { input: body.input }),
        },
        queueDescriber()
      );
      return reply.code(201).send(started);
    } catch (err) {
      if (err instanceof ControlRefused) return reply.code(400).send({ error: err.message });
      return reply.code(502).send({ error: `could not start run: ${errMessage(err)}` });
    }
  });

  /**
   * SNAPSHOT A RUN'S CALLER-WORKFLOW IDENTITY (ADR 0029 §2) — the CLI's half of the stamp.
   *
   * `POST /api/runs` stamps this itself, inside `startRun`. `kontra workflow start` does not go
   * through that route — it dials Temporal directly, which is the whole reason it is fast and works
   * without the orchestrator — so it stamps HERE, right after its start returns. If only one path
   * recorded the identity, what a Dataset is CALLED would depend on which command started the run,
   * which is the same class of silent divergence the derived name exists to remove.
   *
   * WHY THE CLI GOES THROUGH HTTP RATHER THAN WRITING THE STORE. The store is the orchestrator's:
   * on a real install it is Postgres reachable from the controller, and the CLI runs wherever an
   * operator is. The CLI already mutates the Dataset record (tags, renames) over this API for exactly
   * that reason, and giving it database credentials to write one provenance row would be a second,
   * worse way to reach the same table.
   *
   * IDEMPOTENT: `PUT`, an upsert, so a retried stamp is the same fact stated twice. Gated by the run
   * token like `start` and `stop` beside it — it is a write about a Run, and it travels with the same
   * authority that started it. A 400 is a malformed identity (blank or over-long); it is never
   * silently coerced, because a half identity renders a worse name than no identity at all.
   */
  app.put('/api/runs/:runId/workflow', async (req, reply) => {
    const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const runId = runIdOf(req);
    const body = (req.body ?? {}) as { workflow?: unknown; version?: unknown };
    try {
      await runWorkflows.record(runId, String(body.workflow ?? ''), String(body.version ?? ''));
      return (await runWorkflows.get(runId)) ?? { runId, workflow: '', version: '' };
    } catch (err) {
      if (err instanceof InvalidRunWorkflowError) {
        return reply.code(400).send({ error: errMessage(err) });
      }
      return reply.code(502).send({ error: `could not record run workflow: ${errMessage(err)}` });
    }
  });

  /**
   * STOP A RUN. Cancel by default; terminate only when asked, and only after a cancel did not land.
   *
   * Gated by the run token for the same reason `start` is — and arguably more: a run that holds a
   * fleet costs money whichever way this goes, and terminating one costs more than cancelling it.
   *
   * `escalate: false` is `cancel` and never terminates, so an operator who wants the graceful thing
   * gets exactly that and no fallback they did not ask for. `escalate: true` is `terminate`, which
   * cancels first — see `stopRun` for why the forceful-sounding verb has to be the patient one.
   */
  app.post('/api/runs/:runId/stop', async (req, reply) => {
    const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const runId = runIdOf(req);
    const body = (req.body ?? {}) as { escalate?: boolean; force?: boolean; graceMs?: number };
    try {
      // ONE FUNCTION, two verbs, and the difference is one flag — so the two cannot drift into
      // different failure modes. A plain cancel waits only long enough to REPORT (the cancellation
      // is durable and does not need re-sending); a terminate waits the grace period because what
      // happens next depends on the answer.
      const escalate = body.escalate === true;
      return await stopRun(runId, {
        escalate,
        force: escalate && body.force === true,
        graceMs: escalate ? clampGrace(body.graceMs) : CANCEL_REPORT_MS,
      });
    } catch (err) {
      if (err instanceof ControlRefused) return reply.code(404).send({ error: err.message });
      return reply.code(502).send({ error: `could not stop run: ${errMessage(err)}` });
    }
  });

  // One run, across both status dimensions (ADR 0017). 404 only when NEITHER authority has
  // heard of it — a run whose workflow Temporal has dropped for retention, but whose output is
  // still in the lake, is still a run.
  app.get('/api/runs/:runId', async (req, reply) => {
    const runId = runIdOf(req);
    try {
      const view = await runs.read(runId);
      return view ?? reply.code(404).send({ error: 'run not found' });
    } catch (err) {
      return reply.code(502).send({ error: `could not read run: ${errMessage(err)}` });
    }
  });

  /**
   * --- live progress (host @actor.healthcheck beats) ---
   *
   * The actor host POSTs its healthcheck output per dispatch while a Batch runs; the CLI polls GET
   * to stream it. The run id keys it — the host knows the run and the dispatch it serves, and
   * `node` is the wire name that host has always sent for the latter.
   *
   * THIS IS THE ONE WRITE ON THE RUN SURFACE THAT TAKES NO TOKEN, AND THAT IS DELIBERATE — but the
   * combination it used to be half of was not. The map evicted its oldest entry at 500, so any
   * caller who could reach the port could beat 500 fictional run ids and silently delete a live
   * fleet's progress. The eviction is what turned a missing auth check into data loss instead of
   * noise, and only one of the two could be removed:
   *
   * NOT AUTHENTICATED, BECAUSE THE PRODUCER HOLDS NO CREDENTIAL. The only caller is
   * `_post_progress` in `runtime/python/internals/engine.py`, running inside a worker container
   * whose entire environment is the five variables `kontra deploy` prints — `KONTRA_ADDRESS`,
   * `KONTRA_ORCHESTRATOR_URL`, the S3 pair and `KONTRA_REDIS_HOST`. `KONTRA_RUN_TOKEN` is not among
   * them, and that host swallows every failure by design ("progress is a VISIBILITY aid and must
   * never perturb the run"). So `checkOptionalBearer` here would cost an operator who set the token
   * — the operator who did everything right — their live progress panel, with nothing anywhere
   * saying why. That is the same silent failure moved rather than removed.
   *
   * SO THE CAP DOES NOT EVICT. A run already in the map is always updatable; a NEW id is admitted
   * only if there is room, and room is made only by dropping entries that have not beaten in
   * {@link PROGRESS_TTL_MS}. A live run beats continuously, so it can never be the entry that goes
   * — which is precisely the property eviction did not have. A flood of fictional ids now costs a
   * bounded amount of memory and an hour, and can no longer reach into a run somebody is watching.
   *
   * `{ ok: false }` rather than a lie: at the cap with nothing stale, the beat is not recorded, and
   * a 200 saying so keeps the host's contract (never perturb the run) without claiming otherwise.
   */
  app.post('/api/runs/:runId/progress', async (req, reply) => {
    const runId = runIdOf(req);
    const body = (req.body ?? {}) as { node?: string; [k: string]: unknown };
    const node = typeof body.node === 'string' && body.node ? body.node : '_';
    const now = Date.now();

    const cur = progress.get(runId);
    if (cur) {
      cur.nodes[node] = { ...body };
      cur.beatAt = now;
      return reply.send({ ok: true });
    }
    if (progress.size >= PROGRESS_CAP) {
      for (const [id, beats] of progress) {
        if (now - beats.beatAt > PROGRESS_TTL_MS) progress.delete(id);
      }
    }
    if (progress.size >= PROGRESS_CAP) {
      return reply.send({
        ok: false,
        error: `live progress is at its ${PROGRESS_CAP}-run cap and no entry is stale enough to retire`,
      });
    }
    progress.set(runId, { nodes: { [node]: { ...body } }, beatAt: now });
    return reply.send({ ok: true });
  });

  app.get('/api/runs/:runId/progress', async (req) => {
    const runId = runIdOf(req);
    return { nodes: progress.get(runId)?.nodes ?? {} };
  });

  // Live HEARTBEAT progress: the {done,total,node} the RunBatch activity beats, read back off
  // the run's handler backing workflows via DescribeWorkflowExecution. Distinct from /progress
  // (the host healthcheck beat) — this is the Temporal-native per-Batch signal `kontra monitor
  // --state` shows alongside S3 blob counts. Empty (not an error) when the run has no Batch in
  // flight.
  app.get('/api/runs/:runId/heartbeats', async (req, reply) => {
    const runId = runIdOf(req);
    try {
      return { nodes: await describeRunHeartbeats(runId) };
    } catch (err) {
      return reply.code(502).send({ error: `could not read heartbeats: ${errMessage(err)}` });
    }
  });
}
