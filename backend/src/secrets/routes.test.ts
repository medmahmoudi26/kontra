/**
 * The HTTP surface, end to end: create, name, version, rotate, revoke — and the sentinel that must
 * appear in no response this API can produce.
 *
 * THE SWEEP AT THE BOTTOM IS THE REAL TEST. Asserting the value is absent from the routes I
 * happened to think of is worth little; what is worth something is walking every management route
 * on a store that HAS the secret and asserting the sentinel is in no body and no header of any of
 * them. A route added later that returns a value has to delete a test to land.
 */

import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import type { FastifyInstance } from 'fastify';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { Repo } from '../db/repo';
import { buildServer } from '../server';
import { FileSecretBackend } from './fileBackend';
import { mintActorToken } from './identity';
import { SecretStore } from './store';

const SENTINEL = 'dop_v1_SENTINEL_never_in_the_clear_9f3c';

let app: FastifyInstance;
let dir: string;
let saved: Record<string, string | undefined>;

beforeEach(() => {
  saved = {
    KONTRA_SECRETS_TOKEN: process.env.KONTRA_SECRETS_TOKEN,
    KONTRA_STATE_TOKEN: process.env.KONTRA_STATE_TOKEN,
    KONTRA_SECRETS_KEY: process.env.KONTRA_SECRETS_KEY,
  };
  delete process.env.KONTRA_SECRETS_TOKEN;
  delete process.env.KONTRA_STATE_TOKEN;
  delete process.env.KONTRA_SECRETS_KEY;
  dir = mkdtempSync(join(tmpdir(), 'kontra-secrets-'));
  app = buildServer({
    repo: new Repo(':memory:'),
    webRoot: '',
    secrets: new SecretStore(new FileSecretBackend({ dir }), { keyDir: dir }),
  });
});

afterEach(async () => {
  await app.close();
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
});

const write = (name: string, value: string, owner?: string) =>
  app.inject({ method: 'PUT', url: `/api/secrets/${name}`, payload: owner ? { value, owner } : { value } });

describe('creating, naming and versioning', () => {
  it('creates a secret and answers with its metadata, never its value', async () => {
    const res = await write('do-token', SENTINEL);
    expect(res.statusCode).toBe(201);
    expect(res.json()).toMatchObject({ version: 1, rotated: false, secret: { name: 'do-token', current: 1 } });
    expect(res.body).not.toContain(SENTINEL);
  });

  it('lists what exists, with the backend it rests in', async () => {
    await write('do-token', SENTINEL);
    await write('shodan-key', 'another', 'probe');
    const res = await app.inject({ method: 'GET', url: '/api/secrets' });
    const body = res.json() as { backend: string; location: string; secrets: Array<{ name: string; owner?: string }> };
    expect(body.backend).toBe('file');
    expect(body.location).toContain('secrets.json');
    expect(body.secrets.map((s) => s.name)).toEqual(['do-token', 'shodan-key']);
    expect(body.secrets[1]?.owner).toBe('actor:probe');
  });

  it('rotates on the second write and says so', async () => {
    await write('do-token', 'first-value');
    const res = await write('do-token', SENTINEL);
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({ version: 2, rotated: true });
  });

  it('says when it trimmed what was sent, because nobody will ever see it again', async () => {
    const res = await write('do-token', `${SENTINEL}\n`);
    expect(res.json()).toMatchObject({ trimmed: true });
  });

  it('refuses a name that is not one, and a body with no value', async () => {
    expect((await write('DO_TOKEN', SENTINEL)).statusCode).toBe(400);
    expect(
      (await app.inject({ method: 'PUT', url: '/api/secrets/do-token', payload: {} })).statusCode
    ).toBe(400);
  });

  it('404s a secret that does not exist', async () => {
    expect((await app.inject({ method: 'GET', url: '/api/secrets/nothing-here' })).statusCode).toBe(404);
    expect((await app.inject({ method: 'DELETE', url: '/api/secrets/nothing-here' })).statusCode).toBe(404);
  });
});

describe('revocation and destruction', () => {
  it('revokes a version and reports the secret without it', async () => {
    await write('do-token', 'first-value');
    await write('do-token', SENTINEL);
    const res = await app.inject({ method: 'POST', url: '/api/secrets/do-token/versions/2/revoke' });
    expect(res.statusCode).toBe(200);
    const secret = (res.json() as { secret: { current: number; versions: Array<{ revokedAt?: number }> } }).secret;
    expect(secret.current).toBe(1);
    expect(secret.versions[1]?.revokedAt).toBeGreaterThan(0);
  });

  it('destroys the whole secret', async () => {
    await write('do-token', SENTINEL);
    expect((await app.inject({ method: 'DELETE', url: '/api/secrets/do-token' })).statusCode).toBe(200);
    expect((await app.inject({ method: 'GET', url: '/api/secrets/do-token' })).statusCode).toBe(404);
  });
});

describe('the actor path: fetching its own, authenticated as itself', () => {
  const resolve = (name: string, token: string) =>
    app.inject({
      method: 'POST',
      url: '/api/secrets/resolve',
      headers: { authorization: `Bearer ${token}` },
      payload: { name },
    });

  it('answers the actor that owns the secret', async () => {
    await write('shodan-key', SENTINEL, 'probe');
    const res = await resolve('shodan-key', mintActorToken('probe', { dir }).token);
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({ name: 'shodan-key', version: 1, value: SENTINEL });
  });

  it('refuses an actor asking for another actor’s secret — 403, and it names nothing', async () => {
    await write('shodan-key', SENTINEL, 'probe');
    const res = await resolve('shodan-key', mintActorToken('other', { dir }).token);
    expect(res.statusCode).toBe(403);
    expect(res.body).not.toContain(SENTINEL);
    expect(res.body).not.toContain('probe');
  });

  it('refuses an OPERATOR secret to any actor', async () => {
    // The workflow path's credential is resolved in-process at the last hop. There is no HTTP door
    // to it — which is what makes "only the name crosses" true of that path too.
    await write('do-token', SENTINEL);
    const res = await resolve('do-token', mintActorToken('probe', { dir }).token);
    expect(res.statusCode).toBe(403);
    expect(res.body).not.toContain(SENTINEL);
  });

  it('401s a missing, forged or expired identity — separately from an ownership refusal', async () => {
    await write('shodan-key', SENTINEL, 'probe');
    expect((await app.inject({ method: 'POST', url: '/api/secrets/resolve', payload: { name: 'shodan-key' } })).statusCode).toBe(401);
    expect((await resolve('shodan-key', 'kai1.forged.bytes')).statusCode).toBe(401);
    const stale = mintActorToken('probe', { dir, ttlSeconds: 60, now: Date.now() - 120_000 }).token;
    const expired = await resolve('shodan-key', stale);
    expect(expired.statusCode).toBe(401);
    // The one distinguishable failure, because re-serving the actor is what fixes it.
    expect(expired.body).toContain('expired');
  });

  it('410s a revoked secret rather than pretending it is missing', async () => {
    await write('shodan-key', SENTINEL, 'probe');
    await app.inject({ method: 'POST', url: '/api/secrets/shodan-key/versions/1/revoke' });
    const res = await resolve('shodan-key', mintActorToken('probe', { dir }).token);
    expect(res.statusCode).toBe(410);
    expect(res.body).not.toContain(SENTINEL);
  });
});

describe('minting an actor identity is fail-closed', () => {
  it('503s with no operator token configured, and says which variable to set', async () => {
    const res = await app.inject({ method: 'POST', url: '/api/secrets/identity', payload: { actor: 'probe' } });
    expect(res.statusCode).toBe(503);
    expect(res.body).toContain('KONTRA_SECRETS_TOKEN');
  });

  it('401s a wrong token and mints for the right one', async () => {
    process.env.KONTRA_SECRETS_TOKEN = 'operator-token';
    expect(
      (await app.inject({ method: 'POST', url: '/api/secrets/identity', headers: { authorization: 'Bearer nope' }, payload: { actor: 'probe' } })).statusCode
    ).toBe(401);
    const res = await app.inject({
      method: 'POST',
      url: '/api/secrets/identity',
      headers: { authorization: 'Bearer operator-token' },
      payload: { actor: 'probe' },
    });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({ identity: 'actor:probe' });
  });
});

describe('managing secrets is opt-in, like the rest of this API', () => {
  it('is open when no token is configured', async () => {
    expect((await app.inject({ method: 'GET', url: '/api/secrets' })).statusCode).toBe(200);
  });

  it('is enforced on every management route once one is', async () => {
    process.env.KONTRA_SECRETS_TOKEN = 'operator-token';
    await write('do-token', SENTINEL); // 401 now — asserted below
    for (const call of [
      { method: 'GET' as const, url: '/api/secrets' },
      { method: 'GET' as const, url: '/api/secrets/do-token' },
      { method: 'PUT' as const, url: '/api/secrets/do-token', payload: { value: SENTINEL } },
      { method: 'POST' as const, url: '/api/secrets/do-token/versions/1/revoke' },
      { method: 'DELETE' as const, url: '/api/secrets/do-token' },
    ]) {
      expect((await app.inject(call)).statusCode, `${call.method} ${call.url}`).toBe(401);
    }
  });
});

describe('no management route returns the value — swept, not spot-checked', () => {
  it('is in no body and no header of anything but the actor’s own resolve', async () => {
    await write('do-token', SENTINEL);
    await write('do-token', `${SENTINEL}-rotated`);
    await write('shodan-key', SENTINEL, 'probe');
    await app.inject({ method: 'POST', url: '/api/secrets/do-token/versions/1/revoke' });

    const calls = [
      { method: 'GET' as const, url: '/api/secrets' },
      { method: 'GET' as const, url: '/api/secrets/do-token' },
      { method: 'GET' as const, url: '/api/secrets/shodan-key' },
      { method: 'PUT' as const, url: '/api/secrets/do-token', payload: { value: `${SENTINEL}-again` } },
      { method: 'PUT' as const, url: '/api/secrets/do-token', payload: { value: '' } },
      { method: 'PUT' as const, url: '/api/secrets/DO_TOKEN', payload: { value: SENTINEL } },
      { method: 'POST' as const, url: '/api/secrets/do-token/versions/2/revoke' },
      { method: 'POST' as const, url: '/api/secrets/do-token/versions/99/revoke' },
      { method: 'GET' as const, url: '/api/secrets/nothing-here' },
      { method: 'DELETE' as const, url: '/api/secrets/shodan-key' },
    ];
    for (const call of calls) {
      const res = await app.inject(call);
      const seen = `${res.body}\n${JSON.stringify(res.headers)}`;
      expect(seen, `${call.method} ${call.url}`).not.toContain(SENTINEL);
      // Not a fragment either — a suffix affordance is a partial disclosure.
      expect(seen, `${call.method} ${call.url}`).not.toContain(SENTINEL.slice(0, 12));
    }
  });
});
