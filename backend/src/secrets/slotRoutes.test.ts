/**
 * The slot surface end to end: declare, bind, resolve, refuse, and read the ledger back.
 *
 * THE SWEEP AT THE BOTTOM IS THE REAL TEST, exactly as it is in `routes.test.ts`. A sentinel is
 * written as a real secret, bound into a real slot and really resolved — and then every management
 * route, every refusal body and the audit route are searched for it. A route added later that
 * leaks a value has to delete a test to land.
 */

import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import type { FastifyInstance } from 'fastify';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { Repo } from '../db/repo';
import { buildServer } from '../server';
import { FileSecretBackend } from './fileBackend';
import { mintActorToken } from './identity';
import { SlotStore } from './slotStore';
import { SecretStore } from './store';

const SENTINEL = 'sk_live_SENTINEL_never_over_http_2a90';

let app: FastifyInstance;
let dir: string;
let secrets: SecretStore;
let slots: SlotStore;
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
  dir = mkdtempSync(join(tmpdir(), 'kontra-slot-routes-'));
  secrets = new SecretStore(new FileSecretBackend({ dir }), { keyDir: dir });
  slots = new SlotStore(secrets, { dir });
  app = buildServer({ repo: new Repo(':memory:'), webRoot: '', secrets, slots });
});

afterEach(async () => {
  await app.close();
  rmSync(dir, { recursive: true, force: true });
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
});

const declare = (actor: string, version: string, list: unknown[]) =>
  app.inject({ method: 'POST', url: '/api/slots/declare', payload: { actor, version, slots: list } });

const write = (name: string, value: string, owner?: string) =>
  app.inject({ method: 'PUT', url: `/api/secrets/${name}`, payload: owner ? { value, owner } : { value } });

const bind = (actor: string, slot: string, secret: string) =>
  app.inject({ method: 'PUT', url: `/api/slots/actor/${actor}/${slot}`, payload: { secret } });

const resolve = (actor: string, body: Record<string, unknown>) =>
  app.inject({
    method: 'POST',
    url: '/api/slots/resolve',
    headers: { authorization: `Bearer ${mintActorToken(actor, { dir }).token}` },
    payload: body,
  });

describe('an actor declares, an operator binds', () => {
  it('lists what an actor will ask for BEFORE anything has run', async () => {
    await declare('probe', '0.1.0', [{ name: 'api_key', description: 'the vendor key' }]);
    const res = await app.inject({ method: 'GET', url: '/api/slots/actor/probe' });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({
      actor: 'probe',
      version: '0.1.0',
      slots: [{ slot: 'api_key', state: 'unbound', description: 'the vendor key' }],
    });
  });

  it('accepts a bare name beside the object form — the terse spelling must not silently register nothing', async () => {
    await declare('probe', '0.1.0', ['api_key']);
    expect((await app.inject({ method: 'GET', url: '/api/slots/actor/probe' })).json()).toMatchObject({
      slots: [{ slot: 'api_key' }],
    });
  });

  it('binds a slot to a secret, and rebinds it', async () => {
    await write('stripe-prod', SENTINEL);
    await write('stripe-test', 'other');
    await declare('probe', '0.1.0', ['api_key']);

    const first = await bind('probe', 'api_key', 'stripe-prod');
    expect(first.statusCode).toBe(200);
    expect(first.json()).toMatchObject({ actor: { slots: [{ state: 'bound', secret: 'stripe-prod' }] } });

    const again = await bind('probe', 'api_key', 'stripe-test');
    expect(again.json()).toMatchObject({ actor: { slots: [{ secret: 'stripe-test' }] } });
  });

  it('404s an unbind of a slot that was never bound, so a page can tell it apart from a removal', async () => {
    await declare('probe', '0.1.0', ['api_key']);
    expect((await app.inject({ method: 'DELETE', url: '/api/slots/actor/probe/api_key' })).statusCode).toBe(404);
  });

  it('surfaces a new version’s new slot as a change', async () => {
    await declare('probe', '0.1.0', ['api_key']);
    await declare('probe', '0.2.0', ['api_key', 'webhook_secret']);
    expect((await app.inject({ method: 'GET', url: '/api/slots/actor/probe' })).json()).toMatchObject({
      version: '0.2.0',
      added: ['webhook_secret'],
      comparedWith: '0.1.0',
    });
  });

  it('draws the whole binding surface for Settings: actors, grants and the names to bind to', async () => {
    await write('stripe-prod', SENTINEL);
    await declare('probe', '0.1.0', ['api_key']);
    await bind('probe', 'api_key', 'stripe-prod');
    const body = (await app.inject({ method: 'GET', url: '/api/slots' })).json() as {
      actors: unknown[];
      bindings: unknown[];
      secrets: Array<{ name: string; usable: boolean }>;
    };
    expect(body.actors).toHaveLength(1);
    expect(body.bindings).toHaveLength(1);
    expect(body.secrets).toEqual([{ name: 'stripe-prod', usable: true }]);
  });
});

describe('resolving, and the four cases', () => {
  beforeEach(async () => {
    await write('stripe-prod', SENTINEL);
    await declare('probe', '0.1.0', ['api_key']);
  });

  it('BOUND: hands over the value, and never the secret’s name', async () => {
    await bind('probe', 'api_key', 'stripe-prod');
    const res = await resolve('probe', { slot: 'api_key', version: '0.1.0', run: 'nscheck-17' });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toEqual({ slot: 'api_key', value: SENTINEL });
    expect(res.body).not.toContain('stripe-prod');
  });

  it('UNBOUND: 409, and the message is the fix', async () => {
    const res = await resolve('probe', { slot: 'api_key', version: '0.1.0' });
    expect(res.statusCode).toBe(409);
    expect(res.json().error).toMatch(/nothing is bound to it/);
  });

  it('REVOKED: 410, distinct from unbound', async () => {
    await bind('probe', 'api_key', 'stripe-prod');
    await app.inject({ method: 'POST', url: '/api/secrets/stripe-prod/versions/1/revoke' });
    const res = await resolve('probe', { slot: 'api_key', version: '0.1.0' });
    expect(res.statusCode).toBe(410);
  });

  it('UNDECLARED: 403 — an actor asking for a slot it never declared is refused, not satisfied', async () => {
    // Even with a binding sitting there for it.
    await bind('probe', 'sneaky', 'stripe-prod');
    const res = await resolve('probe', { slot: 'sneaky', version: '0.1.0' });
    expect(res.statusCode).toBe(403);
    expect(res.json().error).toMatch(/does not declare a slot named "sneaky"/);
    expect(res.body).not.toContain(SENTINEL);
  });

  it('refuses a request that does not say which version is asking', async () => {
    const res = await resolve('probe', { slot: 'api_key' });
    expect(res.statusCode).toBe(400);
    expect(res.json().error).toMatch(/declared per version/);
  });

  it('401s without an identity, and does not say whether the slot exists', async () => {
    const res = await app.inject({
      method: 'POST',
      url: '/api/slots/resolve',
      payload: { slot: 'api_key', version: '0.1.0' },
    });
    expect(res.statusCode).toBe(401);
  });

  it('another actor’s identity cannot reach this actor’s slot', async () => {
    await bind('probe', 'api_key', 'stripe-prod');
    const res = await resolve('scanner', { slot: 'api_key', version: '0.1.0' });
    expect(res.statusCode).toBe(403);
    expect(res.body).not.toContain(SENTINEL);
  });
});

describe('the preflight and the ledger', () => {
  it('previews the refusal a run start would give', async () => {
    await declare('probe', '0.1.0', ['api_key']);
    const res = await app.inject({
      method: 'POST',
      url: '/api/slots/preflight',
      payload: { actors: ['probe@0.1.0'] },
    });
    expect(res.json()).toMatchObject({ ok: false });
    expect(res.json().refusal).toContain('api_key');
  });

  it('answers "which actor read my key, and when" — successes AND refusals', async () => {
    await write('stripe-prod', SENTINEL);
    await declare('probe', '0.1.0', ['api_key']);
    await resolve('probe', { slot: 'api_key', version: '0.1.0', run: 'r-1' });
    await bind('probe', 'api_key', 'stripe-prod');
    await resolve('probe', { slot: 'api_key', version: '0.1.0', run: 'r-2' });

    const body = (await app.inject({ method: 'GET', url: '/api/slots/audit' })).json() as {
      resolutions: Array<Record<string, unknown>>;
    };
    expect(body.resolutions.map((r) => r.outcome)).toEqual(['resolved', 'unbound']);
    expect(body.resolutions[0]).toMatchObject({
      actor: 'probe',
      version: '0.1.0',
      slot: 'api_key',
      run: 'r-2',
      secret: 'stripe-prod',
    });
  });

  it('filters the ledger by run', async () => {
    await write('stripe-prod', SENTINEL);
    await declare('probe', '0.1.0', ['api_key']);
    await bind('probe', 'api_key', 'stripe-prod');
    await resolve('probe', { slot: 'api_key', version: '0.1.0', run: 'r-1' });
    await resolve('probe', { slot: 'api_key', version: '0.1.0', run: 'r-2' });
    const res = await app.inject({ method: 'GET', url: '/api/slots/audit?run=r-1' });
    expect((res.json() as { resolutions: unknown[] }).resolutions).toHaveLength(1);
  });
});

describe('admission', () => {
  it('enforces the secrets token on binding and on the ledger when one is configured', async () => {
    process.env.KONTRA_SECRETS_TOKEN = 'shh';
    for (const call of [
      app.inject({ method: 'GET', url: '/api/slots' }),
      app.inject({ method: 'GET', url: '/api/slots/audit' }),
      bind('probe', 'api_key', 'stripe-prod'),
    ]) {
      expect((await call).statusCode).toBe(401);
    }
  });

  it('lets a WORKER declare with no operator token — a declaration grants nothing', async () => {
    process.env.KONTRA_SECRETS_TOKEN = 'shh';
    expect((await declare('probe', '0.1.0', ['api_key'])).statusCode).toBe(200);
  });
});

describe('no name can shadow a verb', () => {
  it('leaves a SECRET called "slots" readable — the trap that decided the path', async () => {
    // Fastify prefers a static segment over a parameterised one. Mounted at `/api/secrets/slots`,
    // this surface would have made a secret an operator legitimately called `slots` unreadable
    // through `GET /api/secrets/:name`, silently.
    await write('slots', SENTINEL);
    const res = await app.inject({ method: 'GET', url: '/api/secrets/slots' });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({ name: 'slots', current: 1 });
    expect(res.body).not.toContain(SENTINEL);
  });

  it('leaves an ACTOR called "declare" readable — the same trap one level down', async () => {
    await declare('declare', '0.1.0', ['api_key']);
    const res = await app.inject({ method: 'GET', url: '/api/slots/actor/declare' });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({ actor: 'declare', slots: [{ slot: 'api_key' }] });
  });

  it('leaves an ACTOR called "audit" readable', async () => {
    await declare('audit', '0.1.0', ['api_key']);
    expect((await app.inject({ method: 'GET', url: '/api/slots/actor/audit' })).json()).toMatchObject({
      actor: 'audit',
    });
  });
});

describe('THE SWEEP: the value appears in no response this surface can produce', () => {
  it('is absent from every body and every header, on a store that really holds it', async () => {
    await write('stripe-prod', SENTINEL);
    await write('probe-own', SENTINEL, 'probe');
    await declare('probe', '0.1.0', [{ name: 'api_key', description: 'the vendor key' }]);
    await bind('probe', 'api_key', 'stripe-prod');
    // A real resolution, so the ledger has a line about it and the store has really been read.
    expect((await resolve('probe', { slot: 'api_key', version: '0.1.0', run: 'r-9' })).json().value).toBe(SENTINEL);

    const swept = [
      await app.inject({ method: 'GET', url: '/api/slots' }),
      await app.inject({ method: 'GET', url: '/api/slots/actor/probe' }),
      await app.inject({ method: 'GET', url: '/api/slots/actor/probe?version=0.1.0' }),
      await app.inject({ method: 'GET', url: '/api/slots/audit' }),
      await app.inject({ method: 'GET', url: '/api/slots/audit?actor=probe' }),
      await app.inject({ method: 'POST', url: '/api/slots/preflight', payload: { actors: ['probe'] } }),
      await declare('probe', '0.2.0', ['api_key']),
      await bind('probe', 'api_key', 'stripe-prod'),
      await app.inject({ method: 'DELETE', url: '/api/slots/actor/probe/api_key' }),
      // Refusal bodies too: an error is a read path.
      await resolve('probe', { slot: 'api_key', version: '0.1.0' }),
      await resolve('probe', { slot: 'undeclared_one', version: '0.1.0' }),
      await resolve('scanner', { slot: 'api_key', version: '0.1.0' }),
    ];
    for (const res of swept) {
      expect(res.body).not.toContain(SENTINEL);
      expect(JSON.stringify(res.headers)).not.toContain(SENTINEL);
    }
  });
});
