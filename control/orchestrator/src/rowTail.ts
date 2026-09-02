/**
 * The live row tail: watch a Run's rows land while it is still `open`, read from the ONE durable
 * path and nowhere else (live-datasets slice 05).
 *
 * A Method pushes each record to its own blob under `units/run=<runId>/…/<sha>.json` the moment it
 * produces it (actorkit `unitstore` — one blob per pushed record), and the caller does not publish
 * those into a named Dataset until the call returns (ADR 0028). So during a long call the Dataset's
 * catalog SUM does not move, yet the durable path is filling up. This tail LISTS that path and
 * reports what it holds — the honest count is the object count, because one blob IS one row.
 *
 * WHY THE DURABLE PATH AND NOTHING ELSE. A side channel where the actor emits a row-count as it
 * produces rows would be smoother and would eventually say 1,203 while the durable path holds 1,187
 * — the same divergence family as a node whose units all failed reporting `completed` with empty
 * output. The count on screen must be a reading of what is actually committed, so there is exactly
 * one producer of it: an S3 LIST of `run=<id>/`. That prefix is `run=`-first for a measured 50× LIST
 * win (memory: blob-key hive layout), which is exactly the index a per-run tail wants.
 *
 * ITS OWN RING AND CAP, not `/api/events`. The state stream's ring (ADR live-state-stream PRD §6,
 * `EVENT_CAP` in history.ts) is capped for small ordered facts; one large row scan on it would evict
 * every `catalog:announced` behind it. This is a separate hub with a separate, smaller per-run ring
 * ({@link ROW_TAIL_RING}) and its emission rate is bounded by the poll interval, not a coalescer.
 *
 * GRANULARITY IS CHUNK, NOT ROW, and the UI says so. A poll produces one snapshot: `{rows,
 * lastChunkAt}`. The tail does not fake a per-row trickle — Apify's storage is row-append, this
 * one is chunked blobs, and pretending otherwise is the same category of lie as a merged status
 * field. The seq is per-run monotonic, so a reconnecting client resumes with `Last-Event-ID`.
 */

import type { ListedObject } from './codec/objectStore';
import { runPrefix } from './codec/shard';

/** What one poll of the durable path found. A full state, never a delta — so a client that missed
 *  intermediate snapshots is caught up by the latest one alone. */
export interface RowTally {
  /** Committed rows: `.json` unit blobs under `units/run=<id>/`. One blob is one pushed record. */
  rows: number;
  /** Epoch ms of the newest blob, or `null` when the store reported no mtime / there are no rows.
   *  `null` is honest absence — the UI shows "unknown", never a fabricated age. */
  lastChunkAt: number | null;
}

/** A tally stamped with the run it is about, a per-run monotonic seq (the SSE `id:`) and when the
 *  poll took it. This is what a subscriber receives. */
export interface RowTailSnapshot extends RowTally {
  runId: string;
  seq: number;
  at: number;
}

/**
 * What a subscriber's sink is handed.
 *
 * `snapshot` is a count. `resync` is the ONE control this stream needs: the client reconnected with
 * a `Last-Event-ID` older than the ring's oldest entry, so its seq base is gone — since every
 * snapshot is full state the recovery is trivial (take the next snapshot), but the client is TOLD
 * rather than left to infer it, the same discipline `stream.ts` applies to `elided`.
 */
export type RowTailEvent =
  | { kind: 'snapshot'; snapshot: RowTailSnapshot }
  | { kind: 'resync'; runId: string };

/** Only `.json` unit blobs are rows; a stray prefix marker or a non-unit key is not counted. */
function isRowBlob(key: string): boolean {
  return key.endsWith('.json');
}

/**
 * The whole truth of "how many rows has this Run committed", as a PURE function of a LIST result.
 *
 * Counting keys (not summing a side-channel counter) is the point: overwriting the same record on a
 * resume writes the same content-addressed key, so it does not double-count, and a LIST is the only
 * reading that cannot drift from what is actually stored. `lastChunkAt` is the newest mtime the
 * store reported; a backing store that reports none (the in-memory test store) yields `null`, which
 * is a real answer — "rows are here, their age is unknown" — not a zero.
 */
export function tallyRows(objects: readonly ListedObject[]): RowTally {
  let rows = 0;
  let lastChunkAt: number | null = null;
  for (const o of objects) {
    if (!isRowBlob(o.key)) continue;
    rows += 1;
    const t = o.lastModified ? o.lastModified.getTime() : null;
    if (t !== null && (lastChunkAt === null || t > lastChunkAt)) lastChunkAt = t;
  }
  return { rows, lastChunkAt };
}

/** Two tallies are the same reading — used to coalesce, so an idle Run mints no new seq and quiet
 *  subscribers stay quiet. */
export function tallyEqual(a: RowTally, b: RowTally): boolean {
  return a.rows === b.rows && a.lastChunkAt === b.lastChunkAt;
}

/** Per-run snapshots kept for `Last-Event-ID` replay. Its OWN cap, deliberately smaller than
 *  `EVENT_CAP` (1000) — a count stream needs only enough history to answer a reconnect, and each
 *  entry is full state so one is almost always enough. */
export const ROW_TAIL_RING = 64;

/** How often a watched Run's durable path is LISTed. This — not a coalescer — is the emission-rate
 *  cap: one snapshot per interval per Run, however many rows landed in between. Interactive but not
 *  chatty, and it bounds the LIST load on the object store. */
export const ROW_TAIL_POLL_MS = 2_000;

/**
 * The most Runs this hub will watch at once — the TOTAL cap, and the one that was missing.
 *
 * MEASURED, NOT GUESSED (`.scratch/post-merge-review/TRIAGE-2026-08-25.md` §7): one unauthenticated
 * client opened 250 streams on 250 FICTIONAL run ids and got 250 pollers — one per distinct id,
 * 750 LISTs in five seconds — because the id is never checked against anything and `subscribe` mints
 * a `RunState` and an interval for any string it has not seen. Teardown was already correct; the cap
 * is what was absent, so this is the fix and not a rewrite.
 *
 * 64 IS ABOVE ANY HONEST APPLIANCE AND FAR BELOW A FLOOD. One watched Run is one LIST every
 * {@link ROW_TAIL_POLL_MS}, so the ceiling is 32 LISTs a second against the object store — a load a
 * 4 GB controller carries — and nobody reading this appliance's surfaces has 64 Runs open at once.
 * A refusal is LOUD ({@link RowTailRefused}) rather than a silently dropped subscription: a stream
 * that quietly never arrives is the failure this whole panel exists to avoid.
 */
export const ROW_TAIL_MAX_RUNS = 64;

/**
 * The most subscribers ONE Run fans out to.
 *
 * A SECOND BOUND BECAUSE THE FIRST ONE HAS A HOLE. `maxRuns` alone is satisfied by a flood that
 * puts every connection on the SAME run id — one poller, one `RunState`, and an unbounded `Set` of
 * sinks the poller walks on every tick. The per-client cap on the route bounds one caller; this
 * bounds all of them together on one id.
 */
export const ROW_TAIL_MAX_SINKS = 64;

/** Why a subscription was refused. Two different sentences because they have two different fixes:
 *  one means the appliance is watching too many Runs, the other means too many readers are on this
 *  one. */
export type RowTailRefusal = 'too-many-runs' | 'too-many-readers';

/** A subscription the hub declined to take, with which cap said no. Thrown rather than returned so
 *  no caller can accept a stream that is not running — a subscribe that silently no-ops is a panel
 *  that waits forever for a first snapshot. */
export class RowTailRefused extends Error {
  readonly refusal: RowTailRefusal;
  constructor(refusal: RowTailRefusal, message: string) {
    super(message);
    this.name = 'RowTailRefused';
    this.refusal = refusal;
  }
}

/** Cancel a scheduled repeat. */
type Cancel = () => void;

export interface RowTailDeps {
  /** LIST the durable path — full keys with size/mtime. The server passes `store.list`; a test
   *  passes its own so it controls exactly what the object store "holds". */
  list: (prefix: string) => Promise<ListedObject[]>;
  /** Injected clock, so a test stamps deterministic `at`s. */
  now?: () => number;
  /** How often to poll a watched Run. */
  pollMs?: number;
  /** Per-run ring size. */
  ringCap?: number;
  /** The TOTAL cap on watched Runs. See {@link ROW_TAIL_MAX_RUNS}. */
  maxRuns?: number;
  /** The cap on subscribers to any ONE Run. See {@link ROW_TAIL_MAX_SINKS}. */
  maxSinks?: number;
  /** Arm a repeating tick and return its canceller. The default fires `tick` ONCE immediately (so the
   *  first count is instant, not one interval late) and then every `ms`. Injected so a test drives
   *  polls by hand — the test scheduler records the tick and fires nothing, which is what keeps the
   *  manual `poll` calls the only producer of snapshots. */
  schedule?: (tick: () => void, ms: number) => Cancel;
  /** Where a poll error goes. Defaults to a no-op — a transient LIST failure keeps the last good
   *  snapshot rather than tearing the stream down; the CLIENT degrades when the socket itself dies. */
  onError?: (runId: string, err: unknown) => void;
}

interface RunState {
  ring: RowTailSnapshot[];
  seq: number;
  last: RowTally | null;
  sinks: Set<(e: RowTailEvent) => void>;
  cancel: Cancel | null;
  polling: boolean;
}

/**
 * One poller per watched Run, fanned out to every subscriber — which is the whole reason two tabs
 * agree. A tab does not poll; it subscribes, and when the single server-side poll produces a
 * snapshot every subscriber of that Run receives the SAME object with the SAME seq at the same
 * instant. No amount of independent client polling can give that (live-state-stream PRD §"What
 * this is not"), and it is the mechanism behind "two tabs see the same counts at the same time".
 *
 * A Run with no subscribers is not polled: the poller starts on the first subscribe and stops on
 * the last unsubscribe, so an idle catalog costs no LISTs.
 *
 * AND IT IS BOUNDED IN BOTH DIRECTIONS, WHICH IT WAS NOT. The run id is caller-supplied and is
 * never checked against anything — `made-up-0` is not a Run and was polled anyway — so "one poller
 * per watched Run" was a promise about intent rather than about arithmetic. {@link ROW_TAIL_MAX_RUNS}
 * bounds how many Runs exist here at once and {@link ROW_TAIL_MAX_SINKS} bounds how many readers pile
 * onto one of them; the route in front of it bounds how many either of those one caller may hold.
 */
export class RowTailHub {
  private readonly runs = new Map<string, RunState>();
  private readonly list: (prefix: string) => Promise<ListedObject[]>;
  private readonly now: () => number;
  private readonly pollMs: number;
  private readonly ringCap: number;
  private readonly maxRuns: number;
  private readonly maxSinks: number;
  private readonly schedule: (tick: () => void, ms: number) => Cancel;
  private readonly onError: (runId: string, err: unknown) => void;
  private closed = false;

  constructor(deps: RowTailDeps) {
    this.list = deps.list;
    this.now = deps.now ?? Date.now;
    this.pollMs = deps.pollMs ?? ROW_TAIL_POLL_MS;
    this.ringCap = deps.ringCap ?? ROW_TAIL_RING;
    this.maxRuns = deps.maxRuns ?? ROW_TAIL_MAX_RUNS;
    this.maxSinks = deps.maxSinks ?? ROW_TAIL_MAX_SINKS;
    this.schedule =
      deps.schedule ??
      ((tick, ms) => {
        tick(); // the first count is instant, not one interval late
        const h = setInterval(tick, ms);
        // Do not keep the process alive for a poller — the server's own lifecycle owns that.
        if (typeof h.unref === 'function') h.unref();
        return () => clearInterval(h);
      });
    this.onError = deps.onError ?? (() => {});
  }

  /**
   * Watch one Run. The sink is called with a seed immediately (the latest snapshot, or a resync when
   * the client's `lastEventId` fell out of the ring), then with each new snapshot the poller mints.
   * Returns the unsubscribe.
   *
   * `lastEventId` is the seq the client last saw (its `Last-Event-ID`). Present and still in the
   * ring → replay everything after it. Present but evicted → `resync`, then the latest. Absent → the
   * latest snapshot as a seed. A brand-new Run with no snapshot yet seeds nothing and waits for the
   * first poll.
   */
  /**
   * Which cap would refuse this subscription right now, or `null` for one that would be taken.
   *
   * SEPARATE FROM `subscribe` SO A ROUTE CAN REFUSE BEFORE IT COMMITS. An SSE reply is hijacked and
   * its head written before any sink exists, so a route that only learned of the cap from a throw
   * would already have promised the client a `200 text/event-stream` it then had to abandon. Asking
   * first lets it answer a status code instead. Nothing awaits between this and the `subscribe` that
   * follows it, so there is no window for the answer to go stale.
   */
  refusalFor(runId: string): RowTailRefusal | null {
    const state = this.runs.get(runId);
    if (!state) return this.runs.size >= this.maxRuns ? 'too-many-runs' : null;
    return state.sinks.size >= this.maxSinks ? 'too-many-readers' : null;
  }

  subscribe(
    runId: string,
    sink: (e: RowTailEvent) => void,
    opts: { lastEventId?: number } = {}
  ): Cancel {
    if (this.closed) throw new Error('row tail hub is closed');
    const refusal = this.refusalFor(runId);
    if (refusal) {
      throw new RowTailRefused(
        refusal,
        refusal === 'too-many-runs'
          ? `already streaming rows for ${this.runs.size} runs (cap ${this.maxRuns})`
          : `already streaming rows for ${runId} to ${this.maxSinks} readers`
      );
    }
    let state = this.runs.get(runId);
    if (!state) {
      state = { ring: [], seq: 0, last: null, sinks: new Set(), cancel: null, polling: false };
      this.runs.set(runId, state);
    }
    state.sinks.add(sink);

    this.seed(state, runId, opts.lastEventId, sink);

    // The first subscriber starts the poller. The default scheduler fires an immediate poll, so a
    // brand-new watch delivers its first count at once rather than one interval late; a subscriber
    // that arrives while the poller is already running is seeded above instead.
    if (state.cancel === null) {
      state.cancel = this.schedule(() => void this.poll(runId), this.pollMs);
    }

    return () => {
      const s = this.runs.get(runId);
      if (!s) return;
      s.sinks.delete(sink);
      if (s.sinks.size === 0) {
        s.cancel?.();
        this.runs.delete(runId);
      }
    };
  }

  private seed(
    state: RunState,
    runId: string,
    lastEventId: number | undefined,
    sink: (e: RowTailEvent) => void
  ): void {
    if (state.ring.length === 0) return; // nothing to seed yet; the first poll will deliver
    const oldest = state.ring[0]!;
    if (lastEventId === undefined) {
      sink({ kind: 'snapshot', snapshot: state.ring[state.ring.length - 1]! });
      return;
    }
    if (lastEventId >= oldest.seq - 1) {
      // The client's base is still in the ring: replay strictly newer snapshots. Since each is full
      // state, replaying only the last would also be correct — but honoring the seq contract keeps
      // the resume identical whether or not the ring happened to coalesce.
      for (const snap of state.ring) if (snap.seq > lastEventId) sink({ kind: 'snapshot', snapshot: snap });
      return;
    }
    // Evicted: the client cannot trust its seq base. Tell it, then hand it the current truth.
    sink({ kind: 'resync', runId });
    sink({ kind: 'snapshot', snapshot: state.ring[state.ring.length - 1]! });
  }

  /**
   * LIST the durable path once and, if what it holds changed, mint a snapshot and fan it out.
   * Public so a test drives it deterministically; the interval calls the same method.
   *
   * Overlap is guarded: a slow LIST that outlasts the interval must not stack a second poll on the
   * first (which would double-count nothing but would pile up promises). A Run mid-poll skips the
   * tick.
   */
  async poll(runId: string): Promise<void> {
    const state = this.runs.get(runId);
    if (!state || state.polling) return;
    state.polling = true;
    try {
      const objects = await this.list(runPrefix(runId));
      const tally = tallyRows(objects);
      if (state.last !== null && tallyEqual(state.last, tally)) return; // coalesced: no new seq
      state.last = tally;
      const snapshot: RowTailSnapshot = { runId, seq: ++state.seq, at: this.now(), ...tally };
      state.ring.push(snapshot);
      if (state.ring.length > this.ringCap) state.ring.shift();
      for (const sink of state.sinks) sink({ kind: 'snapshot', snapshot });
    } catch (err) {
      this.onError(runId, err);
    } finally {
      state.polling = false;
    }
  }

  /** Which Runs are currently watched — for tests and for a health surface. */
  activeRuns(): string[] {
    return [...this.runs.keys()];
  }

  /** Stop every poller. Called on server shutdown. */
  close(): void {
    this.closed = true;
    for (const state of this.runs.values()) state.cancel?.();
    this.runs.clear();
  }
}

/** One SSE frame for a row-tail event: the `id:` carries the seq so the browser echoes it as
 *  `Last-Event-ID` on reconnect; a `resync` carries no id (it is not a resumable position). Kept a
 *  pure function so the wire format is asserted without a socket. */
export function rowTailFrame(event: RowTailEvent): string {
  if (event.kind === 'resync') {
    return `event: resync\ndata: ${JSON.stringify({ runId: event.runId })}\n\n`;
  }
  const s = event.snapshot;
  const data = JSON.stringify({ runId: s.runId, rows: s.rows, lastChunkAt: s.lastChunkAt, at: s.at });
  return `id: ${s.seq}\ndata: ${data}\n\n`;
}
