/**
 * The cluster pulse: idle, running, parked, and the four ways a partial look must not read as an
 * all-clear.
 *
 * NO CLUSTER AND NO CLOCK. `readPulse` is a total function over plain values and `clusterPulse`
 * takes its two reads as arguments, so every reading below is stated rather than waited for — the
 * same discipline `runActivity.test.ts` follows, and for the same reason: a reading proven against
 * a live Temporal is one proven against whatever that Temporal happened to be doing.
 *
 * WHAT THESE PIN THAT A ROUTE TEST CANNOT is the arithmetic the chrome is about to draw a word
 * from. `running` counts parked runs because Temporal does; `parked` is a FLOOR the moment the scan
 * is capped; a memo that has not reached the visibility index yet is an unknown and not a zero. Get
 * any of those backwards and the rail says "3 running" over a fleet that is entirely blocked on a
 * person, which is the failure this whole surface exists to end.
 */

import { describe, expect, it } from 'vitest';

import { ASK_MEMO_PREFIX } from './hitl';
import {
  NAMED_PARKED_CAP,
  OPEN_SCAN_CAP,
  clusterPulse,
  readPulse,
  type OpenRunEvidence,
} from './pulse';

const NOW = 1_800_000_000_000;

/** One memo entry as `kontra.hitl` writes it. */
function ask(id: string, over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    [`${ASK_MEMO_PREFIX}${id}`]: {
      id,
      prompt: `Approve ${id}?`,
      askedAt: NOW - 60_000,
      state: 'pending',
      schema: { type: 'object' },
      ...over,
    },
  };
}

function run(runId: string, memo: unknown = {}, type = 'NightlySweep'): OpenRunEvidence {
  return { runId, type, memo };
}

function pulse(open: OpenRunEvidence[], over: { running?: number; capped?: boolean } = {}) {
  return readPulse({
    running: over.running ?? open.length,
    open,
    capped: over.capped ?? false,
    now: NOW,
  });
}

describe('the pulse', () => {
  it('reads idle as idle — nothing running, nothing scanned, nothing invented', () => {
    const p = pulse([]);
    expect(p).toMatchObject({ running: 0, parked: 0, scanned: 0, capped: false });
    expect(p.named).toEqual([]);
    // The instant is the SERVER's, and it is carried even on an empty reading: the browser measures
    // "how long has this been waiting" against it, and a resting cluster still has to timestamp
    // the look it took.
    expect(p.at).toBe(NOW);
  });

  it('reads running as running, and says nothing is waiting on a person', () => {
    const p = pulse([run('sweep-1'), run('sweep-2'), run('sweep-3')]);
    expect(p.running).toBe(3);
    expect(p.parked).toBe(0);
    expect(p.named).toEqual([]);
    expect(p.scanned).toBe(3);
  });

  it('reads parked as parked, and names the run so the mark has somewhere to send you', () => {
    const p = pulse([run('nightly-sweep', ask('approve'), 'NightlySweep')]);
    expect(p.parked).toBe(1);
    expect(p.named).toEqual([
      { runId: 'nightly-sweep', workflow: 'NightlySweep', pending: 1, since: NOW - 60_000 },
    ]);
  });

  it('counts a parked run as RUNNING TOO, because Temporal does', () => {
    // The one arithmetic a surface must not get wrong. A workflow blocked on a human is Running to
    // Temporal, so a pulse that quietly subtracted it would disagree with `temporal workflow list`
    // and with the run list on the same box. The subtraction belongs to the reader, who knows
    // whether the scan saw the whole fleet.
    const p = pulse([run('a', ask('q'))]);
    expect(p.running).toBe(1);
    expect(p.parked).toBe(1);
  });

  it('reads a MIXED fleet as both facts at once — three grinding, one waiting on you', () => {
    const p = pulse([
      run('sweep-1'),
      run('sweep-2'),
      run('nightly', ask('approve'), 'NightlySweep'),
      run('sweep-3'),
    ]);
    expect(p.running).toBe(4);
    expect(p.parked).toBe(1);
    expect(p.named.map((n) => n.runId)).toEqual(['nightly']);
    expect(p.scanned).toBe(4);
    expect(p.capped).toBe(false);
  });

  it('counts a run once however many questions it has open, and reports the questions', () => {
    // The chrome's mark answers "which conversation needs me", so the unit is the RUN. The ask
    // count rides along because "1 run, 3 questions" and "1 run, 1 question" are different amounts
    // of work for the person about to be sent there.
    const p = pulse([run('branchy', { ...ask('a'), ...ask('b'), ...ask('c') })]);
    expect(p.parked).toBe(1);
    expect(p.named[0]?.pending).toBe(3);
  });

  it('is not parked by an ask the run has already settled', () => {
    for (const state of ['answered', 'expired', 'abandoned']) {
      const p = pulse([run('done', ask('q', { state }))]);
      expect(p.parked, `${state} must not park a run`).toBe(0);
    }
  });

  it('ignores memo entries that are not asks, and survives one that is not readable as an ask', () => {
    // An unreadable entry is still an ask — `hitl.ts` keeps and marks the row rather than dropping
    // it, because the run IS parked either way. What must not happen is the whole pulse throwing on
    // one malformed memo and taking a healthy cluster's reading down with it.
    const p = pulse([
      run('tagged', { 'kontra.tenant': 'acme', note: 'not an ask' }),
      run('broken', { [`${ASK_MEMO_PREFIX}oops`]: 'this is a string, not an ask' }),
    ]);
    expect(p.parked).toBe(1);
    expect(p.named.map((n) => n.runId)).toEqual(['broken']);
  });

  it('hands over the LONGEST-waiting question first, and sorts an unreadable instant last', () => {
    const p = pulse([
      run('recent', ask('r', { askedAt: NOW - 1_000 })),
      run('undated', ask('u', { askedAt: 0 })),
      run('oldest', ask('o', { askedAt: NOW - 3_600_000 })),
    ]);
    // `since: 0` is an entry with no readable instant, which is an UNKNOWN and not the oldest park
    // on the cluster — dating it to 1970 would put it at the front of a priority order it has no
    // claim on, and put "waiting 56 years" in the chrome.
    expect(p.named.map((n) => n.runId)).toEqual(['oldest', 'recent', 'undated']);
    expect(p.named[2]?.since).toBe(0);
  });

  it('names at most a handful, and keeps counting past it', () => {
    const open = Array.from({ length: 20 }, (_, i) =>
      run(`parked-${i}`, ask('q', { askedAt: NOW - i * 1000 }))
    );
    const p = pulse(open);
    // THE COUNT IS THE ANSWER AND THE NAMES ARE THE WAY IN. A response that named all twenty would
    // be the retired Runs list wearing a different route.
    expect(p.parked).toBe(20);
    expect(p.named).toHaveLength(NAMED_PARKED_CAP);
    expect(p.named[0]?.runId).toBe('parked-19');
  });

  it('says when the scan fell short, so the parked count reads as a FLOOR', () => {
    // A capped scan that reported "1 parked" as a total would be a partial look presented as an
    // all-clear about the runs it never opened.
    const p = pulse([run('a', ask('q')), run('b')], { running: 400, capped: true });
    expect(p.running).toBe(400);
    expect(p.scanned).toBe(2);
    expect(p.capped).toBe(true);
    expect(p.parked).toBe(1);
  });

  it('does not turn an index that has not caught up into an all-clear', () => {
    // An open run's memo reaches visibility asynchronously, so a run that parked a second ago is
    // scanned with an empty memo. `scanned` below `running` is the same signal as `capped` and the
    // surface reads them the same way: this is a look at part of the fleet.
    const p = pulse([run('a'), run('b')], { running: 5 });
    expect(p.parked).toBe(0);
    expect(p.scanned).toBeLessThan(p.running);
  });
});

describe('the pulse, wired to its two reads', () => {
  it('takes the count from the COUNT and never from the scan', async () => {
    // A headline derived by counting the scan would cap itself at OPEN_SCAN_CAP: a controller with
    // 200 runs in flight would read "64 running" forever, and quietly-wrong numbers get believed.
    const p = await clusterPulse({
      count: async () => 200,
      open: async (cap) => ({
        runs: Array.from({ length: cap }, (_, i) => run(`r-${i}`)),
        capped: true,
      }),
      now: () => NOW,
    });
    expect(p.running).toBe(200);
    expect(p.scanned).toBe(OPEN_SCAN_CAP);
    expect(p.capped).toBe(true);
  });

  it('asks only for RUNNING runs — a controller full of history must not be scanned', async () => {
    const asked: Array<{ status?: string }> = [];
    await clusterPulse({
      count: async (filter) => {
        asked.push(filter);
        return 0;
      },
      open: async () => ({ runs: [], capped: false }),
      now: () => NOW,
    });
    expect(asked).toEqual([{ status: 'running' }]);
  });

  it('takes its two reads concurrently — this sits behind a poll every surface pays for', async () => {
    let open = false;
    let overlapped = false;
    await clusterPulse({
      count: async () => {
        await tick();
        overlapped = open;
        return 0;
      },
      open: async () => {
        open = true;
        await tick();
        open = false;
        return { runs: [], capped: false };
      },
      now: () => NOW,
    });
    expect(overlapped).toBe(true);
  });
});

function tick(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 1));
}
