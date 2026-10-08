/**
 * THE IN-PROCESS RUN EVENT BUS (ADR 0062) — what a live report re-renders on.
 *
 * ── WHY NOT POSTGRES `NOTIFY`, WHICH IS WHAT THE SPECIFICATION ASKED FOR ───────────────────────
 *
 * Four facts, each measured rather than assumed, and together they make `NOTIFY` worse than useless
 * here — it would be GREEN AND INERT, which is the only outcome worse than a visible failure:
 *
 *   1. `resolveStoreUrl` falls through to the literal `'orchestrator.db'` (`data/sql.ts:118-125`), so
 *      the DEFAULT store is SQLite, which has no `LISTEN`/`NOTIFY` at all.
 *   2. `PgPoolLike` is `query` + `end` only (`data/sql.ts:80-84`). There is no client checkout and no
 *      `notification` event, so a listener cannot be attached to it.
 *   3. `createPool` is `max: 2`, "Small on purpose" (`data/sql.ts:185`). A `LISTEN` holds its
 *      connection for the process's life, which is half the pool.
 *   4. `pool.query('LISTEN …')` would SUCCEED and then never deliver anything.
 *
 * ── WHY IN-PROCESS IS CORRECT RATHER THAN MERELY EXPEDIENT ─────────────────────────────────────
 *
 * The orchestrator is three roles in one PID (`main.ts:2`, ADR 0031 §1), and `resolveRoles` returns
 * all three when `KONTRA_ROLES` is unset (`roles.ts:50-55`), which is the shipped default — nothing
 * in `docker-compose.yml` sets it. So the process that commits a batch is the process that serves the
 * stream, and a cross-process transport would be ceremony around a function call.
 *
 * BUT THE SPLIT IS EXPRESSIBLE, so this module refuses to pretend. `KONTRA_ROLES=api` yields a
 * process that serves reports and never sees a commit, and a live view there would stream a document
 * that is frozen for reasons nothing reports. {@link observesCommits} is what the route reads to say
 * DEGRADED instead — a bound that reports itself. Redis pub/sub is the named extension point if a
 * split ever needs to work (`ioredis` is already a dependency; the long-lived-client discipline is in
 * `stateStore.ts`).
 */

import { EventEmitter } from 'node:events';

import { resolveRoles } from './roles';

/**
 * A batch landed in the lake.
 *
 * `runStartedAt` IS NOT DECORATION. A Run is its caller's workflow id (ADR 0023 §12) and ids are
 * REUSED — `listRuns` collapses a reused id to its newest execution, naming the fleet's
 * `kontra-fleet/<name>` as the case it was written for (`temporalClient.ts:240-246`). So anything
 * cached per Run must be keyed by the execution and not by the id, or execution N serves N-1's rows.
 * The start instant is already stamped on every row as `run_started_at`, so it costs nothing to carry.
 */
export interface RunDataEvent {
  runId: string;
  runStartedAt: number;
  /** The Dataset table that grew, so a listener can invalidate one summary rather than all of them. */
  dataset: string;
  /** Rows in the Dataset after this commit — the value `datasets.<name>.rows` publishes. */
  rows: number;
}

export interface RunStatusEvent {
  runId: string;
  runStartedAt: number;
  status: string;
}

export interface RunProgressEvent {
  runId: string;
  runStartedAt: number;
}

type Handler<T> = (event: T) => void;

/**
 * The bus. One per process; {@link runEvents} is the instance everything uses.
 *
 * A THROWING LISTENER MUST NOT TAKE THE EMITTER DOWN WITH IT. `emit` runs listeners synchronously, so
 * an exception in a report renderer would propagate into `publishBatch`'s transaction and fail a
 * commit that had already landed. Every dispatch is therefore guarded.
 */
export class RunEventBus {
  private readonly bus = new EventEmitter();
  /** Node warns at 10 and a live run legitimately has one listener per connected viewer. */
  private readonly max: number;
  private readonly onError: (err: unknown) => void;

  constructor(opts: { maxListeners?: number; onError?: (err: unknown) => void } = {}) {
    this.max = opts.maxListeners ?? 256;
    this.onError = opts.onError ?? (() => undefined);
    this.bus.setMaxListeners(this.max);
  }

  onData(h: Handler<RunDataEvent>): () => void {
    return this.on('data', h);
  }

  onStatus(h: Handler<RunStatusEvent>): () => void {
    return this.on('status', h);
  }

  onProgress(h: Handler<RunProgressEvent>): () => void {
    return this.on('progress', h);
  }

  emitData(e: RunDataEvent): void {
    this.bus.emit('data', e);
  }

  emitStatus(e: RunStatusEvent): void {
    this.bus.emit('status', e);
  }

  emitProgress(e: RunProgressEvent): void {
    this.bus.emit('progress', e);
  }

  /** Listeners currently attached, for the cap the stream route enforces and for a leak test. */
  listenerCount(): number {
    return (['data', 'status', 'progress'] as const).reduce(
      (n, k) => n + this.bus.listenerCount(k),
      0
    );
  }

  /** RETURNS ITS OWN REMOVAL. A stream that forgets to detach is a leak per disconnected viewer. */
  private on<T>(event: string, h: Handler<T>): () => void {
    const guarded = (e: T): void => {
      try {
        h(e);
      } catch (err) {
        this.onError(err);
      }
    };
    this.bus.on(event, guarded);
    return () => this.bus.off(event, guarded);
  }
}

export const runEvents = new RunEventBus();

/**
 * Does THIS process see batch commits?
 *
 * False when the roles were split and this one does not run the materializer — in which case no
 * `data` event can ever arrive and a live report would be frozen with nothing to say why. The route
 * reads this to answer DEGRADED rather than to stream silence.
 */
export function observesCommits(roles: readonly string[] = resolveRoles()): boolean {
  return roles.includes('materializer');
}
