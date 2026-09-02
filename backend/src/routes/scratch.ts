/**
 * THE TWO OPAQUE-DOCUMENT SURFACES — a Scratch, and the design document that preceded it.
 *
 * Both are drawings the editor saves and loads, both are stored whole in the {@link Repo}, and the
 * server reads INTO neither of them to execute anything: dispatching is the caller's, in the
 * caller's own workflow (ADR 0023 §12). They share a module because they share that property and
 * that store — nine routes, one dependency, no Temporal.
 *
 * THE ONE ROUTE THAT IS NOT OPAQUE is `GET /api/scratch/:id/spec`, which resolves the drawing's
 * names against the actor catalog and renders markdown. It reads the catalog through the same
 * `Repo`, which is why the catalog is not a second dependency here.
 *
 * WHAT IT NEEDS: the {@link Repo}. UNGATED, reads and writes alike — see `GET /api/scratch` below
 * for why a sketch that runs nothing is not where the gate belongs.
 */

import type { FastifyInstance } from 'fastify';

import type { Repo } from '../db/repo';
import { renderScratch, resolveScratch } from '../scratch';

export function registerScratchRoutes(app: FastifyInstance, repo: Repo): void {
  /**
   * SCRATCHES — a drawing of an orchestration, and the spec an agent reads back from it.
   *
   * The working loop this exists for: draw it, copy the URL, and tell an agent
   * `use kontra mcp to build this workflow <url>`. So the READ is the important half — a canvas
   * nothing else can read is a whiteboard photo.
   *
   * UNGATED, like every other read here, and the write is too. A Scratch runs nothing: it is a
   * sketch that PRODUCES code somebody then reads. The gate belongs on serving and starting, which
   * is where it is.
   */
  app.get('/api/scratch', async () => repo.listScratches());

  app.get('/api/scratch/:id', async (req, reply) => {
    const { id } = req.params as { id: string };
    const rec = repo.getScratch(id);
    return rec ?? reply.code(404).send({ error: 'not found' });
  });

  /**
   * The Scratch as an AGENT reads it: markdown, every name resolved against the catalog.
   *
   * Not the raw document. That is coordinates and ids, and an agent reading it would spend its
   * attention reconstructing what a person sees at a glance. This is the same information with the
   * geometry dropped, the Methods' own descriptions joined in, and everything the drawing names
   * that the catalog does NOT have called out by name — so the agent writes a dispatch against a
   * Method that exists, or says plainly that it cannot.
   */
  app.get('/api/scratch/:id/spec', async (req, reply) => {
    const { id } = req.params as { id: string };
    const rec = repo.getScratch(id);
    if (!rec) return reply.code(404).send({ error: 'not found' });
    const catalog = repo.listActors();
    const resolved = resolveScratch(rec, catalog);
    return reply.type('text/markdown; charset=utf-8').send(renderScratch(resolved, catalog));
  });

  app.post('/api/scratch', async (req, reply) => {
    const body = (req.body ?? {}) as Partial<{ id: string; name: string; document: unknown }>;
    if (!body.name) return reply.code(400).send({ error: 'a scratch needs a name' });
    return repo.saveScratch({
      ...(body.id === undefined ? {} : { id: body.id }),
      name: body.name,
      document: body.document ?? {},
    });
  });

  app.delete('/api/scratch/:id', async (req, reply) => {
    const { id } = req.params as { id: string };
    return repo.deleteScratch(id) ? { deleted: id } : reply.code(404).send({ error: 'not found' });
  });

  // --- designs ---
  //
  // Opaque documents the editor saves and loads. The server does not read into one and can no
  // longer execute one: dispatching is the caller's, in the caller's own workflow (ADR 0023
  // §12). The route keeps its `/api/graphs` spelling because it is what the editor and the CLI
  // already call; what it stores is a canvas, not an execution plan.
  app.get('/api/graphs', async () => repo.listGraphs());

  app.get('/api/graphs/:id', async (req, reply) => {
    const { id } = req.params as { id: string };
    const rec = repo.getGraph(id);
    return rec ?? reply.code(404).send({ error: 'not found' });
  });

  app.post('/api/graphs', async (req, reply) => {
    const body = req.body as Partial<{ id: string; name: string; document: unknown }>;
    if (!body?.name || body.document === undefined) {
      return reply.code(400).send({ error: 'requires name and document' });
    }
    return repo.saveGraph({ id: body.id, name: body.name, document: body.document });
  });

  app.delete('/api/graphs/:id', async (req, reply) => {
    const { id } = req.params as { id: string };
    return repo.deleteGraph(id) ? { ok: true } : reply.code(404).send({ error: 'not found' });
  });
}
