/**
 * Whether the count is still moving — the second question a lifecycle badge does not answer.
 *
 * Pure functions in node, no jsdom: the phase is what decides whether a socket is opened at all, so
 * it is worth pinning away from anything that has to be rendered to be asserted.
 */

import { describe, expect, it } from 'vitest';

import { accrualPhase, accrualWords, accruing } from './accrual';

describe('accrualPhase', () => {
  it('is accruing only when a Run is OPEN and addressable', () => {
    expect(accrualPhase({ state: 'open', runId: 'nscheck-1' })).toBe('accruing');
    expect(accruing({ state: 'open', runId: 'nscheck-1' })).toBe(true);
  });

  it('tells an open Dataset with NO Run apart from one that is accruing', () => {
    // The live tail is addressed BY RUN. A Dataset several Runs wrote, or one whose Run was never
    // stamped, has nothing to subscribe to — and "watching for rows…" forever would read as "nothing
    // has landed yet" when the truth is "nothing is watching".
    expect(accrualPhase({ state: 'open' })).toBe('open-untailed');
    expect(accruing({ state: 'open' })).toBe(false);
    expect(accrualWords('open-untailed').label).not.toBe(accrualWords('accruing').label);
  });

  it('never opens a socket for a sealed, abandoned or lifecycle-less Dataset', () => {
    // Each is its own phase — three answers, not one "not live" — because the count means something
    // different in each: all of it, part of it and final, or unknown.
    expect(accrualPhase({ state: 'sealed', runId: 'r' })).toBe('sealed');
    expect(accrualPhase({ state: 'abandoned', runId: 'r' })).toBe('abandoned');
    expect(accrualPhase({ state: 'none', runId: 'r' })).toBe('none');
    for (const state of ['sealed', 'abandoned', 'none'] as const) {
      expect(accruing({ state, runId: 'r' })).toBe(false);
    }
  });
});

describe('accrualWords', () => {
  it('gives all five phases their own label, so none can be read as another', () => {
    const labels = (['accruing', 'open-untailed', 'sealed', 'abandoned', 'none'] as const).map(
      (p) => accrualWords(p).label
    );
    expect(new Set(labels).size).toBe(labels.length);
  });

  it('says the abandoned count is BOTH partial and final', () => {
    // "final" alone would read as a complete answer; "partial" alone as one still growing. It is the
    // only phase that is both, and the one an operator most needs not to misread.
    const words = accrualWords('abandoned');
    expect(words.label).toContain('final');
    expect(words.label).toContain('partial');
    expect(words.title).toContain('will not grow');
  });

  it('says of an untailable open Dataset that the number is the catalogʼs, not a live one', () => {
    expect(accrualWords('open-untailed').title).toContain('catalog');
  });
});
