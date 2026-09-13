/**
 * CROSS-LANGUAGE CONFORMANCE: the Python SDK declaring and resolving a SLOT against the REAL routes.
 *
 * The peer of `actorFetch.conformance.test.ts`, and it exists for a reason this slice made concrete.
 * Both sides of this wire are tested on their own — `slotRoutes.test.ts` here,
 * `tests/test_actor_slots.py` there — and neither process can see the other, so each is asserting
 * against its own idea of the contract. The slot surface moved from `/api/secrets/slots/...` to
 * `/api/slots/...` during its own construction (a static segment was shadowing `GET
 * /api/secrets/:name`), and BOTH suites would have stayed green with the SDK pointed at the old
 * path. This is the test that would not have.
 *
 * WHAT IT PINS, beyond the paths: that the declaration the SDK posts is the shape the route stores,
 * that the resolution carries the actor VERSION (a slot is declared per version), and that the two
 * refusals an author actually hits — undeclared and unbound — arrive as two different sentences.
 *
 * SKIPS ITSELF when the repo venv is absent, for `actorFetch.conformance.test.ts`'s reason: a
 * machine without the Python side is one where this proves nothing, not one where it is broken.
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
import { SlotStore } from './slotStore';
import { SecretStore } from './store';

const SENTINEL = 'sk_live_SENTINEL_across_two_languages_5d2f';
const ROOT = path.join(__dirname, '..', '..', '..', '..');
const PY = path.join(ROOT, '.venv', 'bin', 'python');
const HAVE_PY = existsSync(PY);

let app: FastifyInstance;
let secrets: SecretStore;
let slots: SlotStore;
let base = '';

beforeAll(async () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'kontra-slots-conf-'));
  secrets = new SecretStore(new FileSecretBackend({ dir }), { keyDir: dir });
  slots = new SlotStore(secrets, { dir });
  await secrets.put('stripe-prod', SENTINEL);
  app = buildServer({ repo: new Repo(':memory:'), webRoot: '', secrets, slots });
  await app.listen({ port: 0, host: '127.0.0.1' });
  const addr = app.server.address();
  base = typeof addr === 'object' && addr ? `http://127.0.0.1:${addr.port}` : '';
});

afterAll(async () => {
  await app?.close();
});

/** Run one snippet in a real interpreter, as a worker with `actor`'s identity. Async — a
 *  `spawnSync` would block the event loop this server is listening on (measured; see the peer). */
async function run(actor: string, body: string[]): Promise<{ ok: boolean; out: string }> {
  const token = secrets.mintIdentity(actor).token;
  const code = ['import json, sys', 'try:', ...body.map((l) => `    ${l}`), 'except Exception as e:', '    print(json.dumps({"ok": False, "out": str(e)}))'].join('\n');
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

/** Declare through the SDK's own publisher, exactly as a worker does when it registers itself. */
const declareAsWorker = (actor: string, version: string, decls: string): Promise<{ ok: boolean; out: string }> =>
  run(actor, [
    'from kontra import secrets',
    `status = secrets.declare(${JSON.stringify(base)}, ${JSON.stringify(actor)}, ${JSON.stringify(version)}, ${decls})`,
    'print(json.dumps({"ok": True, "out": str(status)}))',
  ]);

/** Resolve through the handle an author actually holds: `actor.slot("…").get_sync(...)`. */
const resolveAsActor = (actor: string, version: string, slot: string, runId = ''): Promise<{ ok: boolean; out: string }> =>
  run(actor, [
    'from kontra.actor import ActorRegistry',
    'r = ActorRegistry()',
    `r.actor_name = ${JSON.stringify(actor)}`,
    `r.version = ${JSON.stringify(version)}`,
    `print(json.dumps({"ok": True, "out": r.slot(${JSON.stringify(slot)}).get_sync(run=${JSON.stringify(runId)})}))`,
  ]);

describe.skipIf(!HAVE_PY)('an actor declaring and resolving a slot, across the languages', () => {
  it('registers what it asks for, in the shape the orchestrator stores', async () => {
    const said = await declareAsWorker('probe', '0.2.0', '[{"name": "api_key", "description": "the vendor key"}]');
    expect(said).toEqual({ ok: true, out: '200' });
    // Read back through the STORE, not through the response the SDK saw: what matters is that the
    // declaration landed where the run gate and the console will look for it.
    expect((await slots.view('probe', '0.2.0')).slots).toEqual([
      expect.objectContaining({ slot: 'api_key', description: 'the vendor key', state: 'unbound' }),
    ]);
  });

  it('is refused an UNBOUND slot with the sentence that names the console fix', async () => {
    await declareAsWorker('probe', '0.2.0', '[{"name": "api_key"}]');
    const said = await resolveAsActor('probe', '0.2.0', 'api_key');
    expect(said.ok).toBe(false);
    expect(said.out).toMatch(/BOUND NOTHING to it/);
    expect(said.out).not.toContain(SENTINEL);
  });

  it('gets the value once the operator binds it, and never learns which secret answered', async () => {
    await declareAsWorker('probe', '0.2.0', '[{"name": "api_key"}]');
    await slots.bind('probe', 'api_key', 'stripe-prod');
    const said = await resolveAsActor('probe', '0.2.0', 'api_key', 'nscheck-17');
    expect(said).toEqual({ ok: true, out: SENTINEL });

    // AND THE LEDGER ANSWERS "WHICH ACTOR READ MY KEY, AND WHEN" — written by the real route, from
    // a real resolution, carrying the run the real SDK sent.
    expect(slots.log.list({ actor: 'probe' })[0]).toMatchObject({
      actor: 'probe',
      version: '0.2.0',
      slot: 'api_key',
      run: 'nscheck-17',
      secret: 'stripe-prod',
      outcome: 'resolved',
    });
  });

  it('is refused an UNDECLARED slot with a DIFFERENT sentence — a code fix, not a console one', async () => {
    await declareAsWorker('probe', '0.2.0', '[{"name": "api_key"}]');
    // Bound, and still refused: the binding is not what makes an ask legitimate.
    await slots.bind('probe', 'sneaky', 'stripe-prod');
    const said = await resolveAsActor('probe', '0.2.0', 'sneaky');
    expect(said.ok).toBe(false);
    expect(said.out).toMatch(/does not DECLARE it/);
    expect(said.out).not.toContain(SENTINEL);
  });

  it('is refused a slot another VERSION of it declared', async () => {
    await declareAsWorker('probe', '0.2.0', '[{"name": "api_key"}]');
    await declareAsWorker('probe', '0.3.0', '[{"name": "api_key"}, {"name": "webhook_secret"}]');
    await slots.bind('probe', 'webhook_secret', 'stripe-prod');
    const said = await resolveAsActor('probe', '0.2.0', 'webhook_secret');
    expect(said.ok).toBe(false);
    expect(said.out).toMatch(/does not DECLARE it/);
  });
});
