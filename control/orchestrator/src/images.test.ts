/**
 * The built-image listing, against a REAL HTTP registry stub rather than a mocked `fetch`.
 *
 * A mocked fetch would test that this file calls the function it calls. What actually breaks on
 * this path is the wire: pagination via `Link`, a repository whose tag list 404s, a manifest with
 * no `Docker-Content-Digest`. Those only show up against a socket.
 */

import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';

import { afterEach, describe, expect, it } from 'vitest';

import { listBuiltImages } from './images';

interface Stub {
  url: string;
  close: () => Promise<void>;
  /** Paths the client actually asked for — so "one request, not one per tag" is assertable. */
  hits: string[];
}

function registry(routes: Record<string, (url: URL) => { status?: number; headers?: Record<string, string>; body?: unknown }>): Promise<Stub> {
  const hits: string[] = [];
  const server: Server = createServer((req, res) => {
    const url = new URL(req.url ?? '/', 'http://stub');
    hits.push(url.pathname + url.search);
    const handler = routes[url.pathname];
    if (!handler) {
      res.writeHead(404).end('{}');
      return;
    }
    const out = handler(url);
    res.writeHead(out.status ?? 200, { 'content-type': 'application/json', ...(out.headers ?? {}) });
    res.end(out.body === undefined ? '' : JSON.stringify(out.body));
  });
  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address() as AddressInfo;
      resolve({
        url: `http://127.0.0.1:${port}`,
        hits,
        close: () => new Promise((r) => server.close(() => r())),
      });
    });
  });
}

let stub: Stub | undefined;
afterEach(async () => {
  await stub?.close();
  stub = undefined;
});

describe('listing what has been built', () => {
  it('reads repositories and tags, newest version first', async () => {
    stub = await registry({
      '/v2/_catalog': () => ({ body: { repositories: ['canary', 'bundles/desync'] } }),
      '/v2/canary/tags/list': () => ({ body: { name: 'canary', tags: ['1.9.0', '1.10.0', '1.1.0'] } }),
      '/v2/bundles/desync/tags/list': () => ({ body: { name: 'bundles/desync', tags: ['1.3.2'] } }),
      '/v2/canary/manifests/1.10.0': () => ({
        headers: { 'docker-content-digest': 'sha256:aaa' },
        body: { config: { size: 100 }, layers: [{ size: 900 }, { size: 1000 }] },
      }),
      '/v2/canary/manifests/1.9.0': () => ({ headers: { 'docker-content-digest': 'sha256:bbb' }, body: {} }),
      '/v2/canary/manifests/1.1.0': () => ({ headers: { 'docker-content-digest': 'sha256:ccc' }, body: {} }),
      '/v2/bundles/desync/manifests/1.3.2': () => ({ body: { config: { size: 5 }, layers: [{ size: 5 }] } }),
    });

    const got = await listBuiltImages({ env: { KONTRA_REGISTRY_URL: stub.url } });

    expect(got.unreachable).toBeUndefined();
    const canary = got.images.filter((i) => i.repository === 'canary');
    // 1.10.0 BEFORE 1.9.0 — a plain string sort puts it after, which reads as the newest build
    // being missing from the top of the list.
    expect(canary.map((i) => i.tag)).toEqual(['1.10.0', '1.9.0', '1.1.0']);
    expect(canary[0].digest).toBe('sha256:aaa');
    expect(canary[0].bytes).toBe(2000);
  });

  it('tells a bundle apart from an actor image', async () => {
    stub = await registry({
      '/v2/_catalog': () => ({ body: { repositories: ['canary', 'bundles/desync'] } }),
      '/v2/canary/tags/list': () => ({ body: { tags: ['1.0.0'] } }),
      '/v2/bundles/desync/tags/list': () => ({ body: { tags: ['1.3.2'] } }),
    });

    const { images } = await listBuiltImages({ env: { KONTRA_REGISTRY_URL: stub.url }, withFacts: false });

    expect(images.find((i) => i.repository === 'canary')?.kind).toBe('actor');
    expect(images.find((i) => i.repository === 'bundles/desync')?.kind).toBe('bundle');
  });

  it('follows Link pagination rather than stopping at the first page', async () => {
    stub = await registry({
      '/v2/_catalog': (url) =>
        url.searchParams.get('last') === 'a'
          ? { body: { repositories: ['b'] } }
          : { headers: { link: '</v2/_catalog?n=200&last=a>; rel="next"' }, body: { repositories: ['a'] } },
      '/v2/a/tags/list': () => ({ body: { tags: ['1'] } }),
      '/v2/b/tags/list': () => ({ body: { tags: ['1'] } }),
    });

    const { images } = await listBuiltImages({ env: { KONTRA_REGISTRY_URL: stub.url }, withFacts: false });

    expect(images.map((i) => i.repository).sort()).toEqual(['a', 'b']);
  });

  /** A registry that is not there is "nothing built yet" on a first run — not an error page. */
  it('reports an unreachable registry as a reason beside an empty list', async () => {
    const got = await listBuiltImages({
      env: { KONTRA_REGISTRY_URL: 'http://127.0.0.1:1' },
      timeoutMs: 500,
    });

    expect(got.images).toEqual([]);
    expect(got.unreachable).toMatch(/could not read http:\/\/127\.0\.0\.1:1/);
  });

  /** One bad repository must not take the listing down with it. */
  it('skips a repository whose tags cannot be read and keeps the rest', async () => {
    stub = await registry({
      '/v2/_catalog': () => ({ body: { repositories: ['good', 'broken'] } }),
      '/v2/good/tags/list': () => ({ body: { tags: ['1.0.0'] } }),
      '/v2/broken/tags/list': () => ({ status: 500, body: { errors: [] } }),
    });

    const { images, unreachable } = await listBuiltImages({
      env: { KONTRA_REGISTRY_URL: stub.url },
      withFacts: false,
    });

    expect(unreachable).toBeUndefined();
    expect(images.map((i) => i.repository)).toEqual(['good']);
  });

  /** A manifest with no digest header still yields a listed tag — the fields are optional. */
  it('keeps a tag whose manifest says nothing useful', async () => {
    stub = await registry({
      '/v2/_catalog': () => ({ body: { repositories: ['x'] } }),
      '/v2/x/tags/list': () => ({ body: { tags: ['1.0.0'] } }),
      '/v2/x/manifests/1.0.0': () => ({ status: 404, body: {} }),
    });

    const { images } = await listBuiltImages({ env: { KONTRA_REGISTRY_URL: stub.url } });

    expect(images).toHaveLength(1);
    expect(images[0].digest).toBeUndefined();
    expect(images[0].bytes).toBeUndefined();
  });

  /** `withFacts: false` is the cheap path, and cheap has to mean fewer requests. */
  it('does not fetch a manifest per tag when facts are not asked for', async () => {
    stub = await registry({
      '/v2/_catalog': () => ({ body: { repositories: ['x'] } }),
      '/v2/x/tags/list': () => ({ body: { tags: ['1', '2', '3'] } }),
    });

    await listBuiltImages({ env: { KONTRA_REGISTRY_URL: stub.url }, withFacts: false });

    expect(stub.hits.filter((h) => h.includes('/manifests/'))).toEqual([]);
  });

  /** A repository with a null tag list (the registry's way of saying "none") is not a crash. */
  it('handles a repository with no tags', async () => {
    stub = await registry({
      '/v2/_catalog': () => ({ body: { repositories: ['x'] } }),
      '/v2/x/tags/list': () => ({ body: { name: 'x', tags: null } }),
    });

    const { images, unreachable } = await listBuiltImages({
      env: { KONTRA_REGISTRY_URL: stub.url },
      withFacts: false,
    });

    expect(unreachable).toBeUndefined();
    expect(images).toEqual([]);
  });
});
