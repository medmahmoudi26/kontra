/**
 * The four readings the chrome makes of "is anything happening at all, anywhere", and the
 * merge behind them.
 *
 * NODE, NO JSDOM, NO FETCH. `readPulse` is props in / reading out, so idle, running, parked, mixed,
 * capped and unreachable are all stated here rather than reproduced through a browser — the same
 * split `sideNav.render.test.ts` relies on, one layer down: that file asserts the rail DRAWS the
 * reading, this one asserts the reading is right.
 *
 * WHAT THESE PIN IS THE SET OF SENTENCES THIS APPLIANCE IS ALLOWED TO SAY ABOUT ITSELF. Three of
 * them are wrong in a way somebody acts on: "idle" over an unreachable cluster, "nothing is
 * waiting" after looking at part of the fleet, and a run still marked as waiting on a human after
 * it has stopped running. Each has its own case below.
 */

import { describe, expect, it } from 'vitest';

import type { Pulse } from '../../run/api';
import { readPulse, type PulsePark } from './pulse';

const NOW = 1_800_000_000_000;

function pulse(over: Partial<Pulse> = {}): Pulse {
  return {
    running: 0,
    parked: 0,
    named: [],
    scanned: over.running ?? 0,
    capped: false,
    at: NOW,
    ...over,
  };
}

function park(runId: string, over: Partial<PulsePark> = {}): PulsePark {
  return { runId, workflow: 'DnsSweep', pending: 1, since: NOW - 60_000, ...over };
}

function read(p: Pulse | null, known: PulsePark[] = [], error: string | null = null) {
  return readPulse({ pulse: p, error, known, now: NOW });
}

describe('before the first answer', () => {
  it('draws nothing — `unread` is not `idle`', () => {
    // Every word available at that moment would be wrong for a fraction of a second, and the rail
    // is on screen for every page load in the app.
    const r = read(null);
    expect(r.tone).toBe('unread');
    expect(r.facts).toEqual([]);
    expect(r.running).toBeNull();
    expect(r.entry).toEqual({ kind: 'none' });
  });
});

describe('idle', () => {
  it('reads as idle, in a word rather than a zero', () => {
    const r = read(pulse());
    expect(r.tone).toBe('idle');
    expect(r.facts).toEqual([
      { key: 'idle', text: 'idle', short: 'idle', value: null, tone: 'idle' },
    ]);
    expect(r.running).toBe(0);
    expect(r.parked).toBe(0);
  });

  it('says the appliance is at rest and NOT that it is unreachable', () => {
    expect(read(pulse()).because).toMatch(/at rest, not unreachable/);
  });

  it('offers nowhere to go, because there is nothing to open', () => {
    expect(read(pulse()).entry).toEqual({ kind: 'none' });
  });
});

describe('running', () => {
  it('reads as running, with the count Temporal gave', () => {
    const r = read(pulse({ running: 4 }));
    expect(r.tone).toBe('running');
    expect(r.facts.map((f) => f.text)).toEqual(['4 running']);
    expect(r.running).toBe(4);
  });

  it('says nothing is waiting on a person — but only when the whole fleet was looked at', () => {
    expect(read(pulse({ running: 3, scanned: 3 })).because).toMatch(/None of them is waiting/);
  });

  it('refuses to say that after a CAPPED look', () => {
    // A partial look presented as an all-clear about the runs it never opened is the failure this
    // whole reading exists to prevent.
    const r = read(pulse({ running: 400, scanned: 64, capped: true }));
    expect(r.because).not.toMatch(/None of them is waiting/);
    expect(r.because).toMatch(/64 of them that were looked at/);
  });

  it('refuses to say it when the index has not caught up either', () => {
    const r = read(pulse({ running: 5, scanned: 2, capped: false }));
    expect(r.because).toMatch(/2 of them that were looked at/);
  });

  it('sends you to the surface — a busy fleet has no single subject', () => {
    expect(read(pulse({ running: 4 })).entry).toEqual({ kind: 'surface' });
  });
});

describe('parked', () => {
  it('reads as parked, which is not the same reading as running', () => {
    const r = read(pulse({ running: 1, parked: 1, scanned: 1, named: [park('nightly')] }));
    expect(r.tone).toBe('parked');
    expect(r.parked).toBe(1);
    expect(r.facts.map((f) => f.text)).toEqual(['1 running', '1 waiting on you']);
  });

  it('reports it BESIDE running, never instead — a busy fleet must not vanish on one question', () => {
    const r = read(pulse({ running: 4, parked: 1, scanned: 4, named: [park('nightly')] }));
    expect(r.running).toBe(4);
    expect(r.parked).toBe(1);
    expect(r.facts).toHaveLength(2);
  });

  it('names the run and how long it has waited, so the mark has somewhere to send you', () => {
    const r = read(
      pulse({ running: 1, parked: 1, scanned: 1, named: [park('nightly', { since: NOW - 300_000 })] })
    );
    expect(r.because).toContain('nightly');
    expect(r.because).toMatch(/the longest for 5m/);
  });

  it('opens THE run when exactly one is waiting, and the surface when several are', () => {
    const one = read(pulse({ running: 2, parked: 1, scanned: 2, named: [park('a')] }));
    expect(one.entry).toEqual({ kind: 'run', runId: 'a' });
    const two = read(
      pulse({ running: 3, parked: 2, scanned: 3, named: [park('a'), park('b')] })
    );
    // Picking the first silently would be a guess, and listing them here would be the retired Runs
    // page rebuilt inside the chrome.
    expect(two.entry).toEqual({ kind: 'surface' });
  });

  it('hands over the longest-waiting first, and sorts an unreadable instant last', () => {
    const r = read(
      pulse({
        running: 3,
        parked: 3,
        scanned: 3,
        named: [
          park('recent', { since: NOW - 1_000 }),
          park('undated', { since: 0 }),
          park('oldest', { since: NOW - 3_600_000 }),
        ],
      })
    );
    expect(r.waiting.map((p) => p.runId)).toEqual(['oldest', 'recent', 'undated']);
  });

  it('says the count is a FLOOR when the scan did not reach every running run', () => {
    const r = read(
      pulse({ running: 300, parked: 1, scanned: 64, capped: true, named: [park('a')] })
    );
    expect(r.because).toMatch(/At least — the scan did not reach every running run/);
  });
});

describe('the two ways a park is learned', () => {
  /* The cluster scan reads memos off Temporal's VISIBILITY index, which is eventually consistent
     for an open run; the page watching a run read the same memo off a describe and knew first. */

  it('carries a park the scan has not seen yet', () => {
    const r = read(pulse({ running: 2, scanned: 2 }), [park('just-parked')]);
    expect(r.tone).toBe('parked');
    expect(r.waiting.map((p) => p.runId)).toEqual(['just-parked']);
  });

  it('counts a run once when both know about it, and lets the PAGE describe it', () => {
    // The page's entry is the more direct read of the same memo, so it wins on the fields.
    const r = read(
      pulse({ running: 1, parked: 1, scanned: 1, named: [park('shared', { pending: 1 })] }),
      [park('shared', { pending: 3 })]
    );
    expect(r.parked).toBe(1);
    expect(r.waiting[0]?.pending).toBe(3);
  });

  it('drops a page-learned park the moment NOTHING is running', () => {
    // The mark outlives the page that set it, deliberately — so without this the entry for a run
    // that has since failed would sit on the rail forever. "Nothing is running" needs no run list
    // to be a proof that nothing is parked.
    const r = read(pulse({ running: 0 }), [park('dead')]);
    expect(r.tone).toBe('idle');
    expect(r.parked).toBe(0);
    expect(r.waiting).toEqual([]);
  });
});

describe('unreachable', () => {
  it('reads as NOT KNOWN, and never as idle', () => {
    const r = read(null, [], 'read the pulse failed: 502 Bad Gateway');
    expect(r.tone).toBe('unknown');
    expect(r.facts.map((f) => f.text)).toEqual(['not known']);
    expect(r.because).toMatch(/not an idle appliance/);
  });

  it('refuses to keep printing the last count as if it were current', () => {
    // A number the rail keeps showing after the appliance stopped answering is a number that gets
    // read as current. The staleness has to be in the VALUE, not only in a tooltip nobody opens.
    const r = readPulse({
      pulse: pulse({ running: 7, scanned: 7 }),
      error: 'connection refused',
      known: [],
      now: NOW + 90_000,
    });
    expect(r.running).toBeNull();
    expect(r.facts.map((f) => f.text)).toEqual(['not known']);
    expect(r.because).toMatch(/Last read 1m 30s ago, when 7 were running/);
  });

  it('keeps telling you a run is waiting on you, because that fact did not come from this route', () => {
    // A page-learned park came from a describe of the run whose transcript was open. An
    // unreachable cluster is no reason to stop telling somebody they are the bottleneck; it is a
    // reason to stop asserting how much is running.
    const r = read(null, [park('nightly')], 'connection refused');
    expect(r.facts.map((f) => f.text)).toEqual(['not known', '1 waiting on you']);
    expect(r.parked).toBe(1);
    expect(r.entry).toEqual({ kind: 'run', runId: 'nightly' });
    expect(r.because).toContain('as of when its transcript was last read');
  });
});

describe('what the rail keeps when it is 48 pixels wide', () => {
  it('gives every fact a micro-label and a number, or a word and no number', () => {
    const busy = read(pulse({ running: 4, parked: 1, scanned: 4, named: [park('a')] }));
    expect(busy.facts.map((f) => [f.short, f.value])).toEqual([
      ['running', 4],
      ['asked', 1],
    ]);
    // `idle` and `not known` are states, not counts — a `0` under either would be a count of
    // something rather than the state it actually is.
    expect(read(pulse()).facts.map((f) => [f.short, f.value])).toEqual([['idle', null]]);
    expect(read(null, [], 'down').facts.map((f) => [f.short, f.value])).toEqual([['runs', null]]);
  });
});
