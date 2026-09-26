/**
 * The Actor probe's server half (ADR 0033) — the count, the four refusals, and the fresh run id.
 *
 * THE COUNT IS THE TEST A REVIEWER APPLIES: *how many Methods can one request name?* One is a
 * probe. Two — in any spelling, however it is dressed — is a topology, and a server that executes
 * a topology is the interpreter ADR 0023 §12 removed. So the refusals are asserted here, and not
 * only the happy path: a shape that CAN express two and refuses by a check eventually has the
 * check relaxed for a good reason.
 *
 * THE PYTHON PEER IS `tests/test_actor_probe.py`, and neither is redundant. This side proves a
 * refused request never starts an execution; that side proves the workflow refuses too, because
 * this route is not the only way to reach it — a caller can dial Temporal — and because the
 * unkeyed dispatch is a property of the SDK call, which lives there.
 *
 * NO TEMPORAL, NO DATABASE. `startProbe` takes its describer, its endpoint list, its starter and
 * its recorder through `ProbeDeps`, the same seam `startRun` takes its describer and recorder
 * through.
 */

import { describe, expect, it } from 'vitest';
import { ControlRefused } from './workflowControl';
import { POLL_FRESH_MS, type QueueDescriber } from './pollers';
import { PROBE_QUEUE, PROBE_WORKFLOW } from './queues';
import {
  PROBE_FIELDS,
  PROBE_IDENTITY,
  PROBE_VERSION,
  ProbeRefused,
  probeDataset,
  probeRequest,
  readProbe,
  startProbe,
  type ProbeDeps,
  type ProbeInput,
} from './probe';

const NOW = 1_755_000_000_000;

/** The request the Actors page sends for a two-Unit Batch of `probe@0.1.0.head`. */
function body(over: Record<string, unknown> = {}): Record<string, unknown> {
  return { method: 'head', units: [{ url: 'https://a.test' }, { url: 'https://b.test' }], ...over };
}

function input(over: Partial<ProbeInput> = {}): ProbeInput {
  return { actor: 'probe', version: '0.1.0', method: 'head', units: [{ url: 'x' }], ...over };
}

/**
 * A describer answering per queue. `lastAccess` is what decides `serving` vs `stale`, and the
 * default is a poll that just happened — the stale case names its own age.
 */
function pollers(byQueue: Record<string, { n: number; lastAccess?: number; error?: string }>): QueueDescriber {
  return {
    pollers: async (queue) => {
      const found = byQueue[queue] ?? { n: 0 };
      if (found.error !== undefined) throw new Error(found.error);
      return Array.from({ length: found.n }, (_, i) => ({
        identity: `${1000 + i}@host-${i}@${queue}`,
        lastAccess: found.lastAccess ?? NOW - 5_000,
      }));
    },
    close: async () => {},
  };
}

/** Everything working: the endpoint exists, the Actor is served, the probe worker is up. */
function ready(over: Partial<ProbeDeps> = {}): ProbeDeps & { started: { type: string; options: any }[] } {
  const started: { type: string; options: any }[] = [];
  return {
    endpoints: async () => new Set(['kontra-probe-0-1-0']),
    describer: pollers({ 'probe-0.1.0': { n: 1 }, [PROBE_QUEUE]: { n: 1 } }),
    start: async (type, options) => {
      started.push({ type, options });
      return options.workflowId;
    },
    recorder: { record: async () => {} },
    now: () => NOW,
    started,
    ...over,
  };
}

// ---------------------------------------------------------------------------------------------
// §1 — the count
// ---------------------------------------------------------------------------------------------

describe('how many Methods can one request name', () => {
  it('takes one Actor, one version, one Method, one Batch — and has no field for a second', () => {
    // The STRUCTURAL half. A probe cannot express a second Method because there is nowhere to put
    // one, which is the difference between a decision and a validator.
    expect([...PROBE_FIELDS]).toEqual(['actor', 'version', 'method', 'units', 'dataset']);
    const ask = probeRequest(body(), 'probe', '0.1.0');
    expect(ask).toEqual({
      actor: 'probe',
      version: '0.1.0',
      method: 'head',
      units: [{ url: 'https://a.test' }, { url: 'https://b.test' }],
    });
  });

  it.each([
    // A second Method, spelled as a list — how one arrives when somebody is being helpful.
    [body({ method: ['head', 'tail'] }), /ONE method, not 2/],
    // A second Method, spelled inside one string. Every separator a topology could use.
    [body({ method: 'head,tail' }), /not one Method name/],
    [body({ method: 'head tail' }), /not one Method name/],
    [body({ method: 'head|tail' }), /not one Method name/],
    [body({ method: 'head->tail' }), /not one Method name/],
    // A second Method under a field of its own — the shape a topology actually arrives in.
    [body({ then: { method: 'tail' } }), /no field "then"/],
    [body({ methods: ['head', 'tail'] }), /no field "methods"/],
    [body({ next: { actor: 'beacon', method: 'ask' } }), /no field "next"/],
    [body({ nodes: [{ method: 'head' }], edges: [] }), /no fields "edges", "nodes"/],
    // The rest of §1's list: a branch, a condition, a loop, a retry policy, a schedule, a width.
    [body({ when: 'results > 0' }), /no field "when"/],
    [body({ repeat: 3 }), /no field "repeat"/],
    [body({ retry: { maximumAttempts: 5 } }), /no field "retry"/],
    [body({ schedule: '0 * * * *' }), /no field "schedule"/],
    [body({ shards: 8 }), /no field "shards"/],
    // An output wired to another input.
    [body({ into: { actor: 'beacon', method: 'ask' } }), /no field "into"/],
  ])('refuses a request naming more than one call: %o', (req, says) => {
    expect(() => probeRequest(req, 'probe', '0.1.0')).toThrow(says);
    expect(() => probeRequest(req, 'probe', '0.1.0')).toThrow(ProbeRefused);
  });

  it('refuses a second Actor and a second version by name', () => {
    expect(() => probeRequest(body({ actor: ['probe', 'beacon'] }), 'probe', '0.1.0')).toThrow(
      /ONE actor, not 2/
    );
    expect(() => probeRequest(body({ version: ['0.1.0', '0.2.0'] }), 'probe', '0.1.0')).toThrow(
      /ONE version, not 2/
    );
  });

  it('refuses a body that aims somewhere other than the card it was pressed on', () => {
    // The Actor and the version are the FOLDER's — the route is keyed by its id. A body naming a
    // different one is not a probe of two Actors; it is a probe of one, aimed by two authorities.
    expect(() => probeRequest(body({ actor: 'beacon' }), 'probe', '0.1.0')).toThrow(
      /names beacon/
    );
    expect(() => probeRequest(body({ version: '0.2.0' }), 'probe', '0.1.0')).toThrow(
      /registered at version 0\.1\.0/
    );
    // Naming the same one it is already aimed at is fine — a client echoing what it was shown.
    expect(probeRequest(body({ actor: 'probe', version: '0.1.0' }), 'probe', '0.1.0').method).toBe(
      'head'
    );
  });

  it('names the field rather than dropping it', () => {
    // Ignoring an unknown field is the worst of the three outcomes: one call runs, the caller reads
    // two, and nothing on either side ever says so.
    let said = '';
    try {
      probeRequest(body({ then: { method: 'tail' } }), 'probe', '0.1.0');
    } catch (err) {
      said = (err as Error).message;
    }
    expect(said).toContain('"then"');
    expect(said).toContain('one Actor, one version, one Method, one Batch');
    expect(said).toContain('interpreter');
  });

  it('REFUSES BEFORE STARTING ANYTHING', async () => {
    // A probe that refused a topology *after* dispatching its first Method would have run half of
    // one, which reads as a success in every surface downstream.
    const deps = ready();
    expect(() => probeRequest(body({ then: {} }), 'probe', '0.1.0')).toThrow(ProbeRefused);
    expect(deps.started).toEqual([]);
  });

  it('is answered 400 by every route that already catches a refusal', () => {
    // `ProbeRefused extends ControlRefused`, which is what makes the route's existing branch answer
    // "fix your request" rather than "the server broke".
    expect(new ProbeRefused('x')).toBeInstanceOf(ControlRefused);
  });
});

describe('the Batch', () => {
  it('is a list of Units, and a bare object is refused rather than wrapped', () => {
    // Wrapping silently would teach the shape wrong on the surface whose job is teaching it.
    expect(() => probeRequest(body({ units: { url: 'x' } }), 'probe', '0.1.0')).toThrow(
      /a Batch is a list of Units/
    );
  });

  it('accepts an empty Batch, which is a Batch', () => {
    expect(probeRequest(body({ units: [] }), 'probe', '0.1.0').units).toEqual([]);
  });

  it('accepts a Method name the Go SDK registers and Python cannot spell as an attribute', () => {
    expect(probeRequest(body({ method: 'dns-facts' }), 'probe', '0.1.0').method).toBe('dns-facts');
  });
});

// ---------------------------------------------------------------------------------------------
// §2 — unkeyed, and fresh per probe
// ---------------------------------------------------------------------------------------------

describe('the dispatch takes no key', () => {
  it('refuses a key rather than quietly accepting one', async () => {
    // A keyed dispatch ATTACHES to the execution already holding that key and returns THAT batch's
    // results. A probe is the one caller most likely to be fired twice in ten seconds, and
    // `backingWorkflowID` carries no Method to keep two apart.
    let said = '';
    try {
      probeRequest(body({ key: 'acme.com' }), 'probe', '0.1.0');
    } catch (err) {
      said = (err as Error).message;
    }
    expect(said).toContain('does not take a key');
    expect(said).toContain('ATTACHES');
    expect(said).toContain('unkeyed');
  });

  it('sends the workflow no key to dispatch with', async () => {
    const deps = ready();
    await startProbe(input(), deps);
    const [arg] = deps.started[0]!.options.args as [Record<string, unknown>];
    expect(Object.keys(arg).sort()).toEqual(['actor', 'dataset', 'method', 'units', 'version']);
    expect(arg).not.toHaveProperty('key');
    expect(arg).not.toHaveProperty('idempotencyKey');
  });

  it('mints a FRESH run id per probe, at the granularity a human clicks at', async () => {
    // Two presses inside one second must not collide: a seconds-granular id alone would make the
    // second start fail as AlreadyStarted, and — worse — a shared id is what would let one probe's
    // backing workflow answer another's.
    const deps = ready();
    const first = await startProbe(input(), deps);
    const second = await startProbe(input(), deps);
    expect(first.runId).not.toBe(second.runId);
    expect(first.runId).toMatch(/^actorprobe-1755000000-[0-9a-f]{8}$/);
    // And two probes of one Method do not write into one Dataset either — the same confusion
    // arriving through the output side.
    expect(first.dataset).not.toBe(second.dataset);
  });
});

// ---------------------------------------------------------------------------------------------
// The four refusals that would otherwise be hangs
// ---------------------------------------------------------------------------------------------

describe('what a probe refuses before it dispatches', () => {
  it('refuses an Actor with no Nexus endpoint, naming the repair', async () => {
    // A real, recorded state: registering creates the endpoint, and `endpoint` is simply absent
    // when the cluster could not be reached. Dispatching to a name nobody created is a call that
    // waits — ADR 0033 §4 names this as the one thing a probe must handle that a caller need not.
    const deps = ready({ endpoints: async () => new Set<string>() });
    await expect(startProbe(input(), deps)).rejects.toThrow(/no Nexus endpoint/);
    await expect(startProbe(input(), deps)).rejects.toThrow(/kontra-probe-0-1-0/);
    expect(deps.started).toEqual([]);
  });

  it('refuses when nothing polls the Actor’s queue', async () => {
    const deps = ready({
      describer: pollers({ 'probe-0.1.0': { n: 0 }, [PROBE_QUEUE]: { n: 1 } }),
    });
    await expect(startProbe(input(), deps)).rejects.toThrow(/nothing is serving probe/);
    expect(deps.started).toEqual([]);
  });

  it('refuses a queue whose only pollers are STALE, which is not a smaller kind of serving', async () => {
    // Temporal lists a poller for about five minutes after it stops, so `identities.length > 0` is
    // not evidence a worker is alive. A dispatch aimed at a dead one sits on a queue nobody drains
    // and reports as slow — and a probe is ALLOWED to take minutes, so "slow" and "nobody is
    // there" are genuinely indistinguishable from the outside.
    const deps = ready({
      describer: pollers({
        'probe-0.1.0': { n: 2, lastAccess: NOW - POLL_FRESH_MS - 1 },
        [PROBE_QUEUE]: { n: 1 },
      }),
    });
    await expect(startProbe(input(), deps)).rejects.toThrow(/none has polled recently/);
    expect(deps.started).toEqual([]);
  });

  it('accepts a queue with one live worker beside a dead one', async () => {
    // The normal state of a fleet losing a Machine, and not an edge case. One live worker means
    // the Actor can run, whatever else Temporal is still remembering.
    const mixed: QueueDescriber = {
      pollers: async (queue) =>
        queue === PROBE_QUEUE
          ? [{ identity: 'p@probe-worker', lastAccess: NOW - 1_000 }]
          : [
              { identity: 'dead@kf-actor-01', lastAccess: NOW - POLL_FRESH_MS - 1 },
              { identity: 'live@kf-actor-02', lastAccess: NOW - 1_000 },
            ],
      close: async () => {},
    };
    const deps = ready({ describer: mixed });
    await expect(startProbe(input(), deps)).resolves.toMatchObject({ actor: 'probe' });
  });

  it('refuses when Temporal cannot be asked, rather than dispatching blind', async () => {
    const deps = ready({
      describer: pollers({ 'probe-0.1.0': { n: 1, error: 'connection refused' }, [PROBE_QUEUE]: { n: 1 } }),
    });
    await expect(startProbe(input(), deps)).rejects.toThrow(/cannot verify a worker is serving/);
  });

  it('refuses when the probe worker itself is not running, and names the command', async () => {
    // kontra's shipped containers are Node-only and the probe worker is Python — ADR 0033 §3
    // prices that deployment addition explicitly. Without this the Run would sit at `scheduled`
    // forever with nothing to say why.
    const deps = ready({
      describer: pollers({ 'probe-0.1.0': { n: 1 }, [PROBE_QUEUE]: { n: 0 } }),
    });
    await expect(startProbe(input(), deps)).rejects.toThrow(/the probe worker is not running/);
    await expect(startProbe(input(), deps)).rejects.toThrow(/python3 -m kontra\.probe/);
    expect(deps.started).toEqual([]);
  });
});

// ---------------------------------------------------------------------------------------------
// What it starts, and what it answers with
// ---------------------------------------------------------------------------------------------

describe('starting the probe', () => {
  it('starts ONE execution of the ONE workflow type on the kontra queue', async () => {
    const deps = ready();
    const started = await startProbe(input({ units: [{ a: 1 }, { a: 2 }] }), deps);

    expect(deps.started).toHaveLength(1);
    expect(deps.started[0]!.type).toBe(PROBE_WORKFLOW);
    expect(deps.started[0]!.options.taskQueue).toBe(PROBE_QUEUE);
    // ONE ARGUMENT, the whole request. Positional arguments are how a second Method eventually
    // arrives as "just one more"; an object with five named fields has nowhere to put it.
    expect(deps.started[0]!.options.args).toHaveLength(1);
    expect(started.units).toBe(2);
    expect(started.queue).toBe(PROBE_QUEUE);
    expect(started.endpoint).toBe('kontra-probe-0-1-0');
  });

  it('names an untagged output Dataset, and gets no new lifecycle for it', async () => {
    // ADR 0033 §5: an ordinary Dataset with an ordinary name and no tags, which ADR 0029 §3
    // already sweeps. A probe flag, a probe-specific expiry or a special sweep class would be a
    // second lifetime for the same object.
    const deps = ready();
    const started = await startProbe(input({ method: 'head' }), deps);
    expect(started.dataset).toMatch(/^probe-probe-head-[0-9a-f]{8}$/);
    const [arg] = deps.started[0]!.options.args as [Record<string, unknown>];
    expect(arg.dataset).toBe(started.dataset);
    expect(arg).not.toHaveProperty('tag');
  });

  it('keeps a Dataset name the caller chose', async () => {
    const deps = ready();
    const started = await startProbe(input({ dataset: 'keepme' }), deps);
    expect(started.dataset).toBe('keepme');
  });

  it('stamps the probe’s OWN identity, so its output reads as a probe’s', async () => {
    // The probe is not a registered folder and has no `workflow.json`, so `stampRunWorkflow` has
    // no manifest to snapshot (ADR 0033 §5). Without this the Dataset renders ADR 0029's
    // Actor-grain fallback — supported, but it would name the Actor rather than the probe.
    const stamped: [string, string, string][] = [];
    const deps = ready({
      recorder: {
        record: async (runId: string, workflow: string, version: string) => {
          stamped.push([runId, workflow, version]);
        },
      },
    });
    const started = await startProbe(input(), deps);
    expect(stamped).toEqual([[started.runId, PROBE_IDENTITY, PROBE_VERSION]]);
    expect(started.workflow).toEqual({ name: PROBE_IDENTITY, version: PROBE_VERSION });
  });

  it('does not report a started Run as a failed start when the stamp cannot land', async () => {
    // The run is already going by then. Rethrowing means the caller retries, Temporal refuses the
    // duplicate id, and an operator concludes the probe never began — the worst outcome available.
    const deps = ready({
      recorder: {
        record: async () => {
          throw new Error('database is down');
        },
      },
    });
    const started = await startProbe(input(), deps);
    expect(started.runId).toBeTruthy();
    expect(started.workflow).toBeUndefined();
  });
});

describe('the output Dataset name', () => {
  it('fits the bound a Dataset name has, with the unique half surviving', () => {
    const long = probeDataset('a'.repeat(80), 'm'.repeat(80), 'deadbeef');
    expect(long.length).toBeLessThanOrEqual(64);
    expect(long.endsWith('deadbeef')).toBe(true);
    expect(long).toMatch(/^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$/);
  });

  it('is a legal name even for an Actor whose name is not one', () => {
    expect(probeDataset('crawl4ai canon', 'dns-facts', 'aabbccdd')).toMatch(
      /^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$/
    );
  });
});

// ---------------------------------------------------------------------------------------------
// Reading one back — and the bound that makes the route safe
// ---------------------------------------------------------------------------------------------

describe('reading a probe back', () => {
  function handles(over: {
    type?: string;
    status?: string;
    result?: unknown;
    throws?: string;
  } = {}) {
    return {
      describe: async () => ({
        type: over.type ?? PROBE_WORKFLOW,
        status: over.status ?? 'COMPLETED',
      }),
      result: async () => {
        if (over.throws !== undefined) throw new Error(over.throws);
        return over.result ?? {};
      },
    };
  }

  it('refuses a run that is not a probe', async () => {
    // A workflow returns whatever its author put in it — rows, a credential, a customer's data.
    // A route that handed back `handle.result()` for any id would be a general read of every
    // workflow's output on the cluster, through a surface nobody would think to audit.
    await expect(readProbe('dnssweep-1755000000', handles({ type: 'DnsSweep' }))).rejects.toThrow(
      /not a probe/
    );
  });

  it('says it is still running rather than guessing at counts', async () => {
    const reading = await readProbe('p', handles({ status: 'RUNNING' }));
    expect(reading.status).toBe('RUNNING');
    expect(reading.result).toBeUndefined();
  });

  it('reports dropped Units DISTINCTLY from an empty result', async () => {
    // The whole reason ADR 0028 §4 made the tuple undestructurable-around. Zero rows because the
    // Method found nothing, and zero rows because every Unit was isolated, must not render alike.
    const nothingFound = await readProbe(
      'p',
      handles({ result: { units: 12, results: 0, isolated: 0, done: true } })
    );
    const allDropped = await readProbe(
      'p',
      handles({ result: { units: 12, results: 0, isolated: 12, done: false } })
    );
    expect(nothingFound.result).toMatchObject({ results: 0, isolated: 0, done: true });
    expect(allDropped.result).toMatchObject({ results: 0, isolated: 12, done: false });
    expect(nothingFound.result).not.toEqual(allDropped.result);
  });

  it('reads a missing count as absent rather than as zero-shaped nonsense', async () => {
    // A worker older than a field omits it. `done` is the one that must default TRUE, matching
    // `Batch.from_ref`: only an explicit `false` says the producer returned before covering input.
    const reading = await readProbe('p', handles({ result: { units: 3, results: 3 } }));
    expect(reading.result).toMatchObject({ units: 3, results: 3, isolated: 0, done: true });
    expect(reading.result?.machine).toBe('');
  });

  it('carries the failure’s own sentence when the workflow failed', async () => {
    /* THE REASON EXISTS IN EXACTLY ONE PLACE. `describe` reports the STATUS and nothing about the
       cause, so a failed probe read from the status alone renders as an empty report beside the
       word "failed" — indistinguishable, on screen, from one still going. `result()` is what
       raises, so it is called for a failed run purely to be caught. */
    const reading = await readProbe(
      'p',
      handles({ status: 'FAILED', throws: 'method "hed" is not registered' })
    );
    expect(reading.status).toBe('FAILED');
    expect(reading.failure).toContain('not registered');
    expect(reading.result).toBeUndefined();
  });

  it('unwraps the author’s sentence from the SDK’s wrapper', async () => {
    // `WorkflowFailedError.message` is "Workflow execution failed" for every failure there has
    // ever been; the sentence a reader needs is one level down in `cause`. Reporting the wrapper
    // replaces the one useful string with a constant.
    const wrapped = Object.assign(new Error('Workflow execution failed'), {
      cause: new Error('a probe does not take a key'),
    });
    const reading = await readProbe('p', {
      describe: async () => ({ type: PROBE_WORKFLOW, status: 'FAILED' }),
      result: async () => {
        throw wrapped;
      },
    });
    expect(reading.failure).toBe('a probe does not take a key');
  });

  it('does not invent counts for a run that is still going', async () => {
    const reading = await readProbe('p', handles({ status: 'RUNNING', throws: 'never called' }));
    expect(reading.result).toBeUndefined();
    expect(reading.failure).toBeUndefined();
  });
});
