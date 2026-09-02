import { describe, expect, it } from 'vitest';
import { queryCommand } from './dataset';

// The BigInt/struct coercion these tests used to cover moved server-side (`jsonSafe` in
// data/datasets.ts) along with the preview itself — the browser receives JSON now, so there is
// no DuckDB value to coerce here.
describe('queryCommand', () => {
  it('addresses an output dataset by name, version and dispatch — never a run id', () => {
    const cmd = queryCommand({ name: 'crawl4ai', version: '1.0.0', dt: '2026-08-03T17-50-50' });
    expect(cmd).toBe(
      'kontra dataset query crawl4ai --version 1.0.0 --dt 2026-08-03T17-50-50 ' +
        "--sql 'SELECT * FROM crawl4ai LIMIT 100'"
    );
    // The regression this pins: the page used to hand over `kontra explore <uuid>`.
    expect(cmd).not.toMatch(/[0-9a-f]{8}-[0-9a-f]{4}-/);
  });

  it('omits the scope flags a standalone list does not have', () => {
    expect(queryCommand({ name: 'scope_paid' })).toBe(
      "kontra dataset query scope_paid --sql 'SELECT * FROM scope_paid LIMIT 100'"
    );
  });

  it('scopes by whichever coordinate exists', () => {
    const cmd = queryCommand({ name: 'echo', version: '0.1.0' });
    expect(cmd).toContain('--version 0.1.0');
    expect(cmd).not.toContain('--dt');
  });

  it('escapes a quote in the name so the command stays one shell argument', () => {
    expect(queryCommand({ name: "it's" })).toContain("--sql 'SELECT * FROM it''s LIMIT 100'");
  });
});
