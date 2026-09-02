/**
 * The retention reading, and its agreement with the sweep that actually decides.
 *
 * EVERY CASE HERE IS A CASE IN `backend/src/data/retention.test.ts`, deliberately: two
 * implementations of one policy drift silently — the failure mode is a page telling an operator
 * their output is kept while the sweeper collects it — so the precedence, the constants and the
 * words are asserted on this side in the same order the server applies them.
 */

import { describe, expect, it } from 'vitest';

import {
  DATASET_GRACE_MS,
  DATASET_TTL_MS,
  datasetExpiry,
  untilText,
} from './expiry';

const NOW = 1_786_895_804_639;
/** The record's key. Retention is keyed by the **Run**, so a Dataset without one is a different
 *  case entirely — see the last describe. */
const RUN = 'nscheck-1755612727';
/** Anything written before this is past TTL + grace. */
const CUTOFF = NOW - (DATASET_TTL_MS + DATASET_GRACE_MS);

describe('the four safeguards, in the sweep’s own precedence', () => {
  it('never sweeps a TEMPORARY Dataset, however old and however untagged', () => {
    // ADR 0028: a temp is a Run's staging table, removed by asking. `kept-temporary` beats every
    // other reading, which is why it is tested with the facts that would otherwise collect it.
    const reading = datasetExpiry({ temporary: true, updatedAt: CUTOFF - 1, state: 'sealed', runId: RUN }, NOW);
    expect(reading.disposition).toBe('kept-temporary');
    expect(reading.due).toBe(false);
    expect(reading.title).toContain('somebody deletes it');
  });

  it('keeps a TAGGED Dataset past the TTL — the whole meaning of a tag', () => {
    const reading = datasetExpiry({ tags: ['keep'], updatedAt: CUTOFF - 1, state: 'sealed', runId: RUN }, NOW);
    expect(reading.disposition).toBe('kept-tagged');
    expect(reading.due).toBe(false);
  });

  it('keeps an OPEN Dataset, because something may still be appending to it', () => {
    const reading = datasetExpiry({ state: 'open', updatedAt: CUTOFF - 1, runId: RUN }, NOW);
    expect(reading.disposition).toBe('kept-open');
    expect(reading.label).toContain('open');
  });

  it('collects only what is durable, untagged, not open, and past TTL + grace', () => {
    const reading = datasetExpiry({ state: 'sealed', updatedAt: CUTOFF - 1, runId: RUN }, NOW);
    expect(reading.disposition).toBe('collect');
    expect(reading.due).toBe(true);
    // "WOULD", never "will" and never a deletion time: collection happens when a sweep runs, which
    // is not a clock this page owns.
    expect(reading.label).toContain('would be collected');
    expect(reading.label).not.toContain('will');
  });

  it('treats an unknown last write as unknown age, not infinite age', () => {
    // The same refusal the unit sweep makes for an object whose store reported no mtime. Guessing
    // the other way would report a Dataset as collectable on the strength of a missing field.
    const reading = datasetExpiry({ state: 'sealed', updatedAt: 0, runId: RUN }, NOW);
    expect(reading.disposition).toBe('kept-fresh');
    expect(reading.due).toBe(false);
    expect(reading.label).toContain('no write time');
  });
});

describe('the countdown', () => {
  it('says how long a fresh Dataset has, from its LAST WRITE', () => {
    // Written 6h ago, TTL 24h + 6h grace: 24h left. The clock is the last write, never the Run's
    // start — a Dataset being appended to keeps resetting it.
    const reading = datasetExpiry({ state: 'sealed', updatedAt: NOW - 6 * 3_600_000, runId: RUN }, NOW);
    expect(reading.disposition).toBe('kept-fresh');
    expect(reading.label).toBe('would be collected in 24h');
  });

  it('is coarse — hours, then days — because a sweep schedule is', () => {
    expect(untilText(0)).toBe('now');
    expect(untilText(59 * 60_000)).toBe('under an hour');
    expect(untilText(4 * 3_600_000)).toBe('4h');
    expect(untilText(72 * 3_600_000)).toBe('3d');
  });

  it('takes the TTL and grace as arguments, so a preview of another TTL reads the same way', () => {
    // `GET /api/datasets/retention/preview?ttlMs=…` exists for exactly this: asking what a different
    // policy would do. The reading must follow the policy it was handed rather than the default.
    const reading = datasetExpiry({ state: 'sealed', updatedAt: NOW - 1000, runId: RUN }, NOW, {
      ttlMs: 0,
      graceMs: 0,
    });
    expect(reading.disposition).toBe('collect');
  });
});

describe('what the sweep never even enumerates', () => {
  it('never collects an operator-loaded list, however old', () => {
    // `gatherCandidates` skips a standalone list: no Run wrote it, so there is nothing to key a
    // record or a purge on. Reading it as `would be collected` would be this page inventing a
    // policy — and in the one direction that matters, the doomed one.
    const reading = datasetExpiry(
      { kind: 'standalone', state: 'sealed', updatedAt: CUTOFF - 1 },
      NOW
    );
    expect(reading.disposition).toBe('not-a-candidate');
    expect(reading.due).toBe(false);
    expect(reading.label).toBe('not swept');
  });

  it('says a Dataset with no resolved Run is a GAP, not a guarantee', () => {
    // The sweep cannot address it — but the honest sentence is "nothing can decide about this",
    // never "this is kept". A Run stamped later puts it straight back on the clock.
    const reading = datasetExpiry({ state: 'sealed', updatedAt: CUTOFF - 1 }, NOW);
    expect(reading.disposition).toBe('not-a-candidate');
    expect(reading.title).toContain('not a promise that it is kept forever');
  });
});

describe('what a tag changes, in one click', () => {
  it('turns a collectable Dataset into a kept one and back', () => {
    const facts = { state: 'sealed', updatedAt: CUTOFF - 1, runId: RUN };
    expect(datasetExpiry(facts, NOW).disposition).toBe('collect');
    expect(datasetExpiry({ ...facts, tags: ['keep'] }, NOW).disposition).toBe('kept-tagged');
    expect(datasetExpiry({ ...facts, tags: [] }, NOW).disposition).toBe('collect');
  });
});

/**
 * A DATASET SEVERAL RUNS WROTE IS SWEPT — one decision per contributing **Run**.
 *
 * The singular `runId` is deliberately absent the moment two Runs share a partition, and reading it
 * alone drew "not swept" over exactly the largest, longest-lived Datasets — the mirror image of the
 * bug the sweeper itself had (`backend/src/data/retention.ts`, TRIAGE-2026-08-25 §2). The
 * candidate test is the PLURAL set, on both sides.
 */
describe('a partition several Runs wrote', () => {
  it('is a candidate on the strength of the plural set alone', () => {
    const reading = datasetExpiry(
      { state: 'sealed', updatedAt: CUTOFF - 1, runs: ['run-a', 'run-b'] },
      NOW
    );
    expect(reading.disposition).toBe('collect');
    expect(reading.due).toBe(true);
  });

  it('says the reading is per-Run, so a keep is not read as a promise about the others', () => {
    const reading = datasetExpiry(
      { state: 'sealed', updatedAt: CUTOFF - 1, runId: 'run-a', runs: ['run-a', 'run-b'], tags: ['keep'] },
      NOW
    );
    expect(reading.disposition).toBe('kept-tagged');
    expect(reading.title).toContain('2 Runs wrote this Dataset');
  });

  it('says nothing about other Runs when there is only one', () => {
    const reading = datasetExpiry(
      { state: 'sealed', updatedAt: CUTOFF - 1, runId: RUN, runs: [RUN] },
      NOW
    );
    expect(reading.title).not.toContain('Runs wrote this Dataset');
  });

  it('is still NOT a candidate when no authority names a Run at all', () => {
    const reading = datasetExpiry({ state: 'sealed', updatedAt: CUTOFF - 1, runs: [] }, NOW);
    expect(reading.disposition).toBe('not-a-candidate');
  });
});
