import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  contains,
  defaultRoot,
  discover,
  expandHome,
  firstParagraph,
  folderDigest,
  inspectFolder,
  kontraHome,
  resolveInside,
  SourceRefused,
  type Source,
} from './sources';

let tmp: string;

beforeEach(() => {
  tmp = mkdtempSync(path.join(os.tmpdir(), 'kontra-sources-'));
});
afterEach(() => {
  rmSync(tmp, { recursive: true, force: true });
});

function actorAt(name: string, manifest: object, description?: string): string {
  const dir = path.join(tmp, name);
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, 'actor.json'), JSON.stringify(manifest));
  writeFileSync(path.join(dir, 'actor.py'), '# code\n');
  if (description !== undefined) writeFileSync(path.join(dir, 'description.md'), description);
  return dir;
}

function workflowAt(name: string, description?: string): string {
  const dir = path.join(tmp, name);
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, 'workflow.py'), '# caller\n');
  if (description !== undefined) writeFileSync(path.join(dir, 'description.md'), description);
  return dir;
}

describe('inspectFolder', () => {
  it('reads an actor’s name and version from its own actor.json', () => {
    const dir = actorAt('probe', { name: 'probe', version: '0.1.0' });
    expect(inspectFolder('actor', dir)).toMatchObject({
      kind: 'actor',
      name: 'probe',
      version: '0.1.0',
      path: dir,
    });
  });

  it('takes a workflow’s name from its FOLDER, which is the only thing that has one', () => {
    // The class inside is the Temporal workflow TYPE (`NsCheck`) and that is a different thing
    // from the name of the code that holds it — reading it would mean parsing the file.
    const dir = workflowAt('nscheck');
    expect(inspectFolder('workflow', dir)).toMatchObject({ kind: 'workflow', name: 'nscheck', version: '' });
  });

  it('refuses a folder with no marker, naming the file it looked for', () => {
    const bare = path.join(tmp, 'empty');
    mkdirSync(bare);
    expect(() => inspectFolder('actor', bare)).toThrow(/actor\.json/);
    expect(() => inspectFolder('workflow', bare)).toThrow(/workflow\.py/);
  });

  it('refuses a path that does not exist, and a file where a folder was meant', () => {
    expect(() => inspectFolder('actor', path.join(tmp, 'nope'))).toThrow(SourceRefused);
    const file = path.join(tmp, 'a.py');
    writeFileSync(file, '');
    expect(() => inspectFolder('workflow', file)).toThrow(/register the folder that contains it/);
  });

  it('refuses a malformed actor.json by name instead of registering a nameless actor', () => {
    const dir = path.join(tmp, 'broken');
    mkdirSync(dir);
    writeFileSync(path.join(dir, 'actor.json'), '{not json');
    expect(() => inspectFolder('actor', dir)).toThrow(/not valid JSON/);
  });

  it('falls back to the folder name when actor.json declares none', () => {
    const dir = actorAt('leftover', { version: '2.0.0' });
    expect(inspectFolder('actor', dir)).toMatchObject({ name: 'leftover', version: '2.0.0' });
  });

  it('stores the REAL path, so a symlinked registration cannot serve something else later', () => {
    const real = actorAt('real', { name: 'real', version: '0.1.0' });
    const link = path.join(tmp, 'link');
    symlinkSync(real, link);
    expect(inspectFolder('actor', link).path).toBe(real);
  });

  it('carries description.md through, and absent is empty rather than a placeholder', () => {
    const described = actorAt('a', { name: 'a', version: '1' }, '# a\n\nGET each target.\n\nMore.');
    expect(inspectFolder('actor', described).description).toBe('GET each target.');
    expect(inspectFolder('actor', actorAt('b', { name: 'b', version: '1' })).description).toBe('');
  });
});

describe('firstParagraph', () => {
  it('skips the title, because a description.md opens with the name as a heading', () => {
    expect(firstParagraph('# probe\n\nHEAD each target.')).toBe('HEAD each target.');
  });

  it('joins a wrapped paragraph into one line and stops at the blank line', () => {
    expect(firstParagraph('one\ntwo\n\nthree')).toBe('one two');
  });

  it('is empty for an empty file and for a file that is only headings', () => {
    expect(firstParagraph('')).toBe('');
    expect(firstParagraph('# a\n\n## b\n')).toBe('');
  });
});

describe('the allowlist', () => {
  const src = (dir: string): Source => ({
    id: 'x',
    kind: 'workflow',
    name: 'w',
    path: dir,
    version: '',
    description: '',
    registeredAt: 0,
  });

  it('resolves a file inside the registered folder', () => {
    const dir = workflowAt('w');
    expect(resolveInside(src(dir), 'workflow.py')).toBe(path.join(dir, 'workflow.py'));
  });

  it('refuses to climb out with ..', () => {
    const dir = workflowAt('w');
    writeFileSync(path.join(tmp, 'secrets.py'), '');
    expect(() => resolveInside(src(dir), '../secrets.py')).toThrow(/refusing to read it/);
  });

  it('refuses an absolute path outright', () => {
    expect(() => resolveInside(src(workflowAt('w')), '/etc/passwd')).toThrow(SourceRefused);
  });

  it('refuses a symlink that points out, which the .. check alone would pass', () => {
    // A name containing no `..` can still be a symlink to anywhere. The comparison that matters
    // is between REAL paths, which is why resolveInside resolves before it compares.
    const dir = workflowAt('w');
    const secret = path.join(tmp, 'secret.py');
    writeFileSync(secret, 'x');
    symlinkSync(secret, path.join(dir, 'innocent.py'));
    expect(() => resolveInside(src(dir), 'innocent.py')).toThrow(/refusing to read it/);
  });

  it('refuses a file that is not there rather than reporting it as empty', () => {
    expect(() => resolveInside(src(workflowAt('w')), 'gone.py')).toThrow(/no such file/);
  });
});

describe('contains', () => {
  it('is true for the root itself and for anything under it', () => {
    expect(contains('/srv/kontra', '/srv/kontra')).toBe(true);
    expect(contains('/srv/kontra', '/srv/kontra/a/b')).toBe(true);
  });

  it('is false for a sibling whose name merely starts the same', () => {
    // The reason this is path.relative and not startsWith.
    expect(contains('/srv/kontra', '/srv/kontra-evil')).toBe(false);
    expect(contains('/srv/kontra', '/srv')).toBe(false);
  });
});

describe('discover', () => {
  it('finds every folder under a root that has the marker, sorted', () => {
    actorAt('zulu', { name: 'zulu', version: '1' });
    actorAt('alpha', { name: 'alpha', version: '1' });
    expect(discover('actor', tmp).map((s) => s.name)).toEqual(['alpha', 'zulu']);
  });

  it('skips what is not one, instead of failing the whole listing', () => {
    // __pycache__, a stray file, a folder mid-write. One bad entry must not empty the page.
    actorAt('good', { name: 'good', version: '1' });
    mkdirSync(path.join(tmp, '__pycache__'));
    writeFileSync(path.join(tmp, 'notes.txt'), '');
    expect(discover('actor', tmp).map((s) => s.name)).toEqual(['good']);
  });

  it('is empty for a root that does not exist', () => {
    // An installation that never ran `kontra init` is legitimate; the page says "nothing
    // registered yet" rather than showing an error about a directory nobody asked for.
    expect(discover('workflow', path.join(tmp, 'never'))).toEqual([]);
  });
});

describe('paths an operator types', () => {
  it('expands ~ the way the form suggests it', () => {
    expect(expandHome('~')).toBe(os.homedir());
    expect(expandHome('~/kontra')).toBe(path.join(os.homedir(), 'kontra'));
    expect(expandHome('/abs')).toBe('/abs');
  });

  it('defaults each kind under KONTRA_HOME', () => {
    const prev = process.env.KONTRA_HOME;
    process.env.KONTRA_HOME = '/tmp/kh';
    expect(defaultRoot('actor')).toBe('/tmp/kh/actors');
    expect(defaultRoot('workflow')).toBe('/tmp/kh/workflows');
    if (prev === undefined) delete process.env.KONTRA_HOME;
    else process.env.KONTRA_HOME = prev;
  });
});

describe('kontraHome — one resolver, one default', () => {
  // THE COLLISION THIS PINS. There were two `kontraHome()`s reading KONTRA_HOME: this one answering
  // `~/.kontra`, and `workflowControl.ts`'s answering `<cwd>/.kontra`. Unset — every test, and any
  // run started from another directory — registration recorded a path under one home and `serve`
  // looked under the other, so registration succeeded and serving the folder it recorded refused.
  const prev = process.env.KONTRA_HOME;
  afterEach(() => {
    if (prev === undefined) delete process.env.KONTRA_HOME;
    else process.env.KONTRA_HOME = prev;
  });

  it('defaults to ~/.kontra — the home an operator was told about, not the process’s cwd', () => {
    delete process.env.KONTRA_HOME;
    expect(kontraHome()).toBe(path.join(os.homedir(), '.kontra'));
    expect(defaultRoot('workflow')).toBe(path.join(os.homedir(), '.kontra', 'workflows'));
  });

  it('is still overridden by KONTRA_HOME, which is the only reason it works in a container', () => {
    // The container's cwd is `/app` and `/app/.kontra` does not exist; compose points this at the
    // mounted `.kontra/`. That override is what the running control plane resolves through, before
    // and after the default changed.
    process.env.KONTRA_HOME = '/srv/checkout/.kontra';
    expect(kontraHome()).toBe('/srv/checkout/.kontra');
    expect(defaultRoot('actor')).toBe('/srv/checkout/.kontra/actors');
  });
});

/**
 * WHAT "THIS CODE" MEANS WHEN THE CODE IS A DIRECTORY.
 *
 * A registration records a path, and a path is a moving target: a pull, an edit in the workbench, a
 * colleague's rebase, and the folder is different code under a name that says it is the same. An
 * Actor's IMAGE already had this (ADR 0011's content-pinned digest, which is what makes a rebuild
 * detectable); the folder did not, so "this workflow" meant "whatever is in that directory now".
 *
 * The three properties below are each a way this could be worse than useless — a digest that moves
 * on its own reports drift constantly and means nothing by the second time; one that misses a
 * rename says two different programs are the same; one that differs between machines holding
 * identical code makes every comparison a false alarm.
 */
describe('folderDigest', () => {
  function folder(files: Record<string, string>): string {
    const dir = mkdtempSync(path.join(tmp, 'dg-'));
    for (const [name, body] of Object.entries(files)) {
      const full = path.join(dir, name);
      mkdirSync(path.dirname(full), { recursive: true });
      writeFileSync(full, body);
    }
    return dir;
  }

  it('is a sha256, spelled the way ADR 0011 spells a digest', () => {
    expect(folderDigest(folder({ 'actor.py': 'x' }))).toMatch(/^sha256:[0-9a-f]{64}$/);
  });

  it('matches the CROSS-LANGUAGE fixture byte-for-byte (cli/workflow_test.go)', () => {
    // The workflow queue is `wf-<name>-<digest12>`, derived on BOTH sides — the CLI serves the
    // worker on this string and the orchestrator reports it — so the two digests MUST agree or a
    // run is dispatched to a queue nobody serves. This exact fixture (a nested file, to exercise the
    // per-directory sort and the separators) is pinned identically in the Go suite.
    const dir = folder({
      'sub/inner.py': 'x = 1\n',
      'workflow.json': '{"name":"canary","version":"0.2.0","workflow":"Canary"}',
      'workflow.py': '# caller\n',
    });
    expect(folderDigest(dir)).toBe(
      'sha256:0f89cbf60a37772e8eba8995e5d37b432b0ea7724d55fdbbf0acbb8eb3aae424'
    );
  });

  it('is the same for two folders holding the same files', () => {
    // The comparison has to survive being made on another machine, in another checkout, at another
    // path — the digest is a property of the CONTENTS, not of where they are.
    const a = folder({ 'actor.py': 'print(1)\n', 'actor.json': '{"name":"probe"}' });
    const b = folder({ 'actor.json': '{"name":"probe"}', 'actor.py': 'print(1)\n' });
    expect(folderDigest(a)).toBe(folderDigest(b));
  });

  it('moves when a byte changes', () => {
    const before = folderDigest(folder({ 'actor.py': 'print(1)\n' }));
    expect(before).not.toBe(folderDigest(folder({ 'actor.py': 'print(2)\n' })));
  });

  it('moves when a file is RENAMED, contents unchanged', () => {
    // Hashing contents alone would miss this, and renaming `workflow.py` to `main.py` is a change
    // that breaks `serve` while leaving every byte in place.
    const a = folderDigest(folder({ 'workflow.py': 'same\n' }));
    const b = folderDigest(folder({ 'main.py': 'same\n' }));
    expect(a).not.toBe(b);
  });

  it('moves when a file is added or removed', () => {
    const one = folderDigest(folder({ 'a.py': 'x' }));
    const two = folderDigest(folder({ 'a.py': 'x', 'b.py': 'y' }));
    expect(one).not.toBe(two);
  });

  it('reaches into subdirectories', () => {
    const a = folderDigest(folder({ 'a.py': 'x', 'lib/util.py': 'one' }));
    const b = folderDigest(folder({ 'a.py': 'x', 'lib/util.py': 'two' }));
    expect(a).not.toBe(b);
  });

  it('IGNORES __pycache__, which serving the folder creates', () => {
    // THE ONE THAT FORCES THE EXCLUSION LIST TO EXIST. Python writes bytecode into whatever
    // directory it imports from, so merely running a workflow changes its folder — and a digest
    // that moved every time you ran something would report drift on every run and mean nothing by
    // the second one.
    const clean = folder({ 'workflow.py': 'x' });
    const before = folderDigest(clean);
    mkdirSync(path.join(clean, '__pycache__'));
    writeFileSync(path.join(clean, '__pycache__', 'workflow.cpython-311.pyc'), 'bytecode');
    expect(folderDigest(clean)).toBe(before);
  });

  it('ignores .git and node_modules for the same reason at a different scale', () => {
    const dir = folder({ 'a.py': 'x' });
    const before = folderDigest(dir);
    mkdirSync(path.join(dir, '.git'));
    writeFileSync(path.join(dir, '.git', 'HEAD'), 'ref: refs/heads/main');
    mkdirSync(path.join(dir, 'node_modules'));
    writeFileSync(path.join(dir, 'node_modules', 'x.js'), 'module');
    expect(folderDigest(dir)).toBe(before);
  });

  it('answers for a folder that is not there rather than throwing', () => {
    // Listing must not fail because a registration's directory went away — the row says `absent`,
    // which is a different and more useful thing than an exception from a hash function.
    expect(folderDigest(path.join(tmp, 'nope'))).toMatch(/^sha256:[0-9a-f]{64}$/);
  });
});
