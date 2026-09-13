/**
 * issue #4, END TO END THROUGH THE SERVER — the half a unit test cannot reach.
 *
 * `workflowControl.test.ts` proves the resolver honours a registered folder when something installs
 * the provider. THIS proves `buildServer` is that something, and it exists because the failure it
 * guards against has already happened twice in this repository: a piece written, tested and then
 * never called from boot. The unit suite would stay green forever over a `setRegisteredFolders`
 * nobody invoked, and the symptom in production is exactly the bug being fixed — a workflow that
 * lists and will not open.
 *
 * SO IT DRIVES THE TWO ROUTES AN OPERATOR DRIVES: register the folder, then read it back. Nothing
 * is stubbed on the path between them.
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
let elsewhere: string;
const saved = { ...process.env };

beforeEach(() => {
  // The DEFAULT root, empty — so anything that resolves does so because it was registered.
  home = mkdtempSync(join(tmpdir(), 'kontra-home-'));
  mkdirSync(join(home, 'workflows'), { recursive: true });
  process.env.KONTRA_WORKFLOW_ROOT = join(home, 'workflows');

  // A checkout somewhere else entirely — `~/kontra-workflows/python/ping` in the report.
  elsewhere = mkdtempSync(join(tmpdir(), 'kontra-checkout-'));
  mkdirSync(join(elsewhere, 'ping'), { recursive: true });
  writeFileSync(join(elsewhere, 'ping', 'workflow.py'), '# served from a checkout\n');
  // BOTH FILES, because registration reads a DECLARATION and the resolver reads the MARKER, and
  // they are not the same file: `workflow.json` carries the name, version and entry that a
  // registration records, `workflow.py` is what gets run. A fixture with only the second is
  // refused at registration — correctly — and would have made every case below fail for a reason
  // that has nothing to do with path resolution.
  writeFileSync(
    join(elsewhere, 'ping', 'workflow.json'),
    JSON.stringify({ name: 'ping', version: '0.1.0', entry: 'workflow.py', workflow: 'Ping' })
  );

  app = buildServer({ repo: new Repo(':memory:') });
});

afterEach(async () => {
  await app?.close();
  app = undefined;
  process.env = { ...saved };
  rmSync(home, { recursive: true, force: true });
  rmSync(elsewhere, { recursive: true, force: true });
});

async function register(path: string): Promise<number> {
  const res = await app!.inject({
    method: 'POST',
    url: '/api/sources/workflow',
    payload: { path },
  });
  return res.statusCode;
}

async function read(name: string): Promise<{ code: number; body: unknown }> {
  const res = await app!.inject({ method: 'GET', url: `/api/workflows/file/${encodeURIComponent(name)}` });
  return { code: res.statusCode, body: res.json() };
}

describe('registering a workflow folder outside the default root', () => {
  it('lists it AND reads it — the two that disagreed', async () => {
    expect(await register(join(elsewhere, 'ping'))).toBe(200);

    const listed = await app!.inject({ method: 'GET', url: '/api/sources/workflow' });
    const sources = (listed.json() as { sources: Array<{ name: string }> }).sources;
    expect(sources.map((s) => s.name)).toContain('ping');

    // THE ASSERTION THE ISSUE IS ABOUT. This answered 404 `no such workflow in …/workflows` while
    // the row above sat in the console.
    const got = await read('ping');
    expect(got.code, JSON.stringify(got.body)).toBe(200);
    expect((got.body as { source: string }).source).toContain('served from a checkout');
  });

  it('reads a folder registered a moment ago, not only one present at boot', async () => {
    // A snapshot taken in `buildServer` would make the register button work for exactly as long as
    // nobody pressed it. The server has already been built by `beforeEach`.
    expect((await read('ping')).code).toBe(404);
    expect(await register(join(elsewhere, 'ping'))).toBe(200);
    expect((await read('ping')).code).toBe(200);
  });

  it('refuses a name nobody registered, with other registrations present', async () => {
    // The widening must be to the registered set and not to the filesystem.
    expect(await register(join(elsewhere, 'ping'))).toBe(200);
    const got = await read('neverregistered');
    expect(got.code).toBe(404);
  });

  it('refuses a traversal and an absolute path through the route', async () => {
    expect(await register(join(elsewhere, 'ping'))).toBe(200);
    for (const name of ['../../etc/passwd', '/etc/passwd', 'ping/../../../etc/passwd']) {
      const got = await read(name);
      expect(got.code, `${name} was not refused`).toBe(404);
    }
  });

  it('still reads a workflow in the default root', async () => {
    // Non-vacuous partner: a boundary that admitted everything would pass all four cases above.
    mkdirSync(join(home, 'workflows', 'local'), { recursive: true });
    writeFileSync(join(home, 'workflows', 'local', 'workflow.py'), '# the ordinary place\n');
    writeFileSync(
      join(home, 'workflows', 'local', 'workflow.json'),
      JSON.stringify({ name: 'local', version: '0.1.0', entry: 'workflow.py', workflow: 'Local' })
    );
    const got = await read('local');
    expect(got.code).toBe(200);
    expect((got.body as { source: string }).source).toContain('the ordinary place');
  });
});
