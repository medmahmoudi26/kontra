/**
 * `history` is what keeps a rebased-away digest alive.
 *
 * `kontra rebase` gives a version a NEW digest for the same version, and the `inuse-` reconciler tags
 * the catalog's digests while retention deletes what carries no such tag. So the digest a Lease is
 * still running loses its protection the moment the rebase lands — unless the store remembers it.
 * Every case here is a way to lose one or to invent one.
 */

import { describe, expect, it } from 'vitest';

import { Repo, type ActorRecord } from './repo';

const DIGEST = (c: string) => 'sha256:' + c.repeat(64);

const descriptor = (digest?: string): Omit<ActorRecord, 'savedAt'> => ({
  key: 'demo@1.0.0',
  name: 'demo',
  version: '1.0.0',
  schemaVersion: 'kontra.actor.v1',
  operations: [],
  ...(digest === undefined ? {} : { digest }),
});

const repo = () => new Repo(':memory:');

describe('a version remembers the digests it has had', () => {
  it('has no history until a digest actually changes', () => {
    const r = repo();
    const first = r.upsertActor(descriptor(DIGEST('a')));
    expect('history' in first, 'a first registration has no predecessor').toBe(false);
  });

  it('keeps the digest it replaced, newest first', () => {
    const r = repo();
    r.upsertActor(descriptor(DIGEST('a')));
    const second = r.upsertActor(descriptor(DIGEST('b')));
    expect(second.history).toEqual([DIGEST('a')]);
    const third = r.upsertActor(descriptor(DIGEST('c')));
    expect(third.history).toEqual([DIGEST('b'), DIGEST('a')]);
  });

  it('does not record a digest that is not changing', () => {
    // Every worker restart re-registers the same version with the same digest. Appending there grows
    // an unbounded list of one repeated value and pushes real predecessors past any cap.
    const r = repo();
    r.upsertActor(descriptor(DIGEST('a')));
    r.upsertActor(descriptor(DIGEST('b')));
    for (let i = 0; i < 5; i++) r.upsertActor(descriptor(DIGEST('b')));
    expect(r.getActor('demo@1.0.0')!.history).toEqual([DIGEST('a')]);
  });

  it('does not record "nothing" as a predecessor', () => {
    // A dev worker with no KONTRA_ACTOR_DIGEST posts none, and the absence of a digest is not a digest
    // a Lease can be running.
    const r = repo();
    r.upsertActor(descriptor());
    const next = r.upsertActor(descriptor(DIGEST('a')));
    expect('history' in next).toBe(false);
  });

  it('does not duplicate a digest the version has already had', () => {
    // A runtime rolled forward and then back. Two entries would consume two of the kept slots for one
    // image.
    const r = repo();
    r.upsertActor(descriptor(DIGEST('a')));
    r.upsertActor(descriptor(DIGEST('b')));
    const back = r.upsertActor(descriptor(DIGEST('a')));
    expect(back.history).toEqual([DIGEST('b')]);
  });

  it('survives the round trip through SQLite, absent when empty', () => {
    // The conformance fixture compares what the catalog hands back key for key, so an empty history
    // must be ABSENT rather than `[]`.
    const r = repo();
    r.upsertActor(descriptor(DIGEST('a')));
    expect('history' in r.getActor('demo@1.0.0')!).toBe(false);
    r.upsertActor(descriptor(DIGEST('b')));
    expect(r.getActor('demo@1.0.0')!.history).toEqual([DIGEST('a')]);
  });

  it('records the predecessor on the digest-only route too', () => {
    // `POST /api/actors/:key/digest` is the path a rebase uses: it changes the digest and nothing else.
    const r = repo();
    r.upsertActor(descriptor(DIGEST('a')));
    const rebased = r.setActorDigest({ key: 'demo@1.0.0', name: 'demo', version: '1.0.0', digest: DIGEST('b') });
    expect(rebased.digest).toBe(DIGEST('b'));
    expect(rebased.history).toEqual([DIGEST('a')]);
  });

  it('lets an explicit history win, because one caller knows the whole lineage', () => {
    // `kontra rebase` replays what it knows; a registration derives it.
    const r = repo();
    r.upsertActor(descriptor(DIGEST('a')));
    const replayed = r.upsertActor({ ...descriptor(DIGEST('z')), history: [DIGEST('y'), DIGEST('x')] });
    expect(replayed.history).toEqual([DIGEST('y'), DIGEST('x')]);
  });
});
