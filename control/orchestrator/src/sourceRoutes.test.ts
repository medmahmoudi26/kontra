import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import type { FastifyInstance } from 'fastify';
import { Repo } from './db/repo';
import { codeRoot } from './sources';
import { buildServer } from './server';

/*
 * THE SERVE ROUTE ASKS THE INFRA ROLE, SO THE QUEUE IS WHAT THIS MOCKS.
 *
 * `serveActor` used to shell out to the CLI here; it starts `serveDevWorkflow` on `kontra-infra`
 * instead, because a serve-dev Worker is a container and starting one needs the Docker socket —
 * which the API process does not have and must not get.
 *
 * WITHOUT THIS THESE TESTS DIAL A REAL TEMPORAL AND TIME OUT AT 30s EACH, reported as assertion
 * failures rather than as a missing stub. That is how they failed when the hop landed.
 */
const executed = vi.hoisted(() => ({
  result: { worker: 'probe', queue: 'probe-0.1.0', detail: '' },
}));

/**
 * THE SERVE-HISTORY READ, stubbed at the seam the route uses.
 *
 * `listServes` reaches Temporal through a module-level memoized client, so stubbing `getClient`
 * (as the serve verb above does) does NOT reach it — the real function calls the module's own
 * binding and would dial a cluster that is not there. Stubbing the exported function is what makes
 * this file about the ROUTE: which ids it admits, which it refuses, and what it does with the
 * answer.
 *
 * WHAT THE READ ITSELF DOES IS PINNED IN `serveHistory.test.ts`, which drives the real `listServes`
 * against a fake `@temporalio/client` — the query it narrows with, the ordering, the cap and the
 * close-event read for a failure's sentence. Asserting those here would assert on this stub.
 *
 * `asked` records the arguments, because the one thing the route owns about the read is WHICH
 * FOLDER it asks about: a handler that passed the wrong id would return a perfectly well-formed
 * history belonging to somebody else.
 */
const serveHistory = vi.hoisted(() => ({
  asked: [] as Array<{ sourceId: string; limit?: number }>,
  answer: { serves: [] as unknown[], capped: false },
  fail: null as Error | null,
}));

vi.mock('./temporalClient', async (orig) => ({
  // PARTIAL, not a whole-module replacement: `server.ts` pulls several bindings out of this module
  // and a factory returning only `getClient` leaves the rest `undefined` — which fails at boot,
  // far from here, in a way that reads as a broken server rather than a stubbed one.
  ...(await orig<typeof import('./temporalClient')>()),
  getClient: vi.fn(async () => ({
    workflow: {
      execute: async () => executed.result,
      start: async (_t: string, o: { workflowId: string }) => ({ workflowId: o.workflowId }),
    },
  })),
  listServes: vi.fn(async (sourceId: string, limit?: number) => {
    serveHistory.asked.push({ sourceId, limit });
    if (serveHistory.fail) throw serveHistory.fail;
    return serveHistory.answer;
  }),
}));

let app: FastifyInstance;
let tmp: string;
let prevHome: string | undefined;

beforeEach(() => {
  tmp = mkdtempSync(path.join(os.tmpdir(), 'kontra-routes-'));
  prevHome = process.env.KONTRA_HOME;
  process.env.KONTRA_HOME = path.join(tmp, 'home');
  app = buildServer({ repo: new Repo(':memory:'), webRoot: '' });
  // The stub is module-level state, so a history left behind by one test would be another test's
  // "this folder has been served" — the exact false positive the empty case asserts against.
  serveHistory.asked = [];
  serveHistory.answer = { serves: [], capped: false };
  serveHistory.fail = null;
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
    // The route's contract is what it RETURNS, not how the Worker got started. The session is
    // `<name>-<version>` folded — the string `cliutil.ActorWorkerName` and
    // `shared/core/src/panels/tmux.ts:actorSession` both mint, pinned across them by
    // `shared/conformance/queues.json` §tmux_session.
    const id = idOf(actorFolder('probe'));
    const res = await serve(id);
    expect(res.statusCode).toBe(200);
    expect(res.json()).toMatchObject({
      actor: 'probe',
      version: '0.1.0',
      session: 'probe-0_1_0',
      // `attach` NAMES THE WORKER. It was `tmux attach -t …`, which only ever worked from a shell on
      // the box; a serve-dev Worker's output is its container's stdout, shipped to the Logs surface
      // by the `KONTRA_WORKER` label, so the useful value is the name to search by.
      attach: 'probe',
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
    // THE ANTI-FEATURE, pinned at the seam. The request that crosses to the infra role carries a
    // source id and a kind and nothing else; a body that could ask for a placement would make an
    // edit and a fleet deployment one field apart — and would hand the socket-holding role an
    // instruction from the HTTP surface, which is the thing the hop exists to prevent.
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

/**
 * THE SHOWING HALF OF SERVE-DEV, at the route.
 *
 * `visibility.ts` keeps `serveDevWorkflow` off the Runs page, correctly — it is kontra's own
 * infrastructure and not a caller's Run. What did not exist was anywhere that answered "when was
 * this folder served, and did it work", so a serve that died on an import error left a button that
 * looked pressed and an actor nothing was polling, with the reason held only in an execution
 * nobody listed.
 *
 * WHAT THESE PIN IS THE ROUTE: which ids it admits, which it refuses before anything reaches a
 * query, that a caller's `?limit` is honoured, and that an unreachable cluster is not reported as
 * an empty history. `serveHistory.test.ts` drives the read itself.
 */
describe('GET /api/sources/:kind/:id/serves', () => {
  const serves = (id: string, qs = '') =>
    app.inject({ method: 'GET', url: `/api/sources/actor/${encodeURIComponent(id)}/serves${qs}` });

  it('answers an EMPTY history rather than a 404 for a folder nobody has served', async () => {
    // A folder that exists and has never been served is not an error, and the page says exactly
    // that about it. 404 here would be indistinguishable from "no such folder", which is the one
    // other thing this route says.
    const res = await serves(idOf(actorFolder('probe')));
    expect(res.statusCode).toBe(200);
    expect(res.json()).toEqual({ serves: [], capped: false });
  });

  it('asks about THIS folder, by the id the serve verb starts under', async () => {
    // `at:<path>` is the Source id (`sourceStore.ts`), and `queues.ts:serveDevWorkflowId` turns it
    // into the workflow id both start sites write. A handler that passed the wrong one would return
    // a perfectly well-formed history belonging to somebody else.
    const dir = actorFolder('probe');
    await serves(idOf(dir));
    expect(serveHistory.asked).toEqual([{ sourceId: `at:${dir}`, limit: undefined }]);
  });

  it('passes a caller\'s limit through, and only when it is a number', async () => {
    const id = idOf(actorFolder('probe'));
    await serves(id, '?limit=3');
    await serves(id, '?limit=all');
    expect(serveHistory.asked.map((a) => a.limit)).toEqual([3, undefined]);
  });

  it('hands back what the read answered, cap flag and all', async () => {
    // The flag is the contract the page draws its "showing the last N of more" sentence from — a
    // silent slice reads as the whole history.
    serveHistory.answer = {
      serves: [{ execId: 'e-1', status: 'failed', startedAt: 10, closedAt: 20, failure: 'boom' }],
      capped: true,
    };
    const res = await serves(idOf(actorFolder('probe')));
    expect(res.json()).toEqual(serveHistory.answer);
  });

  it('404s a folder nobody registered, so an id cannot be read into a query', async () => {
    // The id reaches a visibility query STRING. Resolving it through the store first means only ids
    // the store minted from the workspace listing are ever interpolated — the same "an id names
    // something we issued, or it names nothing" rule the serve hop is built on.
    expect((await serves('at:/not/in/the/workspace')).statusCode).toBe(404);
    expect(serveHistory.asked).toEqual([]);
  });

  it('404s a kind that is not one', async () => {
    const res = await app.inject({ method: 'GET', url: '/api/sources/dataset/at%3A%2Fx/serves' });
    expect(res.statusCode).toBe(404);
    expect(serveHistory.asked).toEqual([]);
  });

  it('502s an unreachable cluster instead of reporting an empty history', async () => {
    // THE DIRECTION THAT MATTERS. `{ serves: [], capped: false }` is the answer for a folder nobody
    // has served, and the page prints "nothing has served this yet" for it — so answering the same
    // shape for a cluster nobody could reach would print that over an actor served all week.
    serveHistory.fail = new Error('connect ECONNREFUSED 127.0.0.1:7233');
    const res = await serves(idOf(actorFolder('probe')));
    expect(res.statusCode).toBe(502);
    expect((res.json() as { error: string }).error).toContain('could not list serves');
  });

  it('serves a WORKFLOW folder too — both kinds start the same type under the same id shape', async () => {
    // `workflowControl.serveWorkflow` starts `serveDevWorkflow` under `serve-dev/at:<dir>` for a
    // Workflow folder, so scoping this route to actors would leave half the serves on this control
    // plane unreadable for no reason anyone could state.
    const dir = path.join(codeRoot('workflow'), 'sweep');
    mkdirSync(dir, { recursive: true });
    writeFileSync(path.join(dir, 'workflow.py'), '# code\n');
    const res = await app.inject({
      method: 'GET',
      url: `/api/sources/workflow/${encodeURIComponent(`at:${dir}`)}/serves`,
    });
    expect(res.statusCode).toBe(200);
    expect(serveHistory.asked).toEqual([{ sourceId: `at:${dir}`, limit: undefined }]);
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
