/**
 * issue #4, END TO END THROUGH THE SERVER — the half a unit test cannot reach.
 *
 * `workflowControl.test.ts` proves the resolver honours the workflow root. THIS proves
 * `buildServer` wires it, and it exists because the failure it guards against has already happened
 * twice in this repository: a piece written, tested and then never called from boot. The unit suite
 * would stay green forever over a provider nobody installed, and the symptom in production is
 * exactly the bug being fixed — **a workflow that lists and will not open.**
 *
 * SO IT DRIVES THE TWO ROUTES AN OPERATOR DRIVES: list the folders, then read one back. Nothing is
 * stubbed on the path between them.
 *
 * ── THIS FILE USED TO BE ABOUT REGISTRATION, AND THAT VERB IS GONE (ADR 0049) ───────────────────
 *
 * Every case here posted to `POST /api/sources/workflow` with an absolute path to a checkout
 * "somewhere else entirely", and asserted that the folder then listed and read. That route answers
 * 410 now: a folder is a Workflow because it is IN THE WORKSPACE, so there is no "outside the
 * default root" to widen the boundary to. All four cases failed on the 410 and nothing updated them.
 *
 * The INVARIANT survives the verb, and it is the one worth keeping: whatever makes a folder
 * listable must be the same thing that makes it readable, proved through the server rather than
 * through the resolver. So the fixture drops a folder into the root instead of registering a path,
 * and every assertion below is the one it always was.
 */

import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { FastifyInstance } from 'fastify';

import { Repo } from './db/repo';
import { buildServer } from './server';

let app: FastifyInstance | undefined;
let home: string;
let root: string;
const saved = { ...process.env };

/** A workflow folder in the root — which is the whole of what registering used to be. */
function workflowFolder(name: string, body: string): string {
  const dir = join(root, name);
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, 'workflow.py'), body);
  // BOTH FILES. The MARKER (`workflow.py`) is what makes the directory a Workflow and what `serve`
  // runs; the MANIFEST (`workflow.json`) carries the name, version and entry. Discovery lists a
  // folder with only the marker — deliberately, so code dropped in the workspace is never silently
  // invisible — but the acts that need a version refuse it, so the fixture writes both.
  writeFileSync(
    join(dir, 'workflow.json'),
    JSON.stringify({ name, version: '0.1.0', entry: 'workflow.py', workflow: name })
  );
  return dir;
}

beforeEach(() => {
  home = mkdtempSync(join(tmpdir(), 'kontra-home-'));
  root = join(home, 'workflows');
  mkdirSync(root, { recursive: true });
  // `KONTRA_WORKFLOW_ROOT` still overrides the workflow root alone (`workflowControl.ts`), which is
  // what lets a test pin its own directory without moving the home the rest of the install sits in.
  process.env.KONTRA_WORKFLOW_ROOT = root;
  process.env.KONTRA_HOME = home;

  app = buildServer({ repo: new Repo(':memory:') });
});

afterEach(async () => {
  await app?.close();
  app = undefined;
  process.env = { ...saved };
  rmSync(home, { recursive: true, force: true });
});

async function read(name: string): Promise<{ code: number; body: unknown }> {
  const res = await app!.inject({ method: 'GET', url: `/api/workflows/file/${encodeURIComponent(name)}` });
  return { code: res.statusCode, body: res.json() };
}

async function listed(): Promise<string[]> {
  const res = await app!.inject({ method: 'GET', url: '/api/sources/workflow' });
  return (res.json() as { sources: Array<{ name: string }> }).sources.map((s) => s.name);
}

describe('a workflow folder in the root', () => {
  it('lists it AND reads it — the two that disagreed', async () => {
    workflowFolder('ping', '# served from the workspace\n');

    expect(await listed()).toContain('ping');

    // THE ASSERTION THE ISSUE IS ABOUT. This answered 404 `no such workflow in …/workflows` while
    // the row above sat in the console.
    const got = await read('ping');
    expect(got.code, JSON.stringify(got.body)).toBe(200);
    expect((got.body as { source: string }).source).toContain('served from the workspace');
  });

  it('reads a folder that appeared a moment ago, not only one present at boot', async () => {
    // A snapshot taken in `buildServer` would make this work for exactly as long as nobody added a
    // workflow. The server has already been built by `beforeEach`, and under ADR 0049 "adding one"
    // is a `mkdir` rather than a button — which makes a stale snapshot MORE likely to be wrong,
    // not less, because nothing tells the server anything happened.
    expect((await read('ping')).code).toBe(404);
    workflowFolder('ping', '# written after boot\n');
    const got = await read('ping');
    expect(got.code, JSON.stringify(got.body)).toBe(200);
    expect((got.body as { source: string }).source).toContain('written after boot');
  });

  it('refuses a name that is not there, with other folders present', async () => {
    // The boundary must be the ROOT and not the filesystem.
    workflowFolder('ping', '# one\n');
    expect((await read('neverwritten')).code).toBe(404);
  });

  it('refuses a traversal and an absolute path through the route', async () => {
    workflowFolder('ping', '# one\n');
    for (const name of ['../../etc/passwd', '/etc/passwd', 'ping/../../../etc/passwd']) {
      const got = await read(name);
      expect(got.code, `${name} was not refused`).toBe(404);
    }
  });

  it('reads a second folder too — so the boundary is not "whatever was first"', async () => {
    // Non-vacuous partner: a resolver that admitted exactly one name would pass every case above.
    workflowFolder('ping', '# one\n');
    workflowFolder('local', '# the other one\n');
    expect(await listed()).toEqual(expect.arrayContaining(['ping', 'local']));
    const got = await read('local');
    expect(got.code).toBe(200);
    expect((got.body as { source: string }).source).toContain('the other one');
  });
});

describe('registering a path is gone (ADR 0049)', () => {
  it('410s, and a folder outside the root stays unreadable', async () => {
    // THE OLD CAPABILITY, PINNED AS REMOVED. This file's whole point used to be that a checkout
    // "somewhere else entirely" could be registered and then read. Both halves must now fail: the
    // verb answers 410, and the folder it would have admitted is not reachable by name.
    const outside = mkdtempSync(join(tmpdir(), 'kontra-checkout-'));
    try {
      mkdirSync(join(outside, 'ping'), { recursive: true });
      writeFileSync(join(outside, 'ping', 'workflow.py'), '# a checkout elsewhere\n');

      const res = await app!.inject({
        method: 'POST',
        url: '/api/sources/workflow',
        payload: { path: join(outside, 'ping') },
      });
      expect(res.statusCode).toBe(410);
      expect((res.json() as { error: string }).error).toContain('the workspace is the registration');

      expect(await listed()).not.toContain('ping');
      expect((await read('ping')).code).toBe(404);
    } finally {
      rmSync(outside, { recursive: true, force: true });
    }
  });
});
