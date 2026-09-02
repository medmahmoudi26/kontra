/**
 * The TypeScript arm of the cross-SDK blob-key contract.
 *
 * Go and Python WRITE unit blob keys; this orchestrator READS them to drive the streaming graph
 * cursor. If the three disagree on how a graph node id becomes a `shard=` segment, the cursor
 * matches nothing and the child node completes EMPTY rather than failing — silent data loss.
 *
 * All three assert the same file: conformance/blobkey.json. TypeScript does not build
 * whole keys, so what it must honour is narrower and asserted directly — that the shard it
 * derives is the shard actually present in the key the writers produce.
 */
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import { legacyRunPrefix, runPrefix, shardOf } from './shard';

type Case = { why: string; node: string; run: string; expect: string };
const fixture = JSON.parse(
  readFileSync(join(__dirname, '../../../conformance/blobkey.json'), 'utf8'),
) as { dt: string; cases: Case[] };

describe('shardOf matches the cross-SDK fixture', () => {
  it('has cases to check', () => {
    // A fixture that silently became empty would turn every assertion below into a no-op.
    expect(fixture.cases.length).toBeGreaterThan(0);
  });

  for (const c of fixture.cases) {
    it(c.why, () => {
      const written = /\/shard=([^/]+)\//.exec(c.expect)?.[1];
      expect(written, `no shard= segment in ${c.expect}`).toBeDefined();
      expect(shardOf(c.node)).toBe(written);
    });
  }

  it('derives the run prefix the writers actually use', () => {
    for (const c of fixture.cases) {
      expect(c.expect.startsWith(runPrefix(c.run))).toBe(true);
    }
  });
});

describe('shardOf edge cases the fixture cannot express', () => {
  it('pads so n1 can never prefix-match n10', () => {
    expect(shardOf('n1')).toBe('0001');
    expect(shardOf('n10')).toBe('0010');
    expect(shardOf('n10').startsWith(shardOf('n1'))).toBe(false);
  });

  it('does not treat a blank or space-padded id as numeric', () => {
    // Number('') === 0 and Number(' 7') === 7, so a lenient parse would mis-shard both.
    expect(shardOf('')).toBe('node');
    expect(shardOf('n 7')).toBe('n_7');
  });

  it('neutralises path and glob metacharacters', () => {
    expect(shardOf('a/b')).toBe('a_b');
    expect(shardOf('a=b')).toBe('a_b');
    expect(shardOf('a*b')).toBe('a_b');
  });

  it('keeps the legacy prefix distinct from the hive one', () => {
    expect(runPrefix('r1')).not.toBe(legacyRunPrefix('r1'));
  });
});
