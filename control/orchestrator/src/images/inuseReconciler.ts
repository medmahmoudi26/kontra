/**
 * The registry half of `inuse-` tagging, and the loop that arms it.
 *
 * The decision of WHICH tags should exist is `./inuse.ts`, which is pure and tested without a
 * registry. This file is the part that talks HTTP, and it is deliberately thin: four operations, none
 * of which parses a layer.
 *
 * WHY THE API ROLE AND NOT THE MATERIALIZER. The spec says this runs "in the materializer role", and
 * `materializer.ts` has no periodic loop to join — it creates one Temporal Worker and awaits the
 * pager, with no timer anywhere. The only in-process reconciler this codebase has is
 * `startHistoryArchiver`, armed in the API role, whose own header argues for that placement. This
 * copies that shape rather than inventing timer infrastructure in a role that has none.
 */

import { INUSE_PREFIX, reconcileInuseTags, type InuseDeps, type InusePass } from './inuse';

/** Ten minutes, per the spec. A tag that appears late keeps too much for a while, which is the safe way to be wrong. */
const DEFAULT_INTERVAL_MS = 10 * 60_000;
const DEFAULT_TIMEOUT_MS = 10_000;

/** The four media types a manifest can arrive as. Asking for one and getting another is a 404 shaped like a bug. */
const MANIFEST_ACCEPT = [
  'application/vnd.oci.image.index.v1+json',
  'application/vnd.docker.distribution.manifest.list.v2+json',
  'application/vnd.oci.image.manifest.v1+json',
  'application/vnd.docker.distribution.manifest.v2+json',
].join(', ');

/**
 * The registry's base URL on the compose network — the same split `images.ts` documents:
 * `KONTRA_REGISTRY` is a bare `host:port` for Docker to resolve, and an HTTP client needs a scheme.
 */
function registryBase(env: NodeJS.ProcessEnv = process.env): string {
  const raw = (env.KONTRA_REGISTRY_URL ?? '').trim();
  if (raw) return raw.replace(/\/+$/, '');
  return 'http://registry:5000';
}

export interface RegistryTagClient {
  listTags(repo: string): Promise<string[]>;
  tag(repo: string, digest: string, tag: string): Promise<void>;
  untag(repo: string, tag: string): Promise<void>;
}

export function registryTagClient(
  base = registryBase(),
  timeoutMs = DEFAULT_TIMEOUT_MS
): RegistryTagClient {
  const signal = (): AbortSignal => AbortSignal.timeout(timeoutMs);
  return {
    async listTags(repo) {
      const res = await fetch(`${base}/v2/${repo}/tags/list?n=200`, {
        signal: signal(),
        headers: { accept: 'application/json' },
      });
      // A repository with no tags and a repository that does not exist are the same fact to this
      // caller, and neither is an error worth waking anyone for.
      if (res.status === 404) return [];
      if (!res.ok) throw new Error(`GET tags ${repo}: ${res.status} ${res.statusText}`);
      const body = (await res.json()) as { tags?: string[] | null };
      return body.tags ?? [];
    },

    /**
     * Tagging is a re-PUT of the SAME manifest bytes under a new name.
     *
     * There is no "add tag" verb in the distribution spec. Because a manifest is content-addressed,
     * putting identical bytes yields the identical digest, so this adds a name and creates nothing —
     * which is why `create`/`update` is enough and `delete` is not needed to ADD protection.
     *
     * The Content-Type must be the one the manifest was stored with. Guessing it produces a second
     * manifest with a different digest, which would protect nothing and silently double the count.
     */
    async tag(repo, digest, tagName) {
      const got = await fetch(`${base}/v2/${repo}/manifests/${digest}`, {
        signal: signal(),
        headers: { accept: MANIFEST_ACCEPT },
      });
      if (!got.ok) throw new Error(`GET manifest ${repo}@${digest}: ${got.status} ${got.statusText}`);
      const contentType = got.headers.get('content-type');
      if (!contentType) throw new Error(`GET manifest ${repo}@${digest}: no content-type to re-put with`);
      const bytes = new Uint8Array(await got.arrayBuffer());

      const put = await fetch(`${base}/v2/${repo}/manifests/${tagName}`, {
        method: 'PUT',
        signal: signal(),
        headers: { 'content-type': contentType },
        body: bytes,
      });
      if (!put.ok) throw new Error(`PUT ${repo}:${tagName}: ${put.status} ${put.statusText}`);
      // The registry is the authority on what it stored, as everywhere else in this system.
      const landed = put.headers.get('docker-content-digest');
      if (landed && landed !== digest) {
        throw new Error(`PUT ${repo}:${tagName} landed ${landed}, wanted ${digest}`);
      }
    },

    /** DELETE by TAG removes the name; DELETE by digest would remove the manifest. Never the latter. */
    async untag(repo, tagName) {
      if (!tagName.startsWith(INUSE_PREFIX)) {
        // A guard and not an assertion: this function can only ever be asked to remove a tag this
        // module wrote, and a bug that passed it a version tag would delete a release's name.
        throw new Error(`refusing to untag ${repo}:${tagName} — not an ${INUSE_PREFIX} tag`);
      }
      const res = await fetch(`${base}/v2/${repo}/manifests/${tagName}`, {
        method: 'DELETE',
        signal: signal(),
      });
      if (!res.ok && res.status !== 404) {
        throw new Error(`DELETE ${repo}:${tagName}: ${res.status} ${res.statusText}`);
      }
    },
  };
}

export interface ReconcilerDeps {
  listActors: InuseDeps['listActors'];
  client?: RegistryTagClient;
  onNote?: (note: string) => void;
  onError?: (err: unknown, where?: string) => void;
  env?: NodeJS.ProcessEnv;
}

/**
 * Arm the loop. Returns a stop function.
 *
 * The shape is `startHistoryArchiver`'s, deliberately and in full: a named env off-switch that NOTES
 * rather than warns, a `KONTRA_*_MS` override, a single-flight guard, every failure swallowed into
 * `onError` so a transient registry blip cannot take the API down, an immediate first pass, and
 * `unref()` so a test or a CLI can still exit.
 */
export function startInuseReconciler(deps: ReconcilerDeps): () => void {
  const env = deps.env ?? process.env;
  if (env.KONTRA_INUSE_TAGS === 'off') {
    deps.onNote?.(
      'inuse tags: OFF (KONTRA_INUSE_TAGS=off). Registry retention has nothing protecting a version ' +
        'that is older than the five most recently pushed and still placed — leave ' +
        'KONTRA_REGISTRY_RETENTION=dryrun while this is off.'
    );
    return () => undefined;
  }
  const client = deps.client ?? registryTagClient(registryBase(env));
  const every = Number(env.KONTRA_INUSE_TAGS_MS ?? DEFAULT_INTERVAL_MS);
  const interval = Number.isFinite(every) && every > 0 ? every : DEFAULT_INTERVAL_MS;

  let running = false;
  const pass = async (): Promise<void> => {
    // One pass at a time. A first pass over a large catalog can outlast the interval, and two of them
    // would re-put the same manifests under the same names concurrently.
    if (running) return;
    running = true;
    try {
      const result: InusePass = await reconcileInuseTags({
        listActors: deps.listActors,
        listTags: (repo) => client.listTags(repo),
        tag: (repo, digest, t) => client.tag(repo, digest, t),
        untag: (repo, t) => client.untag(repo, t),
        onNote: deps.onNote,
        onError: deps.onError,
      });
      if (result.added || result.removed || result.refusedRemovals) {
        deps.onNote?.(
          `inuse tags: +${result.added} -${result.removed} (${result.kept} already correct` +
            (result.refusedRemovals
              ? `, ${result.refusedRemovals} stale tag(s) the registry would not remove — retention ` +
                'will keep more than it must, which is the safe direction'
              : '') +
            ')'
        );
      }
    } catch (err) {
      deps.onError?.(err, 'inuse reconciler');
    } finally {
      running = false;
    }
  };

  void pass();
  const timer = setInterval(() => void pass(), interval);
  timer.unref?.();
  return () => clearInterval(timer);
}
