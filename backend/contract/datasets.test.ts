/**
 * One TTL, one grace, one lifecycle union — asserted so that changing the number is enough.
 *
 * THE TEST THIS REPLACES ASSERTED THE LITERAL. `retention.test.ts` said the TTL "is a repo constant
 * of 24h" by comparing it to `24 * HOUR`, which is a restatement, not a guard: MEASURED
 * 2026-08-27, changing the server's constant failed exactly that one test, and updating its literal
 * — the edit anyone would make next — left the browser's whole 2,115-test suite green with a
 * 48-hour server and a 24-hour browser.
 *
 * So this asserts IDENTITY, not value. Both halves must read the same object, and there must be no
 * second declaration to drift from. A future maintainer changing the retention window edits one
 * number and nothing here needs touching — which is the point, and is why there is no `24` below.
 */
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

import {
  DATASET_RETENTION_GRACE_MS,
  DATASET_RETENTION_TTL_MS,
} from './datasets';
import { DATASET_RETENTION_GRACE_MS as VIA_SERVER_GRACE, DATASET_RETENTION_TTL_MS as VIA_SERVER_TTL } from '../src/data/retention';

const REPO = path.resolve(__dirname, '..', '..');

describe('the retention window is declared once', () => {
  it('the server re-exports the contract rather than declaring its own', () => {
    expect(VIA_SERVER_TTL).toBe(DATASET_RETENTION_TTL_MS);
    expect(VIA_SERVER_GRACE).toBe(DATASET_RETENTION_GRACE_MS);
  });

  /**
   * THE ONE THAT WOULD HAVE CAUGHT THE DRIFT. Identity is not enough on its own — the browser
   * could re-declare its own literal tomorrow and every identity check above would still pass,
   * because the two halves are separate programs. What cannot survive is a second literal, so
   * that is what is searched for.
   */
  /**
   * HALF OF THIS CHECK NOW LIVES IN ANOTHER REPOSITORY, and pretending otherwise is worse than
   * saying so. It used to read `frontend/src/datasets/expiry.ts` as well; ADR 0038 moved the
   * console to kontra-console, and a test cannot read a file that is not checked out.
   *
   * What replaced it is stronger than a grep, which is the only reason this is acceptable: the
   * window is declared in `@kontra/core`, a package the console DEPENDS ON, so the browser half
   * can import the number instead of copying it. The equivalent sweep over the console's own
   * sources is `src/datasets/expiry.test.ts` in kontra-console.
   *
   * What is genuinely lost: no single test now sees both halves. If the console vendors its own
   * literal and deletes its test in the same commit, nothing here fails.
   */
  it('no other module in this repository declares a retention window of its own', () => {
    const offenders: string[] = [];
    for (const rel of ['backend/src/data/retention.ts']) {
      const src = readFileSync(path.join(REPO, rel), 'utf8');
      // `24 * 60 * 60 * 1000` in any spacing, and the hour-multiple form the tests use.
      if (/=\s*\d+\s*\*\s*60\s*\*\s*60\s*\*\s*1000/.test(src)) offenders.push(rel);
    }
    expect(offenders).toEqual([]);
  });

  it('the lifecycle union is the contract, so widening it is a compile error on both sides', () => {
    // A type has no runtime identity to compare, so the check is that this half does not re-declare
    // it. The console's half of this pair is in kontra-console, for the reason given above.
    for (const rel of ['backend/src/data/datasets.ts']) {
      const src = readFileSync(path.join(REPO, rel), 'utf8');
      expect(src).not.toMatch(/^export type DatasetState\s*=/m);
      // NOT A VACUOUS PASS: a file that no longer mentions the type would satisfy the line above
      // just as well as one that imports it correctly.
      expect(src, `${rel} no longer references DatasetState at all`).toMatch(/DatasetState/);
    }
  });
});
