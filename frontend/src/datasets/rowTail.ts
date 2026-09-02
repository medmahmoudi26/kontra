/**
 * The live row tail on the client (live-datasets slice 05): watch a Run's rows land while its
 * Dataset is still `open`, read from the ONE durable path the server LISTs — `/api/datasets/rows/
 * stream`, its own SSE endpoint, not `/api/events`.
 *
 * GRANULARITY IS CHUNK, NOT ROW, AND THIS SAYS SO. The server reports `{rows, lastChunkAt}` per
 * poll of the durable path, so the readout is "1,203 rows · last chunk 4s ago" — a count and the age
 * of the newest blob, never an animated per-row trickle. Apify's storage is row-append; this one is
 * chunked blobs, and pretending otherwise is the same category of lie as a merged status field.
 *
 * A KILLED STREAM DEGRADES, IT DOES NOT FREEZE. `EventSource` fires `onerror` when the socket drops
 * (server gone, network blip); the phase goes `degraded` and the readout SAYS the stream was lost
 * while still showing the last count it knew — the operator must be able to tell "1,203 and still
 * live" from "1,203 and the pipe is dead", which a frozen number cannot.
 */

import { useEffect, useReducer } from 'react';

/** `VITE_API_BASE` overrides the base (default `/api`), matching `run/api.ts`. */
const BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api';

/** One reading of the durable path, as the SSE `data:` payload carries it. */
export interface LiveRows {
  /** Committed rows: the object count of `units/run=<id>/`. One blob is one pushed record. */
  rows: number;
  /** Epoch ms of the newest chunk, or `null` when the store reported no mtime. */
  lastChunkAt: number | null;
  /** When the server took the reading (epoch ms). */
  at: number;
}

/**
 * `connecting` — the socket is up but no count has arrived yet.
 * `live`       — a snapshot has landed and the stream is healthy.
 * `degraded`   — the socket dropped; the last count is shown but is no longer trusted to be current.
 */
export type RowTailPhase = 'connecting' | 'live' | 'degraded';

export interface RowTailState {
  snapshot: LiveRows | null;
  phase: RowTailPhase;
}

export const ROW_TAIL_START: RowTailState = { snapshot: null, phase: 'connecting' };

export type RowTailInbound =
  | { type: 'open' }
  | { type: 'snapshot'; snapshot: LiveRows }
  | { type: 'error' };

/**
 * Fold one stream event into the readout state. A snapshot is FULL STATE (the server sends a count,
 * not a delta), so it simply replaces the last one and marks the stream live. An error degrades the
 * phase but KEEPS the snapshot — a degraded readout that lost its number would be no better than a
 * frozen one. An `open` after a degrade returns to `connecting` until the next snapshot confirms it.
 */
export function rowTailReduce(state: RowTailState, ev: RowTailInbound): RowTailState {
  switch (ev.type) {
    case 'snapshot':
      return { snapshot: ev.snapshot, phase: 'live' };
    case 'error':
      return { snapshot: state.snapshot, phase: 'degraded' };
    case 'open':
      return state.phase === 'degraded' ? { snapshot: state.snapshot, phase: 'connecting' } : state;
    default:
      return state;
  }
}

/** The age of the newest chunk, at the coarse grain the tail actually knows it. `null` in means the
 *  store reported no mtime, which is an honest "unknown", not "just now". */
export function formatChunkAge(lastChunkAt: number | null, now: number): string | null {
  if (lastChunkAt === null) return null;
  const ms = Math.max(0, now - lastChunkAt);
  if (ms < 1_500) return 'just now';
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.round(m / 60);
  return `${h}h ago`;
}

/**
 * The readout string. Built to be honest at every phase:
 *
 *   - live, with an age:     `1,203 rows · last chunk 4s ago`
 *   - live, age unknown:     `1,203 rows` (the store gave no mtime — no fabricated age)
 *   - connecting, no count:  `watching for rows…`
 *   - degraded:              `stream lost · last known 1,203 rows` (or `stream lost` before any count)
 */
export function rowTailLabel(state: RowTailState, now: number): string {
  const rows = state.snapshot ? state.snapshot.rows.toLocaleString() : null;
  if (state.phase === 'degraded') {
    return rows ? `stream lost · last known ${rows} rows` : 'stream lost';
  }
  if (!state.snapshot) return 'watching for rows…';
  const age = formatChunkAge(state.snapshot.lastChunkAt, now);
  return age ? `${rows} rows · last chunk ${age}` : `${rows} rows`;
}

/** Whether the readout should read as degraded — for the caller to style (a dimmed/warning colour)
 *  rather than the healthy live one. Extracted so the styling decision has one source. */
export function rowTailDegraded(state: RowTailState): boolean {
  return state.phase === 'degraded';
}

/**
 * Subscribe to a Run's live row count over SSE. Returns the readout state; the caller renders it.
 *
 * `EventSource` is deliberate: it gives reconnect and `Last-Event-ID` for free, which is the hard
 * part of resume, and the server's seq rides the `id:` field so a blip resumes rather than reseeds.
 * The subscription is torn down on unmount or when `runId`/`enabled` changes — an idle Dataset opens
 * no socket, so the server polls nothing for a Run nobody is watching.
 */
export function useRowTail(runId: string | undefined, enabled: boolean): RowTailState {
  const [state, dispatch] = useReducer(rowTailReduce, ROW_TAIL_START);
  useEffect(() => {
    if (!enabled || !runId) return;
    const es = new EventSource(`${BASE}/datasets/rows/stream?run=${encodeURIComponent(runId)}`);
    es.onopen = () => dispatch({ type: 'open' });
    es.onmessage = (e) => {
      try {
        const d = JSON.parse(e.data) as { rows: number; lastChunkAt: number | null; at?: number };
        if (typeof d.rows === 'number') {
          dispatch({ type: 'snapshot', snapshot: { rows: d.rows, lastChunkAt: d.lastChunkAt ?? null, at: d.at ?? Date.now() } });
        }
      } catch {
        /* a malformed frame is dropped rather than crashing the tail */
      }
    };
    es.onerror = () => dispatch({ type: 'error' });
    return () => es.close();
  }, [runId, enabled]);
  return state;
}
