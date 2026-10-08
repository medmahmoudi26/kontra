/**
 * What has actually been built — read from the REGISTRY, not from the Docker daemon.
 *
 * ── WHY THE REGISTRY AND NOT `docker images` ────────────────────────────────────────────────────
 *
 * Listing images from the daemon needs `/var/run/docker.sock`, and a read-write Docker socket is
 * root on the host — `docker-compose.yml`'s own header says so, and `activities/serveDev.ts` is a
 * whole file explaining why `kontra-api` (the container with the published port and the untrusted
 * HTTP input) must never hold one. A read-only listing is not worth acquiring that authority for.
 *
 * The registry answers the same question over plain HTTP on the compose network, with NO socket and
 * no new privilege anywhere. It is also the more honest answer: `docker images` on this host lists
 * what THIS host happens to have cached, while the registry lists what was actually pushed and is
 * therefore what a Machine or a second host could pull. A fleet places actors by pulling from the
 * registry, so "what can be placed" is exactly this list — the daemon's local cache is not.
 *
 * ── IT IS A BEST-EFFORT SURFACE ─────────────────────────────────────────────────────────────────
 *
 * An install can run without a registry. A listing that throws when one is absent turns "no images
 * yet" into an error page, so {@link listBuiltImages} reports unreachable as a REASON beside an
 * empty list and the caller renders it as a sentence. That distinction matters on a first run,
 * where "nothing built yet" and "the registry is down" look identical and mean opposite things.
 */
import { registryReadAuth } from './images/registryAuth';

/** One tag in the registry — a thing that could be pulled and placed. */
export interface BuiltImage {
  /** Registry repository, e.g. `canary` or `bundles/desync`. */
  repository: string;
  tag: string;
  /**
   * WHAT AN ACTOR IS VERSUS WHAT A RUN CARRIES. `bundles/<name>` holds the code a Machine fetches
   * (ADR 0023's Bundle); everything else is a worker image. They are listed together because they
   * are both build output, and distinguished because only one of them is what `kontra deploy`
   * produces and what the Actors page is about.
   */
  kind: 'actor' | 'bundle';
  /** The immutable content address, when the registry reported one. */
  digest?: string;
  /** Sum of the config and layer sizes from the manifest — what a pull would transfer. */
  bytes?: number;
}

export interface BuiltImages {
  images: BuiltImage[];
  /** The registry this was read from, echoed so a reader knows which one answered. */
  registry: string;
  /** Empty when the read succeeded. A SENTENCE when it did not — see the module header. */
  unreachable?: string;
}

/**
 * The registry's base URL on the compose network.
 *
 * NOT `KONTRA_REGISTRY`, which is deliberately a bare `host:port` for DOCKER to resolve (it is what
 * an image reference is tagged with, and a reference cannot carry a scheme). This is an HTTP client,
 * so it needs one. Keeping them separate is what stops a `10.124.0.2:5000` meant for `docker pull`
 * being handed to `fetch` as a relative URL.
 */
function registryBase(env: NodeJS.ProcessEnv = process.env): string {
  const raw = (env.KONTRA_REGISTRY_URL ?? '').trim();
  if (raw) return raw.replace(/\/+$/, '');
  return 'http://registry:5000';
}

/** The v2 API paginates with RFC 5988 `Link` headers; follow them so a long catalog is complete. */
function nextLink(res: { headers: { get(name: string): string | null } }): string | null {
  const link = res.headers.get('link');
  if (!link) return null;
  const m = /<([^>]+)>\s*;\s*rel="?next"?/i.exec(link);
  return m?.[1] ?? null;
}

async function getJson(url: string, timeoutMs: number): Promise<unknown> {
  const res = await fetch(url, {
    signal: AbortSignal.timeout(timeoutMs),
    headers: { accept: 'application/json', ...registryReadAuth() },
  });
  if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
  return res.json();
}

const MANIFEST_ACCEPT = [
  'application/vnd.docker.distribution.manifest.v2+json',
  'application/vnd.oci.image.manifest.v1+json',
].join(', ');

/**
 * Digest and transfer size for one tag, best effort.
 *
 * NEVER THROWS. A tag whose manifest cannot be read is still a tag that exists, and dropping it
 * from the listing over a missing header would make the surface quietly incomplete — the failure
 * mode this file's header exists to avoid. The fields are optional for exactly this reason.
 */
async function manifestFacts(
  base: string,
  repo: string,
  tag: string,
  timeoutMs: number
): Promise<{ digest?: string; bytes?: number }> {
  try {
    const res = await fetch(`${base}/v2/${repo}/manifests/${encodeURIComponent(tag)}`, {
      signal: AbortSignal.timeout(timeoutMs),
      headers: { accept: MANIFEST_ACCEPT, ...registryReadAuth() },
    });
    if (!res.ok) return {};
    const digest = res.headers.get('docker-content-digest') ?? undefined;
    const body = (await res.json()) as {
      config?: { size?: number };
      layers?: { size?: number }[];
    };
    const bytes =
      (body.config?.size ?? 0) + (body.layers ?? []).reduce((n, l) => n + (l.size ?? 0), 0);
    return { digest, bytes: bytes > 0 ? bytes : undefined };
  } catch {
    return {};
  }
}

/**
 * Everything the registry holds, newest-looking first within each repository.
 *
 * `withFacts: false` skips the per-tag manifest round trip — one request instead of one per tag,
 * for callers that only need names. The Actors page wants the digest (it is what `kontra deploy`
 * printed and what a Machine pulls by), so it asks for them.
 */
export async function listBuiltImages(
  opts: { withFacts?: boolean; timeoutMs?: number; env?: NodeJS.ProcessEnv } = {}
): Promise<BuiltImages> {
  const base = registryBase(opts.env);
  const timeoutMs = opts.timeoutMs ?? 5000;
  const withFacts = opts.withFacts ?? true;

  let repositories: string[] = [];
  try {
    let url: string | null = `${base}/v2/_catalog?n=200`;
    const seen = new Set<string>();
    while (url && !seen.has(url)) {
      seen.add(url);
      const res = await fetch(url, {
        signal: AbortSignal.timeout(timeoutMs),
        // WITH AUTH ON THERE IS NO ANONYMOUS READ — see images/registryAuth.ts. Without this the
        // catalog is 401, the `catch` below turns it into `unreachable`, and the Images page says
        // the registry could not be read on an install whose registry is working perfectly.
        headers: { accept: 'application/json', ...registryReadAuth(opts.env) },
      });
      if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
      const body = (await res.json()) as { repositories?: string[] };
      repositories.push(...(body.repositories ?? []));
      const next = nextLink(res);
      url = next ? new URL(next, base).toString() : null;
    }
  } catch (err) {
    return {
      images: [],
      registry: base,
      unreachable: `could not read ${base}: ${err instanceof Error ? err.message : String(err)}`,
    };
  }
  repositories = [...new Set(repositories)].sort((a, b) => a.localeCompare(b));

  const images: BuiltImage[] = [];
  for (const repo of repositories) {
    let tags: string[] = [];
    try {
      const body = (await getJson(
        `${base}/v2/${repo}/tags/list`,
        timeoutMs
      )) as { tags?: string[] | null };
      tags = body.tags ?? [];
    } catch {
      // A repository with no readable tag list is a repository with nothing to place. Skipping it
      // is right; failing the whole listing over it is not.
      continue;
    }
    const kind: BuiltImage['kind'] = repo.startsWith('bundles/') ? 'bundle' : 'actor';
    // Descending, so the newest version of an actor is the one a reader sees first. `localeCompare`
    // with `numeric` orders 1.10.0 after 1.9.0, which a plain string sort gets backwards.
    for (const tag of [...tags].sort((a, b) => b.localeCompare(a, undefined, { numeric: true }))) {
      const facts = withFacts ? await manifestFacts(base, repo, tag, timeoutMs) : {};
      images.push({ repository: repo, tag, kind, ...facts });
    }
  }
  return { images, registry: base };
}
