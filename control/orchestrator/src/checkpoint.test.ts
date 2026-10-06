/**
 * TypeScript half of the cross-SDK Checkpoint contract.
 *
 * The Go peer (`runtime/go/checkpoint/conformance_test.go`) and the Python peer
 * (`runtime/python/internals/test_checkpoint_conformance.py`) assert the SAME fixture. A divergence
 * means a checkpoint one side writes is one another mis-reads — and because the safe failure is
 * "start over", a drift shows up as work silently re-run or silently skipped rather than as an error.
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import {
  CHECKPOINT_VERSION,
  RangeSet,
  accountedFor,
  decodeCheckpoint,
  resumeFrom,
  type Range,
} from './checkpoint';

/** `src/` -> `orchestrator/` -> `control/` -> repo root. Wrong by one and the fixture disappears. */
const FIXTURE = join(__dirname, '..', '..', '..', 'shared', 'conformance', 'checkpoint.json');

interface Fixture {
  version: number;
  canonical: Array<{ why: string; add: number[]; expect: Range[]; count: number }>;
  members: Array<{ why: string; ranges: Range[]; in: number[]; out: number[] }>;
  wire: Array<{
    why: string;
    batch_id: string;
    commit: number[];
    isolate: number[];
    manifest_ref: string;
    bytes: string;
  }>;
  resume: Array<{
    why: string;
    checkpoint: unknown;
    batch_id: string;
    units: number;
    todo: number[];
    discarded?: boolean;
  }>;
}

const fx = (): Fixture => {
  const parsed = JSON.parse(readFileSync(FIXTURE, 'utf8')) as Fixture;
  // NON-VACUOUS. Every test below loops over one of these, so an empty fixture passes all of them.
  expect(parsed.canonical.length, 'fixture is empty').toBeGreaterThan(0);
  expect(parsed.members.length).toBeGreaterThan(0);
  expect(parsed.resume.length).toBeGreaterThan(0);
  expect(parsed.wire.length).toBeGreaterThan(0);
  return parsed;
};

describe('the range set encodes canonically', () => {
  it('matches the corpus for every insertion order', () => {
    for (const c of fx().canonical) {
      const s = new RangeSet();
      for (const i of c.add) s.add(i);
      expect(s.ranges(), c.why).toEqual(c.expect);
      expect(s.size, `${c.why}: count`).toBe(c.count);
    }
  });

  it('is stable under re-encoding', () => {
    // A set built FROM ranges must encode back to the same ranges, or a checkpoint changes shape
    // every time it passes through a heartbeat.
    for (const c of fx().canonical) {
      const once = new RangeSet(c.expect).ranges();
      expect(once, c.why).toEqual(c.expect);
      expect(new RangeSet(once).ranges(), `${c.why}: second pass`).toEqual(once);
    }
  });

  it('answers membership at the edges', () => {
    for (const c of fx().members) {
      const s = new RangeSet(c.ranges);
      for (const i of c.in) expect(s.has(i), `${c.why}: ${i} in ${JSON.stringify(c.ranges)}`).toBe(true);
      for (const i of c.out) expect(s.has(i), `${c.why}: ${i} not in ${JSON.stringify(c.ranges)}`).toBe(false);
    }
  });
});

describe('resume says what is left to do', () => {
  it('matches the corpus', () => {
    for (const c of fx().resume) {
      expect(resumeFrom(c.checkpoint, c.batch_id, c.units), c.why).toEqual(c.todo);
    }
  });

  it('starts from the beginning on every discard', () => {
    // The `discarded` cases are where resuming would LOSE work, so they are asserted twice: once for
    // the todo list, and once for the property that makes them safe.
    const discarded = fx().resume.filter((c) => c.discarded);
    expect(discarded.length, 'the fixture should carry every discard reason').toBeGreaterThanOrEqual(4);
    for (const c of discarded) {
      const all = Array.from({ length: c.units }, (_, i) => i);
      expect(resumeFrom(c.checkpoint, c.batch_id, c.units), c.why).toEqual(all);
    }
  });

  it('treats no details as a first attempt', () => {
    // `null` must not be mistaken for an empty checkpoint that happens to match.
    for (const nothing of [null, undefined, {}, 'not an object', 42, []]) {
      expect(resumeFrom(nothing, 'b1', 3)).toEqual([0, 1, 2]);
    }
  });
});

describe('decoding refuses what it cannot trust', () => {
  it('refuses a version it does not know', () => {
    for (const v of [0, 2, 99, '1', null, undefined]) {
      expect(decodeCheckpoint({ v, batch_id: 'b', done: [], failed: [] })).toBeNull();
    }
    expect(decodeCheckpoint({ v: CHECKPOINT_VERSION, batch_id: 'b', done: [], failed: [] })).not.toBeNull();
  });

  it('refuses a malformed range rather than reading it as [0,0]', () => {
    // A zero range would claim unit 0 committed, which on the resuming side skips real work.
    for (const done of [[[1]], [[1, 2, 3]], [[]], [{ lo: 1, hi: 2 }], ['nope'], [[1, 'x']], [[1.5, 2]]]) {
      expect(decodeCheckpoint({ v: 1, batch_id: 'b', done, failed: [] }), JSON.stringify(done)).toBeNull();
    }
  });

  it('tolerates missing optional fields without inventing progress', () => {
    const ck = decodeCheckpoint({ v: 1 });
    expect(ck).not.toBeNull();
    expect(ck!.batchId).toBe('');
    expect(ck!.done.ranges()).toEqual([]);
    expect(ck!.failed.size).toBe(0);
    expect(ck!.manifestRef).toBe('');
  });

  /**
   * THE BYTES THE WRITERS PRODUCE, read from the corpus rather than copied from one of them.
   *
   * The orchestrator is a READER — it never writes a checkpoint — so it asserts this direction only.
   * `runtime/go/checkpoint` and `runtime/python/internals/checkpoint.py` assert that their encoders
   * produce these exact strings. Between the three, a drift in any encoder fails here or there, and
   * the failure names the corpus rather than another language's source.
   */
  it('decodes every wire form in the corpus', () => {
    for (const c of fx().wire) {
      const ck = decodeCheckpoint(JSON.parse(c.bytes));
      expect(ck, c.why).not.toBeNull();
      expect(ck!.batchId, c.why).toBe(c.batch_id);
      expect(ck!.manifestRef, c.why).toBe(c.manifest_ref);
      for (const i of c.commit) expect(ck!.done.has(i), `${c.why}: ${i} committed`).toBe(true);
      expect([...ck!.failed].sort((a, b) => a - b), c.why).toEqual([...c.isolate].sort((a, b) => a - b));
    }
  });
});

describe('accountedFor counts a finished Unit however it finished', () => {
  /**
   * An isolated Unit is finished. A denominator that counts only commits never reaches its total on
   * a node that isolated anything, which is how a healthy run reads as stuck forever.
   */
  it('counts commits and isolations together', () => {
    const ck = decodeCheckpoint({ v: 1, batch_id: 'b', done: [[0, 6]], failed: [7, 8, 9] })!;
    expect(accountedFor(ck)).toBe(10);
  });

  it('does not double-count a Unit that is both', () => {
    // Should not happen, but a checkpoint is data from another process: an index in both sets must
    // not make the total exceed the batch.
    const ck = decodeCheckpoint({ v: 1, batch_id: 'b', done: [[0, 2]], failed: [2, 3] })!;
    expect(accountedFor(ck)).toBe(4);
  });

  it('is zero for an empty checkpoint', () => {
    expect(accountedFor(decodeCheckpoint({ v: 1, batch_id: 'b' })!)).toBe(0);
  });
});
