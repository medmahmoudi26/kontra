/**
 * The live row-tail readout (live-datasets slice 05): a small inline element that shows a Run's rows
 * landing while its Dataset is still `open`.
 *
 * SPLIT ON PURPOSE. {@link RowTailReadout} is a pure function of `{state, now}` so every phase —
 * connecting, live-with-age, live-age-unknown, degraded — is asserted with `renderToStaticMarkup`,
 * no socket and no timer. {@link LiveRowTail} is the thin wrapper that owns the `EventSource` (via
 * `useRowTail`) and a one-second clock so "last chunk 4s ago" actually advances.
 */

import { useEffect, useState } from 'react';
import {
  rowTailDegraded,
  rowTailLabel,
  useRowTail,
  type RowTailState,
} from '../datasets/rowTail';

/** Re-render on a clock so the chunk age counts up. One second is finer than the readout's own grain
 *  ("4s ago"), so the number never visibly lags; a handful of open Datasets is a handful of ticks.
 *
 *  EXPORTED for `DatasetAccrual`, which owns the same pair (a subscription and a clock) one surface
 *  over. Two `setInterval`s counting the same seconds would be two answers to "how old is this
 *  chunk" on one screen. */
export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const h = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(h);
  }, [intervalMs]);
  return now;
}

/**
 * The readout, from state alone. A dot carries the phase at a glance — a steady green while live, an
 * amber ring when the stream is lost — and the label spells it out. The whole thing says CHUNK
 * granularity ("last chunk 4s ago"), never a per-row animation, so it cannot imply a liveness the
 * durable path does not have.
 */
export function RowTailReadout({ state, now }: { state: RowTailState; now: number }): JSX.Element {
  const degraded = rowTailDegraded(state);
  const label = rowTailLabel(state, now);
  return (
    <span
      data-testid="live-rowtail"
      data-phase={state.phase}
      className={`inline-flex items-center gap-1 font-mono text-[10px] tabular-nums ${
        degraded ? 'text-amber-600 dark:text-amber-400' : 'text-muted-foreground'
      }`}
      title={
        degraded
          ? 'The live stream dropped — this is the last count it delivered, not a current one. It resumes on reconnect.'
          : 'Rows committed to the durable path so far, updated per chunk (not per row). The count is a LIST of the run’s blobs — it never diverges from what is stored.'
      }
    >
      <span
        aria-hidden
        className={`inline-block size-1.5 rounded-full ${
          degraded
            ? 'bg-amber-500'
            : state.phase === 'live'
              ? 'bg-emerald-500'
              : 'bg-muted-foreground/50'
        }`}
      />
      {label}
    </span>
  );
}

/**
 * The wired readout: subscribes to `runId`'s durable count while `enabled`, and ticks a clock so the
 * age advances. `enabled` is the caller's gate — a Dataset that is not `open`, or that has no Run to
 * address, opens no socket.
 */
export function LiveRowTail({ runId, enabled }: { runId: string | undefined; enabled: boolean }): JSX.Element | null {
  const state = useRowTail(runId, enabled);
  const now = useNow();
  if (!enabled || !runId) return null;
  return <RowTailReadout state={state} now={now} />;
}
