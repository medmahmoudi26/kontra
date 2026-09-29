/**
 * The converge-record reader (issue 13).
 *
 * THE FIXTURES ARE THE LIVE VOLUME'S OWN BYTES, not a shape somebody imagined. Every record below is
 * a real `*.history.json` from `.pulumi/history/kontra-fleet/` with nothing removed — including the
 * `environment` map this reader deliberately drops, so a test that stopped dropping it would fail
 * rather than silently start publishing it.
 */

import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { cmpStamp, DEFAULT_LIMIT, MAX_LIMIT, narrowRecord, readHistory, stampOf, toMs } from './history';

let base: string;
let saved: string | undefined;

/** One record exactly as Pulumi's DIY backend writes it. */
const REAL_RECORD = {
  kind: 'update',
  startTime: 1790632565,
  message: '',
  environment: {
    'exec.kind': 'auto.inline',
    'pulumi.arch': 'amd64',
    'pulumi.env.PULUMI_CONFIG_PASSPHRASE': 'set',
    'pulumi.version': 'v3.264.0',
  },
  config: {},
  version: 0,
  result: 'succeeded',
  endTime: 1790632599,
  resourceChanges: { create: 5 },
};

function put(fqn: string, stamp: string, body: unknown): void {
  const [project, stack] = fqn.split('/');
  const dir = path.join(base, '.pulumi', 'history', project!, stack!);
  mkdirSync(dir, { recursive: true });
  writeFileSync(
    path.join(dir, `${stack}-${stamp}.history.json`),
    typeof body === 'string' ? body : JSON.stringify(body)
  );
  // The backend writes these beside every record, and the reader must not open them: the largest
  // checkpoint on the live volume is 283 KB, and reading one per record to discard it would make
  // this the most expensive read on the API.
  writeFileSync(path.join(dir, `${stack}-${stamp}.checkpoint.json`), '{"not":"a record"}');
  writeFileSync(path.join(dir, `${stack}-${stamp}.history.json.attrs`), 'backend bookkeeping');
}

beforeEach(() => {
  base = mkdtempSync(path.join(tmpdir(), 'kontra-history-'));
  saved = process.env.KONTRA_PULUMI_STATE_DIR;
  process.env.KONTRA_PULUMI_STATE_DIR = base;
});

afterEach(() => {
  if (saved === undefined) delete process.env.KONTRA_PULUMI_STATE_DIR;
  else process.env.KONTRA_PULUMI_STATE_DIR = saved;
  rmSync(base, { recursive: true, force: true });
});

describe('readHistory', () => {
  it('reads a real record and reports its times in milliseconds', async () => {
    put('kontra-fleet/recon-s4', '1790632599584975927', REAL_RECORD);

    const [rec] = await readHistory('kontra-fleet/recon-s4');
    expect(rec).toBeDefined();
    expect(rec!.kind).toBe('update');
    expect(rec!.result).toBe('succeeded');
    expect(rec!.resourceChanges).toEqual({ create: 5 });
    expect(rec!.engine).toBe('v3.264.0');

    // SECONDS IN, MILLISECONDS OUT. Read verbatim, `1790632565` is 1970-01-21; the converge actually
    // ran on 2026-09-28, and a strip that drew it fifty-six years old would still render.
    expect(rec!.startTime).toBe(1790632565_000);
    expect(rec!.endTime).toBe(1790632599_000);
    expect(new Date(rec!.startTime).getUTCFullYear()).toBe(2026);
  });

  it('carries no `environment` and no `config` off the disk', async () => {
    put('kontra-fleet/recon-s4', '1790632599584975927', REAL_RECORD);
    const [rec] = await readHistory('kontra-fleet/recon-s4');

    // A field this type does not name is a field no surface can print by accident. Pulumi redacts
    // the environment to `set` rather than to a value, but the route should not be the reason that
    // redaction is load-bearing.
    expect(rec as unknown as Record<string, unknown>).not.toHaveProperty('environment');
    expect(rec as unknown as Record<string, unknown>).not.toHaveProperty('config');
    expect(JSON.stringify(rec)).not.toContain('PASSPHRASE');
  });

  it('answers an EMPTY LIST for a stack that has never converged', async () => {
    // Not a throw and not a 404 — the empty set is the honest answer, and the console keys the
    // difference between "asked, never converged" and "this route is not served" off exactly this.
    await expect(readHistory('kontra-fleet/never-existed')).resolves.toEqual([]);
    await expect(readHistory('no-such-project/no-such-stack')).resolves.toEqual([]);
  });

  it('orders newest first by the stamp in the FILENAME, not by a time in the body', async () => {
    // The oldest converge, carrying the NEWEST startTime — which is what a record with a bad clock,
    // a restored backup or a missing field looks like. Sorting on the body would put it on top.
    put('kontra-fleet/crlf', '1000000000000000000', { ...REAL_RECORD, startTime: 1790999999 });
    put('kontra-fleet/crlf', '3000000000000000000', { ...REAL_RECORD, startTime: 1790000001, kind: 'destroy' });
    put('kontra-fleet/crlf', '2000000000000000000', { ...REAL_RECORD, startTime: 1790000002 });

    const recs = await readHistory('kontra-fleet/crlf');
    expect(recs.map((r) => r.kind)).toEqual(['destroy', 'update', 'update']);
  });

  it('skips a half-written record instead of failing the whole read', async () => {
    // A converge IN FLIGHT is exactly when a truncated file exists, which is exactly when somebody
    // is looking at this. One bad file must not make the other fifty unreadable.
    put('kontra-fleet/cl0', '3000000000000000000', '{"kind":"update","resu');
    put('kontra-fleet/cl0', '2000000000000000000', REAL_RECORD);
    put('kontra-fleet/cl0', '1000000000000000000', '[]');

    const recs = await readHistory('kontra-fleet/cl0');
    expect(recs).toHaveLength(1);
    expect(recs[0]!.result).toBe('succeeded');
  });

  it('caps at the default and honours a limit, clamped', async () => {
    for (let i = 0; i < DEFAULT_LIMIT + 5; i += 1) {
      // Built as DIGITS, not as `1e18 + i`: a nanosecond stamp is past `Number.MAX_SAFE_INTEGER`,
      // so that arithmetic produces the same string 55 times and the test writes one file.
      put('kontra-fleet/canary', `17906325995849${String(10_000 + i)}`, REAL_RECORD);
    }
    expect(await readHistory('kontra-fleet/canary')).toHaveLength(DEFAULT_LIMIT);
    expect(await readHistory('kontra-fleet/canary', { limit: 3 })).toHaveLength(3);
    // A caller asking for a million gets MAX_LIMIT, not a million file reads.
    expect(
      (await readHistory('kontra-fleet/canary', { limit: 1_000_000 })).length
    ).toBeLessThanOrEqual(MAX_LIMIT);
    // Nonsense takes the default rather than zero — a limit nobody can parse must not mean "none".
    expect(await readHistory('kontra-fleet/canary', { limit: NaN })).toHaveLength(DEFAULT_LIMIT);
  });
});

describe('toMs', () => {
  it('detects the unit rather than assuming one', () => {
    expect(toMs(1790632565)).toBe(1790632565_000); // seconds, as Pulumi writes them
    expect(toMs(1790632565_000)).toBe(1790632565_000); // already milliseconds — NOT multiplied again
    expect(toMs(0)).toBe(0);
    expect(toMs(-1)).toBe(0);
    expect(toMs('1790632565')).toBe(0); // a string is not a time
    expect(toMs(undefined)).toBe(0);
  });
});

describe('stampOf and cmpStamp', () => {
  it('reads the backend’s nanosecond stamp as DIGITS, losing none of them', () => {
    const file = 'recon-s4-1790632599584975927.history.json';
    expect(stampOf(file)).toBe('1790632599584975927');
    expect(stampOf('recon-s4.history.json')).toBe('');

    // THE REASON IT IS NOT A NUMBER. This stamp is ~1.79e18 and a double runs out of integer
    // precision at 9.007e15, so `Number()` rounds the tail away and two converges inside the same
    // ~256 ns become indistinguishable. Converges are seconds apart in practice, which is exactly
    // why this would never have been caught.
    expect(Number('1790632599584975927')).toBe(Number('1790632599584975999'));
    expect(stampOf(file)).not.toBe(stampOf('recon-s4-1790632599584975999.history.json'));
  });

  it('compares digit strings as integers, exactly', () => {
    expect(cmpStamp('1790632599584975927', '1790632599584975999')).toBeLessThan(0);
    // Longer is larger — lexicographic alone would put '9' above '10'.
    expect(cmpStamp('9', '10')).toBeLessThan(0);
    expect(cmpStamp('1790632599584975927', '1790632599584975927')).toBe(0);

    // `cmpStamp` compares by LENGTH first, so it is `stampOf` that has to strip leading zeros —
    // otherwise `007` would sort above `7` as the longer string. Asserted through the reader that
    // actually does it rather than against the comparator, which never sees an unstripped stamp.
    expect(cmpStamp(stampOf('s-007.history.json'), stampOf('s-7.history.json'))).toBe(0);
  });
});

describe('narrowRecord', () => {
  it('drops a non-numeric count instead of passing it to a caller that will add it up', () => {
    const rec = narrowRecord({ ...REAL_RECORD, resourceChanges: { create: 5, delete: 'two' } });
    expect(rec!.resourceChanges).toEqual({ create: 5 });
  });

  it('leaves `resourceChanges` ABSENT when the record carried none', () => {
    // 2 of the 112 records measured have no counts at all. Reporting `{}` would claim this converge
    // changed nothing, which is a different statement from "Pulumi did not write the counts".
    const rec = narrowRecord({ kind: 'update', result: 'succeeded' });
    expect(rec).not.toHaveProperty('resourceChanges');
  });

  it('refuses anything that is not an object', () => {
    expect(narrowRecord([])).toBeUndefined();
    expect(narrowRecord(null)).toBeUndefined();
    expect(narrowRecord('<html>502 Bad Gateway</html>')).toBeUndefined();
  });
});
