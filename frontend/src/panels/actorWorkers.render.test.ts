/**
 * The worker strip, DRAWN — what an operator reads before aiming a dispatch at this Actor.
 *
 * `renderToStaticMarkup` for the reason `HealthChips.test.ts` records: this suite runs in node with
 * no jsdom and no testing-library, and every assertion here is about text, `data-testid` and
 * `data-state`, all three of which are in the markup.
 *
 * THE ASSERTIONS THAT MATTER MOST ARE THE NEGATIVE ONES. A strip that draws nothing is the failure
 * this component exists to prevent — an empty rectangle where "nothing is polling this queue"
 * belongs reads as a widget that failed to load, or worse, as nothing worth worrying about. So
 * every state is asserted to SAY something, and the states that must not read as healthy are
 * asserted not to.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import { POLL_FRESH_MS, type PollerReport } from '../run/workflowState';
import { ActorWorkers } from './ActorWorkers';
import type { Terminal } from './panelsClient';

const NOW = 1_800_000_000_000;
const QUEUE = 'probe-0.1.0';

const identity = (pid: number, host: string): string => `${pid}@${host}@${QUEUE}`;

function report(
  workers: readonly { identity: string; lastPoll: number }[],
  over: Partial<PollerReport> = {}
): PollerReport {
  return {
    queue: QUEUE,
    pollers: workers.length,
    identities: workers.map((w) => w.identity),
    workers: [...workers],
    lastPoll: workers.reduce((max, w) => (w.lastPoll > max ? w.lastPoll : max), 0),
    ...over,
  };
}

const LIVE = { identity: identity(101, 'kf-actor-01'), lastPoll: NOW - 10_000 };
const DEAD = { identity: identity(202, 'kf-actor-02'), lastPoll: NOW - 3 * 60_000 };

function pane(over: Partial<Terminal> & { id: string }): Terminal {
  return {
    machine: 'kf-actor-01',
    host: 'kf-actor-01.kontra.internal',
    publicIp: '10.124.0.5',
    tag: 'worker',
    fleet: 'apex',
    actor: 'probe',
    version: '0.1.0',
    window: '0',
    health: { reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' },
    ...over,
  };
}

function draw(
  over: { report?: PollerReport | null; machines?: Terminal[]; now?: number } = {}
): string {
  return renderToStaticMarkup(
    createElement(ActorWorkers, {
      actor: 'probe',
      queue: QUEUE,
      report: over.report === undefined ? report([LIVE]) : over.report,
      now: over.now ?? NOW,
      machines: over.machines ?? [],
    })
  );
}

/** The state word the strip leads with — `data-state` on the one element that claims it. */
function stateOf(html: string): string | undefined {
  return /data-testid="actor-serving-probe" data-state="([a-z]+)"/.exec(html)?.[1];
}

describe('REGISTERED ONLY — the Actor exists and nothing polls its queue', () => {
  const html = draw({ report: report([]) });

  it('says so in a sentence, never as an empty list', () => {
    expect(stateOf(html)).toBe('registered');
    expect(html).toContain('data-testid="actor-workers-none-probe"');
    expect(html).toContain('nothing is polling');
  });

  it('names the queue, because the operator’s next move is to check it', () => {
    expect(html).toContain(QUEUE);
  });

  it('says what it costs to dispatch anyway', () => {
    // The whole trap in one clause: the dispatch does not fail, it waits, and Temporal reports it
    // as running the entire time.
    expect(html).toContain('sit on a queue nobody polls');
  });

  it('offers no worker row at all', () => {
    expect(html).not.toContain('data-testid="actor-worker-probe-0"');
  });
});

describe('SERVING — a worker is polling now', () => {
  const html = draw({ report: report([LIVE]) });

  it('leads with serving, and lists the worker', () => {
    expect(stateOf(html)).toBe('serving');
    expect(html).toContain('data-testid="actor-worker-probe-0"');
    expect(html).toContain('data-state="serving"');
  });

  it('names the host and keeps the identity beside it', () => {
    expect(html).toContain('kf-actor-01');
    expect(html).toContain(identity(101, 'kf-actor-01'));
  });

  it('shows how long ago it polled, which is what makes the claim checkable', () => {
    expect(html).toContain('10s ago');
  });

  it('draws no “nothing is serving” sentence', () => {
    expect(html).not.toContain('data-testid="actor-workers-none-probe"');
    expect(html).not.toContain('data-testid="actor-workers-stale-probe"');
  });
});

describe('STALE — Temporal still lists a poller that has stopped polling', () => {
  const html = draw({ report: report([DEAD]) });

  it('is its own state, neither serving nor registered', () => {
    expect(stateOf(html)).toBe('stale');
  });

  it('still SHOWS the worker, because which one died is the finding', () => {
    // Hiding it would leave the operator reading the same empty list an unserved Actor draws, and
    // "a worker was here and stopped" is a different thing to go and look at.
    expect(html).toContain('data-testid="actor-worker-probe-0"');
    expect(html).toContain('data-state="stale"');
    expect(html).toContain(identity(202, 'kf-actor-02'));
  });

  it('says outright that no worker is serving this Actor', () => {
    expect(html).toContain('data-testid="actor-workers-stale-probe"');
    expect(html).toContain('no worker is serving this Actor');
  });

  it('marks it as not a dispatch target, in words', () => {
    expect(html).toContain('not a dispatch target');
  });

  it('says why a dead worker is still listed', () => {
    // Without this the row looks like a bug in the page rather than a fact about Temporal.
    expect(html).toContain('about five minutes');
  });
});

describe('MIXED — one worker live, one dead, on the same queue', () => {
  // THE CASE THE QUEUE-LEVEL SIGNAL CANNOT SEE. `lastPoll` for this queue is fresh, `identities` has
  // two entries, and a reader that trusted either would offer the corpse as readily as the worker.
  const html = draw({ report: report([LIVE, DEAD]) });

  it('reads as serving — the Actor can run', () => {
    expect(stateOf(html)).toBe('serving');
  });

  it('draws each worker in its OWN state, not the queue’s', () => {
    expect(html).toContain('data-testid="actor-worker-probe-0" data-state="serving"');
    expect(html).toContain('data-testid="actor-worker-probe-1" data-state="stale"');
  });

  it('counts them apart rather than reporting “2 workers”', () => {
    expect(html).toContain('data-testid="actor-worker-count-probe"');
    expect(html).toContain('1 serving');
    expect(html).toContain('1 stale');
  });
});

describe('NONE — nobody has been asked, and nothing has been probed', () => {
  it('draws a report that has not arrived as unknown, never as un-served', () => {
    const html = draw({ report: null, now: 0 });
    expect(stateOf(html)).toBe('unknown');
    expect(html).toContain('data-testid="actor-workers-unknown-probe"');
    expect(html).not.toContain('data-testid="actor-workers-none-probe"');
  });

  it('draws an unreachable Temporal as unknown, and shows what it said', () => {
    const html = draw({ report: report([], { error: 'Connection refused: localhost:7233' }) });
    expect(stateOf(html)).toBe('unknown');
    expect(html).toContain('Connection refused');
  });

  it('says nobody looked at the health, rather than showing it as fine', () => {
    // ADR 0020's rule, on this surface: `unknown` is not `ok`, and an Actor with no pane in the
    // Monitor has not been found healthy — nothing has looked at it.
    const html = draw({ machines: [] });
    expect(html).toContain('data-testid="actor-health-probe" data-state="unknown"');
    expect(html).toContain('health: unknown');
    expect(html).not.toContain('data-state="ok"');
  });

  it('says the Actor has no live session rather than drawing an empty list', () => {
    const html = draw({ machines: [] });
    expect(html).toContain('data-testid="actor-sessions-none-probe"');
    expect(html).toContain('no pane running probe');
  });
});

describe('health and sessions, when there IS something to see', () => {
  it('draws ok only when every Machine was measured and is fine', () => {
    const html = draw({ machines: [pane({ id: 'fleet:kf-actor-01/kontra-probe/0' })] });
    expect(html).toContain('data-testid="actor-health-probe" data-state="ok"');
  });

  it('draws bad when a Machine has a failing signal', () => {
    const html = draw({
      machines: [
        pane({
          id: 'fleet:kf-actor-01/kontra-probe/0',
          health: { reachable: 'ok', session: 'present', poller: 'none', loads: 'ok' },
        }),
      ],
    });
    expect(html).toContain('data-testid="actor-health-probe" data-state="bad"');
  });

  it('lists the sessions the worker is running in, with node and address', () => {
    const html = draw({
      machines: [
        pane({ id: 'fleet:kf-actor-01/kontra-probe/0' }),
        pane({ id: 'local:localhost/probe-0_1_0/0', publicIp: '', machine: 'localhost' }),
      ],
    });
    expect(html).toContain('data-testid="actor-session-probe-0"');
    expect(html).toContain('data-testid="actor-session-probe-1"');
    expect(html).toContain('kontra-probe');
    expect(html).toContain('probe-0_1_0');
    expect(html).toContain('10.124.0.5');
    expect(html).toContain('local');
  });
});

describe('the measured window, at the boundary', () => {
  it('draws a worker one millisecond past the window as stale, not as serving', () => {
    // THE ASSERTION THE WHOLE SLICE RESTS ON, at the render layer: Temporal's answer is identical
    // either side of this line, so if the window is ever dropped this is what notices.
    const html = draw({
      report: report([{ identity: identity(1, 'kf-actor-01'), lastPoll: NOW - POLL_FRESH_MS - 1 }]),
    });
    expect(stateOf(html)).toBe('stale');
    expect(html).toContain('not a dispatch target');
  });

  it('draws a worker exactly at the window as serving', () => {
    const html = draw({
      report: report([{ identity: identity(1, 'kf-actor-01'), lastPoll: NOW - POLL_FRESH_MS }]),
    });
    expect(stateOf(html)).toBe('serving');
    expect(html).not.toContain('not a dispatch target');
  });

  it('says `never` for an undated poller rather than an age it does not know', () => {
    const html = draw({ report: report([{ identity: identity(1, 'kf-actor-01'), lastPoll: 0 }]) });
    expect(html).toContain('last poll never');
    expect(stateOf(html)).toBe('stale');
  });
});
