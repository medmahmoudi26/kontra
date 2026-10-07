/**
 * `inuse-` tags are what stand between zot's retention policy and a running actor's image, so the
 * assertions here are about the directions in which being wrong is unrecoverable: a tag that protects
 * the wrong manifest, a tag that is silently not written, and a removal that takes something it should
 * not have.
 */

import { describe, expect, it, vi } from 'vitest';

import type { ActorRecord } from '../db/repo';
import { INUSE_PREFIX, desiredInuseTags, inuseTagFor, reconcileInuseTags, repoForActor } from './inuse';
import { registryTagClient } from './inuseReconciler';

const DIGEST_A = 'sha256:' + 'a'.repeat(64);
const DIGEST_B = 'sha256:' + 'b'.repeat(64);

const actor = (name: string, version: string, digest?: string): ActorRecord =>
  ({
    key: `${name}@${version}`,
    name,
    version,
    schemaVersion: 'kontra.actor.v1',
    operations: [],
    savedAt: '2026-10-07T00:00:00.000Z',
    ...(digest === undefined ? {} : { digest }),
  }) as ActorRecord;

describe('inuseTagFor', () => {
  it('is the prefix and twelve hex characters', () => {
    expect(inuseTagFor(DIGEST_A)).toBe(`${INUSE_PREFIX}${'a'.repeat(12)}`);
  });

  it('refuses anything that is not a sha256 digest rather than building a partial tag', () => {
    // A tag built from a truncated or empty digest protects the wrong manifest or nothing at all, and
    // both of those read as success.
    for (const bad of ['', 'sha256:', 'sha256:abc', 'abc', 'sha512:' + 'a'.repeat(64), 'sha256:' + 'A'.repeat(64)]) {
      expect(inuseTagFor(bad), JSON.stringify(bad)).toBeNull();
    }
  });
});

describe('desiredInuseTags', () => {
  it('is one tag per distinct digest, grouped by repository', () => {
    const want = desiredInuseTags([actor('alpha', '1.0.0', DIGEST_A), actor('beta', '2.0.0', DIGEST_B)]);
    expect([...want.keys()].sort()).toEqual(['alpha', 'beta']);
    expect(want.get('alpha')!.get(inuseTagFor(DIGEST_A)!)).toBe(DIGEST_A);
  });

  it('contributes nothing for an actor with no digest', () => {
    // ADR 0011: "" means unset, which is the normal state of a dev actor that was never pushed.
    expect(desiredInuseTags([actor('alpha', '1.0.0'), actor('alpha', '1.1.0', '')]).size).toBe(0);
  });

  it('collapses two versions that recorded the same digest into one tag', () => {
    const want = desiredInuseTags([actor('alpha', '1.0.0', DIGEST_A), actor('alpha', '1.1.0', DIGEST_A)]);
    expect(want.get('alpha')!.size).toBe(1);
  });

  it('writes the bare repository name, which is what this install has', () => {
    // The spec's table says `actors/<name>`; nothing is under that prefix yet, and this is the one
    // place that decides which shape is written.
    expect(repoForActor('webcrawl')).toBe('webcrawl');
  });
});

describe('reconcileInuseTags', () => {
  const deps = (over: Partial<Parameters<typeof reconcileInuseTags>[0]> = {}) => ({
    listActors: () => [actor('alpha', '1.0.0', DIGEST_A)],
    listTags: vi.fn(async () => [] as string[]),
    tag: vi.fn(async () => undefined),
    untag: vi.fn(async () => undefined),
    ...over,
  });

  it('adds a missing tag', async () => {
    const d = deps();
    const pass = await reconcileInuseTags(d);
    expect(pass).toMatchObject({ added: 1, removed: 0, kept: 0 });
    expect(d.tag).toHaveBeenCalledWith('alpha', DIGEST_A, inuseTagFor(DIGEST_A));
  });

  it('leaves a correct tag alone rather than re-putting it every pass', async () => {
    const d = deps({ listTags: vi.fn(async () => [inuseTagFor(DIGEST_A)!, '1.0.0']) });
    const pass = await reconcileInuseTags(d);
    expect(pass).toMatchObject({ added: 0, kept: 1 });
    expect(d.tag).not.toHaveBeenCalled();
  });

  it('removes an inuse tag the catalog no longer implies', async () => {
    const stale = `${INUSE_PREFIX}${'c'.repeat(12)}`;
    const d = deps({ listTags: vi.fn(async () => [inuseTagFor(DIGEST_A)!, stale]) });
    const pass = await reconcileInuseTags(d);
    expect(pass).toMatchObject({ removed: 1, kept: 1 });
    expect(d.untag).toHaveBeenCalledWith('alpha', stale);
  });

  it('never touches a tag that is not an inuse tag', async () => {
    const d = deps({ listTags: vi.fn(async () => ['1.0.0', '0.9.0', 'latest', inuseTagFor(DIGEST_A)!]) });
    await reconcileInuseTags(d);
    expect(d.untag).not.toHaveBeenCalled();
  });

  it('counts a refused removal instead of throwing', async () => {
    // With no credential the store permits read/create/update and NOT delete, on purpose. A stale tag
    // then outlives its reason and retention keeps more than it must — recoverable, unlike the
    // alternative.
    const stale = `${INUSE_PREFIX}${'c'.repeat(12)}`;
    const d = deps({
      listTags: vi.fn(async () => [stale]),
      untag: vi.fn(async () => {
        throw new Error('401 Unauthorized');
      }),
    });
    const pass = await reconcileInuseTags(d);
    expect(pass.refusedRemovals).toBe(1);
    expect(pass.added).toBe(1);
  });

  it('reports a failed tag write and carries on to the next repository', async () => {
    const onError = vi.fn();
    const d = deps({
      listActors: () => [actor('alpha', '1.0.0', DIGEST_A), actor('beta', '2.0.0', DIGEST_B)],
      tag: vi.fn(async (repo: string) => {
        if (repo === 'alpha') throw new Error('nope');
      }),
      onError,
    });
    const pass = await reconcileInuseTags(d);
    expect(pass.added).toBe(1); // beta still got its tag
    expect(onError).toHaveBeenCalled();
  });

  it('survives a repository whose tag list cannot be read', async () => {
    const onError = vi.fn();
    const d = deps({
      listTags: vi.fn(async () => {
        throw new Error('registry down');
      }),
      onError,
    });
    await expect(reconcileInuseTags(d)).resolves.toMatchObject({ added: 0 });
    expect(onError).toHaveBeenCalled();
  });
});

describe('the registry client refuses to untag anything it did not write', () => {
  it('rejects a version tag', async () => {
    // The only guard between a bug in the caller and deleting a release's name.
    await expect(registryTagClient('http://example.invalid').untag('alpha', '1.0.0')).rejects.toThrow(
      /refusing to untag/
    );
  });
});

describe('a rebased-away digest keeps its protection', () => {
  // The reason the store keeps a history at all. Retention deletes what carries no `inuse-` tag, and a
  // Lease keeps its old digest until it drops — so tagging only the current digest means a rebase takes
  // the image an in-flight Run is still pulling.
  const WAS = 'sha256:' + 'e'.repeat(64);
  const NOW = 'sha256:' + 'f'.repeat(64);

  it('tags the current digest and every digest the version has had', () => {
    const rebased = { ...actor('alpha', '1.0.0', NOW), history: [WAS] } as ActorRecord;
    const want = desiredInuseTags([rebased]).get('alpha')!;
    expect([...want.keys()].sort()).toEqual([inuseTagFor(NOW)!, inuseTagFor(WAS)!].sort());
    expect(want.get(inuseTagFor(WAS)!), 'the tag must point at the predecessor itself').toBe(WAS);
  });

  it('ignores an unreadable digest in the history without losing the readable ones', () => {
    // History is data from another process. One bad entry must not cost the rest their protection.
    const a = { ...actor('alpha', '1.0.0', NOW), history: ['not-a-digest', WAS] } as ActorRecord;
    const want = desiredInuseTags([a]).get('alpha')!;
    expect([...want.keys()].sort()).toEqual([inuseTagFor(NOW)!, inuseTagFor(WAS)!].sort());
  });

  it('still contributes nothing for an actor with no digest and no history', () => {
    expect(desiredInuseTags([actor('alpha', '1.0.0')]).size).toBe(0);
  });
});
