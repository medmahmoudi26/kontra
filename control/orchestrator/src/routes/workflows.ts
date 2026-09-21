/**
 * THE WORKFLOW SURFACE — a caller's own code: what is on disk, what a worker registered, what is
 * being served, and what this API admits.
 *
 * Seven routes. Two of them are lists that are deliberately NOT joined here
 * (`GET /api/workflows`), one is the worker's self-registration, one reads a file's bytes, three
 * are the serve/pause/resume control surface, and the last says out loud what those three admit.
 *
 * IT SERVES CODE, WHICH IS WHY THE WRITES ARE GATED. `serve` spawns the operator's own
 * `kontra workflow serve … --tmux` and exits; pause and resume stop and restart that process. All
 * three take `checkOptionalBearer` + `RUN_TOKEN_VARS` — open when `KONTRA_RUN_TOKEN` is unset,
 * which is the operator's choice, and `GET /api/workflows/exposure` exists so a page can SHOW that
 * posture rather than assume it. `POST /api/workflows/catalog` is the one write that is open, for
 * the reason its own header gives: a worker has no credential to send.
 *
 * WHAT IT NEEDS: the {@link Repo}, for the registered half of the two lists. Everything else lives
 * in `workflowControl.ts`, which owns the disk and the spawning.
 *
 * WHAT IS NOT HERE: `GET /api/queues/:queue/pollers` — "is anybody serving this queue" is the
 * third state a workflow can be in, but it is a fact about a task queue and Temporal is its only
 * authority, so it has its own module (`routes/pollers.ts`) and its own dependency.
 */

import type { FastifyInstance } from 'fastify';

import { checkOptionalBearer } from '../auth';
import { DescriptorRefused, parseWorkflowDescriptor } from '../catalog';
import type { Repo } from '../db/repo';
import {
  ControlRefused,
  RUN_TOKEN_VARS,
  describeExposure,
  listWorkflows,
  pauseWorkflow,
  readWorkflow,
  serveWorkflow,
  workflowRoot,
} from '../workflowControl';
import { errMessage } from './errors';

export function registerWorkflowRoutes(app: FastifyInstance, repo: Repo): void {
  // The Workflows page's file surface: `.kontra/workflows/`, which is an operator's own code.
  // Listing is open like every other read here; writing takes the same admission as serve/start,
  // because a file written here is a file `serve` will execute.
  app.get('/api/workflows', async (_req, reply) => {
    try {
      // TWO LISTS, JOINED BY NOBODY HERE. `workflows` is what is on this host's disk; `registered`
      // is what a worker said it was serving, keyed by the `@workflow.defn` TYPE. Only the source
      // knows which types a file declares — a file can hold several, and a served worker may be
      // running code that is not in this directory at all — so the join is made where the source
      // is read (the page's `typeFromSource`), and pretending here that one file is one type would
      // put a contract under the wrong workflow.
      return {
        dir: workflowRoot(),
        workflows: listWorkflows(),
        registered: repo.listWorkflows(),
      };
    } catch (err) {
      return reply.code(502).send({ error: `could not list workflows: ${errMessage(err)}` });
    }
  });

  /**
   * A WORKFLOW SELF-REGISTERS ON SERVE — the caller-side peer of `POST /api/actors`.
   *
   * The worker is the only honest registrar: the name is the type Temporal routes on, the schemas
   * are derived from the annotations the code it just imported declares, and the description is
   * that class's docstring. Before this, `GET /api/workflows` could say a file exists and nothing
   * about what it accepts, what it returns or what it is for.
   *
   * OPEN, like `POST /api/actors` and unlike everything else under `/api/workflows/`. The writes
   * on this prefix are gated because they run code from this host's disk; this one writes a
   * description into a table. A worker pushes it with no credential — it has none to have — so
   * gating it would turn every fleet's workflows invisible rather than unregistered.
   *
   * `/catalog` and not the bare `/api/workflows`, which is the FILE surface: a registration names a
   * TYPE and can arrive from a worker serving code on another machine, so the two must not collide.
   */
  /**
   * WHO IS WATCHING A DESCRIPTOR CHANGE — one set of writers, told when the catalog moves.
   *
   * THE CATALOG WRITE IS THE EVENT, and that is the correction. The console subscribed to the
   * FILESYSTEM instead, which fires on save — and a save is not a new contract: the worker has to
   * re-import the module and re-register, which takes seconds. Measured on a live install: the
   * file event arrived, the console re-read twice inside 1.2s, and both reads returned the old
   * descriptor because the worker was still importing. The form never changed and nothing said why.
   */
  const watchers = new Set<(name: string) => void>();

  app.get('/api/workflows/stream', (req, reply) => {
    const raw = reply.raw;
    reply.hijack();
    raw.writeHead(200, {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache, no-transform',
      Connection: 'keep-alive',
      // A reverse proxy that buffers an SSE response turns this into a page that never updates.
      'X-Accel-Buffering': 'no',
    });
    // So `EventSource` fires `onopen` now: a control plane where nobody is editing is correctly
    // silent for hours, and "connected and quiet" must not look like "still connecting".
    raw.write(': ok\n\n');
    const send = (name: string): void => {
      raw.write(`event: descriptor\ndata: ${JSON.stringify({ name })}\n\n`);
    };
    watchers.add(send);
    const keepalive = setInterval(() => raw.write(': ping\n\n'), 25_000);
    keepalive.unref?.();
    req.raw.on('close', () => {
      watchers.delete(send);
      clearInterval(keepalive);
    });
    return reply;
  });

  app.post('/api/workflows/catalog', async (req, reply) => {
    try {
      const saved = repo.upsertWorkflow(parseWorkflowDescriptor(req.body));
      // AFTER THE WRITE, so a subscriber that re-reads on this event cannot read what was there
      // before it. A throwing subscriber must not fail the worker's registration.
      for (const send of [...watchers]) {
        try {
          send(saved.name);
        } catch {
          /* a dead stream is reaped by its own close handler */
        }
      }
      return saved;
    } catch (err) {
      if (err instanceof DescriptorRefused) return reply.code(400).send({ error: err.message });
      throw err;
    }
  });

  app.get('/api/workflows/file/:name', async (req, reply) => {
    const { name } = req.params as { name: string };
    try {
      return { name, source: readWorkflow(decodeURIComponent(name)) };
    } catch (err) {
      if (err instanceof ControlRefused) return reply.code(404).send({ error: err.message });
      return reply.code(502).send({ error: `could not read workflow: ${errMessage(err)}` });
    }
  });

  // THERE IS NO `PUT /api/workflows/file/:name` (ADR 0030). The Workflows page is a read-only
  // viewer; `GET` above reads the bytes on disk, and changing a workflow is the operator's own editor
  // plus a re-serve, never a write from the browser. A dashboard that could write let the file and
  // the registered digest disagree — the same reason ADR 0020 gave Terminals no input path. This
  // route was the only caller of `saveWorkflow`, which went with it. (`PUT /api/sources/:kind/:id/file`
  // survives because generating a caller writes a NEW workflow.py — ADR 0023 §12 — not an in-place
  // edit of deployed code.)

  app.post('/api/workflows/serve', async (req, reply) => {
    // checkOptionalBearer, NOT checkBearer: these two are open when KONTRA_RUN_TOKEN is unset,
    // which is the operator's choice and the opposite of the fail-closed default. See auth.ts.
    const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    // `restart` REPLACES a worker that is already serving this folder. A missing body is no
    // restart, which is what a bare press means — the same posture the actor route takes.
    const body = (req.body ?? {}) as { file?: string; restart?: unknown };
    try {
      return await serveWorkflow({ file: body.file ?? '', restart: body.restart === true });
    } catch (err) {
      // A refusal is the caller's fault (bad path, bad queue, session already there); anything
      // else is ours. Collapsing them into one status is how "you typed it wrong" reads as
      // "the server is broken".
      if (err instanceof ControlRefused) return reply.code(400).send({ error: err.message });
      return reply.code(502).send({ error: `could not serve: ${errMessage(err)}` });
    }
  });

  /**
   * PAUSE / RESUME the worker a workflow is served in.
   *
   * The same admission as serve and start: it stops (or restarts) a process that runs a caller's
   * own code, and a paused caller holding a fleet is still holding it.
   */
  for (const verb of ['pause', 'resume'] as const) {
    app.post(`/api/workflows/file/:name/${verb}`, async (req, reply) => {
      const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
      if (denied) return reply.code(denied.code).send(denied.body);
      const { name } = req.params as { name: string };
      try {
        // No queue: resume re-derives it from the folder (GitHub #15), so there is nothing to accept.
        return await pauseWorkflow({ file: decodeURIComponent(name) }, verb === 'resume');
      } catch (err) {
        if (err instanceof ControlRefused) return reply.code(400).send({ error: err.message });
        return reply.code(502).send({ error: `could not ${verb}: ${errMessage(err)}` });
      }
    });
  }

  // What the two routes above actually admit. The page reads this to SHOW the posture rather
  // than assume it — an open control surface the operator chose is fine; one nobody mentions
  // is not.
  app.get('/api/workflows/exposure', async () => describeExposure());
}
