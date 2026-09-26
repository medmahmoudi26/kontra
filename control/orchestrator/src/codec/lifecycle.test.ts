/**
 * THE `units/` LIFECYCLE RULE — the window it reads, and the prefix it can never leave.
 *
 * This rule replaces a scheduled sweep with one idempotent PUT, for a reason that was measured on
 * SeaweedFS 3.80 rather than assumed: per-object DELETE marks objects dead INSIDE a volume and the
 * `.dat` file does not shrink, so collecting 102,257 unit objects would have punched 7.3 GiB of
 * holes recoverable only by a `volume.vacuum` that needs as much free disk as the volume it rewrites
 * — on the disk that is full. A TTL rule makes the store segregate those writes into their own
 * volumes and unlink them whole.
 *
 * WHAT IS PINNED HERE is the part that can silently go wrong: the parse of the window, and the fact
 * that the rule names exactly one prefix. Both are about deletion, so both fail towards doing
 * nothing. That the STORE honours the rule is not something a unit test can assert — it was verified
 * against the live store instead (`units/…` came back `ttlSec: 604800`, `cas/…` came back
 * `ttlSec: 0`), and that measurement is written into `ensureLifecycle`'s header.
 */

import { describe, expect, it } from 'vitest';

import {
  UNITS_LIFECYCLE_PREFIX,
  UNITS_LIFECYCLE_RULE_ID,
  UNITS_RETENTION_DAYS_ENV,
  unitsLifecycleDays,
} from './objectStore';

const env = (v: string | undefined): NodeJS.ProcessEnv =>
  (v === undefined ? {} : { [UNITS_RETENTION_DAYS_ENV]: v }) as NodeJS.ProcessEnv;

describe('the units lifecycle window', () => {
  it('states nothing when the deployment states nothing', () => {
    // NO RULE rather than a default one. Writing an expiry policy onto somebody's bucket because
    // they did not mention one is the wrong direction for an operation that deletes.
    expect(unitsLifecycleDays(env(undefined))).toBeNull();
    expect(unitsLifecycleDays(env(''))).toBeNull();
    expect(unitsLifecycleDays(env('   '))).toBeNull();
  });

  it('reads a plain number of days', () => {
    expect(unitsLifecycleDays(env('7'))).toBe(7);
    expect(unitsLifecycleDays(env(' 30 '))).toBe(30);
    expect(unitsLifecycleDays(env('90'))).toBe(90);
  });

  it('REFUSES anything under a day instead of flooring it to zero', () => {
    // THE DANGEROUS ROUNDING. S3 expiry is day-granular, so `0.5` floors to 0 — which does not mean
    // "half a day", it means "expire everything immediately", written by somebody who meant the
    // opposite. A sub-day value is a misunderstanding about deletion, so it does nothing.
    expect(unitsLifecycleDays(env('0'))).toBeNull();
    expect(unitsLifecycleDays(env('0.5'))).toBeNull();
    expect(unitsLifecycleDays(env('-7'))).toBeNull();
  });

  it('refuses a value that is not a number at all', () => {
    expect(unitsLifecycleDays(env('soon'))).toBeNull();
    expect(unitsLifecycleDays(env('7d'))).toBeNull();
    expect(unitsLifecycleDays(env('Infinity'))).toBeNull();
    expect(unitsLifecycleDays(env('NaN'))).toBeNull();
  });

  it('truncates a fractional day rather than rejecting it, once it is at least one', () => {
    // 1.9 days is a real intention badly spelled, and the truncation is towards KEEPING data.
    expect(unitsLifecycleDays(env('1.9'))).toBe(1);
    expect(unitsLifecycleDays(env('7.5'))).toBe(7);
  });

  it('names `units/` and nothing else, and the id is stable across boots', () => {
    // THE WHOLE BLAST-RADIUS ARGUMENT. There is no parameter, no caller input and no code path that
    // could point an expiry rule at `cas/`, whose keys are bare content hashes shared across runs
    // and tenants — age there tells you nothing about liveness, and a rule on it deletes blobs live
    // runs still reference.
    expect(UNITS_LIFECYCLE_PREFIX).toBe('units/');
    // A rule id that changed per boot would accumulate near-identical rules instead of updating one.
    expect(UNITS_LIFECYCLE_RULE_ID).toBe('kontra-expire-units');
  });

  it('reads the SAME variable the sweep reads', () => {
    // The store's rule and the in-repo backstop must not be able to disagree about the window. One
    // variable, two readers.
    expect(UNITS_RETENTION_DAYS_ENV).toBe('KONTRA_UNITS_RETENTION_DAYS');
  });
});
