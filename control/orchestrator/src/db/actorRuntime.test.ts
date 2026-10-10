/**
 * What a registration may do to the runtime an Actor image was built on.
 *
 * The runtime a worker registers is an ECHO of what `kontra deploy` baked into the image's Procfile
 * (`cli/packstage.go`). `kontra rebase` moves the image onto a newer digest of the same runtime major
 * without touching that Procfile, so from then on every worker of the image echoes a digest that is
 * no longer true. Each case here is a way for a worker restart to undo a rebase, or for a real
 * rebuild to be ignored.
 */

import { describe, expect, it } from 'vitest';

import { Repo, type ActorRecord, type ActorRuntimeRecord } from './repo';

const DIGEST = (c: string) => 'sha256:' + c.repeat(64);
const BUILDER = DIGEST('b');

const runtime = (c: string, over: Partial<ActorRuntimeRecord> = {}): ActorRuntimeRecord => ({
  name: 'python-browser',
  major: 1,
  digest: DIGEST(c),
  ...over,
});

/** A worker's registration: what the registrar POSTs, with the build facts it read from the env. */
const registration = (
  over: Partial<Omit<ActorRecord, 'savedAt'>> = {}
): Omit<ActorRecord, 'savedAt'> => ({
  key: 'enrich@0.3.0',
  name: 'enrich',
  version: '0.3.0',
  schemaVersion: 'kontra.actor.v1',
  operations: [],
  digest: DIGEST('1'),
  ...over,
});

describe('the build facts a registration carries', () => {
  it('are recorded by the first registration that carries them', () => {
    const r = new Repo(':memory:');
    const rec = r.upsertActor(registration({ runtime: runtime('a'), builderDigest: BUILDER }));
    expect(rec.runtime).toEqual(runtime('a'));
    expect(r.getActor('enrich@0.3.0')?.builderDigest).toBe(BUILDER);
  });

  it('survive a registration that omits them — absent keeps, as the catalog corpus says', () => {
    // A worker started without the Procfile's stamp (an actor that ships its own Procfile, or one
    // run by `kontra serve` from a folder) must not erase what a deployed image recorded.
    const r = new Repo(':memory:');
    r.upsertActor(registration({ runtime: runtime('a'), builderDigest: BUILDER }));
    r.upsertActor(registration());
    const held = r.getActor('enrich@0.3.0');
    expect(held?.runtime).toEqual(runtime('a'));
    expect(held?.builderDigest).toBe(BUILDER);
  });

  it('cannot undo a rebase: the echo of the BUILD digest does not move the recorded one back', () => {
    const r = new Repo(':memory:');
    r.upsertActor(registration({ runtime: runtime('a'), builderDigest: BUILDER }));
    // `kontra rebase` records the new image AND the runtime it now sits on, through the digest route.
    r.setActorDigest({
      key: 'enrich@0.3.0',
      name: 'enrich',
      version: '0.3.0',
      digest: DIGEST('2'),
      runtime: runtime('c'),
    });
    // A worker of the rebased image boots. Its Procfile still says what the image was BUILT on.
    r.upsertActor(registration({ digest: DIGEST('2'), runtime: runtime('a'), builderDigest: BUILDER }));
    expect(r.getActor('enrich@0.3.0')?.runtime).toEqual(runtime('c'));
  });

  it('fills a digest the catalog holds blank', () => {
    const r = new Repo(':memory:');
    r.upsertActor(registration({ runtime: runtime('a', { digest: '' }) }));
    expect(r.upsertActor(registration({ runtime: runtime('a') })).runtime).toEqual(runtime('a'));
  });

  it('believes a DIFFERENT runtime, because only a rebuild can produce one', () => {
    // A rebase never changes the name or the major, so an echo naming another one is a new build of
    // this version (`deploy --override`) and the recorded runtime is the stale fact.
    const r = new Repo(':memory:');
    r.upsertActor(registration({ runtime: runtime('a') }));
    const moved = runtime('d', { major: 2 });
    expect(r.upsertActor(registration({ runtime: moved })).runtime).toEqual(moved);
    const renamed = runtime('e', { name: 'python' });
    expect(r.upsertActor(registration({ runtime: renamed })).runtime).toEqual(renamed);
  });
});
