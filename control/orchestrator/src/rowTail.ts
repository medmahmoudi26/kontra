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

import { createHash } from 'node:crypto';

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

/**
 * A bounded look at the rows THEMSELVES — what is landing, not just how much (issue 05).
 *
 * "How many" is most of the way to "is this run doing the right thing" and not all of it. A crawl
 * reporting 1,187 rows is indistinguishable from a crawl reporting 1,187 rows of the same 403 page,
 * and telling them apart cost a navigation to another surface and a query.
 *
 * BOUNDED IN BYTES AS WELL AS IN ROWS, and the byte bound is the one that matters. A count cap
 * alone means a Run whose rows are full HTTP header JSON carries orders of magnitude more per
 * snapshot than one emitting hostnames — so the wide Run would evict the ring, and the panel would
 * cost the most exactly where the rows were least readable. The size comes off the LIST, so an
 * oversized blob is skipped WITHOUT being fetched.
 */
/**
 * One chunk in the window: the stored object, and a stable id for it.
 *
 * THE ID IS A HASH OF THE OBJECT KEY, NOT THE KEY. A client needs identity to accumulate a tail
 * across polls — the window is re-sent whole every poll, so without it every chunk arrives again
 * and a naive append duplicates. The key would serve, but this endpoint's rule is that no
 * object-store path leaves the process; a digest dedupes exactly as well and leaks no layout.
 */
export interface RowChunk {
  id: string;
  row: unknown;
}

export interface RowWindow {
  /** The newest committed rows, oldest-first. Parsed unit objects, exactly as stored. */
  recent: RowChunk[];
  /** True when {@link ROW_TAIL_WINDOW_BYTES} cut the window short of {@link ROW_TAIL_WINDOW} rows.
   *  Surfaced so the UI can say why it is showing three rows instead of five, rather than letting a
   *  wide-rowed Run look like a quiet one. */
  clipped: boolean;
}

/** A tally stamped with the run it is about, a per-run monotonic seq (the SSE `id:`) and when the
 *  poll took it. This is what a subscriber receives. */
export interface RowTailSnapshot extends RowTally {
  runId: string;
  seq: number;
  at: number;
  /**
   * The row window, when this snapshot was just minted — ABSENT ON A REPLAY, deliberately.
   *
   * The ring exists to answer a reconnect and holds {@link ROW_TAIL_RING} entries per Run. Storing
   * a window in each would put `ringCap × ROW_TAIL_WINDOW_BYTES` behind every watched Run — 1 MB
   * each, 64 MB across {@link ROW_TAIL_MAX_RUNS}, on a controller that has 512 MB for everything.
   * So ring entries are stripped to counts and the window rides only the live emission: a client
   * resuming gets the counts it missed and the rows on the next poll, which is two seconds away.
   *
   * That is also why this is optional rather than an empty array. `[]` would say "the window was
   * empty"; absent says "this snapshot is not carrying one", and those are different claims.
   */
  window?: RowWindow;
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
 * 64 IS ABOVE ANY HONEST INSTALL AND FAR BELOW A FLOOD. One watched Run is one LIST every
 * {@link ROW_TAIL_POLL_MS}, so the ceiling is 32 LISTs a second against the object store — a load a
 * 4 GB controller carries — and nobody reading this install's surfaces has 64 Runs open at once.
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

/**
 * How many recent rows a snapshot carries, and the hard byte budget that outranks the count.
 *
 * IT WAS FIVE, and the argument for five was "this answers what is landing, not show me the data".
 * That argument is right about the PURPOSE and was using the wrong lever to enforce it: the reader
 * asked to choose a tail depth and the default they wanted was ten, which five cannot serve at all.
 *
 * FIFTY IS THE CEILING, NOT THE SIZE. The byte budget below is what actually bounds a snapshot, and
 * it is unchanged — a window of fifty narrow rows and a window of five wide ones cost the same 16 KB,
 * because the bound that binds is bytes. Raising the row count therefore buys a deeper tail on cheap
 * rows and changes nothing at all on expensive ones, which is the shape this wanted in the first
 * place. The client picks how many of them to DRAW (default ten); this is the most it may ask for.
 *
 * It is still not "show me the data" — that is the Datasets page, which can page and query.
 *
 * 16 KB IS THE BOUND THAT ACTUALLY BINDS. One crawl row carrying full request/response header JSON
 * is kilobytes on its own, so five of them is not five of a hostname row — without a byte budget
 * the wide Run costs ~100× the narrow one per snapshot, which is precisely the eviction asymmetry
 * the issue names. A row larger than the whole budget is skipped rather than truncated: half a JSON
 * object is not a row, and a window that lies about its contents is worse than a shorter one.
 */
/**
 * A chunk's identity on the wire: 16 hex of sha256 over its object key.
 *
 * Stable for the life of the object, which is what lets a client accumulate across polls, and
 * one-way, so the store's layout does not travel with it.
 */
export function chunkId(key: string): string {
  return createHash('sha256').update(key).digest('hex').slice(0, 16);
}

export const ROW_TAIL_WINDOW = 50;
export const ROW_TAIL_WINDOW_BYTES = 16 * 1024;

/** Why a subscription was refused. Two different sentences because they have two different fixes:
 *  one means the install is watching too many Runs, the other means too many readers are on this
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
  /**
   * READ one unit blob — the same durable path {@link list} counts, and nothing else.
   *
   * OPTIONAL, AND ITS ABSENCE IS A FEATURE: without it the tail behaves exactly as it did before
   * the window existed, counts and all. That is what lets a caller that does not want per-poll GETs
   * (or a test that is only asserting the arithmetic) opt out by simply not passing one.
   *
   * It is the SAME source as the count on purpose. A side channel where the actor pushed rows as it
   * produced them would show a row the durable store does not hold — the divergence family this
   * module's header exists to refuse — so a row may only be shown once its blob is committed and
   * listed.
   */
  get?: (key: string) => Promise<Uint8Array | null>;
  /** Injected clock, so a test stamps deterministic `at`s. */
  now?: () => number;
  /** How often to poll a watched Run. */
  pollMs?: number;
  /** Per-run ring size. */
  ringCap?: number;
  /** Rows in the window. See {@link ROW_TAIL_WINDOW}. */
  windowRows?: number;
  /** Bytes in the window — the bound that outranks the count. See {@link ROW_TAIL_WINDOW_BYTES}. */
  windowBytes?: number;
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
  /** The blobs currently in the window, keyed so a steady stream re-GETs only what is new. ONE per
   *  Run and not per snapshot — see {@link RowTailSnapshot.window} for why the ring has none. */
  cached: Map<string, unknown>;
}

/**
 * The newest row blobs a LIST found, newest LAST, bounded by count and then by bytes.
 *
 * SORTED BY mtime AND THEN BY KEY, because mtime alone is not a total order: a chunk commits many
 * blobs in the same millisecond and S3 reports whole-second granularity on some backends, so ties
 * are the normal case rather than the edge one. Without the key tie-break the window's contents
 * would shuffle between two polls that found identical objects, and the panel would flicker through
 * rows at random. The key is a content hash, so the tie-break is arbitrary but STABLE, which is the
 * property that matters.
 *
 * The byte budget is applied newest-first — a reader wants the latest rows, so when the budget runs
 * out it is the OLDEST of the candidates that is dropped.
 */
export function windowCandidates(
  objects: readonly ListedObject[],
  rows: number,
  bytes: number
): { keys: string[]; clipped: boolean } {
  const blobs = objects.filter((o) => isRowBlob(o.key));
  blobs.sort((a, b) => {
    const at = a.lastModified ? a.lastModified.getTime() : 0;
    const bt = b.lastModified ? b.lastModified.getTime() : 0;
    if (at !== bt) return at - bt;
    return a.key < b.key ? -1 : a.key > b.key ? 1 : 0;
  });

  const picked: string[] = [];
  let used = 0;
  let clipped = false;
  // Newest first for the budget, then reversed — so the window reads oldest-to-newest like a log.
  for (let i = blobs.length - 1; i >= 0 && picked.length < rows; i -= 1) {
    const o = blobs[i]!;
    // The size comes off the LIST, so an oversized blob costs no GET at all. A blob the store did
    // not size is assumed to fit: refusing it would hide rows on a backend that reports no size
    // (the in-memory test store), and the parse below still bounds what is kept.
    const size = typeof o.size === 'number' && o.size > 0 ? o.size : 0;
    if (size > 0 && used + size > bytes) {
      clipped = true;
      break;
    }
    used += size;
    picked.push(o.key);
  }
  picked.reverse();
  return { keys: picked, clipped };
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
  private readonly get: ((key: string) => Promise<Uint8Array | null>) | undefined;
  private readonly now: () => number;
  private readonly pollMs: number;
  private readonly ringCap: number;
  private readonly windowRows: number;
  private readonly windowBytes: number;
  private readonly maxRuns: number;
  private readonly maxSinks: number;
  private readonly schedule: (tick: () => void, ms: number) => Cancel;
  private readonly onError: (runId: string, err: unknown) => void;
  private closed = false;

  constructor(deps: RowTailDeps) {
    this.list = deps.list;
    this.get = deps.get;
    this.now = deps.now ?? Date.now;
    this.pollMs = deps.pollMs ?? ROW_TAIL_POLL_MS;
    this.ringCap = deps.ringCap ?? ROW_TAIL_RING;
    this.windowRows = deps.windowRows ?? ROW_TAIL_WINDOW;
    this.windowBytes = deps.windowBytes ?? ROW_TAIL_WINDOW_BYTES;
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
      state = {
        ring: [],
        seq: 0,
        last: null,
        sinks: new Set(),
        cancel: null,
        polling: false,
        cached: new Map(),
      };
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

      const window = await this.readWindow(state, objects);
      const snapshot: RowTailSnapshot = {
        runId,
        seq: ++state.seq,
        at: this.now(),
        ...tally,
        ...(window ? { window } : {}),
      };
      // STRIPPED ON THE WAY INTO THE RING — see `RowTailSnapshot.window`. The live emission below
      // carries the rows; the replay copy carries the counts, and keeping the window out of 64
      // entries per Run is what keeps this panel's memory a function of watched Runs rather than of
      // how long they have been watched.
      const { window: _dropped, ...stored } = snapshot;
      state.ring.push(stored);
      if (state.ring.length > this.ringCap) state.ring.shift();
      for (const sink of state.sinks) sink({ kind: 'snapshot', snapshot });
    } catch (err) {
      this.onError(runId, err);
    } finally {
      state.polling = false;
    }
  }

  /**
   * Fetch what the window needs and nothing it already has.
   *
   * `null` when no `get` was injected — the tail then behaves exactly as it did before windows
   * existed, which is what keeps this an addition rather than a requirement.
   *
   * A BLOB THAT FAILS TO READ OR PARSE IS SKIPPED, not fatal and not rendered. A unit blob is
   * written whole and content-addressed, so a failure here is a store hiccup or a half-written
   * object — and the one moment a half-written object exists is mid-chunk, which is exactly when
   * somebody is watching. Dropping one row from a five-row window is a far better answer than
   * tearing down the stream that was reporting the Run.
   */
  private async readWindow(
    state: RunState,
    objects: readonly ListedObject[]
  ): Promise<RowWindow | null> {
    const get = this.get;
    if (!get) return null;

    const { keys, clipped } = windowCandidates(objects, this.windowRows, this.windowBytes);
    const recent: RowChunk[] = [];
    const next = new Map<string, unknown>();
    let used = 0;
    for (const key of keys) {
      let unit = state.cached.get(key);
      if (unit === undefined) {
        try {
          const body = await get(key);
          if (!body) continue;
          // The budget again, on what actually arrived: a store that reported no size in the LIST
          // gets checked here instead, so an unsized backend cannot smuggle a megabyte row through.
          if (used + body.byteLength > this.windowBytes) continue;
          used += body.byteLength;
          unit = JSON.parse(Buffer.from(body).toString('utf8'));
        } catch {
          continue; // see the docstring
        }
      }
      next.set(key, unit);
      recent.push({ id: chunkId(key), row: unit });
    }
    // The cache is exactly the window, so it cannot outgrow it: keys that fell out are dropped here
    // rather than accumulating for the life of the Run.
    state.cached = next;
    return { recent, clipped };
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
  // `recent` and `clipped` are LIFTED OUT of the window rather than nested, so the frame stays one
  // flat object — and are OMITTED entirely on a snapshot that carries none (a replay), because
  // `recent: []` would tell a client the Run had committed nothing while the count beside it said
  // otherwise. Absent is the honest shape for "not carrying rows"; see `RowTailSnapshot.window`.
  const data = JSON.stringify({
    runId: s.runId,
    rows: s.rows,
    lastChunkAt: s.lastChunkAt,
    at: s.at,
    ...(s.window ? { recent: s.window.recent, clipped: s.window.clipped } : {}),
  });
  return `id: ${s.seq}\ndata: ${data}\n\n`;
}
