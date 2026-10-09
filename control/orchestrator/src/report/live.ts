/**
 * LIVE REPORT MODE (ADR 0062) — one renderer, two modes, and the state that makes the second one
 * cost what the first one does.
 *
 * ── THE RULE THE WHOLE MODULE EXISTS TO HOLD ───────────────────────────────────────────────────
 *
 * ONE RENDER PER DATA CHANGE PER RUN, multicast to every viewer. N open tabs must not cost N
 * renders or N lake reads, and {@link LiveSession.renders} / {@link LiveSession.reads} are public so a
 * test can assert that rather than trust it.
 *
 * ── LIVE RENDERS ARE NEVER PERSISTED, AND THAT IS NOT A PERFORMANCE CHOICE ─────────────────────
 *
 * `report_version` carries `UNIQUE (run_id, render_key)` and `renderKey` hashes the context's data,
 * so a run whose `result` moves every tick would mint a version per tick and the version dropdown
 * would become the tick history. Nothing here calls `declareVersion` or `putSecrets`. The single
 * stored version is rendered at the end, from the final context, by the path that always did it.
 *
 * ── NO reveal/raw WHILE LIVE ───────────────────────────────────────────────────────────────────
 *
 * `/report/blocks/:blockId/reveal` defaults to the LATEST STORED version when `?version=` is absent,
 * and `blockId` is POSITIONAL and exists only on `{% code %}` nodes. So a reveal against a live
 * block would serve a persisted version's block of the same ordinal — a cross-block disclosure, not
 * a 404. Live snapshots therefore ship their blocks as markers and the byte routes are untouched.
 */

import { createHash } from 'node:crypto';

import type { MdNode, ReportSnapshot, SnapshotBlock } from './render';

/** A block over this serialises as a marker instead. Bounds one frame, not the whole document. */
export const BLOCK_BYTE_MAX = 1024 * 1024;

/**
 * Caps. Two numbers from two places, on purpose.
 *
 * 50 per run is the specification's, honoured as written. The global arms are 64, which is
 * `rowTail`'s precedent — and they exist because 50-per-run ALONE is satisfied by a flood that puts
 * every connection on one id, and by a flood spread over ids that do not exist. `rowTail.ts` records
 * the measured incident: one unauthenticated client opened 250 streams on 250 FICTIONAL run ids and
 * got 250 pollers, 750 LISTs in five seconds, because the id was never validated. Hence
 * {@link LiveHub.open} takes an already-validated run and the caps are checked before any per-run
 * state is minted.
 */
export const MAX_VIEWERS_PER_RUN = 50;
export const MAX_LIVE_RUNS = 64;
export const MAX_LIVE_VIEWERS = 256;

/** Coalescing window. A burst of commits inside it costs one render. */
export const DEBOUNCE_MS = 500;

export interface LiveFrame {
  /** Index into `snapshot.root.children` — the block list the client holds. */
  index: number;
  node: MdNode;
}

export type LiveEvent =
  | { type: 'snapshot'; blocks: LiveFrame[]; warnings?: string[]; degraded?: string }
  | { type: 'patch'; blocks: LiveFrame[] }
  | { type: 'status'; status: string }
  | { type: 'final'; version: number };

export type Viewer = (event: LiveEvent) => void;

/** A run whose id and execution have already been validated against Temporal. */
export interface LiveRunKey {
  runId: string;
  runStartedAt: number;
}

/** `runId` alone is NOT a key: ids are reused and collapse to the newest execution. */
export function liveKey(k: LiveRunKey): string {
  return `${k.runId}\u0000${k.runStartedAt}`;
}

function sha256(s: string): string {
  return createHash('sha256').update(s).digest('hex');
}

/** Every `blockId` reachable from one top-level child, so its bytes are part of its identity. */
function referencedBlockIds(node: unknown, out: Set<string>): void {
  if (node === null || typeof node !== 'object') return;
  const n = node as { blockId?: unknown; children?: unknown };
  if (typeof n.blockId === 'string') out.add(n.blockId);
  if (Array.isArray(n.children)) for (const child of n.children) referencedBlockIds(child, out);
}

/**
 * The identity of one top-level block.
 *
 * THE SUBTREE ALONE IS NOT ENOUGH. A `code` node carries a `blockId` indexing a SIBLING map
 * (`snapshot.blocks`) that holds the actual bytes, so a block whose bytes changed while its node
 * stayed identical would hash as unchanged and never be patched. The referenced entries are folded
 * in, sorted so the order is the id's and not the walk's.
 */
export function blockHash(child: MdNode, blocks: Record<string, SnapshotBlock>): string {
  const ids = new Set<string>();
  referencedBlockIds(child, ids);
  const referenced = [...ids].sort().map((id) => [id, blocks[id] ?? null] as const);
  return sha256(JSON.stringify([child, referenced]));
}

export function blockHashes(snapshot: ReportSnapshot): string[] {
  const children = (snapshot.root as { children?: MdNode[] }).children ?? [];
  return children.map((c) => blockHash(c, snapshot.blocks));
}

/** Replace a block whose serialised form is enormous, so one block cannot blow a frame. */
export function boundBlock(node: MdNode): MdNode {
  const json = JSON.stringify(node);
  if (json.length <= BLOCK_BYTE_MAX) return node;
  return {
    type: 'paragraph',
    children: [
      {
        type: 'text',
        value: `[this block is ${json.length} bytes and is not streamed live — it is in the stored report]`,
      },
    ],
  } as unknown as MdNode;
}

export function framesOf(snapshot: ReportSnapshot, indices?: readonly number[]): LiveFrame[] {
  const children = (snapshot.root as { children?: MdNode[] }).children ?? [];
  const want = indices ?? children.map((_, i) => i);
  return want
    .filter((i) => children[i] !== undefined)
    .map((i) => ({ index: i, node: boundBlock(children[i]!) }));
}

/**
 * Which blocks changed, or `null` meaning "the document is a different shape, send it whole".
 *
 * A CHANGED CHILD COUNT IS NOT DIFFABLE BY INDEX. One more row in a `{% for %}` shifts every block
 * after it, so an index-keyed patch would overwrite the wrong ones. The specification already asks
 * for the whole-document fallback and this is where it is decided.
 */
export function changedBlocks(prev: readonly string[], next: readonly string[]): number[] | null {
  if (prev.length !== next.length) return null;
  const out: number[] = [];
  for (let i = 0; i < next.length; i++) if (next[i] !== prev[i]) out.push(i);
  return out;
}

export interface RenderOnce {
  (key: LiveRunKey): Promise<{ snapshot: ReportSnapshot; warnings?: string[] } | { error: string }>;
}

export interface LiveSessionDeps {
  renderOnce: RenderOnce;
  now?: () => number;
  debounceMs?: number;
  schedule?: (fn: () => void, ms: number) => { unref?: () => void };
  onError?: (err: unknown) => void;
  /** False when this process cannot see a commit, so the session says so instead of going quiet. */
  degraded?: string | undefined;
}

/**
 * One run's live state, shared by every viewer of it.
 *
 * `renders` and `reads` are counters rather than internals because the acceptance criteria are about
 * them: twenty batches must produce at most twenty renders whether one viewer or ten are connected,
 * and the lake read count must equal the render count.
 */
export class LiveSession {
  readonly viewers = new Set<Viewer>();
  renders = 0;
  reads = 0;
  private hashes: string[] = [];
  private last: ReportSnapshot | undefined;
  private lastWarnings: string[] | undefined;
  private inFlight = false;
  private again = false;
  private timer: { unref?: () => void } | undefined;
  private closed = false;

  constructor(
    readonly key: LiveRunKey,
    private readonly deps: LiveSessionDeps
  ) {}

  /** The frames a newly-connected viewer needs, from the render already in hand. */
  snapshotEvent(): LiveEvent | undefined {
    if (!this.last) return undefined;
    return {
      type: 'snapshot',
      blocks: framesOf(this.last),
      ...(this.lastWarnings ? { warnings: this.lastWarnings } : {}),
      ...(this.deps.degraded ? { degraded: this.deps.degraded } : {}),
    };
  }

  /**
   * A data change arrived. Coalesce it.
   *
   * TWO LAYERS, AND THEY DO DIFFERENT JOBS. The debounce collapses a burst of commits into one
   * render; `inFlight`/`again` collapses everything that arrives DURING a render into exactly one
   * more, so a run committing faster than it renders converges instead of queueing without bound.
   */
  touch(): void {
    if (this.closed) return;
    if (this.timer) return;
    const schedule = this.deps.schedule ?? ((fn, ms) => setTimeout(fn, ms));
    this.timer = schedule(() => {
      this.timer = undefined;
      void this.renderNow();
    }, this.deps.debounceMs ?? DEBOUNCE_MS);
    this.timer?.unref?.();
  }

  async renderNow(): Promise<void> {
    if (this.closed) return;
    if (this.inFlight) {
      this.again = true;
      return;
    }
    this.inFlight = true;
    try {
      this.renders += 1;
      this.reads += 1;
      const out = await this.deps.renderOnce(this.key);
      if (this.closed) return;
      if ('error' in out) {
        // A LIVE RENDER THAT FAILED IS NOT PUBLISHED AND NOT PERSISTED. The viewer keeps the last
        // good document rather than being shown a stack trace, and nothing writes an `error` version
        // — which `versionByKey` would find and skip for ever, welding the report shut.
        this.deps.onError?.(new Error(out.error));
        return;
      }
      this.publish(out.snapshot, out.warnings);
    } catch (err) {
      this.deps.onError?.(err);
    } finally {
      this.inFlight = false;
      if (this.again && !this.closed) {
        this.again = false;
        void this.renderNow();
      }
    }
  }

  private publish(snapshot: ReportSnapshot, warnings?: string[]): void {
    const next = blockHashes(snapshot);
    const first = this.last === undefined;
    const changed = first ? null : changedBlocks(this.hashes, next);
    this.last = snapshot;
    this.lastWarnings = warnings;
    this.hashes = next;

    if (first || changed === null) {
      const event = this.snapshotEvent();
      if (event) this.emit(event);
      return;
    }
    // NOTHING CHANGED MEANS NOTHING IS SENT. A patch never carries an unchanged block, and a render
    // whose output is identical produces no frame at all.
    if (changed.length === 0) return;
    this.emit({ type: 'patch', blocks: framesOf(snapshot, changed) });
  }

  emit(event: LiveEvent): void {
    for (const v of this.viewers) {
      try {
        v(event);
      } catch (err) {
        this.deps.onError?.(err);
      }
    }
  }

  /** What {@link LiveHub} hangs on a session for its lifetime: the tick and the producers'
   *  subscriptions. Run once, by {@link close}, however the session ends. */
  private readonly onClose: Array<() => void> = [];

  /** Register something to undo when this session closes. Runs at once if it already has. */
  whenClosed(fn: () => void): void {
    if (this.closed) {
      fn();
      return;
    }
    this.onClose.push(fn);
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.viewers.clear();
    for (const fn of this.onClose.splice(0)) {
      try {
        fn();
      } catch (err) {
        this.deps.onError?.(err);
      }
    }
  }
}

/** Why a connection was refused. The route maps these to a status code. */
export type RefusalReason = 'per-run' | 'runs' | 'viewers';

export interface LiveHubDeps extends Omit<LiveSessionDeps, 'degraded'> {
  degraded?: string | undefined;
  onNote?: (note: string) => void;
  /**
   * Re-render every `tickMs` while a session has viewers. Absent means commits are the only trigger.
   *
   * ADR 0062 refused a timer because the context could not change between ticks, and at the time
   * that was true: nothing produced `run.progress`, `datasets` or the open-run `result`. With those
   * produced, the context DOES move without a commit: progress and duration change, and pushed rows
   * land well before their batch is published. A tick goes through {@link LiveSession.touch}, so it
   * shares the debounce and the one-in-flight rule, and a render that changed nothing sends nothing.
   */
  tickMs?: number;
  /**
   * Called once when a session is CREATED (not when a viewer joins one), with that session. Return a
   * cleanup and it runs when the session closes. This is where a producer subscribes to whatever
   * feeds the session's context, and stops when nobody is watching any more.
   */
  onOpen?: (key: LiveRunKey, session: LiveSession) => (() => void) | void;
}

/**
 * Every live session on this process, and the caps around them.
 *
 * THE CAPS ARE CHECKED BEFORE ANY PER-RUN STATE IS MINTED, which is the lesson `rowTail` paid for:
 * a flood over ids that do not exist created one poller per id because the refusal came too late.
 * {@link open} therefore takes a run the caller has already resolved against Temporal, and counts
 * before it allocates.
 */
export class LiveHub {
  private readonly sessions = new Map<string, LiveSession>();

  constructor(private readonly deps: LiveHubDeps) {}

  /** Live sessions, for the cap and for a leak test. */
  size(): number {
    return this.sessions.size;
  }

  viewerCount(): number {
    let n = 0;
    for (const s of this.sessions.values()) n += s.viewers.size;
    return n;
  }

  get(key: LiveRunKey): LiveSession | undefined {
    return this.sessions.get(liveKey(key));
  }

  /**
   * Attach a viewer. Returns its detach, or the reason it was refused.
   *
   * AN EXISTING SESSION IS JOINED RATHER THAN DUPLICATED — that is what makes ten tabs cost one
   * render — and a newly-created one is NOT rendered here: the caller sends the snapshot once it has
   * one, so a refusal costs no work at all.
   */
  open(
    key: LiveRunKey,
    viewer: Viewer
  ): { ok: true; session: LiveSession; detach: () => void } | { ok: false; reason: RefusalReason } {
    const existing = this.sessions.get(liveKey(key));
    if (existing) {
      if (existing.viewers.size >= MAX_VIEWERS_PER_RUN) return { ok: false, reason: 'per-run' };
      if (this.viewerCount() >= MAX_LIVE_VIEWERS) return { ok: false, reason: 'viewers' };
      existing.viewers.add(viewer);
      return { ok: true, session: existing, detach: () => this.detach(key, viewer) };
    }
    if (this.sessions.size >= MAX_LIVE_RUNS) return { ok: false, reason: 'runs' };
    if (this.viewerCount() >= MAX_LIVE_VIEWERS) return { ok: false, reason: 'viewers' };

    const session = new LiveSession(key, { ...this.deps, degraded: this.deps.degraded });
    session.viewers.add(viewer);
    this.sessions.set(liveKey(key), session);
    this.attachProducers(key, session);
    return { ok: true, session, detach: () => this.detach(key, viewer) };
  }

  /** The tick and the {@link LiveHubDeps.onOpen} producers, both undone by the session's close. */
  private attachProducers(key: LiveRunKey, session: LiveSession): void {
    const tickMs = this.deps.tickMs;
    if (tickMs && tickMs > 0) {
      const timer = setInterval(() => session.touch(), tickMs);
      timer.unref?.();
      session.whenClosed(() => clearInterval(timer));
    }
    try {
      const cleanup = this.deps.onOpen?.(key, session);
      if (cleanup) session.whenClosed(cleanup);
    } catch (err) {
      // A producer that cannot subscribe leaves the session on its other triggers; it is not a
      // reason to refuse the viewer a document.
      this.deps.onError?.(err);
    }
  }

  /** The last viewer leaving ends the session, so an unwatched run costs nothing. */
  private detach(key: LiveRunKey, viewer: Viewer): void {
    const id = liveKey(key);
    const session = this.sessions.get(id);
    if (!session) return;
    session.viewers.delete(viewer);
    if (session.viewers.size === 0) {
      session.close();
      this.sessions.delete(id);
    }
  }

  /** A commit landed. Only a WATCHED run re-renders; nothing else is listening. */
  onData(e: LiveRunKey): void {
    this.sessions.get(liveKey(e))?.touch();
  }

  /**
   * The run reached a terminal state. Tell the viewers and end the session.
   *
   * The stored version is minted by the ordinary path, not here — this only hands viewers the number
   * so the page can swap to it. The freeze is a VISIBLE transition: `result` appears and the
   * `{% if result %}` branch flips, so a byte-identical final frame would be the bug, not the goal.
   */
  finalize(key: LiveRunKey, version: number): void {
    const id = liveKey(key);
    const session = this.sessions.get(id);
    if (!session) return;
    session.emit({ type: 'final', version });
    session.close();
    this.sessions.delete(id);
  }

  statusChanged(key: LiveRunKey, status: string): void {
    this.sessions.get(liveKey(key))?.emit({ type: 'status', status });
  }

  closeAll(): void {
    for (const s of this.sessions.values()) s.close();
    this.sessions.clear();
  }
}
