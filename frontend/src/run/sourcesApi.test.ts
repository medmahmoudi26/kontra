/**
 * The register / list / forget / serve calls, against a stubbed server.
 *
 * These are the requests the register form, the workbench and its serve button make, and what is
 * pinned here is what a page cannot see for itself: which URL and method each one uses, and what an
 * operator READS when the server refuses. A typo in a path is the ordinary case, not an exceptional
 * one.
 */

import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  AlreadyServingError,
  fetchSourceFile,
  fetchSourceFiles,
  fetchSources,
  forgetSource,
  generateCaller,
  readProbe,
  registerSource,
  runProbe,
  serveActorSource,
  type Source,
} from './api';

function reply(status: number, body: unknown): Response {
  const text = typeof body === 'string' ? body : JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: status === 400 ? 'Bad Request' : 'OK',
    json: async () => JSON.parse(text) as unknown,
    text: async () => text,
  } as Response;
}

/** Every call the code under test made: `[url, init]`. */
function stubFetch(...answers: Response[]): Array<[string, RequestInit | undefined]> {
  const calls: Array<[string, RequestInit | undefined]> = [];
  let i = 0;
  vi.stubGlobal('fetch', (url: string, init?: RequestInit) => {
    calls.push([url, init]);
    return Promise.resolve(answers[Math.min(i++, answers.length - 1)]!);
  });
  return calls;
}

const probe: Source = {
  id: 'actor:probe:1f3k',
  kind: 'actor',
  name: 'probe',
  path: '/src/probe',
  version: '0.1.0',
  description: 'GET each target.',
  registeredAt: 1_700_000_000_000,
};

afterEach(() => vi.unstubAllGlobals());

describe('listing', () => {
  it('brings back the default root the form prefills with', async () => {
    // Without it the form opens on an empty field, which is a guess about a convention the operator
    // has not read yet.
    const calls = stubFetch(reply(200, { defaultRoot: '/home/me/.kontra/actors', sources: [probe] }));
    const got = await fetchSources('actor');
    expect(calls[0]![0]).toBe('/api/sources/actor');
    expect(got.defaultRoot).toBe('/home/me/.kontra/actors');
    expect(got.sources.map((s) => s.path)).toEqual(['/src/probe']);
  });

  it('carries the absent flag through, so a deleted folder can be drawn as one', async () => {
    stubFetch(reply(200, { defaultRoot: '/root', sources: [{ ...probe, absent: true }] }));
    expect((await fetchSources('actor')).sources[0]!.absent).toBe(true);
  });
});

describe('registering', () => {
  it('posts the path and nothing else — there is no upload', async () => {
    const calls = stubFetch(reply(200, probe));
    expect(await registerSource('actor', '/src/probe')).toMatchObject({ path: '/src/probe' });
    const [url, init] = calls[0]!;
    expect(url).toBe('/api/sources/actor');
    expect(init?.method).toBe('POST');
    expect(JSON.parse(String(init?.body))).toEqual({ path: '/src/probe' });
  });

  it('fails with the SERVER’s reason, not with the envelope it arrived in', async () => {
    // The whole answer to a mistyped path is the sentence naming the marker file. It used to reach
    // the form as `… 400 Bad Request — {"error":"…"}`, with the useful part inside JSON.
    stubFetch(
      reply(400, { error: '/srv/prob has no actor.json — that is what makes a folder an Actor' })
    );
    const said = await registerSource('actor', '/srv/prob').then(
      () => 'it did not fail',
      (e: unknown) => (e as Error).message
    );
    expect(said).toContain('/srv/prob has no actor.json');
    expect(said).not.toContain('{"error"');
  });

  it('keeps a body that is not the envelope, whatever it is', async () => {
    // An HTML page from a proxy in front of the orchestrator is still the most informative thing
    // there is about what answered.
    stubFetch(reply(400, '<html>502 upstream</html>'));
    await expect(registerSource('workflow', '/src/nscheck')).rejects.toThrow(/502 upstream/);
  });
});

describe('the workbench’s two calls', () => {
  it('lists the folder by id, encoded', async () => {
    const calls = stubFetch(reply(200, { path: '/src/probe', files: [] }));
    await fetchSourceFiles('actor', 'actor:probe:1f3k');
    expect(calls[0]![0]).toBe('/api/sources/actor/actor%3Aprobe%3A1f3k/files');
  });

  it('reads one file by name, in the query', async () => {
    const calls = stubFetch(reply(200, { name: 'description.md', source: '# probe\n' }));
    const got = await fetchSourceFile('actor', 'actor:probe:1f3k', 'description.md');
    expect(calls[0]![0]).toBe('/api/sources/actor/actor%3Aprobe%3A1f3k/file?name=description.md');
    expect(got.source).toBe('# probe\n');
  });

  it('has NO third call that writes, and neither does anything else here', async () => {
    /* THE WORKBENCH USED TO HAVE THREE. `saveSourceFile` — `PUT /api/sources/:kind/:id/file` — is
       gone with its route (ADR 0033 §6): its one remaining caller was the Actors page writing a
       generated caller into a Workflow folder, and that errand went when the page started calling
       the Method. ADR 0030 had already made both editors read-only viewers, so there is now no
       write path from this app to any file on the operator's disk.

       ASSERTED ON THE MODULE, because an absent function leaves no trace in a diff. */
    /* THROUGH `fileURLToPath`, NOT `new URL(rel, import.meta.url)`. jsdom puts its own `URL` on the
       global and resolves a relative reference against the DOCUMENT base, so the second form comes
       back as `http://localhost:3000/src/run/api.ts` and the read fails with ENOENT on `/src/run`. */
    const src = readFileSync(path.join(path.dirname(fileURLToPath(import.meta.url)), 'api.ts'), 'utf8');
    expect(src).not.toMatch(/export async function saveSourceFile/);
    expect(src).not.toMatch(/method: 'PUT'[\s\S]{0,200}sources\//);
  });
});

describe('serving an Actor', () => {
  const served = {
    actor: 'probe',
    version: '0.1.0',
    path: '/src/probe',
    session: 'probe-0_1_0',
    attach: 'tmux attach -t probe-0_1_0',
  };

  it('posts to the folder’s serve route, by id, encoded', async () => {
    const calls = stubFetch(reply(200, served));
    await serveActorSource('actor:probe:1f3k');
    const [url, init] = calls[0]!;
    expect(url).toBe('/api/sources/actor/actor%3Aprobe%3A1f3k/serve');
    expect(init?.method).toBe('POST');
  });

  it('sends nothing that could choose a placement', async () => {
    // THE ANTI-FEATURE, pinned. `kontra serve --actor` can also start containers or place the Worker
    // on Machines that keep billing; the server passes `--mode local` and takes no mode. A body
    // that carried one would make this call the dropdown that `actorControl.ts` refuses to have.
    // `restart` is the ONE knob, and it chooses nothing about WHERE — only whether the worker that
    // is already on this machine is replaced.
    const calls = stubFetch(reply(200, served));
    await serveActorSource('actor:probe:1f3k');
    const body = String(calls[0]![1]?.body ?? '{}');
    expect(Object.keys(JSON.parse(body))).toEqual(['restart']);
    expect(body).not.toMatch(/mode|docker|fleet/i);
  });

  it('does not restart unless asked — a bare press must not kill a running worker', async () => {
    // The default matters more than the flag: a worker holds in-flight Units, and a Serve that
    // silently replaced one would lose them for an operator who only wanted to check it was up.
    const calls = stubFetch(reply(200, served));
    await serveActorSource('actor:probe:1f3k');
    expect(JSON.parse(String(calls[0]![1]?.body))).toEqual({ restart: false });

    const asked = stubFetch(reply(200, served));
    await serveActorSource('actor:probe:1f3k', { restart: true });
    expect(JSON.parse(String(asked[0]![1]?.body))).toEqual({ restart: true });
  });

  it('turns the 409 into its own type, because a worker being there is not a failure', async () => {
    // The route answers 409 when a session already exists. Reported as an error it reads as "serve
    // is broken"; as `AlreadyServingError` the console can offer the restart that answers it.
    stubFetch(reply(409, { error: 'a worker is already serving probe in tmux session probe-0_1_0' }));
    await expect(serveActorSource('actor:probe:1f3k')).rejects.toBeInstanceOf(AlreadyServingError);
  });

  it('unwraps the server’s sentence out of the error envelope', async () => {
    // `{"error":…}` raw would put JSON braces in front of the one sentence that says what to do.
    stubFetch(reply(409, { error: 'a worker is already serving probe' }));
    await expect(serveActorSource('actor:probe:1f3k')).rejects.toThrow(/^a worker is already serving probe$/);
  });

  it('brings back the session and the attach command, which are the whole report', async () => {
    // The pane above the console is a SNAPSHOT (ADR 0020): reading a long traceback means
    // attaching in a terminal, and the name is what an operator types to get there.
    stubFetch(reply(200, served));
    const got = await serveActorSource('actor:probe:1f3k');
    expect(got.session).toBe('probe-0_1_0');
    expect(got.attach).toBe('tmux attach -t probe-0_1_0');
  });

  it('fails with the CLI’s own last lines, not with a generic refusal', async () => {
    // `serveActor` puts the final four lines of what `kontra` printed into its refusal, because a
    // worker that dies at import is the case this button exists for — and "could not serve" would
    // send the operator to a terminal to run the command again and read the same output by hand.
    stubFetch(
      reply(400, {
        error:
          'serve failed (exit 1): Traceback (most recent call last): | ' +
          'File "/src/probe/actor.py", line 3 | import httpx | ' +
          "ModuleNotFoundError: No module named 'httpx'",
      })
    );
    const said = await serveActorSource('actor:probe:1f3k').then(
      () => 'it did not fail',
      (e: unknown) => (e as Error).message
    );
    expect(said).toContain("ModuleNotFoundError: No module named 'httpx'");
    expect(said).toContain('serve failed (exit 1)');
    expect(said).not.toContain('{"error"');
  });

  it('surfaces the reason a folder that went away cannot be served', async () => {
    // The registration outlives the directory on purpose (`SourceStore.list` keeps the row and
    // marks it absent), so this is a live case and not a hypothetical.
    stubFetch(
      reply(400, {
        error:
          '/src/probe is not on this machine any more — put the folder back, or forget the registration',
      })
    );
    await expect(serveActorSource('actor:probe:1f3k')).rejects.toThrow(
      /is not on this machine any more/
    );
  });
});

describe('generating a caller for a Method', () => {
  const caller = { filename: 'workflow.py', source: '"""Dispatch probe@0.1.0.head()…"""\n' };

  it('posts the Method and the Batch to the actor folder’s caller route', async () => {
    const calls = stubFetch(reply(200, caller));
    const got = await generateCaller('actor:probe:1f3k', 'head', [{ url: 'https://a.test' }]);
    const [url, init] = calls[0]!;
    expect(url).toBe('/api/sources/actor/actor%3Aprobe%3A1f3k/caller');
    expect(init?.method).toBe('POST');
    expect(JSON.parse(String(init?.body))).toEqual({
      method: 'head',
      units: [{ url: 'https://a.test' }],
    });
    expect(got.filename).toBe('workflow.py');
  });

  it('goes to the CALLER route, which returns bytes and starts nothing', async () => {
    /* THIS ASSERTION SURVIVES ADR 0033, with a different reason under it. It used to pin that
       nothing on this page could dispatch; the page dispatches now, through `runProbe` below. What
       it pins today is that the two are SEPARATE calls: reading the code you are about to run must
       not require running it, and a generate that quietly started something would make every
       keystroke in the Batch form a Run. */
    const calls = stubFetch(reply(200, caller));
    await generateCaller('actor:probe:1f3k', 'head', []);
    expect(calls[0]![0]).not.toMatch(/\/probe\b/);
    expect(calls[0]![0]).not.toMatch(/\/runs\b/);
    expect(calls[0]![0]).toContain('/caller');
  });

  it('sends an empty Batch as an empty list, not as nothing', async () => {
    // `BATCH = []` is a legal generated file. Omitting the field would leave the server defaulting
    // for a Batch the operator emptied on purpose.
    const calls = stubFetch(reply(200, caller));
    await generateCaller('actor:probe:1f3k', 'head', []);
    expect(JSON.parse(String(calls[0]![1]?.body))).toEqual({ method: 'head', units: [] });
  });

  it('fails with the server’s reason for a name that is not a Method', async () => {
    stubFetch(reply(400, { error: '"dns facts" is not a Method name' }));
    const said = await generateCaller('actor:probe:1f3k', 'dns facts', []).then(
      () => 'it did not fail',
      (e: unknown) => (e as Error).message
    );
    expect(said).toContain('is not a Method name');
    expect(said).not.toContain('{"error"');
  });
});

describe('calling a Method — the probe (ADR 0033)', () => {
  const startedProbe = {
    runId: 'actorprobe-1755000000-1a2b3c4d',
    actor: 'probe',
    version: '0.1.0',
    method: 'head',
    units: 2,
    dataset: 'probe-probe-head-1a2b3c4d',
    queue: 'kontra-probe',
    endpoint: 'kontra-probe-0-1-0',
  };

  it('posts the Method and the Batch to the actor folder’s probe route', async () => {
    const calls = stubFetch(reply(201, startedProbe));
    const got = await runProbe('actor:probe:1f3k', 'head', [{ url: 'a' }, { url: 'b' }]);
    const [url, init] = calls[0]!;
    expect(url).toBe('/api/sources/actor/actor%3Aprobe%3A1f3k/probe');
    expect(init?.method).toBe('POST');
    expect(got.runId).toBe('actorprobe-1755000000-1a2b3c4d');
  });

  it('names ONE Method and ONE Batch, and has no field for a second of either', async () => {
    // The COUNT, at the wire (ADR 0033 §1). The Actor and the version are the FOLDER's — they are
    // in the path, not the body — so the body cannot aim this somewhere else, and there is nowhere
    // in it to put a `then`, a `methods` or a fan-out width.
    const calls = stubFetch(reply(201, startedProbe));
    await runProbe('actor:probe:1f3k', 'head', [{ url: 'a' }]);
    const sent = JSON.parse(String(calls[0]![1]?.body)) as Record<string, unknown>;
    expect(Object.keys(sent).sort()).toEqual(['method', 'units']);
    expect(sent.method).toBe('head');
    expect(sent).not.toHaveProperty('key');
  });

  it('sends an empty Batch as an empty list, not as nothing', async () => {
    const calls = stubFetch(reply(201, { ...startedProbe, units: 0 }));
    await runProbe('actor:probe:1f3k', 'head', []);
    expect(JSON.parse(String(calls[0]![1]?.body))).toEqual({ method: 'head', units: [] });
  });

  it('fails with the server’s own refusal, because each one names its fix', async () => {
    stubFetch(
      reply(400, { error: 'the probe worker is not running — nothing polls kontra-probe' })
    );
    const said = await runProbe('actor:probe:1f3k', 'head', []).then(
      () => 'it did not fail',
      (e: unknown) => (e as Error).message
    );
    expect(said).toContain('the probe worker is not running');
    expect(said).not.toContain('{"error"');
  });

  it('reads the answer back by RUN id, not by folder', async () => {
    // The Run is an ordinary Run (ADR 0033's first consequence); what this reads is the workflow's
    // return value, which is where `results`, `isolated` and `done` live.
    const calls = stubFetch(
      reply(200, {
        runId: 'actorprobe-1755000000-1a2b3c4d',
        status: 'COMPLETED',
        result: { units: 12, results: 9, isolated: 3, done: true, machine: '', dataset: 'd' },
      })
    );
    const got = await readProbe('actorprobe-1755000000-1a2b3c4d');
    expect(calls[0]![0]).toBe('/api/probes/actorprobe-1755000000-1a2b3c4d');
    expect(got.result?.isolated).toBe(3);
  });
});

describe('forgetting', () => {
  it('deletes by id, encoded — an id has colons in it', async () => {
    const calls = stubFetch(reply(200, { forgotten: true }));
    await forgetSource('actor', 'actor:probe:1f3k');
    expect(calls[0]![0]).toBe('/api/sources/actor/actor%3Aprobe%3A1f3k');
    expect(calls[0]![1]?.method).toBe('DELETE');
  });

  it('surfaces the refusal for a folder that was never registered', async () => {
    // The UI does not offer forget on a discovered folder; if it ever did, this is what it would
    // have to show.
    stubFetch(
      reply(400, {
        error: 'that folder is not registered — it is in the default directory. Move it to remove it.',
      })
    );
    await expect(forgetSource('actor', 'at:/home/me/.kontra/actors/probe')).rejects.toThrow(
      /not registered/
    );
  });
});
