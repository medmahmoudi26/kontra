import { describe, expect, it } from 'vitest';

import { identityHost, workerIdentity } from './queues';

/**
 * The producer and the parser are one contract, so they are tested as one.
 *
 * EVERY EXPECTATION IS A HAND-WRITTEN LITERAL. Asserting `workerIdentity(1, 'h', 'q')` against
 * `` `${1}@${'h'}@${'q'}` `` would be asserting the implementation against itself — the suite would
 * stay green through any separator change, which is the one change that breaks `identityHost` and
 * every poller listing with it.
 */
describe('workerIdentity', () => {
  it('writes the three-field shape the Go SDK writes by default', () => {
    expect(workerIdentity(531, 'kontra-api', 'kontra-infra')).toBe('531@kontra-api@kontra-infra');
  });

  it('takes a string pid, because a pid read from the environment is one', () => {
    expect(workerIdentity('11', 'kf-dns-01', 'nscheck-0.1.0')).toBe('11@kf-dns-01@nscheck-0.1.0');
  });

  it('keeps the trailing separator for a client, which polls no queue', () => {
    // What the Go SDK writes for a client. `identityHost` must read it without a special case,
    // which the next block asserts.
    expect(workerIdentity(2, 'kontra-api', '')).toBe('2@kontra-api@');
  });
});

describe('identityHost reads what workerIdentity writes', () => {
  /** The real strings this system produces, including the two shapes that predate the producer. */
  const cases: ReadonlyArray<readonly [string, string | undefined]> = [
    // Written by `workerIdentity` now.
    ['531@kontra-api@kontra-infra', 'kontra-api'],
    ['11@kf-dns-01@nscheck-0.1.0', 'kf-dns-01'],
    // A client: empty field three, host still readable.
    ['2@kontra-api@', 'kontra-api'],
    // The PYTHON SDK DEFAULT, which this repo no longer writes but which an older Worker still
    // polling against a new controller does. Attribution must survive the upgrade window.
    ['4147627@kf-desync-01', 'kf-desync-01'],
    // AN EMPTY FIELD ONE IS STILL ATTRIBUTABLE, and deliberately so: the host is field two either
    // way, and a parser that refused this would drop a Worker over a pid it never reads.
    ['@kontra-api@queue', 'kontra-api'],
    // Not a shape we understand. `undefined` means "cannot attribute", which the Monitor draws as
    // `unknown` — never as "not this Machine".
    ['sometext', undefined],
    // Field two present but empty: a host that could not be resolved. `workerid.py` and its Go
    // arms write `unknown` rather than this, so it should not occur — and if it does, it must
    // read as unattributable rather than as a Machine literally named "".
    ['531@@kontra-infra', undefined],
  ];

  for (const [identity, host] of cases) {
    it(`${JSON.stringify(identity)} -> ${JSON.stringify(host)}`, () => {
      expect(identityHost(identity)).toBe(host);
    });
  }

  it('round-trips every host the producer is given', () => {
    // THE PROPERTY, stated once: whatever a Worker calls itself, the Monitor can attribute it.
    for (const host of ['kontra-api', 'kf-dns-01', 'main-droplet']) {
      expect(identityHost(workerIdentity(process.pid, host, 'any-queue'))).toBe(host);
    }
  });
});
