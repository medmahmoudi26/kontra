import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import type { FastifyInstance } from 'fastify';
import { Repo } from './db/repo';
import { codeRoot } from './sources';
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

/**
 * An Actor, made the only way there is: a folder in the WORKSPACE that looks like one (ADR 0049).
 *
 * THIS FILE USED TO `register()` A PATH, and every test in it broke when that verb went — 22 of
 * them, all failing on a 410 from a route that now exists only to say the verb is gone. They were
 * not testing anything wrong; they were testing the previous design, and nothing updated them.
 *
 * `KONTRA_HOME` is set per test above, so `codeRoot('actor')` is `<tmp>/home/actors` and dropping a
 * directory there IS the registration. Nothing records a path, so there is nothing to clean up and
 * no id to read out of a response.
 */
function actorFolder(name: string): string {
  const dir = path.join(codeRoot('actor'), name);
  mkdirSync(dir, { recursive: true });
  writeFileSync(path.join(dir, 'actor.json'), JSON.stringify({ name, version: '0.1.0' }));
  writeFileSync(path.join(dir, 'actor.py'), '# code\n');
  return dir;
}

/**
 * The id of a discovered folder, which is `at:<absolute path>` (`sourceStore.ts`).
 *
 * DERIVED RATHER THAN RETURNED, because discovery has no response to return it in. It is asserted
 * against the listing once, below, so a change to the scheme fails loudly here instead of making
 * every id in this file a string that addresses nothing.
 */
const idOf = (dir: string) => `at:${dir}`;

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

describe('POST /api/sources/:kind — registering is GONE (ADR 0049)', () => {
  /**
   * WHAT REPLACED THE VERB, pinned as the contract it now has.
   *
   * A recorded path outlives the directory it names, so an install accumulated rows for code that
   * no longer existed — `crawl`, listed and clickable, pointing at a folder deleted weeks earlier.
   * Every surface carried an `absent` state for it, `serve` refused it with a 404, and the operator
   * had to `forget` it by hand. The listing IS the filesystem now, so none of that is reachable.
   */
  it('410s a register, and says what to do instead', async () => {
    // 410 AND NOT 404: a script that still posts here is not making a typo, it is doing something
    // that USED to work, and the difference is an hour of somebody's afternoon.
    const res = await app.inject({
      method: 'POST',
      url: '/api/sources/actor',
      payload: { path: path.join(tmp, 'anywhere') },
    });
    expect(res.statusCode).toBe(410);
    const said = (res.json() as { error: string }).error;
    expect(said).toContain('the workspace is the registration');
    expect(said).toContain('Put the folder in the workspace.');
  });

  it('lists a folder dropped into the workspace, with its path — no call required', async () => {
    const dir = actorFolder('probe');
    const listed = (await app.inject({ method: 'GET', url: '/api/sources/actor' })).json() as {
      sources: Array<{ id: string; name: string; path: string }>;
    };
    expect(listed.sources).toHaveLength(1);
    expect(listed.sources[0]).toMatchObject({ name: 'probe', path: dir });
    // THE ID SCHEME, asserted once. Every other test in this file derives ids with `idOf`, so a
    // change here fails loudly instead of leaving them all addressing nothing.
    expect(listed.sources[0]?.id).toBe(idOf(dir));
  });

  it('stops listing a folder that was deleted — there is nothing left to forget', async () => {
    // THE HEADLINE OF ADR 0049. This test used to assert the opposite: that a deleted folder stayed
    // listed with `absent: true`, because the REGISTRATION outlived it and was the operator's to
    // keep. With the directory as the record there is no row to keep.
    const dir = actorFolder('probe');
    rmSync(dir, { recursive: true, force: true });
    const listed = (await app.inject({ method: 'GET', url: '/api/sources/actor' })).json() as {
      sources: unknown[];
    };
    expect(listed.sources).toEqual([]);
  });

  it('does not list a directory that is not an Actor', async () => {
    // No `actor.json`, so it is not one. It used to be a 400 from `register` naming the missing
    // file; discovery has nobody to answer, so the folder is simply not an Actor.
    mkdirSync(path.join(codeRoot('actor'), 'empty'), { recursive: true });
    const listed = (await app.inject({ method: 'GET', url: '/api/sources/actor' })).json() as {
      sources: unknown[];
    };
    expect(listed.sources).toEqual([]);
  });
});

describe('the registration IS the allowlist', () => {
  it('reads a file inside a registered folder', async () => {
    const dir = actorFolder('probe');
    const id = idOf(dir);
    const res = await app.inject({
      method: 'GET',
      url: `/api/sources/actor/${encodeURIComponent(id)}/file?name=actor.py`,
    });
    expect(res.statusCode).toBe(200);
    expect((res.json() as { source: string }).source).toBe('# code\n');
  });

  it('refuses to read out of the folder with ..', async () => {
    const dir = actorFolder('probe');
    // THE BAIT HAS TO EXIST, AND BESIDE THE FOLDER. `resolveInside` refuses MISSING (404) before it
    // refuses OUTSIDE (400), so a file planted anywhere `..` does not reach makes this assert the
    // wrong refusal — it would pass for a server with no containment check at all.
    writeFileSync(path.join(codeRoot('actor'), 'secrets.env'), 'TOKEN=1');
    const id = idOf(dir);
    const res = await app.inject({
      method: 'GET',
      url: `/api/sources/actor/${encodeURIComponent(id)}/file?name=${encodeURIComponent('../secrets.env')}`,
    });
    expect(res.statusCode).toBe(400);
    expect((res.json() as { error: string }).error).toContain('resolves outside');
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
    const id = idOf(dir);
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
    const id = idOf(dir);
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
    const id = idOf(dir);
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
    const id = idOf(dir);
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
    const id = idOf(dir);
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
    const id = idOf(actorFolder('probe'));
    const res = await serve(id);
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({
      actor: 'probe',
      version: '0.1.0',
      session: 'probe-0_1_0',
      attach: 'tmux attach -t probe-0_1_0',
    });
  });

  it('404s a folder that has gone away, because it is not an Actor any more', async () => {
    // THIS ASSERTED 400 AND THE REASONING UNDER IT IS GONE. It said "the registration outlives the
    // directory on purpose, so the row is still here and still clickable" — which was true while a
    // path in a database made a folder an Actor. ADR 0049 made the directory the record, so a
    // deleted folder is not listed, has no id to address, and serve answers the same 404 it
    // answers for an id nobody ever had. The `absent`-but-clickable state it was guarding is
    // unreachable.
    process.env.KONTRA_BIN = 'true';
    const dir = actorFolder('probe');
    const id = idOf(dir);
    rmSync(dir, { recursive: true, force: true });
    const res = await serve(id);
    expect(res.statusCode).toBe(404);
  });

  it('404s a folder nobody registered', async () => {
    expect((await serve('actor:ghost:zz')).statusCode).toBe(404);
  });

  it('takes no placement — there is nothing in the request to choose one with', async () => {
    // THE ANTI-FEATURE, pinned at the seam. `serveActor` passes `--mode local`; a body that could
    // ask for anything else would make an edit and a fleet deployment one field apart.
    process.env.KONTRA_BIN = 'true';
    const id = idOf(actorFolder('probe'));
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
    const id = idOf(actorFolder('probe'));
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
    const id = idOf(actorFolder('probe'));
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
    const id = idOf(actorFolder('probe'));
    for (const bad of ['', 'dns facts', 'head"', 'a\nb', '../head']) {
      const res = await caller(id, { method: bad, units: [] });
      expect(res.statusCode).toBe(400);
      expect((res.json() as { error: string }).error).toContain('is not a Method name');
    }
  });

  it('treats a missing Batch as an empty one rather than failing', async () => {
    // `BATCH = []` is a legal generated file — the operator types the Units into it.
    const id = idOf(actorFolder('probe'));
    const res = await caller(id, { method: 'head' });
    expect(res.statusCode).toBe(200);
    expect((res.json() as { source: string }).source).toContain('BATCH = []');
  });

  it('404s a folder nobody registered', async () => {
    expect((await caller('actor:ghost:zz', { method: 'head' })).statusCode).toBe(404);
  });
});

describe('DELETE /api/sources/:kind/:id — forgetting is GONE (ADR 0049)', () => {
  it('410s, and says that deleting the folder is what forgetting is now', async () => {
    const id = idOf(actorFolder('probe'));
    const res = await app.inject({
      method: 'DELETE',
      url: `/api/sources/actor/${encodeURIComponent(id)}`,
    });
    expect(res.statusCode).toBe(410);
    const said = (res.json() as { error: string }).error;
    expect(said).toContain('forgetting a folder is gone');
    expect(said).toContain('Delete the folder from the workspace.');
  });

  it('and deleting the folder really is enough', async () => {
    const dir = actorFolder('probe');
    rmSync(dir, { recursive: true, force: true });
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
    const id = idOf(dir);
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
    const id = idOf(dir);
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
    const id = idOf(dir);
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
