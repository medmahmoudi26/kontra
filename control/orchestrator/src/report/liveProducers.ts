/**
 * THE PRODUCERS FOR A LIVE REPORT'S CONTEXT (ADR 0062 §2), and the reason they are a module.
 *
 * ADR 0062 shipped the consumer — `run.progress`, `datasets.<name>`, an open run's `result` — and
 * nothing that produced any of them, so a live report re-rendered a context that could not change and
 * showed one frame and then the final document. See the ADR's correction section. These turn what the
 * orchestrator ALREADY observes into those three values. Each is a pure function of what it is given,
 * so the wiring in `server.ts` is the only place that touches Temporal or the object store.
 */
import type { NodeHeartbeat } from '../heartbeat';
import type { DatasetSummary, ProgressContext } from './context';

/**
 * The `datasets` key for rows a running Method has pushed and its caller has not yet published.
 *
 * NOT A DATASET NAME, AND IT SAYS SO. A Method's pushes are durable blobs from the moment they are
 * pushed, but which Dataset they land in is decided by the caller when the call returns (ADR 0028),
 * so mid-call there is no name to put on them. Calling them by the Dataset they will probably reach
 * would be a guess shown as a fact. Once the batch is published they are in the lake under their real
 * name, and this entry is gone because the run is over.
 */
export const IN_FLIGHT = 'in flight';

/**
 * `run.progress` from the running batches' heartbeats.
 *
 * `units_done` INCLUDES `isolated` — ADR 0060: a unit abandoned after repeated failure is finished,
 * and counting it as outstanding makes a healthy run read as stuck. A heartbeat's checkpoint keeps the
 * two as separate sets (`heartbeatRow`), so they are added here.
 *
 * `undefined` when no batch is running — before the first dispatch, between dispatches, and for a
 * workflow that dispatches nothing. A template then sees `run.progress` as null, which is the truth.
 */
export function progressFromHeartbeats(
  nodes: Readonly<Record<string, NodeHeartbeat>>
): ProgressContext | undefined {
  const rows = Object.values(nodes);
  if (rows.length === 0) return undefined;
  let done = 0;
  let total = 0;
  let isolated = 0;
  let last = 0;
  for (const n of rows) {
    done += n.done;
    total += n.total;
    isolated += n.isolated;
    last = Math.max(last, n.lastBeat);
  }
  return {
    units_done: done + isolated,
    units_total: total,
    isolated,
    phase: rows.length === 1 ? 'one batch running' : `${rows.length} batches running`,
    updated_at: last > 0 ? new Date(last).toISOString() : '',
  };
}

/** What the row tail last saw for a run, kept between its polls. */
export interface InFlightRows {
  /** Pushed records so far: one blob is one record (`rowTail.ts`). */
  rows: number;
  /** Epoch ms of the newest blob, or null when the store gave no mtime. */
  lastChunkAt: number | null;
  /** The newest blobs' parsed contents, oldest first, as the row tail's window holds them. */
  recent: readonly unknown[];
}

/**
 * The in-flight rows as a `DatasetSummary`, for the `datasets` root.
 *
 * A UNIT BLOB IS AN ARRAY OF RECORDS — `[{"target": "alpha", "step": 3, …}]` as stored — so it is
 * flattened here; handed over as is, a template's table would have one column named `0`.
 * `columns` is left empty because `buildContext`'s `clampSummary` derives it, together with the
 * head and tail bounds, for every producer alike.
 */
export function inFlightSummary(seen: InFlightRows): DatasetSummary {
  const records = seen.recent.flatMap((r) => (Array.isArray(r) ? r : [r]));
  return {
    rows: seen.rows,
    batches: 0,
    last_commit_at: seen.lastChunkAt ? new Date(seen.lastChunkAt).toISOString() : '',
    head: records,
    tail: records,
    columns: [],
  };
}
