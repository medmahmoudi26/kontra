/**
 * The TYPESCRIPT arm of the cross-SDK catalog contract (shared/conformance/catalog.json).
 *
 * Go and Python WRITE this descriptor — two hand-written JSON emitters that share no code with
 * each other or with this reader (ADR 0002: the wire is JSON, the proto is only the shared type
 * definition). The orchestrator is the READER, so what it must honour is narrower than what they
 * must produce, and it is the half that decides whether any of it survives: a field the route
 * does not name is accepted with a 200 and dropped on the way to the table, which is silent.
 *
 * `source` is the one that already happened in the other direction — it reached the proto, the
 * `actors` table and the Python SDK, and the Go emitter never got it — so a Go actor's row on the
 * Actors page had no way back to its code and looked exactly like an actor nobody had registered
 * a source for.
 *
 * The peers assert the SAME file: runtime/go/registrar/conformance_test.go and
 * tests/test_catalog_conformance.py.
 */
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import type { FastifyInstance } from 'fastify';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { Repo } from './db/repo';
import { buildServer } from './server';

// The run routes reach Temporal; the catalog routes do not. Stubbed so this suite is hermetic.
vi.mock('./temporalClient', () => ({
  describeRun: vi.fn(),
  listRuns: vi.fn(),
  describeRunHeartbeats: vi.fn(),
  fetchRunHistory: vi.fn(),
}));

type Descriptor = {
  key: string;
  name: string;
  version: string;
  schemaVersion: string;
  digest: string;
  source: string;
  operations: Array<Record<string, unknown>>;
};

const fixture = JSON.parse(
  readFileSync(join(__dirname, '../../../shared/conformance/catalog.json'), 'utf8'),
) as { expect: Descriptor; unsetDigest: { why: string; absent: string[] } };

describe('the catalog stores the golden descriptor without loss', () => {
  let app: FastifyInstance;

  beforeEach(() => {
    app = buildServer({ repo: new Repo(':memory:'), webRoot: '' });
  });
  afterEach(async () => {
    await app.close();
  });

  const post = (payload: unknown) => app.inject({ method: 'POST', url: '/api/actors', payload });
  const list = async () =>
    (await app.inject({ method: 'GET', url: '/api/actors' })).json() as Array<
      Descriptor & { savedAt: number }
    >;

  it('has a descriptor to check', () => {
    // A fixture that silently became empty would turn every assertion below into a no-op.
    expect(fixture.expect.operations.length).toBeGreaterThan(0);
  });

  it('gives back exactly what the SDKs posted', async () => {
    const res = await post(fixture.expect);
    expect(res.statusCode).toBe(200);

    const [stored] = await list();
    const { savedAt, ...record } = stored;
    // WHEN the row was written is the catalog's own fact about itself, not something a worker
    // declares — it is the only key the store is allowed to add.
    expect(typeof savedAt).toBe('number');
    expect(record).toEqual(fixture.expect);
  });

  it('keeps every per-Method schema on the Method that declared it', async () => {
    // The §9 property, checked on the READER's side: `title` takes what `fetch` emits, so a store
    // that flattened the operations — or kept one Actor-level pair, as this once did — would hand
    // back an actor whose two Methods advertise the same input, and every node typechecked
    // against the wrong one.
    await post(fixture.expect);
    const [stored] = await list();
    for (const [i, op] of fixture.expect.operations.entries()) {
      expect(stored.operations[i]).toEqual(op);
    }
  });

  it('keeps a registered digest when a re-registration omits it', async () => {
    // This is WHY both SDKs omit `digest` rather than sending "" (the fixture's `unsetDigest`
    // case). The preserve rule is `body.digest ?? prev.digest`, and `??` only fires on
    // null/undefined — an empty string is a value and overwrites, so a dev worker with no
    // KONTRA_ACTOR_DIGEST would unpin the image on every restart.
    expect(fixture.unsetDigest.absent).toContain('digest');
    await post(fixture.expect);

    const { digest, ...withoutDigest } = fixture.expect;
    await post(withoutDigest);
    expect((await list())[0].digest).toBe(digest);

    await post({ ...fixture.expect, digest: '' });
    expect((await list())[0].digest).toBe('');
  });

  it('keeps every build fact the corpus names when a re-registration omits it', async () => {
    // The same rule, for the keys a worker cannot discover at all. `runtime` and `builderDigest` used
    // to be erased by any registration without them, while this corpus said otherwise — so a worker
    // started without the Procfile's stamp unpinned a deployed image's runtime and `kontra rebase`
    // stopped seeing it.
    const absent = fixture.unsetDigest.absent;
    expect(absent).toEqual(expect.arrayContaining(['runtime', 'builderDigest']));
    await post(fixture.expect);

    const stripped: Record<string, unknown> = { ...fixture.expect };
    for (const key of absent) delete stripped[key];
    await post(stripped);
    const { savedAt: _savedAt, ...kept } = (await list())[0];
    expect(kept).toEqual(fixture.expect);
  });
});
