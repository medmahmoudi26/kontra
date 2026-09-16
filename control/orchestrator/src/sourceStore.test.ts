/**
 * What the workspace lists — and what it no longer remembers.
 *
 * This file used to be about REGISTERING: what a registration records, that it survives a restart,
 * that re-registering moves the digest. None of that exists now (`sourceStore.ts` says why), and
 * the cases worth keeping are the ones about a folder appearing and disappearing on its own,
 * because that is the whole behaviour the change buys.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { SourceStore } from './sourceStore';
import { codeRoot } from './sources';

let tmp: string;
let home: string;
let prevHome: string | undefined;
let prevWorkspace: string | undefined;
let store: SourceStore;

beforeEach(() => {
  tmp = mkdtempSync(path.join(os.tmpdir(), 'kontra-store-'));
  home = path.join(tmp, 'home');
  prevHome = process.env.KONTRA_HOME;
  prevWorkspace = process.env.KONTRA_WORKSPACE;
  process.env.KONTRA_HOME = home;
  delete process.env.KONTRA_WORKSPACE;
  store = new SourceStore();
});
afterEach(() => {
  if (prevHome === undefined) delete process.env.KONTRA_HOME;
  else process.env.KONTRA_HOME = prevHome;
  if (prevWorkspace === undefined) delete process.env.KONTRA_WORKSPACE;
  else process.env.KONTRA_WORKSPACE = prevWorkspace;
  rmSync(tmp, { recursive: true, force: true });
});

function actorAt(dir: string, name: string, description?: string): string {
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, 'actor.json'), JSON.stringify({ name, version: '0.1.0' }));
  writeFileSync(path.join(dir, 'actor.py'), '# actor\n');
  if (description !== undefined) writeFileSync(path.join(dir, 'description.md'), description);
  return dir;
}

function workflowAt(dir: string, name: string, version = '0.1.0'): string {
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, 'workflow.py'), '# workflow\n');
  writeFileSync(
    path.join(dir, 'workflow.json'),
    JSON.stringify({ name, version, entry: 'workflow.py', workflow: 'NsCheck' })
  );
  return dir;
}

describe('the workspace is the registration', () => {
  it('lists a folder because it is there, with no act in between', () => {
    actorAt(path.join(home, 'actors', 'probe'), 'probe');
    expect(store.list('actor').map((s) => s.name)).toEqual(['probe']);
  });

  it('stops listing it the moment the folder goes', () => {
    const dir = actorAt(path.join(home, 'actors', 'probe'), 'probe');
    expect(store.list('actor')).toHaveLength(1);
    rmSync(dir, { recursive: true, force: true });
    // THE CASE THIS CHANGE EXISTS FOR. A recorded path survives its directory: a live install
    // listed `crawl` for weeks after the folder was deleted, offered a Run button, and answered
    // `404 no such workflow` on the first click. A listing cannot do that.
    expect(store.list('actor')).toEqual([]);
  });

  it('keeps the two kinds apart', () => {
    actorAt(path.join(home, 'actors', 'probe'), 'probe');
    workflowAt(path.join(home, 'workflows', 'nscheck'), 'nscheck');
    expect(store.list('actor').map((s) => s.name)).toEqual(['probe']);
    expect(store.list('workflow').map((s) => s.name)).toEqual(['nscheck']);
  });

  it('reads name, version and description from the folder as it is NOW', () => {
    const dir = path.join(home, 'actors', 'probe');
    actorAt(dir, 'probe', 'First paragraph.\n\nSecond.');
    expect(store.list('actor')[0]).toMatchObject({ name: 'probe', version: '0.1.0' });
    expect(store.list('actor')[0]?.description).toContain('First paragraph.');

    writeFileSync(path.join(dir, 'actor.json'), JSON.stringify({ name: 'probe', version: '0.2.0' }));
    // No re-register, no restart: the next listing is the next read.
    expect(store.list('actor')[0]?.version).toBe('0.2.0');
  });

  it('lists a workflow folder with no manifest, because the CODE is what makes it one', () => {
    const dir = path.join(home, 'workflows', 'bare');
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, 'workflow.py'), '# no manifest beside me\n');
    // It is listed and it is incomplete — a state worth seeing and fixing (`kontra workflow init`),
    // not one to hide. Starting it is what refuses, and it names the missing file.
    expect(store.list('workflow').map((s) => s.name)).toEqual(['bare']);
  });

  it('ignores a directory that is not either kind', () => {
    mkdirSync(path.join(home, 'actors', 'notes'), { recursive: true });
    writeFileSync(path.join(home, 'actors', 'notes', 'README.md'), '# not an actor\n');
    expect(store.list('actor')).toEqual([]);
  });
});

describe('where it reads', () => {
  it('follows KONTRA_WORKSPACE when one is set, and stops reading the old home', () => {
    actorAt(path.join(home, 'actors', 'inhome'), 'inhome');
    const ws = path.join(tmp, 'workspace');
    actorAt(path.join(ws, 'actors', 'inworkspace'), 'inworkspace');
    process.env.KONTRA_WORKSPACE = ws;

    // ONE ROOT, NOT A UNION. A workspace that also showed `~/.kontra` would make "what is served
    // here" depend on what an install happened to accumulate before the workspace existed.
    expect(store.list('actor').map((s) => s.name)).toEqual(['inworkspace']);
    expect(codeRoot('actor')).toBe(path.join(ws, 'actors'));
  });

  it('falls back to ~/.kontra when nothing names a workspace', () => {
    expect(codeRoot('workflow')).toBe(path.join(home, 'workflows'));
  });
});

describe('get', () => {
  it('resolves the id a listing minted, and nothing else', () => {
    actorAt(path.join(home, 'actors', 'probe'), 'probe');
    const listed = store.list('actor')[0]!;
    expect(store.get('actor', listed.id)?.path).toBe(listed.path);
    expect(store.get('actor', 'actor:probe:whatever')).toBeUndefined();
    // The id carries the path, which is what every path-taking route resolves through.
    expect(listed.id).toBe(`at:${listed.path}`);
  });
});
