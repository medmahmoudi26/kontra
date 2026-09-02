/**
 * The live row-tail readout logic (live-datasets slice 05). Everything here is about NOT lying about
 * what the durable path holds: the label states chunk granularity and last-chunk age, a lost socket
 * degrades rather than freezing on its last number, and a store that reported no mtime shows no age
 * rather than a fabricated "just now".
 */

import { describe, expect, it } from 'vitest';

import {
  formatChunkAge,
  ROW_TAIL_START,
  rowTailDegraded,
  rowTailLabel,
  rowTailReduce,
  type RowTailState,
} from './rowTail';

const NOW = 1_000_000;

function live(rows: number, lastChunkAt: number | null): RowTailState {
  return { snapshot: { rows, lastChunkAt, at: lastChunkAt ?? NOW }, phase: 'live' };
}

describe('formatChunkAge', () => {
  it('renders coarse ages, never a per-second illusion of per-row liveness', () => {
    expect(formatChunkAge(NOW - 500, NOW)).toBe('just now');
    expect(formatChunkAge(NOW - 4_000, NOW)).toBe('4s ago');
    expect(formatChunkAge(NOW - 90_000, NOW)).toBe('2m ago');
    expect(formatChunkAge(NOW - 2 * 3_600_000, NOW)).toBe('2h ago');
  });

  it('is null when the store reported no mtime — an honest unknown, not a zero', () => {
    expect(formatChunkAge(null, NOW)).toBeNull();
  });

  it('clamps a future mtime to just now rather than a negative age', () => {
    expect(formatChunkAge(NOW + 5_000, NOW)).toBe('just now');
  });
});

describe('rowTailLabel — states chunk granularity and last-chunk age', () => {
  it('a healthy count reads "N rows · last chunk Xs ago"', () => {
    expect(rowTailLabel(live(1203, NOW - 4_000), NOW)).toBe('1,203 rows · last chunk 4s ago');
  });

  it('omits the age, not the count, when the age is unknown', () => {
    expect(rowTailLabel(live(1203, null), NOW)).toBe('1,203 rows');
  });

  it('before any count, says it is watching — not "0 rows", which would be a claim', () => {
    expect(rowTailLabel(ROW_TAIL_START, NOW)).toBe('watching for rows…');
  });
});

describe('rowTailReduce — a killed stream degrades, it does not freeze', () => {
  it('a snapshot goes live and replaces the last count (full state, not a delta)', () => {
    const s1 = rowTailReduce(ROW_TAIL_START, { type: 'snapshot', snapshot: { rows: 10, lastChunkAt: NOW, at: NOW } });
    expect(s1).toEqual({ snapshot: { rows: 10, lastChunkAt: NOW, at: NOW }, phase: 'live' });
    const s2 = rowTailReduce(s1, { type: 'snapshot', snapshot: { rows: 25, lastChunkAt: NOW, at: NOW } });
    expect(s2.snapshot!.rows).toBe(25);
  });

  it('an error degrades the phase but KEEPS the last count — a degraded readout that lost its number is no better than a frozen one', () => {
    const liveState = live(1203, NOW);
    const degraded = rowTailReduce(liveState, { type: 'error' });
    expect(degraded.phase).toBe('degraded');
    expect(degraded.snapshot!.rows).toBe(1203);
    expect(rowTailDegraded(degraded)).toBe(true);
  });

  it('a degraded readout SAYS the stream was lost while still showing the last known count', () => {
    const degraded = rowTailReduce(live(1203, NOW), { type: 'error' });
    expect(rowTailLabel(degraded, NOW)).toBe('stream lost · last known 1,203 rows');
  });

  it('degraded before any count says only that it is lost', () => {
    const degraded = rowTailReduce(ROW_TAIL_START, { type: 'error' });
    expect(rowTailLabel(degraded, NOW)).toBe('stream lost');
  });

  it('reopening after a degrade returns to connecting until the next snapshot confirms it', () => {
    const degraded = rowTailReduce(live(5, NOW), { type: 'error' });
    const reopened = rowTailReduce(degraded, { type: 'open' });
    expect(reopened.phase).toBe('connecting');
    // The count survives the blip so the readout is not blank on reconnect.
    expect(reopened.snapshot!.rows).toBe(5);
  });

  it('an open while already live is a no-op — it does not reset a healthy stream', () => {
    const liveState = live(5, NOW);
    expect(rowTailReduce(liveState, { type: 'open' })).toBe(liveState);
  });
});
