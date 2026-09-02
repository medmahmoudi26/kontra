/**
 * Search-attribute registration (ADR 0029 §4, issue 03).
 *
 * `KontraTag` is the projection an in-workflow `publish(..., tag=…)` mirrors to over the live
 * window — a fast query path, never the retention truth (that is the Dataset record). For the fast
 * path to exist at all it must be REGISTERED on the namespace, in the next free Keyword slot beside
 * the three kontra already registers. This pins that registration; the "never read as truth" half
 * is a discipline the sweeper (issue 04) and the record store carry, not something this file asserts.
 */

import { describe, expect, it } from 'vitest';

import { registerSearchAttributes } from './visibility';

/** A stand-in Connection that records every addSearchAttributes call — the one seam registration
 *  reaches. Registration must be idempotent and self-healing, so an AlreadyExists is swallowed;
 *  here every add succeeds and we read back which names were offered. */
function fakeConnection(onAdd: (name: string) => void) {
  return {
    operatorService: {
      async addSearchAttributes(req: { searchAttributes: Record<string, unknown> }) {
        for (const name of Object.keys(req.searchAttributes)) onAdd(name);
      },
    },
  } as unknown as Parameters<typeof registerSearchAttributes>[0];
}

describe('registerSearchAttributes', () => {
  it('registers KontraTag beside the other kontra search attributes', async () => {
    const registered: string[] = [];
    await registerSearchAttributes(fakeConnection((n) => registered.push(n)), 'default');

    // KontraTag is the new one this slice adds — without it the live-window query has no column.
    expect(registered).toContain('KontraTag');
    // The three that were already there stay, so the mirror is additive, not a replacement.
    expect(registered).toEqual(
      expect.arrayContaining(['KontraTenant', 'KontraRunId', 'KontraActor', 'KontraTag'])
    );
  });

  it('never throws — a registration hiccup must not break run reads', async () => {
    const flaky = {
      operatorService: {
        async addSearchAttributes() {
          throw new Error('already exists');
        },
      },
    } as unknown as Parameters<typeof registerSearchAttributes>[0];
    await expect(registerSearchAttributes(flaky, 'default')).resolves.toBeUndefined();
  });
});
