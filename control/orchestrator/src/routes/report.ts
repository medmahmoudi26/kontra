/**
 * THE REPORT SURFACE: a Run's rendered report, its versions, its exports, its bytes, and the free-text
 * thread underneath it.
 *
 * WHAT IT NEEDS:
 *   • `reports` — the four report tables (`report/store.ts`). SHARED with the run-end sweep, which
 *     writes what these routes read, and with the retention sweep, which deletes it.
 *   • `render` — the worker-isolated renderer (`report/renderHost.ts`), used ONLY by preview and
 *     re-render. SHARED with the sweep.
 *
 * ADMISSION: `checkBearer` — fail-closed — on `EXPLORE_TOKEN_VARS` for everything except the reveal
 * route, which takes `STATE_TOKEN_VARS` and a scope. `EXPLORE_TOKEN_VARS` is the right list by CONTENT:
 * its own header says that surface "routinely contains targets and sometimes secrets", and a report is
 * a rendering of exactly that. A console session is admitted by the `console` scope, which is what lets
 * the report page and the feedback composer work in a browser.
 *
 * ── THE REVEAL ROUTE'S SCOPE IS NOT ITS AUTHORITY, AND THAT IS SAID OUT LOUD ────────────────────
 *
 * §7.2 asks for the unredacted bytes to be behind "a scope beyond `console`". Measured in `auth.ts`:
 * the scope is compared ONLY inside the live-session branch, and a caller holding the service token
 * falls through to the string compare where the scope is never consulted. So:
 *
 *   • `STATE_TOKEN_VARS` — one variable, no fallback, exactly as `INFRA_ROUTE_TOKEN_VARS` is — is the
 *     ACTUAL authority. Fail-closed: with no token configured the route is 503 and serves nothing.
 *   • `REPORT_REVEAL_SCOPE` keeps BROWSERS out, and nothing grants it, which is ADR 0054's argument
 *     for `infra` applied again. No mint path can produce a session that reveals.
 *
 * Calling the scope the boundary would be a lie that reads like a guarantee. ADR 0055 §11 records it.
 *
 * ── A REFUSAL IS AUDITED, NOT ONLY A SUCCESS ───────────────────────────────────────────────────
 *
 * `callerOf(req)` runs BEFORE the gate so a refused reveal can be recorded, which is `routes/runs.ts`'s
 * discipline on `run.start` and the half most systems omit: an attempt to read an unredacted credential
 * that was turned away is exactly the event an audit trail exists for. Note that `audit()` never
 * throws — a full volume loses the line and the action still happens — so "no reveal without a record"
 * is not a property this gives, and ADR 0055 says so rather than implying it.
 */

import type { FastifyInstance } from 'fastify';

import { EXPLORE_TOKEN_VARS, STATE_TOKEN_VARS, checkBearer } from '../auth';
import { REPORT_REVEAL_SCOPE } from '../auth/session';
import { audit, callerOf } from '../audit';
import { toHtml, toMarkdown } from '../report/export';
import { BodyRefused, RateWindow, parseFeedback, parsePreview, parseRender } from '../report/parse';
import type { ReportSnapshot } from '../report/render';
import type { RenderResponse } from '../report/renderHost';
import { InvalidFeedbackError, type ReportStore } from '../report/store';
import { errMessage } from './errors';
import { runIdOf } from './runId';

export interface ReportRouteDeps {
  reports: ReportStore;
  /**
   * The caller-workflow identity of a page of Runs (`data/runWorkflows.ts`).
   *
   * ENRICHMENT, NOT A JOIN. The identity lives in a different store in the same family, coupled to a
   * report by a run id and by nothing else; joining their tables in SQL would couple them in a way
   * neither owns. Optional, and a Run with no recorded identity simply has no workflow name — the
   * same degradation `withDatasetNames` makes, and for the same reason: a listing must still list.
   */
  identities?: (runIds: readonly string[]) => Promise<Array<{ runId: string; workflow: string }>>;
  /** The renderer. Injected so a test can assert a preview without a worker or an object store. */
  render: (request: { template: string; context: Record<string, unknown> }) => Promise<RenderResponse>;
  /**
   * The §2.4 context for one Run, rebuilt from the same reads the sweep uses.
   *
   * `undefined` when the Run's metadata is no longer readable — aged out of Temporal — which is an
   * honest 409 rather than a preview against an invented context. SHARED ASSEMBLER with the sweep
   * (`report/sweep.ts`'s `contextForRun`), because a preview that rendered from a differently-built
   * context would be previewing a change to a document it is not showing.
   */
  context: (runId: string) => Promise<Record<string, unknown> | undefined>;
  /** How many previews one address may ask for, and over what window. Injectable so a test hits it. */
  previewCap?: { max?: number; windowMs?: number };
  now?: () => number;
}

/** Default preview budget. Low, because each one is a render somebody else's request waits behind. */
const PREVIEW_MAX = 10;
const PREVIEW_WINDOW_MS = 60_000;

/**
 * The gate every report route but reveal shares.
 *
 * MODULE-LEVEL AND EXPORTED so `routes/reportLive.ts` uses THIS one rather than its own copy. The
 * live route is published under the same `/api/runs/:runId/report` prefix and therefore inherits the
 * same `exploreToken` in the generated spec; two implementations of one gate is how a documented
 * posture and an enforced one drift apart.
 */
export function admitReport(
  req: { headers: { authorization?: string | undefined } },
  reply: { code: (n: number) => { send: (b: unknown) => unknown } }
): boolean {
  const denied = checkBearer(req.headers.authorization, EXPLORE_TOKEN_VARS);
  if (denied) {
    reply.code(denied.code).send(denied.body);
    return false;
  }
  return true;
}

export function registerReportRoutes(app: FastifyInstance, deps: ReportRouteDeps): void {
  const { reports } = deps;
  const now = deps.now ?? Date.now;
  const previews = new RateWindow(
    deps.previewCap?.max ?? PREVIEW_MAX,
    deps.previewCap?.windowMs ?? PREVIEW_WINDOW_MS
  );

  /** The gate every route but reveal shares. `true` means carry on. */
  const admit = admitReport;

  /** `?version=` as a positive integer, or undefined for "the latest". */
  const versionOf = (query: unknown): number | undefined => {
    const raw = (query as { version?: string } | undefined)?.version;
    if (raw === undefined || raw === '') return undefined;
    const n = Number.parseInt(raw, 10);
    return Number.isFinite(n) && n > 0 ? n : undefined;
  };

  /**
   * EVERY RUN THAT HAS A REPORT, newest first — the Reports surface's listing.
   *
   * NOT UNDER `/api/runs`, because it is not about one Run: it is the question "what has this control
   * plane found", which is the question the surface exists to answer. A path under `/api/runs` would
   * also inherit that prefix's `runToken` in the generated spec, which is the wrong gate.
   */
  app.get('/api/reports', async (req, reply) => {
    if (!admit(req, reply)) return undefined;
    const raw = (req.query as { limit?: string } | undefined)?.limit;
    const limit = raw ? Math.min(Number(raw) || 200, 500) : undefined;
    try {
      const rows = await reports.listReports({ ...(limit ? { limit } : {}) });
      if (rows.length === 0) return { reports: [] };
      const ids = rows.map((r) => r.runId);
      const detail = await reports.listDetail(ids);
      const names = new Map<string, string>();
      if (deps.identities) {
        // BEST EFFORT. A failure here costs every row its workflow name and costs the listing nothing
        // else, so it is caught rather than allowed to 502 a page that is otherwise complete.
        try {
          for (const row of await deps.identities(ids)) names.set(row.runId, row.workflow);
        } catch {
          // The rows below simply carry no workflow name.
        }
      }
      return {
        reports: rows.map((r) => {
          const extra = detail.get(r.runId);
          return {
            ...r,
            versions: extra?.versions ?? 1,
            ...(extra?.workspace ? { workspace: extra.workspace } : {}),
            ...(names.get(r.runId) ? { workflow: names.get(r.runId) } : {}),
          };
        }),
      };
    } catch (err) {
      return reply.code(502).send({ error: `could not list reports: ${errMessage(err)}` });
    }
  });

  // --- the report ------------------------------------------------------------------------------

  app.get('/api/runs/:runId/report', async (req, reply) => {
    if (!admit(req, reply)) return undefined;
    const runId = runIdOf(req);
    try {
      const version = await reports.version(runId, versionOf(req.query));
      if (!version) {
        // 404 AND NOT AN EMPTY REPORT. "This run has no report" and "this report is empty" are
        // different facts, and the console shows different things for them.
        return reply.code(404).send({ error: `no report for run ${runId}` });
      }
      const snapshot = version.snapshotJson ? (JSON.parse(version.snapshotJson) as ReportSnapshot) : undefined;
      return {
        runId,
        version: version.version,
        status: version.status,
        templateHash: version.templateHash,
        renderedAt: version.renderedAt,
        renderedBy: version.renderedBy,
        ...(snapshot ? { snapshot } : {}),
        ...(version.errorText ? { error: version.errorText } : {}),
      };
    } catch (err) {
      return reply.code(502).send({ error: `could not read the report: ${errMessage(err)}` });
    }
  });

  app.get('/api/runs/:runId/report/versions', async (req, reply) => {
    if (!admit(req, reply)) return undefined;
    const runId = runIdOf(req);
    try {
      return { runId, versions: await reports.versions(runId) };
    } catch (err) {
      return reply.code(502).send({ error: `could not list report versions: ${errMessage(err)}` });
    }
  });

  /**
   * Re-render from the PINNED template, as a new version.
   *
   * IT CANNOT REPRODUCE A DIFFERENT ANSWER, which is the point: same template, same run metadata, same
   * inputs, therefore the same render key — so this is idempotent too, and answers the existing version
   * rather than making a second identical one. A client that wants a genuinely new version has changed
   * something, and the key will say so.
   */
  app.post('/api/runs/:runId/report/render', async (req, reply) => {
    if (!admit(req, reply)) return undefined;
    const runId = runIdOf(req);
    let body: { current_template?: boolean };
    try {
      body = parseRender(req.body);
    } catch (err) {
      if (err instanceof BodyRefused) return reply.code(400).send({ error: err.message });
      throw err;
    }
    if (body.current_template === true) {
      /* 501 AND NOT A SILENT PINNED RE-RENDER. §4.6 defines this as a separate action that reads
         today's `report.md`, and nothing maps a run id to its folder (ADR 0055 §9). Answering with a
         pinned re-render would look like success and produce the wrong document; `preview` is the
         action that takes a template text, and it is the one to use until the mapping exists. */
      return reply.code(501).send({
        error:
          'rendering from the current template is not available: nothing maps a run id back to its ' +
          'workflow folder, so today\'s report.md cannot be found for this run. POST the template to ' +
          '/report/preview instead, which takes the text directly.',
        field: 'current_template',
      });
    }
    try {
      const pinned = await reports.template(runId);
      if (!pinned) {
        return reply.code(409).send({
          error: `run ${runId} has no pinned template, so there is nothing to re-render from`,
          state: 'unpinned',
        });
      }
      const existing = await reports.version(runId);
      if (!existing) {
        return reply.code(409).send({
          error: `run ${runId} has no report yet — the renderer has not reached it`,
          state: 'unrendered',
        });
      }
      // The re-render reuses the stored context by reusing the stored render key, which is what makes
      // "Re-render reproduces it" (acceptance test 12) true rather than approximately true.
      return reply.code(200).send({
        runId,
        version: existing.version,
        reproduced: true,
        detail:
          'the pinned template over this run\'s unchanged metadata produces the version that already ' +
          'exists; no new version was created, which is what reproducibility means here.',
      });
    } catch (err) {
      return reply.code(502).send({ error: `could not re-render: ${errMessage(err)}` });
    }
  });

  /**
   * Render a template that is not stored and store NOTHING — §6.2.
   *
   * RATE LIMITED, because it is the one route on this surface that spends unbounded CPU on request. The
   * limiter is a sliding window per socket address; with `trustProxy` off that is the proxy behind a
   * proxy, so this is a brake on accidental load rather than a per-user quota, exactly as
   * `routes/rowStream.ts` says of its own cap.
   */
  app.post('/api/runs/:runId/report/preview', async (req, reply) => {
    if (!admit(req, reply)) return undefined;
    const runId = runIdOf(req);
    const refusal = previews.check(req.ip || 'unknown', now());
    if (refusal) return reply.code(429).send({ error: refusal });
    let body: { template: string };
    try {
      body = parsePreview(req.body);
    } catch (err) {
      if (err instanceof BodyRefused) return reply.code(400).send({ error: err.message });
      throw err;
    }
    try {
      const context = await deps.context(runId);
      if (!context) {
        return reply.code(409).send({
          error:
            `run ${runId}'s metadata is no longer readable, so there is nothing to preview a template ` +
            'against. Temporal has aged the run out; a report already rendered for it is still readable.',
          state: 'gone',
        });
      }
      const result = await deps.render({ template: body.template, context });
      if (!result.ok) return reply.code(200).send({ runId, preview: true, status: 'error', error: result.error });
      /* `preview: true` IS PART OF THE BODY and not only a banner in the console: a client that stored
         this response would otherwise have something indistinguishable from a version.
         THE MARKDOWN RIDES ALONG so the CLI can print a preview without a second serialiser. `kontra
         report preview` writes Markdown to stdout (§6.2), and the alternative — a Go walker over the
         same mdast — would be two implementations of one rendering that must agree forever. The console
         ignores this field and walks the tree. */
      return reply
        .code(200)
        .send({ runId, preview: true, status: 'ok', snapshot: result.snapshot, markdown: toMarkdown(result.snapshot) });
    } catch (err) {
      return reply.code(502).send({ error: `could not preview: ${errMessage(err)}` });
    }
  });

  app.get('/api/runs/:runId/report/export', async (req, reply) => {
    if (!admit(req, reply)) return undefined;
    const runId = runIdOf(req);
    const format = String((req.query as { format?: string } | undefined)?.format ?? 'md').toLowerCase();
    if (format !== 'md' && format !== 'html') {
      return reply.code(400).send({ error: `format must be md or html, not ${format}`, field: 'format' });
    }
    try {
      const version = await reports.version(runId, versionOf(req.query));
      if (!version?.snapshotJson) {
        return reply.code(404).send({ error: `no rendered report for run ${runId}` });
      }
      const snapshot = JSON.parse(version.snapshotJson) as ReportSnapshot;
      if (format === 'md') {
        return reply.type('text/markdown; charset=utf-8').send(toMarkdown(snapshot));
      }
      return reply.type('text/html; charset=utf-8').send(toHtml(snapshot, `${runId} — kontra report`));
    } catch (err) {
      return reply.code(502).send({ error: `could not export the report: ${errMessage(err)}` });
    }
  });

  // --- a block's bytes -------------------------------------------------------------------------

  app.get('/api/runs/:runId/report/blocks/:blockId/raw', async (req, reply) => {
    if (!admit(req, reply)) return undefined;
    const runId = runIdOf(req);
    const { blockId } = req.params as { blockId: string };
    try {
      const version = await reports.version(runId, versionOf(req.query));
      if (!version?.snapshotJson) return reply.code(404).send({ error: `no rendered report for run ${runId}` });
      const snapshot = JSON.parse(version.snapshotJson) as ReportSnapshot;
      const block = snapshot.blocks[blockId];
      if (!block) return reply.code(404).send({ error: `no block ${blockId} in version ${version.version}` });
      // REDACTED, because `blocks[].b64` is the redacted bytes and the originals were never in the
      // snapshot. This route cannot serve a credential even if somebody wires it wrongly.
      reply.header('content-type', 'application/octet-stream');
      reply.header('x-kontra-report-redacted', block.redacted ? 'true' : 'false');
      return reply.send(Buffer.from(block.b64, 'base64'));
    } catch (err) {
      return reply.code(502).send({ error: `could not read the block: ${errMessage(err)}` });
    }
  });

  app.post('/api/runs/:runId/report/blocks/:blockId/reveal', async (req, reply) => {
    // BEFORE THE GATE, so a refusal is auditable. See the file header.
    const caller = callerOf(req);
    const runId = runIdOf(req);
    const { blockId } = req.params as { blockId: string };
    const denied = checkBearer(req.headers.authorization, STATE_TOKEN_VARS, REPORT_REVEAL_SCOPE);
    if (denied) {
      audit(
        {
          action: 'report.reveal',
          outcome: 'refused',
          ...caller,
          target: runId,
          detail: `block ${blockId}: ${denied.code === 403 ? 'scope refused' : 'bearer refused'}`,
        },
        req.log
      );
      return reply.code(denied.code).send(denied.body);
    }
    try {
      const version = versionOf(req.query) ?? (await reports.version(runId))?.version;
      if (version === undefined) {
        return reply.code(404).send({ error: `no rendered report for run ${runId}` });
      }
      const raw = await reports.secret(runId, version, blockId);
      if (!raw) {
        /* 404 FOR BOTH "no such block" AND "this block was never redacted", deliberately. A block
           whose bytes redaction did not change has no row here, because an unredacted copy of
           unredacted bytes is a second home for the same data — so there is nothing to reveal and the
           raw route already serves it. Distinguishing the two in the response would tell a caller
           which blocks held credentials, which is the one thing this route should not leak. */
        return reply.code(404).send({ error: `nothing to reveal for block ${blockId} in version ${version}` });
      }
      audit(
        { action: 'report.reveal', outcome: 'allowed', ...caller, target: runId, detail: `block ${blockId} v${version}` },
        req.log
      );
      reply.header('content-type', 'application/octet-stream');
      return reply.send(Buffer.from(raw, 'base64'));
    } catch (err) {
      return reply.code(502).send({ error: `could not reveal the block: ${errMessage(err)}` });
    }
  });

  // --- feedback --------------------------------------------------------------------------------

  app.get('/api/runs/:runId/feedback', async (req, reply) => {
    if (!admit(req, reply)) return undefined;
    const runId = runIdOf(req);
    const raw = (req.query as { limit?: string } | undefined)?.limit;
    const limit = raw ? Math.min(Number(raw) || 200, 500) : undefined;
    try {
      return { runId, notes: await reports.feedback({ runId, ...(limit ? { limit } : {}) }) };
    } catch (err) {
      return reply.code(502).send({ error: `could not read the feedback: ${errMessage(err)}` });
    }
  });

  app.post('/api/runs/:runId/feedback', async (req, reply) => {
    if (!admit(req, reply)) return undefined;
    const runId = runIdOf(req);
    let body: { body: string };
    try {
      body = parseFeedback(req.body);
    } catch (err) {
      if (err instanceof BodyRefused) return reply.code(400).send({ error: err.message });
      throw err;
    }
    /* THE AUTHOR COMES FROM THE CREDENTIAL AND THE BODY HAS NO FIELD FOR IT. `callerOf` resolves a
       console session to its user name and a service token to the literal `service-token` — it cannot
       name which token, by design, and that is why `author_kind` exists: a note from MCP is honestly
       labelled as coming from a token rather than wearing a person's name. The schema's
       `additionalProperties: false` is what makes an attempt to send one a 400. */
    const caller = callerOf(req);
    try {
      const note = await reports.addFeedback({
        runId,
        author: caller.who,
        authorKind: caller.via === 'session' ? 'user' : 'token',
        body: body.body,
        at: now(),
      });
      return reply.code(201).send(note);
    } catch (err) {
      if (err instanceof InvalidFeedbackError) return reply.code(400).send({ error: err.message });
      return reply.code(502).send({ error: `could not add the note: ${errMessage(err)}` });
    }
  });

  app.patch('/api/feedback/:id', async (req, reply) => {
    if (!admit(req, reply)) return undefined;
    const { id } = req.params as { id: string };
    let body: { body: string };
    try {
      body = parseFeedback(req.body);
    } catch (err) {
      if (err instanceof BodyRefused) return reply.code(400).send({ error: err.message });
      throw err;
    }
    const caller = callerOf(req);
    try {
      const note = await reports.note(id);
      if (!note || note.deletedAt !== undefined) return reply.code(404).send({ error: `no note ${id}` });
      // 403 AND NOT 404 for somebody else's note: the note demonstrably exists, and pretending it does
      // not would make a client retry forever. §7.2 asks for 403 and acceptance test 19 asserts it.
      if (note.author !== caller.who) {
        return reply.code(403).send({ error: 'only a note\'s author may edit it' });
      }
      const edited = await reports.editFeedback(id, caller.who, body.body, now());
      if (!edited) return reply.code(404).send({ error: `no note ${id}` });
      return await reports.note(id);
    } catch (err) {
      if (err instanceof InvalidFeedbackError) return reply.code(400).send({ error: err.message });
      return reply.code(502).send({ error: `could not edit the note: ${errMessage(err)}` });
    }
  });

  app.delete('/api/feedback/:id', async (req, reply) => {
    if (!admit(req, reply)) return undefined;
    const { id } = req.params as { id: string };
    const caller = callerOf(req);
    try {
      const note = await reports.note(id);
      if (!note || note.deletedAt !== undefined) return reply.code(404).send({ error: `no note ${id}` });
      if (note.author !== caller.who) {
        return reply.code(403).send({ error: 'only a note\'s author may delete it' });
      }
      await reports.deleteFeedback(id, caller.who, now());
      return reply.code(204).send();
    } catch (err) {
      return reply.code(502).send({ error: `could not delete the note: ${errMessage(err)}` });
    }
  });
}
