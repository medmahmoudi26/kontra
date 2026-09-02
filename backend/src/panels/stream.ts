/**
 * Per-Terminal buffering and the byte caps (ADR 0020).
 *
 * The hazard this bounds was measured on the MACHINE, not here: with a reading client stalled, a
 * tmux server in control mode grew 10 MB → 80 MB in five seconds and then segfaulted, because
 * `%output` is a log and a lagging client forces retention of every byte. Screen attachment moves
 * the ceiling to our side of the wire — tmux can always satisfy a laggard with a full redraw — but
 * only if OUR side actually has a ceiling. This file is that ceiling:
 *
 *   - {@link Ring}: 1 MiB per Terminal, oldest bytes dropped first, in memory, lost on restart.
 *     A Terminal is not a record; the Manifest, the journal and the lake are.
 *   - {@link FrameBudget}: ≤15 frames/s and ≤256 KiB/s per Terminal, sliding over one second.
 *     Over budget, the OLDEST pending bytes go and the client is told exactly how many, as an
 *     `elided` control message — a tile that silently skips output is a tile that lies.
 *
 * "Elided" is a first-class outcome for the same reason `heartbeat.ts` keeps "unknown" and "zero"
 * distinguishable: the operator has to be able to tell a quiet Worker from a dropped stream.
 */

/** Scrollback held per Terminal, replayed to a client that (re)subscribes. */
export const RING_BYTES = 1024 * 1024;

/** Frames per second per Terminal, after coalescing. */
export const MAX_FRAMES_PER_SEC = 15;

/** Bytes per second per Terminal. */
export const MAX_BYTES_PER_SEC = 256 * 1024;

/**
 * A byte ring with a hard cap.
 *
 * Chunk-granular: it drops whole chunks from the front until it fits, then trims the new front
 * chunk if that is still not enough. Byte-exact trimming of terminal output cannot be made
 * "correct" anyway — a screen cut mid-escape-sequence is garbage either way, which is why every
 * snapshot frame carries a clear-screen prefix and why a replay is a seed, not a transcript.
 */
export class Ring {
  private chunks: Buffer[] = [];
  private total = 0;

  constructor(readonly cap: number = RING_BYTES) {}

  get bytes(): number {
    return this.total;
  }

  push(chunk: Buffer): void {
    if (!chunk.length) return;
    if (chunk.length >= this.cap) {
      // A single chunk larger than the ring: keep its tail, which is the part a tile would show.
      this.chunks = [chunk.subarray(chunk.length - this.cap)];
      this.total = this.cap;
      return;
    }
    this.chunks.push(chunk);
    this.total += chunk.length;
    while (this.total > this.cap) {
      const head = this.chunks[0];
      if (!head) break;
      const over = this.total - this.cap;
      if (head.length <= over) {
        this.chunks.shift();
        this.total -= head.length;
      } else {
        this.chunks[0] = head.subarray(over);
        this.total -= over;
      }
    }
  }

  /** Everything held, oldest first. */
  concat(): Buffer {
    return this.chunks.length === 1 ? (this.chunks[0] as Buffer) : Buffer.concat(this.chunks);
  }

  /** Replace the contents — what a snapshot does, since a snapshot IS the whole screen. */
  reset(chunk?: Buffer): void {
    this.chunks = [];
    this.total = 0;
    if (chunk) this.push(chunk);
  }
}

export interface Admission {
  /** Bytes to send now. Empty when the budget is spent. */
  send: Buffer | null;
  /** Bytes dropped, ever, that this client has not yet been told about. */
  elided: number;
}

/**
 * The per-Terminal send budget.
 *
 * `offer()` either hands the bytes straight back (send them) or holds them, dropping the oldest
 * held bytes once the hold exceeds one second's worth. `drain()` releases what the next second
 * allows. Coalescing is deliberate: for a snapshot stream the newest screen is the truth, so
 * dropping the oldest held bytes loses nothing an operator wanted.
 */
export class FrameBudget {
  private frameTimes: number[] = [];
  private byteEvents: Array<{ at: number; bytes: number }> = [];
  private pending: Buffer[] = [];
  private pendingBytes = 0;
  private elided = 0;

  constructor(
    private readonly now: () => number = () => Date.now(),
    readonly maxFrames = MAX_FRAMES_PER_SEC,
    readonly maxBytes = MAX_BYTES_PER_SEC
  ) {}

  /** Bytes held back, waiting for budget. */
  get held(): number {
    return this.pendingBytes;
  }

  private trim(t: number): void {
    const cutoff = t - 1000;
    while (this.frameTimes.length && (this.frameTimes[0] as number) <= cutoff) this.frameTimes.shift();
    while (this.byteEvents.length && (this.byteEvents[0] as { at: number }).at <= cutoff) {
      this.byteEvents.shift();
    }
  }

  private usedBytes(): number {
    let n = 0;
    for (const e of this.byteEvents) n += e.bytes;
    return n;
  }

  private hold(chunk: Buffer): void {
    this.pending.push(chunk);
    this.pendingBytes += chunk.length;
    // The hold is itself bounded — otherwise a stalled tile becomes this process's memory
    // profile, which is the failure ADR 0020 found on the Machine, moved here.
    while (this.pendingBytes > this.maxBytes) {
      const head = this.pending[0];
      if (!head) break;
      const over = this.pendingBytes - this.maxBytes;
      if (head.length <= over) {
        this.pending.shift();
        this.pendingBytes -= head.length;
        this.elided += head.length;
      } else {
        this.pending[0] = head.subarray(over);
        this.pendingBytes -= over;
        this.elided += over;
      }
    }
  }

  /** Offer bytes for sending. */
  offer(chunk: Buffer): Admission {
    const t = this.now();
    this.trim(t);
    if (chunk.length) this.hold(chunk);
    return this.release(t);
  }

  /** Release whatever the budget now allows, without offering anything new. */
  drain(): Admission {
    const t = this.now();
    this.trim(t);
    return this.release(t);
  }

  /** Bytes dropped since the last call, for the `elided` control message. */
  takeElided(): number {
    const n = this.elided;
    this.elided = 0;
    return n;
  }

  private release(t: number): Admission {
    if (!this.pendingBytes) return { send: null, elided: this.elided };
    if (this.frameTimes.length >= this.maxFrames) return { send: null, elided: this.elided };
    const room = this.maxBytes - this.usedBytes();
    if (room <= 0) return { send: null, elided: this.elided };

    const out: Buffer[] = [];
    let taken = 0;
    while (this.pending.length && taken < room) {
      const head = this.pending[0] as Buffer;
      const want = Math.min(head.length, room - taken);
      if (want === head.length) {
        out.push(head);
        this.pending.shift();
      } else {
        out.push(head.subarray(0, want));
        this.pending[0] = head.subarray(want);
      }
      taken += want;
      this.pendingBytes -= want;
    }
    if (!taken) return { send: null, elided: this.elided };
    this.frameTimes.push(t);
    this.byteEvents.push({ at: t, bytes: taken });
    return { send: out.length === 1 ? (out[0] as Buffer) : Buffer.concat(out), elided: this.elided };
  }
}
