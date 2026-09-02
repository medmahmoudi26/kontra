/**
 * The live row-tail readout, drawn (live-datasets slice 05).
 *
 * `renderToStaticMarkup` for the reason the other render tests record: this suite runs in node with
 * no jsdom, and every assertion is about the text and the `data-phase` in the markup. It pins the
 * three readings an operator must be able to tell apart — a live count with a chunk age, the same
 * count after the stream dropped, and the pre-count "watching" state — because a frozen number and a
 * degraded one looking identical is the exact failure this slice exists to prevent.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import { RowTailReadout } from './LiveRowTail';
import type { RowTailState } from '../datasets/rowTail';

const NOW = 2_000_000;

function draw(state: RowTailState): string {
  return renderToStaticMarkup(createElement(RowTailReadout, { state, now: NOW }));
}

describe('RowTailReadout', () => {
  it('a live count states rows and chunk age, and marks the phase live', () => {
    const html = draw({ snapshot: { rows: 1203, lastChunkAt: NOW - 4_000, at: NOW }, phase: 'live' });
    expect(html).toContain('1,203 rows · last chunk 4s ago');
    expect(html).toContain('data-phase="live"');
  });

  it('a dropped stream reads "stream lost" with the last known count and a degraded phase', () => {
    const html = draw({ snapshot: { rows: 1203, lastChunkAt: NOW - 4_000, at: NOW }, phase: 'degraded' });
    expect(html).toContain('stream lost · last known 1,203 rows');
    expect(html).toContain('data-phase="degraded"');
    // The degraded readout is visibly distinct — an amber tone rather than the muted live one.
    expect(html).toContain('amber');
  });

  it('before a count arrives it says it is watching, not "0 rows"', () => {
    const html = draw({ snapshot: null, phase: 'connecting' });
    expect(html).toContain('watching for rows…');
    expect(html).toContain('data-phase="connecting"');
  });
});
