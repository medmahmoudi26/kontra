import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Repo } from './db/repo';
import { SourceStore } from './sourceStore';
import { SourceRefused } from './sources';

let tmp: string;
let home: string;
let prevHome: string | undefined;
let store: SourceStore;

beforeEach(() => {
  tmp = mkdtempSync(path.join(os.tmpdir(), 'kontra-store-'));
  home = path.join(tmp, 'home');
  prevHome = process.env.KONTRA_HOME;
  process.env.KONTRA_HOME = home;
  store = new SourceStore(new Repo(':memory:'));
});
afterEach(() => {
  if (prevHome === undefined) delete process.env.KONTRA_HOME;
  else process.env.KONTRA_HOME = prevHome;
  rmSync(tmp, { recursive: true, force: true });
});

function actorAt(dir: string, name: string, description?: string): string {
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, 'actor.json'), JSON.stringify({ name, version: '0.1.0' }));
  writeFileSync(path.join(dir, 'actor.py'), '# actor\n');
  if (description !== undefined) writeFileSync(path.join(dir, 'description.md'), description);
  return dir;
}

/** A registrable workflow folder: the CODE (`workflow.py`, the marker) and the DECLARATION
 *  (`workflow.json`, what registering reads for a version). Both, because registering wants both. */
function workflowAt(dir: string, name: string, version = '0.1.0'): string {
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, 'workflow.py'), '# workflow\n');
  writeFileSync(
    path.join(dir, 'workflow.json'),
    JSON.stringify({ name, version, entry: 'workflow.py', workflow: 'NsCheck' })
  );
  return dir;
}

describe('registering a folder', () => {
  it('remembers the path, which is the whole point', () => {
    const dir = actorAt(path.join(tmp, 'checkout', 'probe'), 'probe');
    const registered = store.register('actor', dir);
    expect(registered.path).toBe(dir);
    expect(store.list('actor').map((s) => s.path)).toEqual([dir]);
  });

  it('survives a new store over the same database', () => {
    const repo = new Repo(path.join(tmp, 'db.sqlite'));
    const dir = actorAt(path.join(tmp, 'checkout', 'probe'), 'probe');
    new SourceStore(repo).register('actor', dir);
    expect(new SourceStore(repo).list('actor').map((s) => s.path)).toEqual([dir]);
  });

  it('is idempotent: registering twice does not double the list', () => {
    const dir = actorAt(path.join(tmp, 'probe'), 'probe');
    const first = store.register('actor', dir);
    const second = store.register('actor', dir);
    expect(second.id).toBe(first.id);
    expect(store.list('actor')).toHaveLength(1);
  });

  it('refuses a folder that is not one, and stores nothing', () => {
    const bare = path.join(tmp, 'empty');
    mkdirSync(bare);
    expect(() => store.register('actor', bare)).toThrow(SourceRefused);
    expect(store.list('actor')).toEqual([]);
  });

  it('keeps the two kinds apart', () => {
    const a = actorAt(path.join(tmp, 'probe'), 'probe');
    const w = workflowAt(path.join(tmp, 'nscheck'), 'nscheck');
    store.register('actor', a);
    store.register('workflow', w);
    expect(store.list('actor').map((s) => s.name)).toEqual(['probe']);
    expect(store.list('workflow').map((s) => s.name)).toEqual(['nscheck']);
  });
});

/**
 * REGISTERING IS ITS OWN ACT, and what it records is more than a path.
 *
 * The whole point of separating it from serving: a caller can be written against code nobody has
 * started, because registering has already said what that code is, what version it is, and what it
 * held at the time. These are the three facts a directory listing cannot supply, so they are the
 * three this has to get right.
 */
describe('what registering records', () => {
  it('takes a content digest, so “is this the code I registered” has an answer', () => {
    const dir = actorAt(path.join(tmp, 'probe'), 'probe');
    const got = store.register('actor', dir);
    expect(got.digest).toMatch(/^sha256:[0-9a-f]{64}$/);
  });

  it('moves the digest when the code changes and re-registering says so', () => {
    // A registration records a PATH, and a path is a moving target — a pull, an edit in the
    // workbench, a colleague's rebase. Re-registering is how an operator says "this is the version
    // I mean now", and it is the only thing that moves the claim forward.
    const dir = actorAt(path.join(tmp, 'probe'), 'probe');
    const first = store.register('actor', dir).digest;
    writeFileSync(path.join(dir, 'actor.py'), 'print("changed")\n');
    const second = store.register('actor', dir).digest;
    expect(second).not.toBe(first);
  });

  it('keeps the id and the registration time across a re-register', () => {
    // The id is in URLs and in whatever the operator bookmarked; minting a new one to record a fact
    // about the CONTENTS would break every one of those.
    const dir = actorAt(path.join(tmp, 'probe'), 'probe');
    const first = store.register('actor', dir);
    writeFileSync(path.join(dir, 'actor.py'), 'print("changed")\n');
    const second = store.register('actor', dir);
    expect(second.id).toBe(first.id);
    expect(second.registeredAt).toBe(first.registeredAt);
    expect(store.list('actor')).toHaveLength(1);
  });

  it('stores the manifest verbatim, including fields this code has never heard of', () => {
    // `actor.json` is the SDKs' file. Registration is not the place that gets to decide what may be
    // in it, and a caller reading a schema an author declared must find it here unchanged.
    const dir = path.join(tmp, 'probe');
    mkdirSync(dir, { recursive: true });
    writeFileSync(
      path.join(dir, 'actor.json'),
      JSON.stringify({ name: 'probe', version: '0.1.0', engine: 'py', somethingNew: [1, 2] })
    );
    const got = store.register('actor', dir);
    expect(got.manifest).toMatchObject({ engine: 'py', somethingNew: [1, 2] });
  });

  it('refuses a workflow folder with no workflow.json, and names what to write', () => {
    // The one refusal every existing workflow on every installation meets exactly once. A message
    // that said "add workflow.json" and stopped would move the problem rather than solve it.
    const w = path.join(tmp, 'nscheck');
    mkdirSync(w, { recursive: true });
    writeFileSync(path.join(w, 'workflow.py'), '');
    expect(() => store.register('workflow', w)).toThrow(/has no workflow\.json/);
    expect(() => store.register('workflow', w)).toThrow(/"workflow": "<the @workflow\.defn class>"/);
    expect(store.list('workflow')).toEqual([]);
  });

  it('gives a workflow a VERSION, which it never had before', () => {
    const w = workflowAt(path.join(tmp, 'nscheck'), 'nscheck', '0.4.0');
    expect(store.register('workflow', w).version).toBe('0.4.0');
  });

  it('still LISTS a folder whose manifest is missing — refusing is registration’s job alone', () => {
    // The trap this avoids: `discover` and `list` also read folders, and refusing there would make
    // every existing workflow vanish from the page that holds the button to fix it.
    const w = workflowAt(path.join(tmp, 'nscheck'), 'nscheck');
    store.register('workflow', w);
    rmSync(path.join(w, 'workflow.json'));
    const listed = store.list('workflow');
    expect(listed.map((s) => s.name)).toEqual(['nscheck']);
    expect(listed[0]?.absent).toBeUndefined();
  });

  it('keeps the digest recorded at registration when listing, rather than re-reading it', () => {
    // The asymmetry that makes drift visible: name/version/description are re-read so the row is
    // honest about the folder as it IS; the digest is kept so the row can also say what it WAS.
    // Re-reading it would also hash every file on every page load.
    const dir = actorAt(path.join(tmp, 'probe'), 'probe');
    const registered = store.register('actor', dir);
    writeFileSync(path.join(dir, 'actor.py'), 'print("changed")\n');
    expect(store.list('actor')[0]?.digest).toBe(registered.digest);
  });
});

describe('the default root', () => {
  it('lists what is in ~/.kontra without anybody registering it', () => {
    // The zero-config path: drop a folder in the conventional place and it is there. This is the
    // nuclei feel — a directory of things, not a registry you push to.
    actorAt(path.join(home, 'actors', 'probe'), 'probe');
    expect(store.list('actor').map((s) => s.name)).toEqual(['probe']);
  });

  it('does not write a discovered folder down: deleting it removes it', () => {
    const dir = actorAt(path.join(home, 'actors', 'probe'), 'probe');
    expect(store.list('actor')).toHaveLength(1);
    rmSync(dir, { recursive: true, force: true });
    expect(store.list('actor')).toEqual([]);
  });

  it('refuses to forget a discovered folder, because it was never registered', () => {
    actorAt(path.join(home, 'actors', 'probe'), 'probe');
    const [found] = store.list('actor');
    expect(() => store.forget(found!.id)).toThrow(/not registered/);
  });

  it('does not list the same folder twice when it is both registered and discovered', () => {
    const dir = actorAt(path.join(home, 'actors', 'probe'), 'probe');
    store.register('actor', dir);
    expect(store.list('actor')).toHaveLength(1);
    // and it is the REGISTERED row, which carries a real id and a registration time
    expect(store.list('actor')[0]!.id.startsWith('at:')).toBe(false);
  });
});

describe('a registered folder that moved', () => {
  it('still lists, so the operator can see WHICH registration is broken', () => {
    // Dropping the row would make a `git checkout` that renames a directory look like a
    // registration the operator never made.
    const dir = actorAt(path.join(tmp, 'probe'), 'probe');
    store.register('actor', dir);
    rmSync(dir, { recursive: true, force: true });
    const [row] = store.list('actor');
    expect(row?.name).toBe('probe');
    expect(row?.path).toBe(dir);
  });

  it('says it is absent, so the row does not read like a healthy one', () => {
    // The row carries the name and path recorded at registration, so without this flag a deleted
    // folder listed exactly like a present one and the first thing to fail was `serve`.
    const dir = actorAt(path.join(tmp, 'probe'), 'probe');
    store.register('actor', dir);
    expect(store.list('actor')[0]!.absent).toBeUndefined();
    rmSync(dir, { recursive: true, force: true });
    expect(store.list('actor')[0]!.absent).toBe(true);
  });

  it('drops the flag again when the folder comes back', () => {
    const dir = actorAt(path.join(tmp, 'probe'), 'probe');
    store.register('actor', dir);
    rmSync(dir, { recursive: true, force: true });
    expect(store.list('actor')[0]!.absent).toBe(true);
    actorAt(dir, 'probe');
    expect(store.list('actor')[0]!.absent).toBeUndefined();
  });

  it('can be forgotten once it is gone', () => {
    const dir = actorAt(path.join(tmp, 'probe'), 'probe');
    const { id } = store.register('actor', dir);
    rmSync(dir, { recursive: true, force: true });
    expect(store.forget(id)).toBe(true);
    expect(store.list('actor')).toEqual([]);
  });
});

describe('what is read from disk every time', () => {
  it('picks up an edited description.md without re-registering', () => {
    // description/name/version are re-read on every listing rather than frozen at registration:
    // editing the file IS the update, which is the only behaviour that matches "it is just files".
    const dir = actorAt(path.join(tmp, 'probe'), 'probe', '# probe\n\nfirst.');
    store.register('actor', dir);
    expect(store.list('actor')[0]!.description).toBe('first.');
    writeFileSync(path.join(dir, 'description.md'), '# probe\n\nsecond.');
    expect(store.list('actor')[0]!.description).toBe('second.');
  });

  it('follows a rename inside actor.json', () => {
    const dir = actorAt(path.join(tmp, 'probe'), 'probe');
    store.register('actor', dir);
    writeFileSync(path.join(dir, 'actor.json'), JSON.stringify({ name: 'prober', version: '0.2.0' }));
    expect(store.list('actor')[0]).toMatchObject({ name: 'prober', version: '0.2.0' });
  });
});

describe('get', () => {
  it('addresses a registered and a discovered folder the same way', () => {
    const registered = store.register('actor', actorAt(path.join(tmp, 'probe'), 'probe'));
    actorAt(path.join(home, 'actors', 'beacon'), 'beacon');
    const discovered = store.list('actor').find((s) => s.name === 'beacon');
    expect(store.get('actor', registered.id)?.name).toBe('probe');
    expect(store.get('actor', discovered!.id)?.name).toBe('beacon');
  });

  it('is undefined for an id nothing holds', () => {
    expect(store.get('actor', 'actor:ghost:zzz')).toBeUndefined();
  });
});
