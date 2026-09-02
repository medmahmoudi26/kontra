import { describe, expect, it } from 'vitest';
import { datasetBadge, datasetState, DATASET_STATES } from './state';

describe('a Dataset that has not been sealed never reads as finished', () => {
  it('carries every state the server can record through unchanged', () => {
    // The words are a four-way contract with no shared code (backend/src/data/datasets.ts,
    // and both SDK writers). If this build stopped recognising one of them, the badge would be
    // wrong forever and nothing would fail.
    for (const state of DATASET_STATES) expect(datasetState({ state })).toBe(state);
  });

  it('never promotes an unrecognised value to sealed', () => {
    // A wire from a NEWER peer: say this build cannot tell, rather than guessing "done".
    expect(datasetState({ state: 'quiesced' })).toBe('none');
  });

  it('reads a Dataset with no recorded lifecycle as `none`, not `open`', () => {
    // Not a nit: `open` claims a Run may still be appending. Every dataset materialized before
    // §11, and every operator-loaded list, has no writer and never will — labelling those
    // "open" is the same lie as calling a partial Dataset finished, pointed the other way.
    expect(datasetState({})).toBe('none');
  });
});

describe('the states are told apart on sight', () => {
  it('gives each state, and the absence of one, its own label and styling', () => {
    const drawn = [...DATASET_STATES, 'none' as const].map(datasetBadge);
    expect(new Set(drawn.map((b) => b.label)).size).toBe(drawn.length);
    expect(new Set(drawn.map((b) => b.className)).size).toBe(drawn.length);
    // Every badge carries the sentence an operator needs; a bare word is what made `completed`
    // on an empty run readable as success.
    for (const b of drawn) expect(b.title.length).toBeGreaterThan(20);
  });
});
