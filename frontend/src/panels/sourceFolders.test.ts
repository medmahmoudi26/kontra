import { describe, expect, it } from 'vitest';
import type { Source } from '../run/api';
import {
  canForget,
  catalogForFolder,
  folderForActor,
  isDiscovered,
  mergeWorkflowFolders,
  registeredActors,
  withRegistered,
  withoutSource,
} from './sourceFolders';

function registered(name: string, dir: string, over: Partial<Source> = {}): Source {
  return {
    id: `actor:${name}:1f3k`,
    kind: 'actor',
    name,
    path: dir,
    version: '0.1.0',
    description: '',
    registeredAt: 1_700_000_000_000,
    ...over,
  };
}

/** What `GET /api/sources/:kind` returns for a folder it found under the default root. */
function discovered(name: string, dir: string): Source {
  return { ...registered(name, dir), id: `at:${dir}`, registeredAt: 0 };
}

describe('discovered vs registered', () => {
  it('reads the id, which is the rule the server refuses forget by', () => {
    expect(isDiscovered(discovered('probe', '/home/me/.kontra/actors/probe'))).toBe(true);
    expect(isDiscovered(registered('probe', '/src/probe'))).toBe(false);
  });

  it('offers forget only where there is a registration to remove', () => {
    // A forget button on a discovered folder would 400 — `SourceStore.forget` refuses `at:` — so
    // the affordance must not be drawn rather than fail when it is pressed.
    expect(canForget(discovered('probe', '/home/me/.kontra/actors/probe'))).toBe(false);
    expect(canForget(registered('probe', '/src/probe'))).toBe(true);
  });
});

describe('folding in what register returned', () => {
  it('adds a folder that was not listed', () => {
    const list = withRegistered([], registered('probe', '/src/probe'));
    expect(list.map((s) => s.path)).toEqual(['/src/probe']);
  });

  it('does not make a second entry when the same folder is registered twice', () => {
    // The server answers the second POST with the FIRST row — same id — and appending it would put
    // two identical rows on screen.
    const first = registered('probe', '/src/probe');
    const once = withRegistered([], first);
    expect(withRegistered(once, { ...first })).toHaveLength(1);
  });

  it('replaces the discovered row for a folder that was just registered', () => {
    // The ids differ by construction (`at:<path>` vs `actor:probe:<hash>`), so deduping by id would
    // leave the folder listed twice until the next reload — proof, to the operator who just pressed
    // register, that registering duplicates things.
    const dir = '/home/me/.kontra/actors/probe';
    const list = withRegistered([discovered('probe', dir)], registered('probe', dir));
    expect(list).toHaveLength(1);
    expect(isDiscovered(list[0]!)).toBe(false);
  });

  it('leaves the other folders alone, in name order', () => {
    const list = withRegistered(
      [registered('zeta', '/src/zeta'), registered('alpha', '/src/alpha')],
      registered('mid', '/src/mid')
    );
    expect(list.map((s) => s.name)).toEqual(['alpha', 'mid', 'zeta']);
  });
});

describe('finding a catalogued Actor’s folder', () => {
  it('finds nothing when nobody registered one — which is a state, not a failure', () => {
    // A worker on a droplet registers itself into the catalog; its code is not on this disk. The
    // card has to say that rather than open an editor over nothing.
    expect(folderForActor({ name: 'probe', version: '0.1.0' }, [])).toBeUndefined();
    expect(
      folderForActor({ name: 'probe', version: '0.1.0' }, [registered('beacon', '/src/beacon')])
    ).toBeUndefined();
  });

  it('takes the path the worker loaded from over a name that matches', () => {
    const loaded = registered('renamed', '/srv/checkout/examples/python/probe');
    const other = registered('probe', '/home/me/.kontra/actors/probe');
    const found = folderForActor(
      { name: 'probe', version: '0.1.0', source: '/srv/checkout/examples/python/probe' },
      [other, loaded]
    );
    expect(found?.path).toBe('/srv/checkout/examples/python/probe');
  });

  it('falls back to the name, because a fleet worker’s path is on the fleet', () => {
    // `actor.source` on a Machine is `/opt/kontra/actor/probe`, which matches nothing here. Without
    // the name fallback every actor that has ever run on a droplet would read as unregistered.
    const found = folderForActor(
      { name: 'probe', version: '0.1.0', source: '/opt/kontra/actor/probe' },
      [registered('probe', '/srv/checkout/probe')]
    );
    expect(found?.path).toBe('/srv/checkout/probe');
  });

  it('prefers the checkout at the version the worker announced', () => {
    const old = { ...registered('probe', '/srv/old/probe'), version: '0.1.0' };
    const now = { ...registered('probe', '/srv/new/probe'), version: '0.2.0' };
    expect(folderForActor({ name: 'probe', version: '0.2.0' }, [old, now])?.path).toBe(
      '/srv/new/probe'
    );
  });

  it('prefers a folder that is there over a registration whose directory is gone', () => {
    // Every read inside an absent folder 400s, so opening it would answer the click with refusals.
    const gone = { ...registered('probe', '/srv/gone/probe'), absent: true };
    const here = { ...registered('probe', '/srv/here/probe'), version: '9.9.9' };
    expect(folderForActor({ name: 'probe', version: '0.1.0' }, [gone, here])?.path).toBe(
      '/srv/here/probe'
    );
  });

  it('still returns the absent one when it is the only registration', () => {
    // The card says "not on disk" — which is a different sentence from "never registered", and the
    // operator's fix differs too.
    const gone = { ...registered('probe', '/srv/gone/probe'), absent: true };
    expect(folderForActor({ name: 'probe', version: '0.1.0' }, [gone])?.absent).toBe(true);
  });
});

describe('forgetting', () => {
  it('removes the one row and keeps the rest', () => {
    const gone = registered('probe', '/src/probe');
    const list = withoutSource([gone, registered('beacon', '/src/beacon')], gone.id);
    expect(list.map((s) => s.name)).toEqual(['beacon']);
  });

  it('is a no-op for an id the list does not hold', () => {
    const list = [registered('probe', '/src/probe')];
    expect(withoutSource(list, 'actor:ghost:zz')).toHaveLength(1);
  });
});

/**
 * NO FOLDER, NO ACTOR — the join read from the folder's side.
 *
 * The Actors page drew the CATALOG, which is everything any worker ever registered about itself on
 * any machine: after one run, twenty-three cards, most saying `source unknown` and `no
 * registered folder`, none of them openable, servable or forgettable — and an empty page once those
 * workers stopped, with the folders still registered. The folder is the unit now, and a catalog row
 * with no folder is not drawn at all — so what this has to get right is which catalog entry a
 * folder IS.
 */
describe('the catalogued Actor a folder is', () => {
  it('takes the entry whose source IS this folder', () => {
    const probe = registered('probe', '/srv/checkout/probe');
    const got = catalogForFolder(probe, [
      { name: 'other', version: '9.9.9', source: '/elsewhere' },
      { name: 'probe', version: '0.1.0', source: '/srv/checkout/probe' },
    ]);
    expect(got?.source).toBe('/srv/checkout/probe');
  });

  it('falls back to the name when the worker loaded from somewhere else', () => {
    // A worker on a droplet loaded from /opt/kontra/actor/probe; the code is in a checkout here.
    // The path cannot match and the folder is still that Actor.
    const probe = registered('probe', '/srv/checkout/probe');
    const got = catalogForFolder(probe, [
      { name: 'probe', version: '0.1.0', source: '/opt/kontra/actor/probe' },
    ]);
    expect(got?.name).toBe('probe');
  });

  it('prefers the version ON DISK when the catalog holds several', () => {
    // The noise this change removes: four `cachebuster` deploys, one directory. One card, and it
    // is the version the folder actually holds.
    const folder = registered('cachebuster', '/src/cachebuster', { version: '0.3.0' });
    const got = catalogForFolder(folder, [
      { name: 'cachebuster', version: '0.1.0' },
      { name: 'cachebuster', version: '0.3.0' },
      { name: 'cachebuster', version: '0.4.0' },
    ]);
    expect(got?.version).toBe('0.3.0');
  });

  it('answers undefined for a folder nothing has served', () => {
    // Not a failure — it is the window register → edit → serve starts in, and the card says
    // `unserved` rather than claiming a deployment there is no evidence for.
    expect(catalogForFolder(registered('probe', '/src/probe'), [])).toBeUndefined();
    expect(
      catalogForFolder(registered('probe', '/src/probe'), [{ name: 'beacon', version: '0.2.0' }])
    ).toBeUndefined();
  });
});

/**
 * THE SAME RULE, APPLIED TO EVERY OTHER SURFACE THAT DRAWS THE CATALOG.
 *
 * Scratch's palette offered all twenty-three Actors as nodes to drag. A sketch built from the
 * twenty-two that are not on this disk is a drawing of a system the operator does not have, and the
 * workflow it generates dispatches to queues nobody serves — which fails as a Run that waits
 * forever, hours later, rather than as anything the canvas could have said at the time.
 */
describe('the catalog, narrowed to what is registered here', () => {
  const CATALOG = [
    { name: 'probe', version: '0.1.0', source: '/srv/checkout/probe' },
    { name: 'cachebuster', version: '0.3.0' },
    { name: 'bbscope', version: '0.1.0' },
  ];

  it('keeps only the Actors a registered folder resolves to', () => {
    const got = registeredActors([registered('probe', '/srv/checkout/probe')], CATALOG);
    expect(got.map((a) => a.name)).toEqual(['probe']);
  });

  it('drops everything when nothing is registered, rather than falling back to all of it', () => {
    // The failure mode worth pinning: an "empty means show everything" fallback would put the wall
    // back on the one installation that has no way to act on any of it.
    expect(registeredActors([], CATALOG)).toEqual([]);
  });

  it('leaves out a folder nothing has served — a node with no Methods has no ports', () => {
    // Different from the Actors GRID, which draws it as `unserved` because that card is the way in
    // to serving it. A palette node with no Methods can be placed and never connected.
    expect(registeredActors([registered('newthing', '/src/newthing')], CATALOG)).toEqual([]);
  });

  it('lists an Actor once when two checkouts resolve to the same entry', () => {
    // The palette lists Actors, not checkouts — two rows for one draggable node is the version
    // noise the grouped palette already exists to remove.
    const got = registeredActors(
      [registered('cachebuster', '/src/a', { version: '0.3.0' }), registered('cachebuster', '/src/b', { version: '0.3.0' })],
      CATALOG
    );
    expect(got).toHaveLength(1);
  });

  it('takes the version ON DISK when the catalog holds several', () => {
    const many = [
      { name: 'cachebuster', version: '0.1.0' },
      { name: 'cachebuster', version: '0.3.0' },
      { name: 'cachebuster', version: '0.6.0' },
    ];
    const got = registeredActors([registered('cachebuster', '/src/cb', { version: '0.3.0' })], many);
    expect(got.map((a) => a.version)).toEqual(['0.3.0']);
  });
});

/**
 * NO FOLDER, NO WORKFLOW — the sidecar's rule, and the Actors page's.
 *
 * The list drew both the files in `.kontra/workflows/` and the registered folders under them, which
 * on an ordinary installation is the same two workflows twice in two vocabularies. The folder is
 * the unit: it is what can be opened, served, started and forgotten. A file the operator cannot
 * re-point or forget is not a row.
 */
describe('the workflow rows', () => {
  const file = (name: string) => ({ name, description: 'Hunts lame delegations.' });

  it('is one row per folder, carrying its path', () => {
    const nscheck = registered('nscheck', '/home/me/.kontra/workflows/nscheck');
    const rows = mergeWorkflowFolders([file('nscheck')], [nscheck]);
    expect(rows).toHaveLength(1);
    expect(rows[0]?.folder.path).toBe('/home/me/.kontra/workflows/nscheck');
  });

  it('keeps a folder registered OUTSIDE the workflows directory', () => {
    // `listWorkflows()` reads one directory. A folder in a checkout is a workflow this console can
    // serve and appears in no file listing.
    const outside = registered('sweep', '/srv/checkout/workflows/sweep');
    const rows = mergeWorkflowFolders([file('nscheck')], [outside]);
    expect(rows.map((r) => r.name)).toEqual(['sweep']);
    expect(rows[0]?.file).toBeUndefined();
  });

  it('drops a file nobody registered, because there is nothing to control', () => {
    expect(mergeWorkflowFolders([file('ping')], [])).toEqual([]);
  });

  it('joins the file in for its description, matching `nscheck.py` to the `nscheck` folder', () => {
    // The folder migration left both spellings in the wild; the description is the half only the
    // file carries, and a mismatch here would lose it silently.
    const rows = mergeWorkflowFolders(
      [file('nscheck.py')],
      [registered('nscheck', '/home/me/.kontra/workflows/nscheck')]
    );
    expect(rows).toHaveLength(1);
    expect(rows[0]?.file?.description).toBe('Hunts lame delegations.');
  });
});
