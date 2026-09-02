/**
 * THE QUERY WORKBENCH — operator-typed SQL over the lake, its autocomplete, and its download.
 *
 * Three routes, and they are the PRIVILEGED half of the dataset surface. Everything in
 * `routes/datasets.ts` composes its own SQL from a name checked against the catalog; these take a
 * statement a person wrote. That is the only line that matters here, and it is why all three sit
 * behind `checkBearer` + `EXPLORE_TOKEN_VARS` — fail-closed, unlike the run surface's opt-in token
 * — and belong on the private network, not the internet.
 *
 * WHAT THEY CANNOT DO IS BOUNDED BY THE ENGINE, NOT BY THIS FILE. The connection is attached
 * READ_ONLY with LocalFileSystem disabled and the configuration locked, so no statement reaching it
 * can write to the lake, touch a local file, fetch an extension, or raise its own memory ceiling.
 * See data/queryEngine.ts. A route-level allowlist would be a second, weaker copy of that.
 *
 * WHAT IT NEEDS:
 *   - `store`   the {@link ObjectStore} the lake sits on. SHARED — the same one the browser reads.
 *   - `lake`    DuckLake overrides; tests point this at a local directory.
 *   - `harden`  whether the engine keeps its sandbox. TRUE in production, always: the only caller
 *               that passes `false` is a test whose lake is a local directory a hardened engine
 *               cannot open. `queryRoutes.test.ts`'s `serves nothing unsandboxed by default` pins
 *               that a server built without the flag really is hardened.
 */

import { randomUUID } from 'node:crypto';
import type { FastifyInstance } from 'fastify';

import { EXPLORE_TOKEN_VARS, checkBearer } from '../auth';
import type { ObjectStore } from '../codec/objectStore';
import type { LakeConfig } from '../data/parquet';
import {
  EXPORT_FORMATS,
  type ExportFormat,
  exportQuery,
  querySchema,
  runQuery,
} from '../data/queryEngine';
import { errMessage } from './errors';

/** Content types for a downloaded export, so the browser saves rather than renders. */
const CONTENT_TYPES: Record<ExportFormat, string> = {
  csv: 'text/csv',
  parquet: 'application/vnd.apache.parquet',
  json: 'application/json',
  jsonl: 'application/x-ndjson',
};

export interface QueryRouteDeps {
  store: ObjectStore;
  lake: Partial<LakeConfig>;
  /** The engine's sandbox. On unless a test explicitly asked otherwise — see ServerOptions. */
  harden: boolean;
}

export function registerQueryRoutes(app: FastifyInstance, deps: QueryRouteDeps): void {
  const { store, lake, harden } = deps;

  // --- the query workbench ---
  //
  // Operator-typed SQL over the lake. This is a PRIVILEGED surface: it can read every dataset,
  // so it is gated by the same bearer as /api/state/* and belongs on the private network, not
  // the internet. What it cannot do is bounded by the engine, not by this route — the
  // connection is attached READ_ONLY with LocalFileSystem disabled and the configuration
  // locked, so no statement reaching it can write to the lake, touch a local file, fetch an
  // extension, or raise its own memory ceiling. See data/queryEngine.ts.
  app.post('/api/datasets/query', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, EXPLORE_TOKEN_VARS);
    if (denied) {
      if (denied.code === 401) req.log?.warn?.({ path: req.url, ip: req.ip }, 'workbench: rejected');
      return reply.code(denied.code).send(denied.body);
    }
    const { sql, limit, offset, scope } = (req.body ?? {}) as Partial<{
      sql: string;
      limit: number;
      offset: number;
      /** Optional `--version`/`--dt` narrowing of ONE dataset, as the CLI passes it. */
      scope: { name: string; version?: string; dt?: string };
    }>;
    if (typeof sql !== 'string' || !sql.trim()) {
      return reply.code(400).send({ error: 'sql is required' });
    }
    try {
      return await runQuery(store, sql, { limit, offset, lake, harden, scope });
    } catch (err) {
      // A bad query is the OPERATOR'S input, not a server fault — 400 with the engine's message,
      // so the workbench can print it under the editor instead of showing a broker error.
      return reply.code(400).send({ error: errMessage(err) });
    }
  });

  /** Every dataset's columns — the editor's autocomplete and the sidebar tree. */
  app.get('/api/datasets/schema', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, EXPLORE_TOKEN_VARS);
    if (denied) {
      if (denied.code === 401) req.log?.warn?.({ path: req.url, ip: req.ip }, 'workbench: rejected');
      return reply.code(denied.code).send(denied.body);
    }
    try {
      return { datasets: await querySchema(store, { lake, harden }) };
    } catch (err) {
      return reply.code(502).send({ error: `could not read the dataset schema: ${errMessage(err)}` });
    }
  });

  /**
   * Download a query's FULL result.
   *
   * Not row-capped: "show me a page" and "give me the data" are different questions. DuckDB
   * encodes the file (so Parquet costs no encoder here) into a temp object beside the lake —
   * the one filesystem the hardened connection still has — which is streamed back and then
   * DELETED, in a finally, so a failed download does not leave one behind.
   *
   * GET rather than POST because it has to be reachable by a plain browser navigation, which is
   * what makes "Save as…" work without the page buffering the whole file in memory first.
   */
  app.get('/api/datasets/export', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, EXPLORE_TOKEN_VARS);
    if (denied) {
      if (denied.code === 401) req.log?.warn?.({ path: req.url, ip: req.ip }, 'workbench: rejected');
      return reply.code(denied.code).send(denied.body);
    }
    const { sql, format, filename, name, version, dt } = req.query as Partial<{
      sql: string;
      format: string;
      filename: string;
      name: string;
      version: string;
      dt: string;
    }>;
    if (typeof sql !== 'string' || !sql.trim()) {
      return reply.code(400).send({ error: 'sql is required' });
    }
    const fmt = (format ?? 'csv') as ExportFormat;
    if (!EXPORT_FORMATS.includes(fmt)) {
      return reply.code(400).send({ error: `format must be one of ${EXPORT_FORMATS.join(', ')}` });
    }
    let key: string | undefined;
    try {
      const scope = name ? { name, version, dt } : undefined;
      ({ key } = await exportQuery(store, sql, fmt, randomUUID(), { lake, harden, scope }));
      const body = await store.get(key);
      // A downloaded file should be named after the dataset an operator asked for, not after a
      // uuid — but the name comes from the client, so it is reduced to a safe basename rather
      // than trusted: anything that could steer the Content-Disposition header (a quote, a
      // semicolon) or read as a path becomes `_`. The dot is NOT in the allowlist, because the
      // extension is appended here — which also means a `..` cannot survive into a saved name.
      const base =
        (filename ?? 'kontra-export').replace(/[^A-Za-z0-9_-]/g, '_').slice(0, 80) || 'kontra-export';
      reply.header('content-disposition', `attachment; filename="${base}.${fmt}"`);
      reply.header('content-type', CONTENT_TYPES[fmt]);
      return reply.send(body);
    } catch (err) {
      return reply.code(400).send({ error: errMessage(err) });
    } finally {
      // Best-effort: the export object is scratch, and leaving one behind would put a stray
      // object next to the datasets. Never let cleanup failure mask the response.
      if (key) await store.delete(key).catch(() => undefined);
    }
  });
}
