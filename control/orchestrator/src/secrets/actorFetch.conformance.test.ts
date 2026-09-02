/**
 * CROSS-LANGUAGE CONFORMANCE: the Python SDK fetching its own secret from the REAL route.
 *
 * Both sides of this wire are tested on their own — `routes.test.ts` here, `tests/test_actor_secrets.py`
 * there — and neither can see the other's process, so each is asserting against its own idea of the
 * contract. That is exactly the shape of gap where a path, a header name or a status code drifts and
 * every suite stays green while a served actor cannot load. This closes it: a real Fastify server on
 * a real port, and the real `actorkit.secrets` module in a real interpreter, talking to each other.
 *
 * SKIPS ITSELF when the repo venv is absent, rather than failing: this is a TS suite, and a machine
 * without the Python side is a machine where this proves nothing rather than one where it is broken.
 */

import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { existsSync, mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

import type { FastifyInstance } from 'fastify';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { Repo } from '../db/repo';
import { buildServer } from '../server';
import { FileSecretBackend } from './fileBackend';
import { SecretStore } from './store';

const SENTINEL = 'dop_v1_SENTINEL_never_in_the_clear_9f3c';
const ROOT = path.join(__dirname, '..', '..', '..', '..');
const PY = path.join(ROOT, '.venv', 'bin', 'python');
const HAVE_PY = existsSync(PY);

let app: FastifyInstance;
let store: SecretStore;
let base = '';

beforeAll(async () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'kontra-secrets-'));
  store = new SecretStore(new FileSecretBackend({ dir }), { keyDir: dir });
  await store.put('shodan-key', SENTINEL, { owner: 'probe' });
  await store.put('do-token', `${SENTINEL}-operator`);
  app = buildServer({ repo: new Repo(':memory:'), webRoot: '', secrets: store });
  await app.listen({ port: 0, host: '127.0.0.1' });
  const addr = app.server.address();
  base = typeof addr === 'object' && addr ? `http://127.0.0.1:${addr.port}` : '';
});

afterAll(async () => {
  await app?.close();
});

/**
 * Run one `actorkit.secrets` call in a real interpreter, as a worker with the given identity.
 *
 * ASYNC, AND `spawnSync` IS A TRAP HERE — measured, not guessed. The Fastify server under test runs
 * on THIS event loop, so a synchronous spawn blocks the thread that would accept the connection:
 * every call failed as `TimeoutError` against a server that was listening and healthy. A test that
 * hangs its own dependency looks exactly like a broken route.
 */
async function fetchAsActor(actor: string, name: string): Promise<{ ok: boolean; out: string }> {
  const token = store.mintIdentity(actor).token;
  const code = [
    'import json, sys',
    'from actorkit import secrets',
    'try:',
    `    print(json.dumps({"ok": True, "out": secrets.get_sync(${JSON.stringify(name)})}))`,
    'except Exception as e:',
    '    print(json.dumps({"ok": False, "out": str(e)}))',
  ].join('\n');
  const res = await promisify(execFile)(PY, ['-c', code], {
    encoding: 'utf8',
    env: {
      ...process.env,
      KONTRA_ORCHESTRATOR_URL: base,
      KONTRA_ACTOR_TOKEN: token,
      PYTHONPATH: [path.join(ROOT, 'sdk', 'python'), path.join(ROOT, 'runtime', 'python'),
        path.join(ROOT, 'sdk', 'python', '_gen')].join(':'),
    },
  });
  return JSON.parse(res.stdout.trim().split('\n').pop() ?? '{}') as { ok: boolean; out: string };
}

describe.skipIf(!HAVE_PY)('an actor fetching its own secret, across the languages', () => {
  it('gets its value from the route the SDK actually calls', async () => {
    const got = await fetchAsActor('probe', 'shodan-key');
    expect(got.ok, got.out).toBe(true);
    expect(got.out).toBe(SENTINEL);
  });

  it('is refused another actor’s secret, with the sentence that names the fix', async () => {
    const got = await fetchAsActor('other', 'shodan-key');
    expect(got.ok).toBe(false);
    expect(got.out).toContain('does not belong to this actor');
    expect(got.out).not.toContain(SENTINEL);
  });

  it('is refused an OPERATOR secret, which is resolved at the last hop and never over HTTP', async () => {
    const got = await fetchAsActor('probe', 'do-token');
    expect(got.ok).toBe(false);
    expect(got.out).toContain('does not belong to this actor');
    expect(got.out).not.toContain(SENTINEL);
  });

  it('says "revoked" when the version it resolves to has been revoked', async () => {
    await store.put('rotate-me', SENTINEL, { owner: 'probe' });
    await store.revoke('rotate-me', 1);
    const got = await fetchAsActor('probe', 'rotate-me');
    expect(got.ok).toBe(false);
    expect(got.out).toContain('REVOKED');
  });

  it('sees a rotation without the worker being restarted', async () => {
    await store.put('rolling-key', 'first-value', { owner: 'probe' });
    expect((await fetchAsActor('probe', 'rolling-key')).out).toBe('first-value');
    await store.put('rolling-key', SENTINEL);
    // The SDK does not cache, so the next load gets the new version. This is the property that
    // makes rotation worth doing rather than a thing everybody puts off.
    expect((await fetchAsActor('probe', 'rolling-key')).out).toBe(SENTINEL);
  });
});
