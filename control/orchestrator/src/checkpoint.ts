/**
 * The Checkpoint an actor carries in its Temporal activity heartbeat — the orchestrator's reader,
 * and the third implementation of one contract.
 *
 * WHY THIS EXISTS. The record of which Units had committed lived in Redis, in a hash under a 24 h
 * TTL that `maxmemory-policy volatile-lru` evicted FIRST under memory pressure (ADR 0059). Losing
 * it is not a slowdown: a retry reads an absent commit map and re-runs finished work, or reads a
 * partly-evicted one and skips work that never ran, and nothing raises either way. A heartbeat's
 * details live in the activity's own history, so the commit map travels with the retry instead.
 *
 * THIS SIDE IS A READER, NOT A WRITER. The actor writes checkpoints; the orchestrator decodes them
 * to say how far a node got. `resumeFrom` is here anyway, and it is not dead code — it is the
 * assertion that this implementation agrees with the two that do resume, run against the same
 * corpus. A reader that decodes a checkpoint differently from the SDK that wrote it reports
 * progress that never happened.
 *
 * `done` IS A RANGE SET BECAUSE A HEARTBEAT IS SMALL. Heartbeat details are a Temporal payload with
 * a size limit, so a per-unit list of 10,000 integers is a batch-size ceiling in disguise.
 *
 * CANONICAL FORM IS PART OF THE CONTRACT: inclusive `[lo, hi]` pairs, sorted ascending, merged so no
 * two ranges touch or overlap. `shared/conformance/checkpoint.json` holds this file,
 * `runtime/go/checkpoint` and `runtime/python/internals/checkpoint.py` to it together — two
 * implementations that agree on membership and disagree on encoding produce different bytes for the
 * same facts, and this repository has shipped that bug before.
 */

/** The only version this reader understands. A checkpoint that does not say `1` is discarded whole. */
export const CHECKPOINT_VERSION = 1;

/** One inclusive span of unit indices, on the wire as a two-element array. */
export type Range = [number, number];

/** The heartbeat payload. THE FIELD NAMES ARE THE CONTRACT — see the Go and Python peers. */
export interface CheckpointDetails {
  v: number;
  batch_id: string;
  done: Range[];
  failed: number[];
  manifest_ref: string;
}

/**
 * Sort and coalesce. ADJACENCY COUNTS: `[0,2]` and `[3,5]` touch, so they become `[0,5]`.
 *
 * Without that, adding one index at a time yields one range per index and the compaction the
 * encoding exists for never happens.
 */
export function mergeRanges(input: readonly Range[]): Range[] {
  if (input.length === 0) return [];
  const sorted = [...input].sort((a, b) => (a[0] !== b[0] ? a[0] - b[0] : a[1] - b[1]));
  const out: Range[] = [[sorted[0]![0], sorted[0]![1]]];
  for (const [lo, hi] of sorted.slice(1)) {
    const last = out[out.length - 1]!;
    if (lo <= last[1] + 1) {
      if (hi > last[1]) last[1] = hi;
      continue;
    }
    out.push([lo, hi]);
  }
  return out;
}

/** A set of unit indices as merged inclusive ranges. Canonical at every moment. */
export class RangeSet {
  private r: Range[];

  constructor(ranges: readonly Range[] = []) {
    this.r = mergeRanges(ranges);
  }

  /** Record one index. Re-adding a member is a no-op — a retry re-commits what it re-ran. */
  add(i: number): void {
    this.r = mergeRanges([...this.r, [i, i]]);
  }

  /**
   * Membership. Linear rather than a binary search: a batch has a handful of ranges, not thousands,
   * and the bisecting version is the one place an off-by-one hides without failing a test.
   */
  has(i: number): boolean {
    return this.r.some(([lo, hi]) => i >= lo && i <= hi);
  }

  /** How many INDICES the set holds, not how many ranges. */
  get size(): number {
    return this.r.reduce((n, [lo, hi]) => n + hi - lo + 1, 0);
  }

  /** The canonical encoding, as it goes into a heartbeat. */
  ranges(): Range[] {
    return this.r.map(([lo, hi]) => [lo, hi] as Range);
  }
}

/** How far one Batch got, decoded. */
export interface Checkpoint {
  batchId: string;
  done: RangeSet;
  failed: ReadonlySet<number>;
  manifestRef: string;
}

/**
 * Decode one heartbeat payload, or `null` if it may not be trusted.
 *
 * REFUSED RATHER THAN PARTIALLY READ. A version this code does not know may have moved a field's
 * meaning, and a reader that ignores what it does not recognise reports progress from a checkpoint
 * it only half understood. `null` means "know nothing", which is always safe.
 */
export function decodeCheckpoint(raw: unknown): Checkpoint | null {
  if (typeof raw !== 'object' || raw === null) return null;
  const d = raw as Partial<CheckpointDetails>;
  if (d.v !== CHECKPOINT_VERSION) return null;
  // Each range is validated rather than trusted: a malformed pair decoded as a zero range would
  // claim unit 0 committed, which on the resuming side skips real work.
  const done: Range[] = [];
  for (const pair of Array.isArray(d.done) ? d.done : []) {
    if (!Array.isArray(pair) || pair.length !== 2) return null;
    const [lo, hi] = pair;
    if (!Number.isInteger(lo) || !Number.isInteger(hi)) return null;
    done.push([lo as number, hi as number]);
  }
  const failed = new Set<number>();
  for (const i of Array.isArray(d.failed) ? d.failed : []) {
    if (Number.isInteger(i)) failed.add(i as number);
  }
  return {
    batchId: typeof d.batch_id === 'string' ? d.batch_id : '',
    done: new RangeSet(done),
    failed,
    manifestRef: typeof d.manifest_ref === 'string' ? d.manifest_ref : '',
  };
}

/**
 * Which unit indices still need running, given what a previous attempt reported.
 *
 * AN EMPTY `batchId` MATCHES NOTHING, including another empty one. Unit indices are positions
 * WITHIN ONE BATCH, so applying a checkpoint from a different batch would skip units by index in a
 * batch that never ran them. An unidentified checkpoint is not evidence about any particular batch.
 */
export function resumeFrom(raw: unknown, batchId: string, units: number): number[] {
  const n = Math.max(Math.trunc(units) || 0, 0);
  const all = Array.from({ length: n }, (_, i) => i);
  const ck = decodeCheckpoint(raw);
  if (!ck || !batchId || ck.batchId !== batchId) return all;
  return all.filter((i) => !ck.done.has(i) && !ck.failed.has(i));
}

/**
 * How many Units a checkpoint accounts for — committed plus isolated.
 *
 * This is what the run page's per-node progress should read rather than a bare `done` counter: a
 * node that isolated three Units and committed seven has finished ten, and a denominator that
 * excludes the isolated ones never reaches its total.
 */
export function accountedFor(ck: Checkpoint): number {
  let n = ck.done.size;
  for (const i of ck.failed) if (!ck.done.has(i)) n += 1;
  return n;
}
