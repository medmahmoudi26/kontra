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

import { AlreadyServing, callerFor, diskSchema, serveActor } from '../actorControl';
import { checkOptionalBearer } from '../auth';
import { ensureEndpoint, removeEndpoint } from '../nexusRegistry';
import { watchDir } from '../schemaWatch';
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

  /**
   * REGISTERING IS GONE, and this route says so rather than 404ing.
   *
   * A folder is an Actor or a Workflow because it is in the WORKSPACE (see `sourceStore.ts`). The
   * two verbs that used to be here — register a path, forget a path — have no meaning against a
   * directory listing: the way to add one is to put it in the workspace, and the way to remove one
   * is to delete it.
   *
   * 410 AND NOT 404, because an old console or a script that still posts here is not making a typo:
   * it is doing something that USED to work, and the difference between "no such route" and "this
   * was removed, here is what replaced it" is an hour of somebody's afternoon.
   *
   * THE NEXUS ENDPOINT MOVED WITH IT. Registering an Actor used to create `kontra-<name>-<version>`
   * as a side effect; a worker creates it at boot, which is the path every fleet Actor already took
   * — so what is lost is an endpoint for an Actor that has never run, which addressed nothing.
   */
  const gone = (verb: string, instead: string) => ({
    error:
      `${verb} is gone: the workspace is the registration. ${instead}\n` +
      `The workspace is a mounted volume — ${'`'}actors/${'`'} and ${'`'}workflows/${'`'} under it are what this ` +
      `control plane serves, and nothing else is recorded anywhere.`,
  });

  app.post('/api/sources/:kind', async (_req, reply) =>
    reply.code(410).send(gone('registering a folder', 'Put the folder in the workspace.'))
  );

  app.delete('/api/sources/:kind/:id', async (_req, reply) =>
    reply.code(410).send(gone('forgetting a folder', 'Delete the folder from the workspace.'))
  );

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
   * What this folder's Methods take RIGHT NOW, from the files — see `actorControl.ts:diskSchema`.
   *
   * THE EDITOR PANE'S ROUTE. The catalog describes what a worker published at boot, which is the
   * honest answer for the Actors grid and the wrong one beside an editor: the point of a form next
   * to the code is that it tracks the code, and a parameter added a second ago is not in any
   * catalog. Reading it is not a state change, so admission matches the other reads here.
   */
  app.get('/api/sources/actor/:id/schema', async (req, reply) => {
    const { id } = req.params as { id: string };
    const source = sources.get('actor', decodeURIComponent(id));
    if (!source) return reply.code(404).send({ error: `${id}: no such registered folder` });
    try {
      return await diskSchema(source);
    } catch (err) {
      // 400, NOT 502: a schema that will not derive is almost always the author's own file — a
      // syntax error, an import that is not installed, a type the SDK cannot turn into a schema —
      // and the CLI has already put the traceback in the message. Reporting it as a gateway fault
      // would send the reader to the server instead of to the line they just wrote.
      if (err instanceof ControlRefused) return reply.code(400).send({ error: err.message });
      return reply.code(502).send({ error: `could not read the schema: ${errMessage(err)}` });
    }
  });

  /**
   * WHEN to read that schema again. The route above is the read; this one is the doorbell.
   *
   * THE EDITOR IS NOT THE ONLY CLIENT ANY MORE, and that is the whole reason this exists. The VS
   * Code extension made the form instant by hooking its own save event and pushing a reload into
   * the pane, so "instant" was a property of the pane rather than of the runner. A browser tab —
   * now the supported way to use the runner, because a cross-origin frame inside a webview cannot
   * reach the clipboard — has no editor to hook. Without this it falls back to a poll, and a form
   * that catches up eventually is the thing the pane was built to stop being.
   *
   * IT SENDS `changed`, NOT THE SCHEMA. Deriving one is a subprocess (`kontra actor schema`), and
   * doing that per inode event, per watcher, would put a fork on the critical path of every
   * keystroke that happens to hit ⌘S. The client already owns the read and its error handling; it
   * only ever needed to be told. That also keeps this endpoint honest when derivation FAILS — a
   * syntax error mid-edit must leave the last good form on screen, which is `readDisk`'s existing
   * behaviour and would be much harder to preserve if the truth arrived over two channels.
   *
   * Ungated, matching the read it serves and `rowStream.ts` beside it: no `EventSource` can send an
   * Authorization header, and what crosses here is one word that says a file moved.
   */
  app.get('/api/sources/actor/:id/schema/stream', (req, reply) => {
    const { id } = req.params as { id: string };
    const source = sources.get('actor', decodeURIComponent(id));
    if (!source) return reply.code(404).send({ error: `${id}: no such registered folder` });

    const raw = reply.raw;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const changed = (): void => {
      // COALESCED, because one ⌘S is not one event. Editors write a temp file and rename over the
      // target, so a single save arrives as several — and each one unbatched would cost the client
      // a fork. 150ms is below what reads as lag and above the spread of one save's events.
      clearTimeout(timer);
      timer = setTimeout(() => raw.write('data: changed\n\n'), 150);
    };

    // SUBSCRIBE BEFORE THE HEAD IS WRITTEN. `watchDir` refuses at its cap by throwing, and a
    // refusal is only sayable while this is still an ordinary reply — after `writeHead` the only
    // remaining way to decline is to hang up, which a client reads as a network fault rather than
    // as a limit it could act on.
    let stop: () => void;
    try {
      stop = watchDir(source.path, changed);
    } catch (err) {
      return reply.code(503).send({ error: `${errMessage(err)} — close a runner tab and retry` });
    }

    reply.hijack();
    raw.writeHead(200, {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache, no-transform',
      Connection: 'keep-alive',
      // Defeat any reverse-proxy response buffering: an SSE stream held until it "finishes" is a
      // frozen page, which is the exact failure this endpoint exists to prevent.
      'X-Accel-Buffering': 'no',
    });
    // Opens the stream so `EventSource` fires `onopen` now rather than on the first real event —
    // an actor nobody is editing is correctly silent for hours, and "connected and quiet" must not
    // look like "still connecting".
    raw.write(': ok\n\n');

    // Nothing reads this; it exists so an idle proxy does not reap a connection for being quiet.
    const keepalive = setInterval(() => raw.write(': ping\n\n'), 25_000);
    keepalive.unref?.();

    req.raw.on('close', () => {
      clearTimeout(timer);
      clearInterval(keepalive);
      stop();
    });
  });

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
