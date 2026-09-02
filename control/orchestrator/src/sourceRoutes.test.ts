import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import type { FastifyInstance } from 'fastify';
import { Repo } from './db/repo';
import { buildServer } from './server';

let app: FastifyInstance;
let tmp: string;
let prevHome: string | undefined;

beforeEach(() => {
  tmp = mkdtempSync(path.join(os.tmpdir(), 'kontra-routes-'));
  prevHome = process.env.KONTRA_HOME;
  process.env.KONTRA_HOME = path.join(tmp, 'home');
  app = buildServer({ repo: new Repo(':memory:'), webRoot: '' });
});

afterEach(async () => {
  await app.close();
  if (prevHome === undefined) delete process.env.KONTRA_HOME;
  else process.env.KONTRA_HOME = prevHome;
  rmSync(tmp, { recursive: true, force: true });
});

function actorFolder(name: string): string {
  const dir = path.join(tmp, name);
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, 'actor.json'), JSON.stringify({ name, version: '0.1.0' }));
  writeFileSync(path.join(dir, 'actor.py'), '# code\n');
  return dir;
}

const register = (kind: string, dir: string) =>
  app.inject({ method: 'POST', url: `/api/sources/${kind}`, payload: { path: dir } });

describe('GET /api/sources/:kind', () => {
  it('names the default root so the form can suggest it', async () => {
    const res = await app.inject({ method: 'GET', url: '/api/sources/actor' });
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({ kind: 'actor', sources: [] });
    expect((res.json() as { defaultRoot: string }).defaultRoot).toContain('actors');
  });

  it('404s a kind that is not one, instead of returning an empty list', async () => {
    // An empty list for `/api/sources/datasets` would read as "you have no datasets".
    const res = await app.inject({ method: 'GET', url: '/api/sources/dataset' });
    expect(res.statusCode).toBe(404);
  });
});

describe('POST /api/sources/:kind', () => {
  it('registers a folder and lists it back with its path', async () => {
    const dir = actorFolder('probe');
    expect((await register('actor', dir)).statusCode).toBe(200);
    const listed = (await app.inject({ method: 'GET', url: '/api/sources/actor' })).json() as {
      sources: Array<{ name: string; path: string }>;
    };
    expect(listed.sources).toHaveLength(1);
    expect(listed.sources[0]).toMatchObject({ name: 'probe', path: dir });
  });

  it('still lists a folder that was deleted, marked absent', async () => {
    // The page draws the row either way — the registration is the operator's to keep or forget —
    // so the flag is the only thing that stops a dangling path reading like a healthy one.
    const dir = actorFolder('probe');
    await register('actor', dir);
    rmSync(dir, { recursive: true, force: true });
    const listed = (await app.inject({ method: 'GET', url: '/api/sources/actor' })).json() as {
      sources: Array<{ path: string; absent?: boolean }>;
    };
    expect(listed.sources).toEqual([expect.objectContaining({ path: dir, absent: true })]);
  });

  it('400s a folder that is not one, and says which file was missing', async () => {
    const bare = path.join(tmp, 'empty');
    mkdirSync(bare);
    const res = await register('actor', bare);
    expect(res.statusCode).toBe(400);
    expect((res.json() as { error: string }).error).toMatch(/actor\.json/);
  });

  it('400s a path that does not exist rather than registering a dangling row', async () => {
    expect((await register('workflow', path.join(tmp, 'ghost'))).statusCode).toBe(400);
  });
});

describe('the registration IS the allowlist', () => {
  it('reads a file inside a registered folder', async () => {
    const dir = actorFolder('probe');
    const { id } = (await register('actor', dir)).json() as { id: string };
    const res = await app.inject({
      method: 'GET',
      url: `/api/sources/actor/${encodeURIComponent(id)}/file?name=actor.py`,
    });
    expect(res.statusCode).toBe(200);
    expect((res.json() as { source: string }).source).toBe('# code\n');
  });

  it('refuses to read out of the folder with ..', async () => {
    const dir = actorFolder('probe');
    writeFileSync(path.join(tmp, 'secrets.env'), 'TOKEN=1');
    const { id } = (await register('actor', dir)).json() as { id: string };
    const res = await app.inject({
      method: 'GET',
      url: `/api/sources/actor/${encodeURIComponent(id)}/file?name=${encodeURIComponent('../secrets.env')}`,
    });
    expect(res.statusCode).toBe(400);
  });

  it('404s a folder nobody registered, so an id cannot be guessed into authority', async () => {
    const res = await app.inject({
      method: 'GET',
      url: '/api/sources/actor/actor%3Aghost%3Azz/file?name=actor.py',
    });
    expect(res.statusCode).toBe(404);
  });

  /**
   * A FILE THE FOLDER DOES NOT HAVE IS A 404, on both paths that reach that fact.
   *
   * `description.md` is optional — an Actor written without one is an Actor, not a malformed
   * request — so this is the ordinary case, not an edge, and it was answered wrongly twice over:
   * `resolveInside`'s `existsSync` refused it as a plain `SourceRefused` and the route sent 400,
   * while the `readFileSync` ENOENT behind it (the file going between those two calls) fell into
   * the catch-all and sent 502 — "kontra is broken" about an Actor with no description.
   *
   * The 404 comes from `SourceMissing`, a `SourceRefused` subclass, so the route's ORDER is what
   * separates it from the 400s beside it. That is why the assertion is on the status AND on the
   * message: the folder-not-registered 404 above is a different fact with the same code.
   */
  it('404s a file the registered folder does not have, rather than 400ing or 502ing', async () => {
    const dir = actorFolder('probe');
    const { id } = (await register('actor', dir)).json() as { id: string };
    const res = await app.inject({
      method: 'GET',
      url: `/api/sources/actor/${encodeURIComponent(id)}/file?name=description.md`,
    });
    expect(res.statusCode).toBe(404);
    // The answer names the file and the folder, because "not found" about an unnamed thing is
    // indistinguishable from the 404 above, which is about the FOLDER and means something else.
    expect((res.json() as { error: string }).error).toMatch(/description\.md/);
    expect((res.json() as { error: string }).error).toContain(dir);
  });

  it('HAS NO WRITE AT ALL, which is the authority it no longer holds', async () => {
    /* `PUT /api/sources/:kind/:id/file` is gone (ADR 0033 §6, ADR 0030). Its last caller was the
       Actors page writing a generated caller into a Workflow folder, and that errand went when the
       page started CALLING the Method — an unused route is removed outright rather than left as a
       surface with no caller. It is worth an assertion rather than a deletion because of what the
       route carried: a registered folder is one `serve` executes code from, so writing into it was
       the same authority as serving, reachable over HTTP.

       404, not 405: no handler is registered for the method at all. */
    const dir = actorFolder('probe');
    const { id } = (await register('actor', dir)).json() as { id: string };
    const res = await app.inject({
      method: 'PUT',
      url: `/api/sources/actor/${encodeURIComponent(id)}/file`,
      payload: { name: 'description.md', source: '# probe\n\nHEAD each target.' },
    });
    expect(res.statusCode).toBe(404);
    expect(existsSync(path.join(dir, 'description.md'))).toBe(false);
  });

  it('picks up a description.md edited on disk, on the next listing', async () => {
    // The folder is a directory the operator owns, and their own editor is how it changes now.
    // Listing RE-READS name, version and description, which is what keeps a row honest about the
    // folder as it is rather than as it was registered.
    const dir = actorFolder('probe');
    await register('actor', dir);
    writeFileSync(path.join(dir, 'description.md'), '# probe\n\nGET each target.');
    const listed = (await app.inject({ method: 'GET', url: '/api/sources/actor' })).json() as {
      sources: Array<{ description: string }>;
    };
    expect(listed.sources[0]?.description).toBe('GET each target.');
  });
});

describe('GET /api/sources/:kind/:id/files', () => {
  it('lists what is in the folder, with the path it is listing', async () => {
    // The editor opens a FOLDER: `actor.json`, `actor.py` and `description.md` all matter, and a
    // list that named only the marker would be an editor for the least interesting of them.
    const dir = actorFolder('probe');
    writeFileSync(path.join(dir, 'description.md'), '# probe\n\nHEAD each target.\n');
    const { id } = (await register('actor', dir)).json() as { id: string };
    const res = await app.inject({
      method: 'GET',
      url: `/api/sources/actor/${encodeURIComponent(id)}/files`,
    });
    expect(res.statusCode).toBe(200);
    const body = res.json() as { path: string; files: Array<{ name: string; bytes: number }> };
    expect(body.path).toBe(dir);
    expect(body.files.map((f) => f.name)).toEqual(['actor.json', 'actor.py', 'description.md']);
    expect(body.files.find((f) => f.name === 'actor.py')?.bytes).toBe('# code\n'.length);
  });

  it('lists only names the editor could write back', async () => {
    // `filesIn` and `writeInside` share one rule, so every row is a row Save can land on. A `.env`
    // beside an actor is not editable here and must not be offered as if it were.
    const dir = actorFolder('probe');
    writeFileSync(path.join(dir, '.env'), 'TOKEN=1');
    mkdirSync(path.join(dir, 'tests'));
    const { id } = (await register('actor', dir)).json() as { id: string };
    const body = (
      await app.inject({ method: 'GET', url: `/api/sources/actor/${encodeURIComponent(id)}/files` })
    ).json() as { files: Array<{ name: string }> };
    expect(body.files.map((f) => f.name)).toEqual(['actor.json', 'actor.py']);
  });

  it('404s a folder nobody registered', async () => {
    const res = await app.inject({ method: 'GET', url: '/api/sources/actor/actor%3Aghost%3Azz/files' });
    expect(res.statusCode).toBe(404);
  });

  it('shows a file that appeared after registration, with its size', async () => {
    // The reload button: the folder changed underneath the browser — a `git pull`, an editor in a
    // terminal — and the list is where the operator sees what is there now. Nothing in this app
    // writes it (ADR 0033 §6), which is exactly why the reload matters.
    const dir = actorFolder('probe');
    const { id } = (await register('actor', dir)).json() as { id: string };
    writeFileSync(path.join(dir, 'description.md'), '# probe\n\nHEAD each target.\n');
    const body = (
      await app.inject({ method: 'GET', url: `/api/sources/actor/${encodeURIComponent(id)}/files` })
    ).json() as { files: Array<{ name: string; bytes: number }> };
    expect(body.files.find((f) => f.name === 'description.md')?.bytes).toBe(
      '# probe\n\nHEAD each target.\n'.length
    );
    const read = await app.inject({
      method: 'GET',
      url: `/api/sources/actor/${encodeURIComponent(id)}/file?name=description.md`,
    });
    expect((read.json() as { source: string }).source).toContain('HEAD each target.');
  });
});

describe('POST /api/sources/actor/:id/serve', () => {
  const prevBin = process.env.KONTRA_BIN;
  afterEach(() => {
    if (prevBin === undefined) delete process.env.KONTRA_BIN;
    else process.env.KONTRA_BIN = prevBin;
  });

  const serve = (id: string) =>
    app.inject({ method: 'POST', url: `/api/sources/actor/${encodeURIComponent(id)}/serve` });

  it('answers with the session the workbench then looks for a pane by', async () => {
    // `true` stands in for the CLI — the route's contract is what it returns, not what `kontra serve
    // --actor` does. The session is `<name>-<version>` with tmux's own rewriting applied, which is
    // the string `cli/internal/tmux/tmux.go`, `cli/fleet.go` and `panels/discovery.ts` also mint.
    process.env.KONTRA_BIN = 'true';
    const { id } = (await register('actor', actorFolder('probe'))).json() as { id: string };
    const res = await serve(id);
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({
      actor: 'probe',
      version: '0.1.0',
      session: 'probe-0_1_0',
      attach: 'tmux attach -t probe-0_1_0',
    });
  });

  it('refuses a folder that has gone away, and names it', async () => {
    // The registration outlives the directory on purpose, so the row is still here and still
    // clickable. Without the folder check this fails inside `spawn` as `ENOENT` on the cwd and is
    // reported as a missing `kontra` binary.
    process.env.KONTRA_BIN = 'true';
    const dir = actorFolder('probe');
    const { id } = (await register('actor', dir)).json() as { id: string };
    rmSync(dir, { recursive: true, force: true });
    const res = await serve(id);
    expect(res.statusCode).toBe(400);
    expect((res.json() as { error: string }).error).toContain(dir);
    expect((res.json() as { error: string }).error).toContain('is not on this machine any more');
  });

  it('404s a folder nobody registered', async () => {
    expect((await serve('actor:ghost:zz')).statusCode).toBe(404);
  });

  it('takes no placement — there is nothing in the request to choose one with', async () => {
    // THE ANTI-FEATURE, pinned at the seam. `serveActor` passes `--mode local`; a body that could
    // ask for anything else would make an edit and a fleet deployment one field apart.
    process.env.KONTRA_BIN = 'true';
    const { id } = (await register('actor', actorFolder('probe'))).json() as { id: string };
    const res = await app.inject({
      method: 'POST',
      url: `/api/sources/actor/${encodeURIComponent(id)}/serve`,
      payload: { mode: 'fleet' },
    });
    // Ignored, not honoured: the serve succeeds and the answer says nothing about a mode.
    expect(res.statusCode).toBe(200);
    expect(Object.keys(res.json() as object)).toEqual(['actor', 'version', 'path', 'session', 'attach']);
  });
});

describe('POST /api/sources/actor/:id/caller', () => {
  const caller = (id: string, payload: unknown) =>
    app.inject({
      method: 'POST',
      url: `/api/sources/actor/${encodeURIComponent(id)}/caller`,
      payload: payload as object,
    });

  it('answers with SOURCE and a filename, and starts nothing', async () => {
    /* THE SEPARATION IS IN THIS ASSERTION, and the reason under it has changed. It used to say the
       server could not dispatch at all ("`POST /api/runs` 404s on purpose" — which stopped being
       true on 2026-08-15). The page CALLS the Method now, through `…/probe` beside this route. What
       this pins is that the two are separate calls: this one returns bytes, and reading the code
       you are about to run must not require running it — otherwise every keystroke in the Batch
       form, which regenerates this, would be a Run. */
    const { id } = (await register('actor', actorFolder('probe'))).json() as { id: string };
    const res = await caller(id, { method: 'head', units: [{ url: 'https://a.test' }] });
    expect(res.statusCode).toBe(200);
    const body = res.json() as { filename: string; source: string };
    expect(body.filename).toBe('workflow.py');
    expect(body.source).toContain('results, dropped = await probe.head(batch)');
    expect(body.source).toContain('"url": "https://a.test"');
    expect(Object.keys(body)).toEqual(['filename', 'source']);
  });

  it('generates for a Method name Python cannot spell as an attribute', async () => {
    // `core.Registry.AddMethod` in the Go SDK takes any non-empty string, so `dns-facts` is a
    // Method a worker really does self-register. This route used to demand a Python identifier and
    // refused every one of them as "not a Method name" — about a name the catalog on the page
    // beside it was showing.
    const { id } = (await register('actor', actorFolder('probe'))).json() as { id: string };
    const res = await caller(id, { method: 'dns-facts', units: [] });
    expect(res.statusCode).toBe(200);
    // `probe.dns-facts` is a SyntaxError, so the callable handle is reached through `getattr` —
    // still one Method call, still the `(results, dropped)` tuple.
    expect((res.json() as { source: string }).source).toContain(
      'results, dropped = await getattr(probe, "dns-facts")(batch)'
    );
  });

  it('refuses a name that could escape the file it is written into', async () => {
    // The name lands in the generated module's docstring as prose. A quote or a newline in it is
    // the difference between a file the operator reads and a file that carries something else.
    const { id } = (await register('actor', actorFolder('probe'))).json() as { id: string };
    for (const bad of ['', 'dns facts', 'head"', 'a\nb', '../head']) {
      const res = await caller(id, { method: bad, units: [] });
      expect(res.statusCode).toBe(400);
      expect((res.json() as { error: string }).error).toContain('is not a Method name');
    }
  });

  it('treats a missing Batch as an empty one rather than failing', async () => {
    // `BATCH = []` is a legal generated file — the operator types the Units into it.
    const { id } = (await register('actor', actorFolder('probe'))).json() as { id: string };
    const res = await caller(id, { method: 'head' });
    expect(res.statusCode).toBe(200);
    expect((res.json() as { source: string }).source).toContain('BATCH = []');
  });

  it('404s a folder nobody registered', async () => {
    expect((await caller('actor:ghost:zz', { method: 'head' })).statusCode).toBe(404);
  });
});

describe('DELETE /api/sources/:kind/:id', () => {
  it('forgets a registration', async () => {
    const { id } = (await register('actor', actorFolder('probe'))).json() as { id: string };
    const res = await app.inject({
      method: 'DELETE',
      url: `/api/sources/actor/${encodeURIComponent(id)}`,
    });
    // FORGETTING TAKES THE ENDPOINT WITH IT. Under the old rule — worker boot created endpoints and
    // nothing removed one — thirty-one accumulated on this cluster, each a live route to a queue
    // nobody polls and each indistinguishable from a real Actor in the endpoint list.
    expect(res.json()).toMatchObject({ forgotten: true, endpoint: 'kontra-probe-0-1-0' });
    expect((await app.inject({ method: 'GET', url: '/api/sources/actor' })).json()).toMatchObject({
      sources: [],
    });
  });
});

describe('POST /api/sources/actor/:id/probe', () => {
  /**
   * THE ROUTE'S OWN HALF OF THE COUNT (ADR 0033 §1). `probe.test.ts` proves `probeRequest` and
   * `startProbe`; what is proved here is that the route reaches the refusal BEFORE it reaches
   * Temporal — a request naming a topology must cost nothing, and these assertions would hang
   * rather than pass if the parse happened after the endpoint lookup.
   */
  it('refuses a request naming a second Method, and names the field', async () => {
    const dir = actorFolder('probe');
    const { id } = (await register('actor', dir)).json() as { id: string };
    const res = await app.inject({
      method: 'POST',
      url: `/api/sources/actor/${encodeURIComponent(id)}/probe`,
      payload: { method: 'head', units: [], then: { method: 'tail' } },
    });
    expect(res.statusCode).toBe(400);
    const said = (res.json() as { error: string }).error;
    expect(said).toContain('"then"');
    expect(said).toContain('one Actor, one version, one Method, one Batch');
  });

  it('refuses a key, because a keyed dispatch attaches (ADR 0033 §2)', async () => {
    const dir = actorFolder('probe');
    const { id } = (await register('actor', dir)).json() as { id: string };
    const res = await app.inject({
      method: 'POST',
      url: `/api/sources/actor/${encodeURIComponent(id)}/probe`,
      payload: { method: 'head', units: [], key: 'acme.com' },
    });
    expect(res.statusCode).toBe(400);
    expect((res.json() as { error: string }).error).toContain('does not take a key');
  });

  it('refuses a Batch that is not a list', async () => {
    const dir = actorFolder('probe');
    const { id } = (await register('actor', dir)).json() as { id: string };
    const res = await app.inject({
      method: 'POST',
      url: `/api/sources/actor/${encodeURIComponent(id)}/probe`,
      payload: { method: 'head', units: { url: 'a' } },
    });
    expect(res.statusCode).toBe(400);
    expect((res.json() as { error: string }).error).toContain('a Batch is a list of Units');
  });

  it('404s a folder nobody registered, so an id cannot be probed into authority', async () => {
    const res = await app.inject({
      method: 'POST',
      url: '/api/sources/actor/actor%3Aghost%3Azz/probe',
      payload: { method: 'head', units: [] },
    });
    expect(res.statusCode).toBe(404);
  });
});
