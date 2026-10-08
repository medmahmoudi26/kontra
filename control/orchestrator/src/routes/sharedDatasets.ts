/**
 * THE CROSS-WORKSPACE BOUNDARY, AS A PERSON OPERATES IT (ADR 0053).
 *
 * Four routes, and they are the only HTTP surface in this server that can read one workspace's lake
 * on behalf of a caller sitting in another:
 *
 *   `GET    /api/datasets/shared`                          what is exposed, across every workspace
 *   `PUT    /api/datasets/shared/:workspace/:name`         grant
 *   `DELETE /api/datasets/shared/:workspace/:name`         revoke
 *   `GET    /api/datasets/shared/:workspace/:name/preview` a BOUNDED read of the granted rows
 *
 * ── THE WORKSPACE IS IN THE PATH, NOT INFERRED ──────────────────────────────────────────────────
 *
 * Every one of them NAMES the owning workspace. The active workspace is never a default, here or in
 * `data/sharedRead.ts`, because a default would make a cross-workspace act expressible by leaving a
 * field out — and "I shared the wrong workspace's Dataset" is not a mistake an address should allow
 * you to make silently. It also means `PUT …/bugbounty/lame` from a console switched to `scraping`
 * is a legible, auditable act rather than an accident.
 *
 * ── ALL FOUR FAIL CLOSED, ON THE EXPLORE TOKEN ──────────────────────────────────────────────────
 *
 * `checkBearer` + `EXPLORE_TOKEN_VARS`, not the `checkOptionalBearer` the tag/rename routes beside
 * them use, and `auth.ts` is what decides it: `checkOptionalBearer`'s own header says "Nothing that
 * reads live scan data or spends money on its own may use this. Fail-closed is the default for a
 * reason and this is the documented exception." A cross-workspace read IS live scan data, from a
 * lake the caller's own workspace does not own — the workbench's threat model, not the Run
 * surface's. The grant WRITES are held to the same bar because a grant is what creates that read:
 * gating the read and leaving the grant opt-in would be the inverse of the mistake the tag routes'
 * comment records, where "a route that can read every dataset was gated while the route that can
 * DELETE one was not".
 *
 * So on a box with no `KONTRA_EXPLORE_TOKEN` and no `KONTRA_STATE_TOKEN`, every route here answers
 * 503 and nothing is shared or read. A boundary that defaults to closed is the only kind worth
 * drawing. A console SESSION admits all four (ADR 0045), which is how the toggle works in a browser
 * that holds no service token.
 *
 * ── AND EVERY GRANT CHANGE IS AN AUDIT EVENT ────────────────────────────────────────────────────
 *
 * `dataset.share` / `dataset.unshare`, for the reason `dataset.delete` is one: the question "who
 * opened this up, and when" has no other answer, and unlike a delete the effect of a grant is
 * invisible until somebody uses it.
 */

import type { FastifyInstance } from 'fastify';

import { audit, callerOf } from '../audit';
import { EXPLORE_TOKEN_VARS, checkBearer } from '../auth';
import type { ObjectStore } from '../codec/objectStore';
import { NoSuchDatasetError } from '../data/datasets';
import type { LakeConfig } from '../data/parquet';
import {
  InvalidShareError,
  sharedKind,
  type SharedDatasetStore,
} from '../data/sharedDatasets';
import {
  NotSharedError,
  UnsupportedOrderByError,
  listSharedDatasets,
  previewSharedDataset,
} from '../data/sharedRead';
import { WorkspaceRefused } from '../workspaces';
import { errMessage } from './errors';

export interface SharedDatasetRouteDeps {
  store: ObjectStore;
  lake: Partial<LakeConfig>;
  /** The grants. SHARED with `routes/datasets.ts`, whose listing draws the badge from it. */
  shares: SharedDatasetStore;
}

export function registerSharedDatasetRoutes(
  app: FastifyInstance,
  deps: SharedDatasetRouteDeps
): void {
  const { store, lake, shares } = deps;

  /** One admission check for all four routes, so none of them can be added without it. */
  const admit = (authorization: string | undefined) =>
    checkBearer(authorization, EXPLORE_TOKEN_VARS);

  /**
   * WHAT IS EXPOSED — every grant on the install, or one workspace's with `?workspace=`.
   *
   * THE SCOPE IS STATED IN THE RESPONSE, not left for the reader to assume: `scope` is either the
   * workspace asked for or the literal `all-workspaces`. A cross-workspace listing whose breadth is
   * implicit is how an operator comes to believe they have audited the boundary when they have
   * audited one corner of it.
   *
   * It reads the grant table and attaches NO lake, so it costs one indexed SQL read however many
   * workspaces exist, and a workspace whose lake is unreachable still has its exposures listed.
   * What it therefore cannot say is whether each named Dataset still exists — see
   * `listSharedDatasets`, which spells that out, and the preview below, which is where a dangling
   * grant becomes loud.
   */
  app.get('/api/datasets/shared', async (req, reply) => {
    const denied = admit(req.headers.authorization);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { workspace } = req.query as Partial<{ workspace: string }>;
    try {
      const entries = await listSharedDatasets(shares, workspace ? { workspace } : {});
      return { scope: workspace || 'all-workspaces', entries };
    } catch (err) {
      if (err instanceof WorkspaceRefused) return reply.code(400).send({ error: err.message });
      return reply.code(502).send({ error: `could not list shared datasets: ${errMessage(err)}` });
    }
  });

  /**
   * GRANT: this workspace's Dataset may be READ from another.
   *
   * Idempotent, and the response carries the stored `sharedAt` rather than now — a repeat grant
   * reports the FIRST exposure's time, because that is the answer an operator auditing "since when"
   * needs.
   *
   * IT DOES NOT CHECK THE CATALOG, deliberately. A grant is a record about an ADDRESS, and refusing
   * to record one because the table is not there yet would make the obvious workflow impossible:
   * expose the Dataset a scheduled run is about to write. The read path checks the catalog and says
   * plainly when a grant points at nothing, which keeps the dangling case loud at the moment it
   * matters instead of forbidding a legitimate order of operations.
   */
  app.put('/api/datasets/shared/:workspace/:name', async (req, reply) => {
    const denied = admit(req.headers.authorization);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { workspace, name } = req.params as { workspace: string; name: string };
    const { kind } = { ...(req.query as object), ...(req.body as object) } as { kind?: unknown };
    try {
      const grant = await shares.share(workspace, name, sharedKind(kind));
      // THE EXPOSURE IS THE EVENT, not the read it later permits: a grant is invisible until
      // somebody uses it, so the trail has to record the moment it was opened.
      audit(
        {
          action: 'dataset.share',
          outcome: 'allowed',
          ...callerOf(req),
          target: `${grant.workspace}/${grant.kind}/${grant.name}`,
        },
        req.log
      );
      return { shared: true, ...grant };
    } catch (err) {
      if (err instanceof WorkspaceRefused || err instanceof InvalidShareError) {
        return reply.code(400).send({ error: errMessage(err) });
      }
      return reply.code(502).send({ error: `could not share dataset: ${errMessage(err)}` });
    }
  });

  /**
   * REVOKE. Revoking a grant that is not there is a 200 with `removed: false` — the caller asked for
   * the Dataset not to be shared and it is not, which is success; the flag is there so "it was
   * already closed" and "I just closed it" are not the same answer.
   */
  app.delete('/api/datasets/shared/:workspace/:name', async (req, reply) => {
    const denied = admit(req.headers.authorization);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { workspace, name } = req.params as { workspace: string; name: string };
    const { kind } = req.query as Partial<{ kind: string }>;
    try {
      const k = sharedKind(kind);
      const removed = await shares.unshare(workspace, name, k);
      audit(
        {
          action: 'dataset.unshare',
          outcome: 'allowed',
          ...callerOf(req),
          target: `${workspace}/${k}/${name}`,
        },
        req.log
      );
      return { shared: false, removed, workspace, name, kind: k };
    } catch (err) {
      if (err instanceof WorkspaceRefused || err instanceof InvalidShareError) {
        return reply.code(400).send({ error: errMessage(err) });
      }
      return reply.code(502).send({ error: `could not unshare dataset: ${errMessage(err)}` });
    }
  });

  /**
   * A BOUNDED READ of a shared Dataset, from outside the workspace that owns it.
   *
   * THIS IS THE OPERATOR'S AND THE CONSOLE'S PATH, NOT A WORKFLOW'S. A workflow in another
   * workspace reads through the `pageSharedDataset` ACTIVITY (`activities/datasets.ts`), for two
   * reasons neither of which is convenience: a workflow holds no bearer token and must not be given
   * one, and a page must become a CLAIM-CHECKED REF so the workflow's history carries ~110-byte
   * pointers instead of rows. Serving pages over HTTP here would quietly invite the second mistake.
   *
   * Bounded exactly as `/api/datasets/:name/preview` is — a clamped LIMIT, and `version`/`dt` prune
   * partitions rather than filter rows. No caller SQL is accepted; see `data/sharedRead.ts` for why
   * that is the boundary rather than a simplification.
   */
  app.get('/api/datasets/shared/:workspace/:name/preview', async (req, reply) => {
    const denied = admit(req.headers.authorization);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { workspace, name } = req.params as { workspace: string; name: string };
    const { kind, version, dt, limit } = req.query as Partial<{
      kind: string;
      version: string;
      dt: string;
      limit: string;
    }>;
    const n = limit ? Number.parseInt(limit, 10) : undefined;
    try {
      return await previewSharedDataset(
        store,
        shares,
        {
          workspace,
          name,
          kind: sharedKind(kind),
          version,
          dt,
          limit: Number.isFinite(n) ? n : undefined,
        },
        lake
      );
    } catch (err) {
      // 404 FOR BOTH REFUSALS, BY TYPE. `NotSharedError` is "no grant" and `NoSuchDatasetError` is
      // "the grant points at nothing" — different sentences, deliberately the same status, so this
      // surface cannot be used to learn whether another workspace holds a table of a given name.
      // Matched on the type and never on the message text, for the reason `NoSuchDatasetError`
      // records: the console prints the sentence, so the sentence must not be the classification.
      if (err instanceof NotSharedError || err instanceof NoSuchDatasetError) {
        return reply.code(404).send({ error: errMessage(err) });
      }
      if (err instanceof WorkspaceRefused || err instanceof UnsupportedOrderByError) {
        return reply.code(400).send({ error: errMessage(err) });
      }
      return reply.code(502).send({ error: `could not read shared dataset: ${errMessage(err)}` });
    }
  });
}
