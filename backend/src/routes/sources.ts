/**
 * REGISTERED FOLDERS — where an operator's own Actors and Workflows live on disk (src/sources.ts).
 *
 * Seven routes: list and register a folder, forget one, list what is in it, read one of its files,
 * serve its worker on this machine, and generate the caller workflow that would dispatch one of
 * its Methods.
 *
 * WHAT IT NEEDS: the {@link SourceStore}, which reads its list through the same `Repo` the catalog
 * does, so a registration survives a restart the way an actor's catalog entry does. `ensureEndpoint`
 * and `removeEndpoint` reach Temporal's Nexus registry from inside the two write handlers; nothing
 * is dialled by registering these routes.
 *
 * ADMISSION: listing and reading are open, like every read on this API. REGISTERING, FORGETTING AND
 * SERVING ARE NOT, and take the same `checkOptionalBearer` + `RUN_TOKEN_VARS` as `serve` and
 * `start`: a registered folder is one `serve` may execute code from, so registering one grants
 * exactly the authority that route spends. Getting that wrong would make the read-only-by-default
 * posture a fiction — an open register endpoint is an open serve endpoint one call later. The
 * caller-generation route is open because it returns bytes and starts nothing.
 *
 * WHAT IS NOT HERE: `POST /api/sources/actor/:id/probe`. It hangs off this prefix because it takes
 * the folder's Actor and version, but it STARTS A RUN — see `routes/probe.ts`, which takes the same
 * `SourceStore` for exactly that reason.
 */

import { readFileSync } from 'node:fs';
import type { FastifyInstance } from 'fastify';

import { AlreadyServing, callerFor, serveActor } from '../actorControl';
import { checkOptionalBearer } from '../auth';
import { ensureEndpoint, removeEndpoint } from '../nexusRegistry';
import { MARKER, SourceMissing, SourceRefused, defaultRoot, filesIn, resolveInside } from '../sources';
import type { SourceStore } from '../sourceStore';
import { ControlRefused, RUN_TOKEN_VARS } from '../workflowControl';
import { errMessage } from './errors';

export function registerSourceRoutes(app: FastifyInstance, sources: SourceStore): void {
  /**
   * REGISTERED FOLDERS — where an operator's Actors and Workflows live on disk (src/sources.ts).
   *
   * Listing is open, like every read here. REGISTERING IS NOT, and takes the same admission as
   * serve: a registered folder is one `serve` may execute code from, so registering one grants
   * exactly the authority that route spends. Getting that wrong would make the read-only-by-default
   * posture a fiction — an open register endpoint is an open serve endpoint one call later.
   */
  app.get('/api/sources/:kind', async (req, reply) => {
    const { kind } = req.params as { kind: string };
    if (kind !== 'actor' && kind !== 'workflow') {
      return reply.code(404).send({ error: `${kind}: not a kind of source` });
    }
    try {
      return { kind, defaultRoot: defaultRoot(kind), sources: sources.list(kind) };
    } catch (err) {
      return reply.code(502).send({ error: `could not list ${kind}s: ${errMessage(err)}` });
    }
  });

  app.post('/api/sources/:kind', async (req, reply) => {
    const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { kind } = req.params as { kind: string };
    if (kind !== 'actor' && kind !== 'workflow') {
      return reply.code(404).send({ error: `${kind}: not a kind of source` });
    }
    const body = (req.body ?? {}) as { path?: string };
    let registered;
    try {
      registered = sources.register(kind, body.path ?? '');
    } catch (err) {
      // A refusal is the operator's typo — the message names the path and the file it wanted, and
      // it is the entire content of the answer. A 502 here would read as "kontra is broken".
      if (err instanceof SourceRefused) return reply.code(400).send({ error: err.message });
      return reply.code(502).send({ error: `could not register: ${errMessage(err)}` });
    }

    /**
     * THE NEXUS ENDPOINT IS PART OF REGISTERING, AND IT IS THE SECOND HALF.
     *
     * A registration is now two facts: this folder is on this disk (written above, from a
     * filesystem read that cannot fail on a cluster being down), and this Actor is ADDRESSABLE at
     * `kontra-<name>-<version>` (written here, over the network). Doing them in that order is what
     * makes a cluster outage cost the endpoint rather than the registration.
     *
     * ONLY FOR AN ACTOR. A workflow is a CALLER — it dispatches, nothing dispatches to it — so an
     * endpoint aimed at its queue would be a route with no service behind it, and the first call
     * through it would time out instead of failing as the design error it is.
     *
     * A FAILURE IS REPORTED, NOT RAISED. The folder is registered either way and the answer says
     * which state it is in, because a 502 here would tell an operator that registering failed while
     * the row they asked for sits in the database.
     */
    if (kind === 'actor' && registered.version !== '') {
      const result = await ensureEndpoint(registered.name, registered.version);
      if (result.state !== 'failed') {
        sources.recordEndpoint(kind, registered.id, result.endpoint);
        return { ...registered, endpoint: result.endpoint, endpointState: result.state };
      }
      return { ...registered, endpointError: result.detail };
    }
    return registered;
  });

  app.delete('/api/sources/:kind/:id', async (req, reply) => {
    const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { kind, id } = req.params as { kind: string; id: string };
    const decoded = decodeURIComponent(id);
    // READ BEFORE DELETE: the row is what names the endpoint to remove, and after `forget` there is
    // nothing left to ask. A folder whose registration predates endpoints has none, which is not a
    // failure — it is a row written before this existed.
    const before = kind === 'actor' ? sources.get('actor', decoded) : undefined;
    let forgotten: boolean;
    try {
      forgotten = sources.forget(decoded);
    } catch (err) {
      if (err instanceof SourceRefused) return reply.code(400).send({ error: err.message });
      return reply.code(502).send({ error: `could not forget: ${errMessage(err)}` });
    }

    /* THE ENDPOINT GOES WITH THE REGISTRATION THAT MADE IT. Thirty-one of them accumulated on this
       cluster under the old rule, where worker boot created them and nothing ever removed one —
       each a live route to a task queue nobody polls, and each one indistinguishable from a real
       Actor to anybody reading the endpoint list.

       A FAILURE HERE DOES NOT UN-FORGET ANYTHING. The registration is the operator's to drop and it
       is already dropped; a leaked endpoint is a fact to report, not a reason to put a row back
       that somebody asked to remove. */
    if (forgotten && before?.name && before.version) {
      const result = await removeEndpoint(before.name, before.version);
      if (result.state === 'failed') {
        return { forgotten, endpointError: result.detail, endpoint: result.endpoint };
      }
      return { forgotten, endpoint: result.endpoint, endpointState: result.detail ?? 'deleted' };
    }
    return { forgotten };
  });

  /**
   * What is IN a registered folder, for the editor's file list.
   *
   * An Actor's folder is not one file. `actor.json` names it, `actor.py` is the code, and
   * `description.md` is the Method documentation the pages read — so an editor that could only
   * open the marker (which is what `GET …/file` with no `name` does) could never open the two
   * files anybody wants to edit, and the operator had no way to learn what else was in there.
   *
   * `filesIn` lists one level and only names `resolveInside` would accept, so every row it returns
   * is a row the viewer can open. A dotfile in the folder is therefore not listed — which is the
   * honest answer, because a `.env` beside an actor is not readable here either.
   */
  app.get('/api/sources/:kind/:id/files', async (req, reply) => {
    const { kind, id } = req.params as { kind: string; id: string };
    if (kind !== 'actor' && kind !== 'workflow') {
      return reply.code(404).send({ error: `${kind}: not a kind of source` });
    }
    const source = sources.get(kind, decodeURIComponent(id));
    if (!source) return reply.code(404).send({ error: `${id}: no such registered folder` });
    return { path: source.path, files: filesIn(source) };
  });

  /**
   * One registered folder's file, for the editor. The allowlist is the registration.
   *
   * A FILE THE FOLDER DOES NOT HAVE IS A 404, on both of the two paths that reach that fact.
   *
   * `description.md` is optional — an Actor written without one is an Actor, not a malformed
   * request — so this is the ordinary case rather than an edge, and it used to be answered twice
   * over and wrongly both times: `resolveInside`'s `existsSync` refused it as an ordinary
   * `SourceRefused` (400, "you asked for something you may not have"), and the `readFileSync`
   * ENOENT that gets there when the file goes between the two calls fell into the catch-all (502,
   * "kontra is broken"). The register route beside this one already names that reading in so many
   * words. 502 keeps the meaning it should have had: this process could not do its job, which for
   * a local read is a permission or an I/O error and not an absent file.
   *
   * 404 BEFORE 400: `SourceMissing` extends `SourceRefused`, so the ORDER of the two lines is the
   * whole distinction — exactly as `AlreadyServing` extends `ControlRefused` on the serve route
   * below, and with the same consequence if they are ever swapped.
   */
  app.get('/api/sources/:kind/:id/file', async (req, reply) => {
    const { kind, id } = req.params as { kind: string; id: string };
    const { name } = (req.query ?? {}) as { name?: string };
    if (kind !== 'actor' && kind !== 'workflow') {
      return reply.code(404).send({ error: `${kind}: not a kind of source` });
    }
    const source = sources.get(kind, decodeURIComponent(id));
    if (!source) return reply.code(404).send({ error: `${id}: no such registered folder` });
    const file = name || MARKER[kind];
    try {
      return { name: file, source: readFileSync(resolveInside(source, file), 'utf8') };
    } catch (err) {
      if (err instanceof SourceMissing) return reply.code(404).send({ error: err.message });
      if (err instanceof SourceRefused) return reply.code(400).send({ error: err.message });
      if ((err as NodeJS.ErrnoException).code === 'ENOENT') {
        return reply.code(404).send({ error: `${file}: no such file in ${source.path}` });
      }
      return reply.code(502).send({ error: `could not read: ${errMessage(err)}` });
    }
  });

  /* THERE IS NO `PUT /api/sources/:kind/:id/file` ANY MORE (ADR 0033 §6, ADR 0030).
     It had exactly one caller: the Actors page writing a generated caller into a registered
     Workflow folder, which was the errand ADR 0033 removed — the page CALLS the Method now, and
     the generated caller survives beside the Run button as a read-only, copyable artefact. ADR
     0030 had already made both editors read-only viewers and warned that a later simplification
     "must not fold that write away with it"; this is not that simplification. It removes the write
     because the feature that needed it is gone, and an unused route is removed outright rather
     than left as a surface with no caller. `sources.writeInside` went with it, being its only use. */

  /**
   * Serve a registered Actor's worker on THIS machine. Local only — see actorControl.ts for why
   * the docker and fleet modes are not reachable from a button.
   */
  app.post('/api/sources/actor/:id/serve', async (req, reply) => {
    const denied = checkOptionalBearer(req.headers.authorization, RUN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { id } = req.params as { id: string };
    const source = sources.get('actor', decodeURIComponent(id));
    if (!source) return reply.code(404).send({ error: `${id}: no such registered folder` });
    // `restart` KILLS A RUNNING WORKER, so it is only ever what the caller asked for — never a
    // retry this route decides on. A missing body is no restart, which is what a bare press means.
    const restart = (req.body as { restart?: unknown } | undefined)?.restart === true;
    try {
      return await serveActor(source, { restart });
    } catch (err) {
      // 409 BEFORE 400: `AlreadyServing` extends `ControlRefused`, so the order of these two lines
      // is the difference between a state the page can offer a button for and a failure it reports.
      if (err instanceof AlreadyServing) {
        return reply.code(409).send({ error: err.message, serving: true });
      }
      if (err instanceof ControlRefused) return reply.code(400).send({ error: err.message });
      return reply.code(502).send({ error: `could not serve: ${errMessage(err)}` });
    }
  });

  /**
   * The caller workflow that dispatches one Method over a Batch — SOURCE, and only source.
   *
   * IT SURVIVES ADR 0033 AS AN ARTEFACT, not as an errand. The Actors page no longer offers to
   * write this anywhere: it shows it read-only beside the Run button, regenerating as the form
   * changes, because it is the code the probe itself runs — `catalog.actor(...)`, a callable
   * handle, a destructured `(results, dropped)`, an optional Dataset writer. Copying beats writing
   * for the reason ADR 0030 gave the viewers: a file on disk can diverge from what actually ran,
   * and this one now always has something to diverge from.
   *
   * STILL NOT A DISPATCH. This route returns bytes; `POST …/probe` (routes/probe.ts) is the one
   * that starts anything, and the two are deliberately separate — reading the code you are about
   * to run should not require running it.
   */
  app.post('/api/sources/actor/:id/caller', async (req, reply) => {
    const { id } = req.params as { id: string };
    const source = sources.get('actor', decodeURIComponent(id));
    if (!source) return reply.code(404).send({ error: `${id}: no such registered folder` });
    const body = (req.body ?? {}) as { method?: string; units?: unknown; dataset?: string };
    const method = (body.method ?? '').trim();
    // An OPTIONAL output Dataset name (ADR 0028 §2). Given one, the generated caller opens its
    // writer and hands it to the Method as the second positional argument, so the results publish
    // under a name a query can reach; omitted, they stay a chainable Batch. Bounded like a Method
    // name for the same reason — this string reaches the file the operator is about to run.
    const dataset = (body.dataset ?? '').trim();
    if (dataset && !/^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$/.test(dataset)) {
      return reply.code(400).send({ error: `${JSON.stringify(dataset)} is not a Dataset name` });
    }
    // THE BOUND IS A NAME'S, NOT A PYTHON IDENTIFIER'S, and it used to be the latter. The Go SDK's
    // `core.Registry.AddMethod` takes any non-empty string, so `dns-facts` is a Method a worker can
    // and does self-register — and every one of them was refused here as "not a Method name" about a
    // name the catalog on the page beside it was showing. `callerFor` names the Method in a string
    // argument rather than as an attribute, so a dash costs it nothing. What is still refused is
    // what could escape the file: quotes, backslashes, whitespace, newlines.
    if (!/^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$/.test(method)) {
      return reply.code(400).send({ error: `${JSON.stringify(method)} is not a Method name` });
    }
    return {
      filename: 'workflow.py',
      source: callerFor(source.name, source.version, method, body.units ?? [], dataset || undefined),
    };
  });
}
