/**
 * How long a run took.
 *
 * This is the number an operator asks for after every run and can never reconstruct, and it has one
 * trap: `closedAt` is `0` while the run is open, so the naive subtraction reports a duration of
 * roughly fifty-six years. That is the case these tests exist for.
 */

import { describe, expect, it } from 'vitest';

import { fmtDuration, runDuration } from './api';

describe('runDuration', () => {
  it('measures an OPEN run to now, not to its zero closedAt', () => {
    // `closedAt: 0` is the wire's "still going". Subtracting it would date the run to 1970 and
    // print a duration in decades — on the row an operator reads to decide whether to wait.
    const now = 1_700_000_600_000;
    expect(runDuration({ startedAt: now - 60_000, closedAt: 0 }, now)).toBe(60_000);
  });

  it('measures a CLOSED run to when it closed, and does not keep ticking', () => {
    const started = 1_700_000_000_000;
    const run = { startedAt: started, closedAt: started + 90_000 };
    expect(runDuration(run, started + 90_000)).toBe(90_000);
    // An hour later, the same answer.
    expect(runDuration(run, started + 3_690_000)).toBe(90_000);
  });

  it('says nothing rather than something wrong for a run with no start', () => {
    expect(runDuration({ startedAt: 0, closedAt: 0 }, 1_700_000_000_000)).toBe(0);
  });
});

describe('fmtDuration', () => {
  it('reads as seconds, minutes and hours as the run gets longer', () => {
    expect(fmtDuration(9_000)).toBe('9s');
    expect(fmtDuration(64_000)).toBe('1m 04s');
    expect(fmtDuration(3_720_000)).toBe('1h 02m');
  });

  it('pads, so a column of durations lines up', () => {
    expect(fmtDuration(61_000)).toBe('1m 01s');
  });

  it('draws an unmeasurable duration as an em dash rather than as zero', () => {
    // `0s` is a claim that the run was instant; `—` is the truth, which is that there is nothing to
    // measure yet.
    expect(fmtDuration(0)).toBe('—');
    expect(fmtDuration(-5)).toBe('—');
    expect(fmtDuration(Number.NaN)).toBe('—');
  });
});
