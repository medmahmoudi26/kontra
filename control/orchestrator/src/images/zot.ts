/**
 * Reading the image store, for the Images page.
 *
 * THREE SOURCES AND NO FOURTH. zot's GraphQL search answers what exists; a config BLOB answers what an
 * image is made of; the catalog and placements answer what is in use. No layer tarball is opened, and
 * nothing is inferred from a tag name.
 *
 * WHY THE CONFIG BLOB RATHER THAN GraphQL's `Labels`. Measured: `Labels` comes back as an empty string
 * for an image carrying ten labels, including the `io.buildpacks.lifecycle.metadata` the whole layer
 * breakdown depends on. The blob is where labels actually live, and GraphQL hands over the
 * `ConfigDigest` that addresses it — so the two are used together rather than one being preferred.
 *
 * `ImageListForDigest` IS THE SHARING PRIMITIVE, and it took a measurement to establish: it matches
 * LAYER digests, not just manifest digests. Pushed two images sharing a base layer, asked for the
 * shared digest, and got both back; asked for one image's own layer and got only that image. That is
 * what `referenced_by`, `size_unique` and the hover-highlight are built on.
 */
import { registryReadAuth } from './registryAuth';

const DEFAULT_TIMEOUT_MS = 15_000;

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
export function zotBase(env: NodeJS.ProcessEnv = process.env): string {
  const raw = (env.KONTRA_REGISTRY_URL ?? '').trim();
  if (raw) return raw.replace(/\/+$/, '');
  return 'http://registry:5000';
}

export interface ZotImage {
  repo: string;
  tag: string;
  digest: string;
  configDigest: string;
  size: number;
  layers: Array<{ digest: string; size: number }>;
  signed: boolean;
  pushedAt?: string;
  lastPulledAt?: string;
}

export class Zot {
  constructor(
    private readonly base = zotBase(),
    private readonly timeoutMs = DEFAULT_TIMEOUT_MS
  ) {}

  private signal(): AbortSignal {
    return AbortSignal.timeout(this.timeoutMs);
  }

  /** One GraphQL query. Errors in the body are errors here — a `data: null` is not an empty registry. */
  private async gql<T>(query: string): Promise<T> {
    const res = await fetch(`${this.base}/v2/_zot/ext/search`, {
      method: 'POST',
      signal: this.signal(),
      // A POST, AND STILL A READ. zot's search extension is behind the same accessControl as every
      // repository, so with auth on an unauthenticated query is 401 — and this one throws, so the
      // whole Images surface fails rather than degrading.
      headers: { 'content-type': 'application/json', ...registryReadAuth() },
      body: JSON.stringify({ query }),
    });
    if (!res.ok) throw new Error(`zot search: ${res.status} ${res.statusText}`);
    const body = (await res.json()) as { data?: T; errors?: Array<{ message: string }> };
    if (body.errors?.length) throw new Error(`zot search: ${body.errors.map((e) => e.message).join('; ')}`);
    if (!body.data) throw new Error('zot search returned no data');
    return body.data;
  }

  /** Every repository the registry holds. */
  async repositories(): Promise<string[]> {
    const d = await this.gql<{ RepoListWithNewestImage: { Results: Array<{ Name: string }> } }>(
      '{ RepoListWithNewestImage { Results { Name } } }'
    );
    return d.RepoListWithNewestImage.Results.map((r) => r.Name).sort();
  }

  /** Every tagged image in one repository, with the layer list the breakdown needs. */
  async images(repo: string): Promise<ZotImage[]> {
    const d = await this.gql<{
      ImageList: {
        Results: Array<{
          RepoName: string;
          Tag: string;
          Digest: string;
          Size: string;
          IsSigned: boolean;
          PushTimestamp?: string;
          LastPullTimestamp?: string;
          Manifests: Array<{ ConfigDigest: string; Layers: Array<{ Digest: string; Size: string }> }>;
        }>;
      };
    }>(`{ ImageList(repo: ${JSON.stringify(repo)}) { Results {
          RepoName Tag Digest Size IsSigned PushTimestamp LastPullTimestamp
          Manifests { ConfigDigest Layers { Digest Size } } } } }`);
    return d.ImageList.Results.map((r) => {
      const m = r.Manifests[0];
      return {
        repo: r.RepoName,
        tag: r.Tag,
        digest: r.Digest,
        configDigest: m?.ConfigDigest ?? '',
        size: Number(r.Size) || 0,
        layers: (m?.Layers ?? []).map((l) => ({ digest: l.Digest, size: Number(l.Size) || 0 })),
        signed: Boolean(r.IsSigned),
        ...(r.PushTimestamp ? { pushedAt: r.PushTimestamp } : {}),
        ...(r.LastPullTimestamp ? { lastPulledAt: r.LastPullTimestamp } : {}),
      };
    });
  }

  /**
   * Which images reference a digest — a LAYER digest as well as a manifest digest, which is the
   * property the Layers tab is built on and which was established by measurement rather than docs.
   */
  async referencedBy(digest: string): Promise<Array<{ repo: string; tag: string }>> {
    const d = await this.gql<{ ImageListForDigest: { Results: Array<{ RepoName: string; Tag: string }> } }>(
      `{ ImageListForDigest(id: ${JSON.stringify(digest)}) { Results { RepoName Tag } } }`
    );
    return d.ImageListForDigest.Results.map((r) => ({ repo: r.RepoName, tag: r.Tag }));
  }

  /** An image's config blob, where the labels actually are. */
  async configBlob(repo: string, configDigest: string): Promise<unknown> {
    const res = await fetch(`${this.base}/v2/${repo}/blobs/${configDigest}`, {
      signal: this.signal(),
      headers: { accept: 'application/json', ...registryReadAuth() },
    });
    if (!res.ok) throw new Error(`config blob ${repo}@${configDigest}: ${res.status} ${res.statusText}`);
    return res.json();
  }

  /** The rootfs diff_ids, in order — the other half of the layer join. */
  async diffIds(repo: string, configDigest: string): Promise<string[]> {
    const cfg = (await this.configBlob(repo, configDigest)) as { rootfs?: { diff_ids?: string[] } };
    return cfg?.rootfs?.diff_ids ?? [];
  }

  /**
   * The OCI annotations a runtime image carries. Read from the manifest rather than GraphQL, because
   * the annotations are a manifest property and GraphQL does not surface them.
   */
  async annotations(repo: string, reference: string): Promise<Record<string, string>> {
    const res = await fetch(`${this.base}/v2/${repo}/manifests/${reference}`, {
      signal: this.signal(),
      headers: { accept: MANIFEST_ACCEPT, ...registryReadAuth() },
    });
    if (!res.ok) return {};
    const m = (await res.json()) as { annotations?: Record<string, string> };
    return m.annotations ?? {};
  }

  /** The digest a tag currently resolves to, or `''` when the registry does not have it. */
  async digestOf(repo: string, reference: string): Promise<string> {
    const res = await fetch(`${this.base}/v2/${repo}/manifests/${reference}`, {
      method: 'HEAD',
      signal: this.signal(),
      headers: { accept: MANIFEST_ACCEPT, ...registryReadAuth() },
    });
    if (!res.ok) return '';
    return res.headers.get('docker-content-digest') ?? '';
  }
}
