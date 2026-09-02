/**
 * "Call this Method", drawn — every state an operator can put it in.
 *
 * `renderToStaticMarkup` for the reason `HealthChips.test.ts` records: this suite runs in node with
 * no jsdom and no testing-library, and every assertion here is about text and `data-testid`, both of
 * which are in the markup. `MethodCallPanel` takes props and returns markup precisely so it can be
 * drawn here; `MethodCall.tsx` is the half that fetches.
 *
 * THE SOURCE IN THESE TESTS IS THE REAL GENERATOR'S. `callerFor` comes from `@core/actorControl` —
 * the orchestrator's own — so what the panel is asked to draw is a file the server would actually
 * have sent, and it is the file the probe itself runs (ADR 0033 §3).
 *
 * AND ONE OF THESE IS STILL ABOUT WHAT IS NOT THERE, but it is the opposite absence from the one
 * this file used to assert. It pinned that no control here started anything — true while
 * generate-and-hand-over was the whole feature. The page calls the Method now; what must be absent
 * is a SECOND call. There is no control that names a second Method, a branch, a loop, a schedule or
 * a fan-out width, because the request has no field for one (ADR 0033 §1) — and there is no write
 * into a folder (§6). An absent control leaves no trace in a diff, so it is asserted.
 */

import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { callerFor } from '@core/actorControl';
import { MethodCallPanel, type ServeReading } from './MethodCallPanes';
import { draftFor, setCell, setJson, unitsOf, type BatchDraft } from './methodCall';
import type { GeneratedCaller, ProbeReading, ProbeStarted } from '../run/api';
import type { ActorOperation, CatalogActor } from '../types';

/**
 * `import.meta.url` RESOLVED THROUGH NODE, NEVER THROUGH THE GLOBAL `URL`.
 *
 * `new URL('./x.ts', import.meta.url)` is wrong in this suite and silently so: the tests run in a
 * jsdom environment now, jsdom installs its OWN `URL` on the global, and it resolves a relative
 * reference against the document's base — so a `file:///…/frontend/src/…` base came back as
 * `http://localhost:3000/src/…`, `readFileSync` took the pathname, and the read failed with
 * `ENOENT /src/panels/MethodCall.tsx`. `fileURLToPath` is `node:url`'s own parser and is unaffected.
 */
const HERE = path.dirname(fileURLToPath(import.meta.url));

const PROBE: CatalogActor = {
  key: 'probe@0.1.0',
  name: 'probe',
  version: '0.1.0',
  schemaVersion: '1',
  operations: [],
  source: '/srv/checkout/examples/python/probe',
};

const HEAD: ActorOperation = {
  name: 'head',
  description: 'GET each target and record what came back.',
  input: {
    type: 'object',
    properties: { url: { type: 'string' }, depth: { type: 'integer' } },
    required: ['url'],
  },
  output: { type: 'object', properties: { status: { type: 'integer' } } },
};

/** A Method whose author declared nothing — `examples/go/dnsfacts` is a real one. */
const FACTS: ActorOperation = { name: 'facts' };

/** A worker is polling the Actor's queue right now — the only state a call is offered in. */
const SERVING: ServeReading = {
  state: 'serving',
  words: { label: 'serving', title: 'a worker is polling probe-0.1.0 now. This Actor can run.' },
  queue: 'probe-0.1.0',
  serving: 1,
};

function serve(over: Partial<ServeReading> = {}): ServeReading {
  return { ...SERVING, ...over };
}

function generated(method = 'head', units: unknown[] = [{ url: 'https://a.test' }]): GeneratedCaller {
  return { filename: 'workflow.py', source: callerFor('probe', '0.1.0', method, units) };
}

function started(over: Partial<ProbeStarted> = {}): ProbeStarted {
  return {
    runId: 'actorprobe-1755000000-1a2b3c4d',
    actor: 'probe',
    version: '0.1.0',
    method: 'head',
    units: 12,
    dataset: 'probe-probe-head-1a2b3c4d',
    queue: 'kontra-probe',
    endpoint: 'kontra-probe-0-1-0',
    ...over,
  };
}

function draw(
  over: {
    op?: ActorOperation;
    draft?: BatchDraft;
    caller?: GeneratedCaller | null;
    serve?: ServeReading;
    busy?: string | null;
    error?: string | null;
    started?: ProbeStarted | null;
    reading?: ProbeReading | null;
    copied?: boolean;
  } = {}
): string {
  const op = over.op ?? HEAD;
  const draft = over.draft ?? draftFor(op.input);
  return renderToStaticMarkup(
    createElement(MethodCallPanel, {
      actor: PROBE,
      op,
      draft,
      batch: unitsOf(draft),
      onDraft: () => {},
      caller: over.caller ?? null,
      serve: over.serve ?? SERVING,
      busy: over.busy ?? null,
      error: over.error ?? null,
      started: over.started ?? null,
      reading: over.reading ?? null,
      onRun: () => {},
      onCopy: () => {},
      onOpenDataset: () => {},
      copied: over.copied ?? false,
      onClose: () => {},
    })
  );
}

/** A Batch that parses: one Unit with the required field filled in. */
const READY = setCell(draftFor(HEAD.input), 0, 'url', 'https://a.test');

describe('before anything is called', () => {
  it('says the button calls the Method, and says what bounds the call', () => {
    // Every other console on this page does the thing its button says, and this one now does too.
    // What an operator has to learn instead is the COUNT: one call, and the second is theirs.
    const html = draw();
    expect(html).toContain('calls the Method');
    expect(html).toContain('one Actor, one version,');
    expect(html).toContain('untagged Dataset');
    expect(html).toContain('data-testid="method-call-run"');
  });

  it('draws a field per declared property, with its type and whether it is required', () => {
    const html = draw();
    expect(html).toContain('data-testid="method-call-field-0-url"');
    expect(html).toContain('data-testid="method-call-field-0-depth"');
    expect(html).toContain('integer');
    expect(html).toContain('required');
    expect(html).not.toContain('data-testid="method-call-json"');
  });

  it('refuses to call while the Batch is not one, and names what is wrong', () => {
    // `url` is required and blank. Dispatching anyway would fail inside the Method, minutes later,
    // in a worker's pane.
    const html = draw();
    expect(html).toContain('data-testid="method-call-batch-error"');
    expect(html).toContain('unit 1 · url is required');
    expect(html).toMatch(/data-testid="method-call-run"[^>]*disabled/);
  });

  it('offers the button once the Batch is one', () => {
    const html = draw({ draft: READY });
    expect(html).not.toContain('data-testid="method-call-batch-error"');
    expect(html).not.toMatch(/data-testid="method-call-run"[^>]*disabled/);
    expect(html).toContain('1 Unit');
  });

  it('counts the Units, because a Batch is a list', () => {
    const draft = setJson(draftFor(undefined), '[{"a": 1}, {"a": 2}, {"a": 3}]');
    expect(draw({ op: FACTS, draft })).toContain('3 Units');
  });
});

describe('an Actor that declares no schema', () => {
  it('says so and NAMES THE FIX, rather than offering an empty form', () => {
    // An empty field table would claim the Method takes nothing, which is the one confusion this
    // whole page exists to prevent. `examples/go/dnsfacts` and `examples/go/nscheck` are real
    // Actors in this state, so the sentence has to name a fix in both SDKs' words.
    const html = draw({ op: FACTS });
    expect(html).toContain('data-testid="method-call-no-schema"');
    expect(html).toContain('declares no input schema');
    expect(html).toContain('annotate');
    expect(html).toContain('.Takes(');
    expect(html).toContain('serve the Actor again');
    expect(html).not.toContain('data-testid="method-call-field-0-url"');
  });

  it('still offers the call, because a hand-typed Batch dispatches the same way', () => {
    // Naming the fix must not become a refusal: an Actor with no declared schema is a normal Actor,
    // and the one thing an operator wants from this page is to find out whether it works.
    const draft = setJson(draftFor(undefined), '[{"host": "a.test"}]');
    const html = draw({ op: FACTS, draft });
    expect(html).toContain('data-testid="method-call-json"');
    expect(html).not.toMatch(/data-testid="method-call-run"[^>]*disabled/);
  });
});

describe('who can actually run it', () => {
  it('will not offer a call against a STALE worker, and says why not', () => {
    // Temporal lists a poller for about five minutes after it stops. A call aimed at one sits on a
    // queue nobody drains and reads as slow — and a probe is allowed to take minutes, so "slow" and
    // "nobody is there" are indistinguishable from the outside.
    const html = draw({
      draft: READY,
      serve: serve({ state: 'stale', words: { label: 'stale', title: 'nothing has polled' }, serving: 0 }),
    });
    expect(html).toMatch(/data-testid="method-call-run"[^>]*disabled/);
    expect(html).toContain('data-testid="method-call-blocked"');
    expect(html).toContain('none has polled recently');
    expect(html).toContain('five more minutes');
  });

  it('draws UNKNOWN as a missing answer, never as a negative', () => {
    const html = draw({
      draft: READY,
      serve: serve({ state: 'unknown', words: { label: 'unknown', title: 'could not ask' }, serving: 0 }),
    });
    expect(html).toMatch(/data-testid="method-call-run"[^>]*disabled/);
    expect(html).toContain('missing answer, not a negative');
  });

  it('tells "nothing has ever polled this" apart from "a worker stopped"', () => {
    const html = draw({
      draft: READY,
      serve: serve({
        state: 'registered',
        words: { label: 'registered', title: 'nothing is polling' },
        serving: 0,
      }),
    });
    expect(html).toContain('data-testid="method-call-blocked"');
    expect(html).toContain('nothing is polling probe-0.1.0');
    expect(html).not.toContain('five more minutes');
  });

  it('shows the worker state beside the button, in `actorWorkers`’ own word', () => {
    expect(draw({ draft: READY })).toContain('data-testid="method-call-serve"');
    expect(draw({ draft: READY })).toContain('serving');
  });
});

describe('the run it started', () => {
  it('names the Run, the endpoint and the Dataset before anything comes back', () => {
    const html = draw({ draft: READY, started: started() });
    expect(html).toContain('data-testid="method-call-run-result"');
    expect(html).toContain('actorprobe-1755000000-1a2b3c4d');
    expect(html).toContain('kontra-probe-0-1-0');
    expect(html).toContain('probe-probe-head-1a2b3c4d');
    // The output's lifetime is ADR 0029's ordinary one, and the page says so rather than implying
    // a probe's Dataset is special.
    expect(html).toContain('untagged Dataset');
    expect(html).toContain('ordinary TTL');
  });

  it('says a slow probe is the measurement, not a stall', () => {
    // The backing workflow retries `RunBatch` ten times with a two-minute heartbeat, so a probe of
    // a hanging `@actor.load` takes minutes. A probe that gave up faster than production would
    // report a failure production would not have had.
    const html = draw({ draft: READY, started: started(), reading: { runId: 'x', status: 'RUNNING' } });
    expect(html).toContain('data-testid="method-call-running"');
    expect(html).toContain('two-minute heartbeat');
    expect(html).toContain('12 Units dispatched');
  });

  it('DRAWS DROPPED UNITS, and not as an empty result', () => {
    // ADR 0033's own consequence: a probe UI that shows only `results` reproduces the failure that
    // let a 15,814-target run report `completed` in seven minutes having scanned almost nothing.
    const html = draw({
      draft: READY,
      started: started(),
      reading: {
        runId: 'x',
        status: 'COMPLETED',
        result: { units: 12, results: 9, isolated: 3, done: true, machine: '', dataset: 'd' },
      },
    });
    expect(html).toContain('data-testid="method-call-verdict-dropped"');
    expect(html).toContain('3 of 12 Units were dropped');
    expect(html).toContain('data-testid="method-call-counts"');
    expect(html).toContain('isolated');
  });

  it('draws an empty result differently from a run that dropped everything', () => {
    const empty = draw({
      draft: READY,
      started: started(),
      reading: {
        runId: 'x',
        status: 'COMPLETED',
        result: { units: 12, results: 0, isolated: 0, done: true, machine: '', dataset: 'd' },
      },
    });
    const dropped = draw({
      draft: READY,
      started: started(),
      reading: {
        runId: 'x',
        status: 'COMPLETED',
        result: { units: 12, results: 0, isolated: 12, done: false, machine: '', dataset: 'd' },
      },
    });
    expect(empty).toContain('data-testid="method-call-verdict-empty"');
    expect(dropped).toContain('data-testid="method-call-verdict-dropped"');
    expect(empty).not.toContain('data-testid="method-call-verdict-dropped"');
  });

  it('names the Machine when the handler stamped one, and claims none when it did not', () => {
    const known = draw({
      draft: READY,
      started: started(),
      reading: {
        runId: 'x',
        status: 'COMPLETED',
        result: { units: 2, results: 2, isolated: 0, done: true, machine: 'kf-actor-03', dataset: 'd' },
      },
    });
    expect(known).toContain('kf-actor-03');
    const unknown = draw({
      draft: READY,
      started: started(),
      reading: {
        runId: 'x',
        status: 'COMPLETED',
        result: { units: 2, results: 2, isolated: 0, done: true, machine: '', dataset: 'd' },
      },
    });
    expect(unknown).not.toContain('machine');
  });


  it('offers a way to OPEN the Dataset once there is one, and not before', () => {
    /* EVIDENCE YOU CANNOT REACH IS A CLAIM — and a link offered while the probe is still running
       points at a name the lake has never heard of, because the awaited form publishes when the
       call returns. So the name is text until the run answers, and a link after. */
    const running = draw({ draft: READY, started: started(), reading: { runId: 'x', status: 'RUNNING' } });
    expect(running).toContain('data-testid="method-call-dataset"');
    expect(running).not.toContain('data-testid="method-call-open-dataset"');

    const done = draw({
      draft: READY,
      started: started(),
      reading: {
        runId: 'x',
        status: 'COMPLETED',
        result: { units: 2, results: 2, isolated: 0, done: true, machine: '', dataset: 'd' },
      },
    });
    expect(done).toContain('data-testid="method-call-open-dataset"');
  });


  it('draws a FAILED probe as a failure, not as one still going', () => {
    /* A run that ended with no counts and no sentence reads exactly like a run still going — and
       "still going" is the reading this panel spends a paragraph defending, because a probe
       legitimately takes minutes. So a failure gets the reason and nothing else. */
    const html = draw({
      draft: READY,
      started: started(),
      reading: {
        runId: 'x',
        status: 'FAILED',
        failure: 'method "hed" is not registered on probe@0.1.0',
      },
    });
    expect(html).toContain('data-testid="method-call-failed"');
    expect(html).toContain('is not registered');
    expect(html).not.toContain('data-testid="method-call-running"');
    expect(html).not.toContain('data-testid="method-call-counts"');
    // And nothing offers to open a Dataset that may hold nothing at all.
    expect(html).not.toContain('data-testid="method-call-open-dataset"');
  });

  it('shows the server’s refusal verbatim when the call could not be started', () => {
    const html = draw({
      draft: READY,
      error:
        'call the Method failed: 400 Bad Request — probe has no Nexus endpoint on this cluster',
    });
    expect(html).toContain('data-testid="method-call-error"');
    expect(html).toContain('has no Nexus endpoint');
  });

  it('disables the button while a call is in flight', () => {
    const html = draw({ draft: READY, busy: 'run' });
    expect(html).toContain('Calling…');
    // On the ATTRIBUTE, not on the word: the button's own class list carries `disabled:opacity-50`
    // in every state, so counting `disabled` in the markup counts styling and always passes.
    expect(html).toMatch(/data-testid="method-call-run"[^>]*disabled/);
  });
});

describe('the caller, as a read-only artefact', () => {
  it('shows the source, with the typed Batch in it as the BATCH constant', () => {
    const html = draw({ draft: READY, caller: generated() });
    expect(html).toContain('data-testid="method-call-source"');
    expect(html).toContain('BATCH = [');
    expect(html).toContain('https://a.test');
    expect(html).toContain('class ProbeHead:');
  });

  it('says it is the code the button runs, which is what makes it worth keeping', () => {
    const html = draw({ draft: READY, caller: generated() });
    expect(html).toContain('the code the button runs');
    expect(html).toContain('data-testid="method-call-copy"');
  });

  it('offers COPY and nothing that writes — the errand is gone (ADR 0033 §6)', () => {
    // The save target, the folder shelf and the `PUT …/file` call all went with the feature that
    // needed them. A file on disk is code that can diverge from what actually ran, and this one now
    // always has something to diverge from.
    const html = draw({ draft: READY, caller: generated() });
    expect(html).toContain('Nothing is written anywhere');
    expect(html).not.toContain('data-testid="method-call-save"');
    expect(html).not.toContain('data-testid="method-call-target"');
    expect(html).not.toMatch(/>\s*Replace\b/);
  });

  it('confirms a copy for a moment, so the button says it did something', () => {
    expect(draw({ draft: READY, caller: generated(), copied: true })).toContain('Copied');
  });

  it('carries a Method whose name is not a legal Python identifier', () => {
    // The Go SDK registers any non-empty name, so `dns-facts` is a real catalogued Method, reached
    // through `getattr(handle, "dns-facts")(batch)` — the name rides a string, because
    // `probe.dns-facts(batch)` is a SyntaxError, not a slightly-wrong call. The probe workflow
    // reaches it the same way, which is why this artefact stays honest for it.
    const html = draw({ op: { ...FACTS, name: 'dns-facts' }, caller: generated('dns-facts', []) });
    expect(html).toContain('getattr(probe, &quot;dns-facts&quot;)(batch)');
    expect(html).not.toContain('probe.dns-facts(batch)');
    expect(html).toContain('class ProbeDnsFacts:');
  });
});

describe('what this panel cannot do', () => {
  it('has no control that names a SECOND call', () => {
    // The count is the line (ADR 0033 §1): one Actor, one version, one Method, one Batch. Two — in
    // any spelling — is a topology, and the moment the probe accepts one it is the interpreter
    // ADR 0023 §12 removed. There is no field for it in the request, and no control for it here.
    for (const html of [draw(), draw({ draft: READY, caller: generated(), started: started() })]) {
      expect(html).not.toMatch(/>\s*Add (Method|step|node)\b/i);
      expect(html).not.toMatch(/data-testid="method-call-(then|next|chain|method-2)"/);
      expect(html).not.toMatch(/>\s*Chain\b/);
    }
  });

  it('never writes anything to disk', () => {
    // `saveSourceFile` is gone from `run/api.ts` along with its route (ADR 0033 §6); this is the
    // half of that removal a diff cannot show, because what is asserted is an import nobody makes.
    for (const file of ['MethodCall.tsx', 'MethodCallPanes.tsx', 'methodCall.ts']) {
      const src = readFileSync(path.join(HERE, file), 'utf8');
      // THE IMPORT, not the word: `methodCall.ts` still NAMES what it dropped, in the comment that
      // explains why an unused surface was removed outright rather than left behind. A test that
      // banned the string would ban the record of the decision along with the code.
      expect(src).not.toMatch(/^\s*saveSourceFile,?$/m);
      expect(src).not.toMatch(/\bsaveSourceFile\(/);
      expect(src).not.toMatch(/\bsaveTargets\(/);
    }
  });

  it('starts exactly one thing, and it is the probe', () => {
    // `startRun` is the function in `run/api.ts` that starts a CALLER's workflow from a folder. The
    // Actors page must not reach it: a probe is started by its own route, which is what bounds it
    // to one Method.
    for (const file of ['MethodCall.tsx', 'MethodCallPanes.tsx', 'methodCall.ts']) {
      const src = readFileSync(path.join(HERE, file), 'utf8');
      expect(src).not.toContain('startRun');
      expect(src).not.toContain('stopRun');
    }
  });
});
