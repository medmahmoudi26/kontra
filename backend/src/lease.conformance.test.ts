import { readFileSync } from 'node:fs';
import * as path from 'node:path';
import { describe, expect, it } from 'vitest';

import {
  DROP_ACTIVITY,
  HOLD_ACTIVITY,
  leaseId,
  parseLeaseId,
  type FleetLeaseSet,
  type LeaseView,
} from './lease';
import * as leaseActivities from './activities/lease';

/**
 * THE TYPESCRIPT ARM of `conformance/lease.json`.
 *
 * This side is two things at once, which is why it drives two of the corpus's three sections:
 *
 *   • the READER of the lease-id grammar `actorkit.fleet` writes. `parseLeaseId` is what answers
 *     "who is holding this Fleet" out of an id, and a reader that split differently from the writer
 *     would name the wrong Run on the one screen an operator reads when a Fleet will not die.
 *   • the WRITER of the **Lease** workflow wire `cli/lease.go` decodes. Its key set is pinned EXACTLY here,
 *     because a key this side stops sending is one that side is still waiting for.
 *
 * The Python arm is `tests/test_lease_conformance.py` and the Go arm is
 * `cli/lease_conformance_test.go`. No section has one implementation: a value only one language
 * computes is pinned by that language's own test, not by a corpus.
 */

interface Corpus {
  names: { cases: Array<{ name: string; value: string; why: string }> };
  lease_id: {
    cases: Array<{ why: string; holder: string; nonce: string; lease: string; builds?: boolean }>;
  };
  lease_set_wire: {
    keys: {
      envelope: { required: string[]; optional: string[] };
      lease: { required: string[]; optional: string[] };
    };
    golden: { body: FleetLeaseSet };
  };
}

const corpus = JSON.parse(
  readFileSync(path.join(__dirname, '..', '..', 'conformance', 'lease.json'), 'utf8')
) as Corpus;

/**
 * THE GUARD THAT KEEPS THIS FILE FROM PASSING VACUOUSLY. Every describe below loops, and a loop over
 * an empty array reports success for having found nothing — the shape `conformance/README.md` step 3
 * exists to prevent, and the shape half the guards it replaced actually had.
 */
describe('the corpus itself', () => {
  it('carries cases, and still carries the ones that break a naive implementation', () => {
    expect(corpus.lease_id.cases.length).toBeGreaterThan(0);
    expect(corpus.names.cases.length).toBeGreaterThan(0);
    expect(corpus.lease_set_wire.keys.envelope.required.length).toBeGreaterThan(0);
    expect(corpus.lease_set_wire.keys.lease.required.length).toBeGreaterThan(0);

    // A holder containing the separator — without this row a first-separator split passes.
    const sep = corpus.names.cases.find((c) => c.name === 'separator')?.value;
    expect(sep).toBeTruthy();
    expect(corpus.lease_id.cases.some((c) => c.holder.includes(sep as string))).toBe(true);
    // An UNATTRIBUTED lease — without this row an empty holder is never exercised, and that is the
    // state a drifted `holder` key would hide.
    expect(corpus.lease_id.cases.some((c) => c.holder === '')).toBe(true);
    // A parse-only id with no separator at all.
    expect(corpus.lease_id.cases.some((c) => c.builds === false)).toBe(true);
  });
});

describe('the names both sides declare', () => {
  it('are the ones the corpus pins', () => {
    const byName = new Map(corpus.names.cases.map((c) => [c.name, c]));
    expect(HOLD_ACTIVITY).toBe(byName.get('hold_activity')?.value);
    expect(DROP_ACTIVITY).toBe(byName.get('drop_activity')?.value);
    // The separator is not exported as a constant on this side — it is spelled inside `leaseId` and
    // `parseLeaseId` — so it is pinned through the functions, which is the value that matters.
    const sep = byName.get('separator')?.value as string;
    expect(leaseId('a', 'b')).toBe(`a${sep}b`);
  });

  it('name activities that are actually EXPORTED, which is what Temporal registers', () => {
    // A name that matches a constant and not a function is an activity scheduled onto a queue whose
    // worker has never heard of it. That does not error — the task sits there until ScheduleToStart
    // fires, which presents as a Run hanging at the first line of its fleet scope.
    const exported = leaseActivities as unknown as Record<string, unknown>;
    expect(typeof exported[HOLD_ACTIVITY]).toBe('function');
    expect(typeof exported[DROP_ACTIVITY]).toBe('function');
  });
});

describe('the lease id grammar', () => {
  it('builds every case the corpus says is buildable', () => {
    const buildable = corpus.lease_id.cases.filter((c) => c.builds !== false);
    expect(buildable.length).toBeGreaterThan(0);
    for (const c of buildable) {
      expect(leaseId(c.holder, c.nonce), c.why).toBe(c.lease);
    }
  });

  it('parses every case back to the holder and nonce it was built from', () => {
    let sawEmbeddedSeparator = false;
    for (const c of corpus.lease_id.cases) {
      const got = parseLeaseId(c.lease);
      expect(got.holder, c.why).toBe(c.holder);
      expect(got.nonce, c.why).toBe(c.nonce);
      if (c.holder.includes('#')) sawEmbeddedSeparator = true;
    }
    // Asserted here as well as in the corpus check above, because THIS is the loop it protects: a
    // corpus that lost that row would leave this test green against a first-separator split.
    expect(sawEmbeddedSeparator).toBe(true);
  });
});

describe('the Lease-set wire', () => {
  it('carries EXACTLY the keys the corpus pins — this side is the writer', () => {
    // The writer's key set is pinned exactly and the reader's only has to be a subset
    // (`cli/lease_conformance_test.go` holds that half). A key this side stops sending is a key some
    // reader is still waiting for, and `encoding/json` renders it as a zero value rather than an
    // error — which for `holder` is a real state and therefore invisible.
    const golden = corpus.lease_set_wire.golden.body;
    const declared: FleetLeaseSet = {
      fleet: golden.fleet,
      leases: golden.leases,
      destroyed: true,
    };
    expect(Object.keys(declared).sort()).toEqual(
      [...corpus.lease_set_wire.keys.envelope.required, ...corpus.lease_set_wire.keys.envelope.optional].sort()
    );

    const oneLease: LeaseView = { lease: 'a#b', holder: 'a', expiresAt: 1 };
    expect(Object.keys(oneLease).sort()).toEqual(
      [...corpus.lease_set_wire.keys.lease.required, ...corpus.lease_set_wire.keys.lease.optional].sort()
    );
  });

  it('the golden envelope is the SHARED case, with one attributed and one unattributed Lease', () => {
    // The two states, so a drifted `holder` key cannot pass on either side. A golden of only
    // attributed Leases passes the day somebody adopts a Fleet; one of only unattributed Leases
    // passes with a `holder` key that decodes nothing at all.
    const leases = corpus.lease_set_wire.golden.body.leases;
    expect(leases.length).toBe(2);
    expect(leases.filter((l) => l.holder === '')).toHaveLength(1);
    expect(leases.filter((l) => l.holder !== '')).toHaveLength(1);
    for (const l of leases) {
      expect(typeof l.expiresAt).toBe('number');
      // Milliseconds, not seconds. A ten-digit number here would be seconds and would render as
      // 1970 on one side and as a plausible date on the other.
      expect(l.expiresAt).toBeGreaterThan(1e12);
      // Every golden id round-trips through the grammar, so the two sections cannot drift apart.
      expect(parseLeaseId(l.lease).holder).toBe(l.holder);
    }
  });

  it('the golden is sorted the way the **Lease** workflow sorts', () => {
    // A query answer that reorders between two reads reads as churn on a screen an operator
    // refreshes. `#` sorts before an alphanumeric, which is why the unattributed Lease is first —
    // recorded so nobody "fixes" the order and quietly changes the contract.
    const ids = corpus.lease_set_wire.golden.body.leases.map((l) => l.lease);
    expect([...ids].sort()).toEqual(ids);
  });
});
