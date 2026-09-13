/**
 * The upload route, and the filename rule that got itself wrong once already.
 *
 * TWO THINGS ARE WORTH THE SUITE. The first is that an upload lands in the SAME content-addressed
 * place `kontra.fetch_blob` reads from — if it does not, every actor that takes a file gets a sha it
 * cannot dereference, and the failure surfaces minutes later inside a worker. The second is
 * `safeName`, whose character class was briefly `/[^@-^_]/` — literal control characters written
 * into a regex do not survive being written — and which would silently have renamed every upload to
 * `upload`. That is the case this file pins hardest, because nothing else would have caught it: the
 * bytes still store, the run still starts, and only the actor ever sees the wrong name.
 */

import { describe, expect, it } from 'vitest';
import Fastify from 'fastify';
import { createHash } from 'node:crypto';

import { MAX_UPLOAD_BYTES, registerUploadRoutes, safeName } from './uploads';
import { ObjectStore, type BackingStore } from '../codec/objectStore';

/**
 * An in-memory backing, so the suite exercises the real `putContentAddressed` and real key rules.
 *
 * `exists` IS PART OF THE INTERFACE AND IS WHAT THE PUT-IF-ABSENT CAS WRITE CALLS. Omitting it
 * threw inside the route and answered 500 — which looked, from the test, exactly like a store
 * misconfiguration.
 */
function memoryStore(): { store: ObjectStore; objects: Map<string, Uint8Array> } {
  const objects = new Map<string, Uint8Array>();
  const backing: BackingStore = {
    get: async (key) => objects.get(key) ?? null,
    put: async (key, data) => {
      objects.set(key, data);
    },
    exists: async (key) => objects.has(key),
  };
  return { store: new ObjectStore({ backing }), objects };
}

function serve(store: ObjectStore) {
  const app = Fastify({ bodyLimit: MAX_UPLOAD_BYTES + 1024 });
  registerUploadRoutes(app, store);
  return app;
}

const AUTH = { authorization: 'Bearer test-state-token' };

/**
 * The route is fail-closed, so every case needs the env var the guard reads.
 *
 * `async` AND `await fn()`, WHICH IT WAS NOT. A synchronous `try/finally` around a function that
 * returns a promise restores the variable when the promise is CREATED, not when it settles — so
 * every case below ran with the token already unset and failed 503 `disabled: set one of
 * KONTRA_STATE_TOKEN`, which reads exactly like a route that forgot to configure itself.
 */
async function withToken(fn: () => Promise<void>): Promise<void> {
  const before = process.env.KONTRA_STATE_TOKEN;
  process.env.KONTRA_STATE_TOKEN = 'test-state-token';
  try {
    await fn();
  } finally {
    if (before === undefined) delete process.env.KONTRA_STATE_TOKEN;
    else process.env.KONTRA_STATE_TOKEN = before;
  }
}

describe('a filename is reduced, not trusted', () => {
  it('keeps an ordinary name exactly', () => {
    // THE CASE THE BROKEN CHARACTER CLASS FAILED. `/[^@-^_]/` stripped the dot and every letter
    // outside a narrow range, so this returned `upload` — and nothing else in the system would
    // have noticed.
    expect(safeName('hosts.txt')).toBe('hosts.txt');
    expect(safeName('Corpus 2026-09.csv')).toBe('Corpus 2026-09.csv');
    expect(safeName('résumé.pdf')).toBe('résumé.pdf');
  });

  it('takes the last segment, so a path cannot travel', () => {
    // The name is not used to address anything here — the sha does that — but it travels into a
    // workflow argument and into whatever the actor joins it onto.
    expect(safeName('../../etc/passwd')).toBe('passwd');
    expect(safeName('/tmp/x/y.json')).toBe('y.json');
    expect(safeName('C:\\Users\\me\\a.txt')).toBe('a.txt');
  });

  it('strips control characters and nothing else', () => {
    expect(safeName('a\u0000b.txt')).toBe('ab.txt');
    expect(safeName('a\u007fb.txt')).toBe('ab.txt');
    expect(safeName('tab\there.txt')).toBe('tabhere.txt');
    // Non-vacuous partner: punctuation is NOT control, and a class that took it would be the bug.
    expect(safeName('a!b#c$d%e&f(g).txt')).toBe('a!b#c$d%e&f(g).txt');
  });

  it('names something rather than nothing when the name reduces to nothing', () => {
    for (const raw of ['', '   ', '.', '..', '/', '\u0000']) {
      expect(safeName(raw), JSON.stringify(raw)).toBe('upload');
    }
  });

  it('is bounded', () => {
    expect(safeName('a'.repeat(400))).toHaveLength(255);
  });
});

describe('an upload lands where fetch_blob looks', () => {
  it('stores at the CAS key derived from the sha, and answers with it', async () => {
    await withToken(async () => {
      const { store, objects } = memoryStore();
      const app = serve(store);
      const bytes = Buffer.from('host,port\nexample.com,443\n');
      const res = await app.inject({
        method: 'POST',
        url: '/api/uploads?name=hosts.csv&type=text/csv',
        headers: { ...AUTH, 'content-type': 'application/octet-stream' },
        payload: bytes,
      });
      expect(res.statusCode).toBe(200);
      const body = res.json() as { name: string; sha256: string; size: number; contentType: string };

      const expected = createHash('sha256').update(bytes).digest('hex');
      expect(body.sha256).toBe(expected);
      expect(body.size).toBe(bytes.length);
      expect(body.name).toBe('hosts.csv');
      expect(body.contentType).toBe('text/csv');

      // THE ASSERTION THAT MATTERS: the key is the one the codec writes and the actor reads. A
      // different prefix here is a sha the worker cannot dereference, and the failure would surface
      // inside a run rather than at upload.
      expect(objects.has(store.casKey(expected))).toBe(true);
      expect(Buffer.from(objects.get(store.casKey(expected))!)).toEqual(bytes);
      await app.close();
    });
  });

  it('dedupes — the same bytes twice write once and answer the same sha', async () => {
    await withToken(async () => {
      const { store, objects } = memoryStore();
      const app = serve(store);
      const send = (name: string) =>
        app.inject({
          method: 'POST',
          url: `/api/uploads?name=${name}`,
          headers: { ...AUTH, 'content-type': 'application/octet-stream' },
          payload: Buffer.from('same bytes'),
        });
      const a = (await send('a.txt')).json() as { sha256: string };
      const b = (await send('b.txt')).json() as { sha256: string };
      expect(a.sha256).toBe(b.sha256);
      expect(objects.size).toBe(1);
      await app.close();
    });
  });

  it('refuses an empty file, which is a drag that picked nothing up', async () => {
    await withToken(async () => {
      const { store, objects } = memoryStore();
      const app = serve(store);
      const res = await app.inject({
        method: 'POST',
        url: '/api/uploads?name=nothing.txt',
        headers: { ...AUTH, 'content-type': 'application/octet-stream' },
        payload: Buffer.alloc(0),
      });
      expect(res.statusCode).toBe(400);
      expect(res.json()).toEqual({ error: 'that file is empty' });
      expect(objects.size).toBe(0);
      await app.close();
    });
  });
});

describe('it is fail-closed and it says what is missing', () => {
  it('refuses with no bearer', async () => {
    await withToken(async () => {
      const app = serve(memoryStore().store);
      const res = await app.inject({
        method: 'POST',
        url: '/api/uploads?name=a.txt',
        headers: { 'content-type': 'application/octet-stream' },
        payload: Buffer.from('x'),
      });
      expect(res.statusCode).toBe(401);
      await app.close();
    });
  });

  it('says the store is unconfigured rather than crashing', async () => {
    await withToken(async () => {
      // An installation with no object store is a real configuration, not a fault.
      const app = serve(new ObjectStore({ endpoint: '' }));
      const res = await app.inject({
        method: 'POST',
        url: '/api/uploads?name=a.txt',
        headers: { ...AUTH, 'content-type': 'application/octet-stream' },
        payload: Buffer.from('x'),
      });
      expect(res.statusCode).toBe(503);
      expect((res.json() as { error: string }).error).toContain('KONTRA_S3_ENDPOINT');
      await app.close();
    });
  });

  it('names the content type it wants when sent something else', async () => {
    await withToken(async () => {
      const app = serve(memoryStore().store);
      const res = await app.inject({
        method: 'POST',
        url: '/api/uploads?name=a.txt',
        headers: { ...AUTH, 'content-type': 'application/json' },
        payload: JSON.stringify({ not: 'bytes' }),
      });
      expect(res.statusCode).toBe(415);
      expect((res.json() as { error: string }).error).toContain('application/octet-stream');
      await app.close();
    });
  });
});
