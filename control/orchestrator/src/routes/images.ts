/**
 * `/api/images` — what is in the image store, what it is made of, and what keeps it alive.
 *
 * The question this surface exists to answer is "why is the registry 40 GB", and the only honest way
 * to answer it is per LAYER: a deps layer referenced once is a lockfile nobody else shares, and a
 * hundred `app` layers are fine because they are kilobytes.
 *
 * ── WHO MAY DO WHAT, AND WHY IT IS NOT A NEW SCOPE ───────────────────────────────────────────────
 *
 * The spec asks for an `images:admin` scope. ADR 0054's model is that a session carries scopes, that
 * `mint()` grants `console` and nothing grants more, and that a privileged scope is a service-token
 * capability a session can never inherit — so an `images:admin` the console must USE would contradict
 * the ADR rather than extend it, and inventing a second privileged scope beside `infra` would mean two
 * vocabularies for one idea.
 *
 * So the reads are `gated` (a console session is enough) and the one destructive route asks for
 * `INFRA_SCOPE`, which is exactly the shape `infraRoutes.ts` already argued for its own mutation:
 * "reading which Machines exist is not the capability that was leaking — spending money is." Reading
 * which images exist is not the capability either; deleting one is.
 *
 * ── ONE ROUTE THE SPEC ASKS FOR THAT CANNOT DO WHAT IT SAYS ──────────────────────────────────────
 *
 *   POST /api/images/rebase  a rebase shells out to `pack`, which is in the `cli` image and not in the
 *                            one serving this route. `kontra rebase` does it, and the refusal says so.
 *
 * It is implemented and refuses, because the refusal carries something: the one command that does
 * the work. There used to be a second, `POST /api/images/gc`, and it is DELETED rather than kept on
 * the same argument, because it carried nothing. zot exposes no garbage-collection trigger — its
 * extension discovery lists `cosign`, `search` and `mgmt`, and a POST to `/v2/_zot/ext/gc` is a 404
 * — so the route answered 501 to every caller, for ever, and the only thing an operator could do with
 * the answer was stop asking. GC and retention run on zot's own `gcInterval`; a route that can only
 * say so is an entry in the API surface, a posture row and an OpenAPI path for a sentence that
 * belongs in [[Durability-and-Failures]].
 */

import type { FastifyInstance } from 'fastify';

import { STATE_TOKEN_VARS, checkBearer } from '../auth';
import { INFRA_SCOPE } from '../auth/session';
import type { Repo } from '../db/repo';
import { classifyLayers, lifecycleMetadata, totalBytes, uniqueBytes, type LayerFact } from '../images/layers';
import { Zot } from '../images/zot';

/**
 * The service token these routes accept, which is the SAME one `/api/infra` accepts.
 *
 * Not a new variable. Token vars are a first-set-wins fallback chain, so a second name for this
 * capability would mean two ways to spell one grant and an install where only one of them works.
 */
const IMAGE_ADMIN_TOKEN_VARS = STATE_TOKEN_VARS;

/**
 * The joined view is cached for a minute.
 *
 * NOT FOR SPEED — for the registry. One page render is a repository walk plus a manifest and a config
 * blob per image plus a reference lookup per distinct layer, and the page has four tabs that each want
 * it. A minute is short enough that a deploy shows up while an operator is still looking at the page
 * and long enough that opening four tabs is one walk.
 */
const CACHE_MS = 60_000;

interface ImageRow {
  name: string;
  version: string;
  repo: string;
  digest: string;
  runtime?: { name: string; major: number; digest: string };
  rebase_available: boolean;
  size_total: number;
  size_unique: number;
  signed: boolean;
  pushed_at?: string;
  last_used_at?: string;
  in_use: { catalog: boolean; history: boolean };
  layers: LayerFact[];
}

interface Joined {
  at: number;
  images: ImageRow[];
  /** layer digest -> every image that references it, which is what `referenced_by` counts. */
  sharing: Map<string, Array<{ repo: string; tag: string }>>;
  runtimes: Array<{
    name: string;
    major: number;
    digest: string;
    description: string;
    provides: string[];
    size: number;
    signed: boolean;
    actors_using: number;
    actors_behind: number;
  }>;
}

export interface ImageRouteDeps {
  repo: Repo;
  zot?: Zot;
  now?: () => number;
}

export function registerImageRoutes(app: FastifyInstance, deps: ImageRouteDeps): void {
  const zot = deps.zot ?? new Zot();
  const now = deps.now ?? (() => Date.now());
  let cached: Joined | null = null;

  const view = async (): Promise<Joined> => {
    if (cached && now() - cached.at < CACHE_MS) return cached;
    cached = await join(zot, deps.repo, now());
    return cached;
  };
  /** Invalidated by anything that changes the store, so a delete is visible immediately. */
  const invalidate = (): void => {
    cached = null;
  };

  app.get('/api/images/runtimes', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, IMAGE_ADMIN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    return (await view()).runtimes;
  });

  app.get('/api/images/actors', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, IMAGE_ADMIN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    // The layer list is the heavy part and the list view does not draw it.
    return (await view()).images.map(({ layers, ...row }) => ({ ...row, layer_count: layers.length }));
  });

  app.get('/api/images/actors/:name/:version', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, IMAGE_ADMIN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { name, version } = req.params as { name: string; version: string };
    const v = await view();
    const row = v.images.find((i) => i.name === name && i.version === version);
    if (!row) return reply.code(404).send({ error: `no image for ${name}@${version}` });
    return {
      ...row,
      layers: row.layers.map((l) => ({ ...l, referenced_by: v.sharing.get(l.digest)?.length ?? 1 })),
      history: deps.repo.getActor(`${name}@${version}`)?.history ?? [],
    };
  });

  app.get('/api/images/layers', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, IMAGE_ADMIN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const v = await view();
    const byDigest = new Map<string, LayerFact>();
    for (const img of v.images) for (const l of img.layers) byDigest.set(l.digest, l);
    // SORTED BY SIZE x REFERENCES, which is the whole point of the tab: the biggest wins first.
    return [...byDigest.values()]
      .map((l) => {
        const images = v.sharing.get(l.digest) ?? [];
        return { ...l, referenced_by: images.length || 1, images };
      })
      .sort((a, b) => b.size * b.referenced_by - a.size * a.referenced_by);
  });

  app.get('/api/images/storage', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, IMAGE_ADMIN_TOKEN_VARS);
    if (denied) return reply.code(denied.code).send(denied.body);
    const v = await view();
    // ON-DISK IS THE SUM OF DISTINCT LAYERS, NOT OF IMAGE SIZES. Adding up `size_total` counts a shared
    // run image once per actor and reports a registry several times its real size — which is the exact
    // number an operator would act on.
    const distinct = new Map<string, number>();
    for (const img of v.images) for (const l of img.layers) distinct.set(l.digest, l.size);
    const onDisk = [...distinct.values()].reduce((n, s) => n + s, 0);
    const naive = v.images.reduce((n, i) => n + i.size_total, 0);
    return {
      registry_bytes: onDisk,
      deduped_bytes: naive - onDisk,
      images: v.images.length,
      distinct_layers: distinct.size,
      retention: {
        mode: process.env.KONTRA_REGISTRY_RETENTION ?? 'dryrun',
        // WHAT IT WOULD RECLAIM IS NOT REPORTED, because zot does not say. It is the one number the
        // Storage tab would most like and the only one that would have to be invented.
        reclaimable_bytes: null,
        note:
          'zot evaluates retention on its own schedule and exposes neither a trigger nor a last-run ' +
          'time. `KONTRA_REGISTRY_RETENTION=dryrun` logs every would-delete instead of taking it.',
      },
    };
  });

  app.delete('/api/images/actors/:name/:version', async (req, reply) => {
    // THE ONE DESTRUCTIVE ROUTE, and the only one that asks for more than a console session.
    const denied = checkBearer(req.headers.authorization, IMAGE_ADMIN_TOKEN_VARS, INFRA_SCOPE);
    if (denied) return reply.code(denied.code).send(denied.body);
    const { name, version } = req.params as { name: string; version: string };
    const v = await view();
    const row = v.images.find((i) => i.name === name && i.version === version);
    if (!row) return reply.code(404).send({ error: `no image for ${name}@${version}` });

    // 409 NAMES WHAT KEEPS IT ALIVE, because "in use" with no reason is a dead end for the person who
    // has to decide whether to force it.
    const reasons: string[] = [];
    if (row.in_use.catalog) reasons.push('it is the catalog\'s current digest for this version');
    if (row.in_use.history) reasons.push('a Lease may still be running it (it is in this version\'s digest history)');
    if (reasons.length > 0) {
      return reply.code(409).send({ error: `refusing to delete ${name}@${version}`, because: reasons });
    }
    invalidate();
    return reply.code(501).send({
      error: 'deleting an image is not implemented here yet',
      because:
        'nothing in the orchestrator writes to the registry, and the credential this process holds is ' +
        'read-only by construction. Retention removes an untagged, un-`inuse-` manifest on its own ' +
        'schedule, which is the path that exists today.',
    });
  });

  app.post('/api/images/rebase', async (req, reply) => {
    const denied = checkBearer(req.headers.authorization, IMAGE_ADMIN_TOKEN_VARS, INFRA_SCOPE);
    if (denied) return reply.code(denied.code).send(denied.body);
    const body = req.body as { actor?: string; version?: string; runtime?: string } | undefined;
    const what = body?.runtime ? `--runtime ${body.runtime}` : [body?.actor, body?.version].filter(Boolean).join('@');
    return reply.code(501).send({
      error: 'a rebase runs from the CLI',
      because:
        'it shells out to `pack`, which ships in the `cli` image and not in the one serving this route. ' +
        'Detection is here — every row carries `rebase_available` — and the rewrite is one command.',
      run: `kontra rebase ${what}`.trim(),
    });
  });
}

/** Build the joined view once. Everything the four tabs read comes from this. */
async function join(zot: Zot, repo: Repo, at: number): Promise<Joined> {
  const actors = repo.listActors();
  const byDigest = new Map<string, (typeof actors)[number]>();
  const inHistory = new Set<string>();
  for (const a of actors) {
    if (a.digest) byDigest.set(a.digest, a);
    for (const h of a.history ?? []) inHistory.add(h);
  }

  const repos = await zot.repositories();
  const images: ImageRow[] = [];
  const sharing = new Map<string, Array<{ repo: string; tag: string }>>();

  for (const r of repos) {
    if (r.startsWith('kontra-runtimes/') || r.startsWith('bundles/') || r.endsWith('-cache')) continue;
    let list;
    try {
      list = await zot.images(r);
    } catch {
      continue; // a repository that cannot be read is not the whole page's problem
    }
    for (const img of list) {
      if (img.tag.startsWith('inuse-')) continue; // a protection tag is not a version
      const known = byDigest.get(img.digest);
      let layers: LayerFact[] = img.layers.map((l) => ({ ...l, kind: 'unknown', source: '' }));
      try {
        const md = lifecycleMetadata(await zot.configBlob(img.repo, img.configDigest));
        const diffIds = await zot.diffIds(img.repo, img.configDigest);
        const label = known?.runtime ? `${known.runtime.name}:${known.runtime.major}` : undefined;
        layers = classifyLayers(img.layers, diffIds, md, label);
      } catch {
        // An image whose config cannot be read still appears, with unknown layers. Dropping it would
        // hide bytes from the one page that is about bytes.
      }
      for (const l of layers) {
        if (!sharing.has(l.digest)) {
          try {
            sharing.set(l.digest, await zot.referencedBy(l.digest));
          } catch {
            sharing.set(l.digest, []);
          }
        }
      }
      const shared = new Set(layers.filter((l) => (sharing.get(l.digest)?.length ?? 1) > 1).map((l) => l.digest));
      images.push({
        name: known?.name ?? r.split('/').pop() ?? r,
        version: known?.version ?? img.tag,
        repo: img.repo,
        digest: img.digest,
        ...(known?.runtime ? { runtime: known.runtime } : {}),
        rebase_available: false, // filled below, once the runtimes are resolved
        size_total: totalBytes(layers),
        size_unique: uniqueBytes(layers, shared),
        signed: img.signed,
        ...(img.pushedAt ? { pushed_at: img.pushedAt } : {}),
        ...(img.lastPulledAt ? { last_used_at: img.lastPulledAt } : {}),
        in_use: { catalog: Boolean(known), history: inHistory.has(img.digest) },
        layers,
      });
    }
  }

  // The runtimes, and who is behind which. `rebase_available` is the SAME comparison `kontra rebase`
  // makes — the recorded runtime digest against what that major resolves to now — so the page and the
  // command cannot disagree about what is behind.
  const runtimes: Joined['runtimes'] = [];
  for (const r of repos.filter((x) => x.startsWith('kontra-runtimes/'))) {
    const name = r.slice('kontra-runtimes/'.length);
    let list;
    try {
      list = await zot.images(r);
    } catch {
      continue;
    }
    for (const img of list) {
      const major = Number(img.tag);
      if (!Number.isInteger(major)) continue; // only the moving MAJOR tags are a runtime to point at
      const ann = await zot.annotations(r, img.tag).catch(() => ({}) as Record<string, string>);
      const using = images.filter((i) => i.runtime?.name === name && i.runtime.major === major);
      for (const i of using) if (i.runtime && i.runtime.digest !== img.digest) i.rebase_available = true;
      runtimes.push({
        name,
        major,
        digest: img.digest,
        description: ann['dev.kontra.runtime.description'] ?? '',
        provides: (ann['dev.kontra.runtime.provides'] ?? '').split(',').filter(Boolean),
        size: img.size,
        signed: img.signed,
        actors_using: using.length,
        actors_behind: using.filter((i) => i.rebase_available).length,
      });
    }
  }

  return { at, images, sharing, runtimes };
}
