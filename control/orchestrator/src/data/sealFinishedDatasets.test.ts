/**
 * The rule that decides whether an `open` Dataset is finished, and which word says so.
 *
 * Pure, so every case is a value — which is the point of splitting the rule from the three lines of
 * I/O. The cases that matter are the ones where the honest answer is "leave it alone": a false
 * `sealed` tells a reader the data is whole, which is worse than the `open` it replaces.
 */

import { describe, expect, it } from 'vitest';

import {
  decideSeal,
  outcomeOfStatus,
  sealFinishedDatasets,
  type DatasetContributors,
  type RunOutcome,
} from './sealFinishedDatasets';

function ds(over: Partial<DatasetContributors> = {}): DatasetContributors {
  return { name: 'canary_signals', state: 'open', runIds: ['r1'], ...over };
}

/** An outcome lookup from a plain map; anything unnamed is unknown to Temporal. */
function outcomes(m: Record<string, RunOutcome>): (r: string) => RunOutcome {
  return (r) => m[r] ?? 'unknown';
}

describe('deciding whether an open Dataset is finished', () => {
  it('seals one whose every contributing Run completed', () => {
    const d = decideSeal(ds({ runIds: ['r1', 'r2'] }), outcomes({ r1: 'completed', r2: 'completed' }));
    expect(d.to).toBe('sealed');
    expect(d.why).toMatch(/all 2 contributing Run/);
  });

  /** `sealed` for a killed Run is the comfortable lie — it claims a deliberate ending. */
  it('abandons one whose Run ended badly', () => {
    const d = decideSeal(ds(), outcomes({ r1: 'failed' }));
    expect(d.to).toBe('abandoned');
  });

  /** A Dataset half of whose contributors were killed is not a complete result. */
  it('abandons when ANY contributor ended badly, even if another completed', () => {
    const d = decideSeal(ds({ runIds: ['ok', 'dead'] }), outcomes({ ok: 'completed', dead: 'failed' }));
    expect(d.to).toBe('abandoned');
  });

  /**
   * THE ONE THAT WOULD DO REAL DAMAGE. One marker covers a whole NAME, and a name is written by
   * many Runs — so sealing because one finished declares a Dataset whole while another is still
   * appending to it.
   */
  it('leaves it open while any contributor is still running', () => {
    const d = decideSeal(ds({ runIds: ['done', 'live'] }), outcomes({ done: 'completed', live: 'running' }));
    expect(d.to).toBeNull();
    expect(d.why).toMatch(/still running/);
    expect(d.why).toMatch(/open is TRUE here/);
  });

  /** "We cannot tell" is not a licence to write `sealed`. */
  it('leaves it alone when a contributor is unknown to Temporal', () => {
    const d = decideSeal(ds({ runIds: ['known', 'ghost'] }), outcomes({ known: 'completed' }));
    expect(d.to).toBeNull();
    expect(d.why).toMatch(/unknown to Temporal/);
  });

  /** ABSENT IS NOT OPEN — `datasets.ts` warns about exactly this confusion. */
  it('never writes a marker where none existed', () => {
    const d = decideSeal(ds({ state: null }), outcomes({ r1: 'completed' }));
    expect(d.to).toBeNull();
    expect(d.why).toMatch(/no writer ever wrote a marker/);
  });

  it('does not re-seal one that is already sealed or abandoned', () => {
    expect(decideSeal(ds({ state: 'sealed' }), outcomes({ r1: 'completed' })).to).toBeNull();
    expect(decideSeal(ds({ state: 'abandoned' }), outcomes({ r1: 'failed' })).to).toBeNull();
  });

  it('leaves one with no recorded contributor alone', () => {
    const d = decideSeal(ds({ runIds: [] }), outcomes({}));
    expect(d.to).toBeNull();
    expect(d.why).toMatch(/no contributing Run recorded/);
  });
});

describe('reading a Temporal status', () => {
  it('maps the terminal statuses onto the right outcome', () => {
    for (const s of ['COMPLETED', 'WORKFLOW_EXECUTION_STATUS_COMPLETED', 'completed']) {
      expect(outcomeOfStatus(s)).toBe('completed');
    }
    for (const s of ['FAILED', 'TERMINATED', 'CANCELED', 'CANCELLED', 'TIMED_OUT']) {
      expect(outcomeOfStatus(s)).toBe('failed');
    }
    expect(outcomeOfStatus('RUNNING')).toBe('running');
    expect(outcomeOfStatus('executing')).toBe('running');
  });

  /** An absent status must never read as a terminal one. */
  it('treats absent and unrecognised as unknown', () => {
    expect(outcomeOfStatus(undefined)).toBe('unknown');
    expect(outcomeOfStatus(null)).toBe('unknown');
    expect(outcomeOfStatus('')).toBe('unknown');
    expect(outcomeOfStatus('CONTINUED_AS_NEW')).toBe('unknown');
  });
});

describe('reconciling a catalog', () => {
  const catalog: DatasetContributors[] = [
    { name: 'finished', state: 'open', runIds: ['a'] },
    { name: 'killed', state: 'open', runIds: ['b'] },
    { name: 'inflight', state: 'open', runIds: ['c'] },
    { name: 'already', state: 'sealed', runIds: ['a'] },
    { name: 'never', state: null, runIds: ['a'] },
  ];
  const look = outcomes({ a: 'completed', b: 'failed', c: 'running' });

  it('a dry run writes nothing and still reports every decision', async () => {
    const wrote: string[] = [];
    const r = await sealFinishedDatasets(catalog, look, async (n) => void wrote.push(n));

    expect(r.applied).toBe(false);
    expect(wrote).toEqual([]);
    expect(r.sealed).toBe(1);
    expect(r.abandoned).toBe(1);
    expect(r.untouched).toBe(3);
    expect(r.decisions).toHaveLength(5);
  });

  it('applying writes exactly the two that changed', async () => {
    const wrote: Array<[string, string]> = [];
    const r = await sealFinishedDatasets(catalog, look, async (n, s) => void wrote.push([n, s]), {
      apply: true,
    });

    expect(r.applied).toBe(true);
    expect(wrote).toEqual([
      ['finished', 'sealed'],
      ['killed', 'abandoned'],
    ]);
  });
});
