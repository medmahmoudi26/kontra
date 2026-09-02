/**
 * Following one Run's transcript while it is still going.
 *
 * WATCHING IS THE DEFAULT AND REFRESHING IS NOT A FEATURE. A transcript that had to be re-fetched by
 * hand made "what is happening" and "what happened" two surfaces, and only the second one was any
 * good: the first was a stale account with a badge on it saying so. One reader answers both, and the
 * only difference between them is how many times it reads.
 *
 * IT POLLS `/history`, AND THAT IS A DECISION RATHER THAN A SHORTCUT. The obvious move is a second
 * SSE hub beside `rowTail.ts` pushing turns from the server, and it is the wrong one twice over.
 * `turns.ts` already argues the first half — a turns route would re-serialise a nine-arm union to
 * tell the browser something it can compute from the response it just received, and it would need
 * its own cap, its own archive fallback and its own `?exec=`, which is a second spelling of
 * `/history` that can drift from the first. The second half is arithmetic: a server-side history
 * stream is a poll loop per watched Run, keyed by a caller-supplied id, against Temporal rather than
 * against an S3 LIST — which is the shape of the defect this slice was briefed not to reproduce
 * (`.scratch/post-merge-review/TRIAGE-2026-08-25.md` §7), only more expensive. A poll is a request
 * that ends; nothing is held server-side for a reader that walks away.
 *
 * ONE READER PER RUN, FANNED OUT, which is `RowTailHub`'s doctrine on this side of the wire and buys
 * the same thing: two components watching one Run cost one read and cannot show each other
 * different ages. It is also what makes {@link TranscriptHub.activeRuns} a number worth capping.
 *
 * AN EMISSION IS AN ARRIVAL. A poll that finds the log unchanged tells nobody — {@link
 * transcriptFingerprint} coalesces it — so a sink firing means the run actually did something, and
 * the surface can update in place without a pulse, a flash or a spinner to prove it was listening.
 * Nothing here animates and nothing here is on a timer the operator can see.
 *
 * BOUNDED, AND LOUDLY. Three caps, and every one of them refuses in a way the surface can print:
 * {@link FOLLOW_MAX_RUNS} on how many Runs one page follows at once, {@link FOLLOW_MAX_READERS} on
 * how many sinks pile onto one of them, and {@link FOLLOW_MAX_MISSES} on how long a reader keeps
 * asking a route that will not answer. A refused reader says it is not following; it never shows a
 * frozen account with a live badge on it, which is the one outcome worse than not following at all.
 */

import { useEffect, useState } from 'react';

import { fetchRunTurns, type RunTurns, type RunTurnsRead } from './turns';
import type { RunHistory } from './api';

/**
 * How often an open Run is re-read.
 *
 * THE SAME NUMBER `RunTail` USES, deliberately: the compact panel and the transcript are two
 * readings of one Run on one screen, and two cadences would let them disagree about its age in a
 * way that reads as one of them being broken.
 */
export const FOLLOW_POLL_MS = 2_000;

/**
 * How many Runs one page follows at once — the TOTAL cap.
 *
 * FOUR IS FOUR TIMES THE HONEST NEED. The thread shows one conversation at a time, so the real
 * number is one; the headroom is for a second surface that wants the same Run and for a drill that
 * outlives a selection change. What it actually stops is a leak: a reader that fails to release
 * hits a wall on the fourth Run instead of quietly accumulating one poll loop per run an operator
 * has ever clicked.
 */
export const FOLLOW_MAX_RUNS = 4;

/** How many sinks may share one Run's reader. Bounds the fan-out the way the total cap bounds the
 *  readers: a component that re-subscribes on every render is a bug, and it should be a refusal
 *  rather than an ever-growing `Set` the poller walks. */
export const FOLLOW_MAX_READERS = 8;

/**
 * Consecutive failed reads before a reader gives up.
 *
 * A BOUND ON TIME, WHICH THE OTHER TWO ARE NOT. A run whose first event has not landed answers 404,
 * and so does one Temporal dropped at retention — the same status for "wait a moment" and for
 * "never" (`api.ts`). Five reads is ten seconds of patience for the first and a stop for the second,
 * so a tab left open on an id that will never answer is not a request every two seconds forever.
 */
export const FOLLOW_MAX_MISSES = 5;

/** Cancel a subscription, or a scheduled repeat. */
type Cancel = () => void;

/**
 * What the reader is doing, in the one word the surface prints.
 *
 *  - `reading`   the first read is out; there is no account yet.
 *  - `following` the run is open and the reader is armed. New turns arrive here.
 *  - `settled`   the run has closed. The account is final and the reader has stopped — not idle,
 *                FINISHED, which is why it is a different word from `reading`.
 *  - `refused`   a cap declined this reader. There is no account and none is coming.
 *  - `stale`     reads are failing. Whatever account is on screen was true when it was read and is
 *                not being kept true.
 */
export type FollowPhase = 'reading' | 'following' | 'settled' | 'refused' | 'stale';

/**
 * One Run, as its reader currently understands it. FULL STATE, never a delta — so a sink that
 * missed an emission is caught up by the next one alone, and a late subscriber is seeded with this
 * and needs nothing else.
 */
export interface Following {
  runId: string;
  /** The account, or `null` before the first answer landed. Never cleared by a later failure: an
   *  account that was true when it was read stays on screen with {@link phase} saying it is no
   *  longer being kept true. */
  turns: RunTurns | null;
  /** The reduced log the account was read from — carried so a drill into the raw events costs no
   *  second fetch. `null` for the same reasons `turns` is. */
  history: RunHistory | null;
  /** Why there is no account, or why the last read failed. `gone` is retention or a run that has
   *  not written its first event; anything else is the cluster or the orchestrator being unwell. */
  failure: { gone: boolean; detail: string } | null;
  phase: FollowPhase;
  /** How many reads have LANDED, successful or not. The honest arrival counter, and what a test
   *  asserts on instead of a wall clock. */
  reads: number;
  /** How many emissions this reader has made — that is, how many times the log actually changed.
   *  On a settled run read once, this is 1 and stays 1 however long the tab is open. */
  arrivals: number;
}

/**
 * The reading, reduced to a string that changes exactly when the run does.
 *
 * WHY NOT COMPARE THE OBJECTS. Every poll builds a fresh `RunTurns` — new arrays, new turn objects —
 * so identity says "changed" every two seconds and a deep compare walks the whole transcript to say
 * "unchanged" almost every time. This walks the COLLAPSED turns, which is dozens of rows for a run
 * whose log is twenty thousand events, and touches every field a poll can move: a loop that gained a
 * member, a dispatch that closed, a park that was answered, a failure that arrived.
 *
 * IT IS DELIBERATELY NOT A HASH OF THE HISTORY. Two logs that differ only in events the reader
 * folded into the same turns are the same account, and re-rendering for them would make the surface
 * churn on Temporal's own bookkeeping — which is precisely the noise the transcript exists to remove.
 */
export function transcriptFingerprint(turns: RunTurns): string {
  const t = turns.named.transcript;
  const head = [
    t.live ? 'live' : 'closed',
    t.outcome ?? '',
    t.continued ? 'cont' : '',
    t.published,
    t.elided,
    t.truncated ? 'trunc' : '',
    t.archived ? 'arch' : '',
  ].join(',');
  const rows = t.turns
    .map((turn) => {
      const last = turn.events.length > 0 ? turn.events[turn.events.length - 1] : turn.at;
      const pending = turn.kind === 'parked' ? (turn.pending ? 'p' : 'a') : '';
      return `${turn.kind}${turn.count}${turn.open ? 'o' : 'c'}${turn.failures}${turn.attempt}${turn.dur}${last}${pending}${turn.error ?? ''}`;
    })
    .join(';');
  return `${head}#${t.turns.length}#${rows}`;
}

export interface TranscriptHubDeps {
  /** Read one Run's turns. The default is the seam in `turns.ts`; a test passes its own so the
   *  whole reader is asserted with no network and no clock. */
  read?: (runId: string) => Promise<RunTurnsRead>;
  /** How often an open Run is re-read. */
  pollMs?: number;
  /** Total Runs followed at once. */
  maxRuns?: number;
  /** Sinks per Run. */
  maxReaders?: number;
  /** Consecutive failed reads before a reader stops. */
  maxMisses?: number;
  /**
   * Arm a repeating tick and return its canceller. The default reads ONCE immediately — a transcript
   * that appeared one interval after it was opened would look like a slow page rather than a live
   * one — and then every `ms`. Injected so a test drives every read by hand, which is what makes
   * "turns arrived without a refresh" an assertion rather than a wait.
   */
  schedule?: (tick: () => void, ms: number) => Cancel;
}

interface RunReader {
  sinks: Set<(s: Following) => void>;
  state: Following;
  fingerprint: string | null;
  cancel: Cancel | null;
  reading: boolean;
  misses: number;
}

/**
 * Every Run this page is following, and the one reader each of them has.
 *
 * A HUB RATHER THAN A HOOK because the caps have to be a property of the PAGE and not of whichever
 * component happened to mount. A per-component follower cannot answer "how many Runs is this tab
 * polling", which is the only question a total cap is about.
 */
export class TranscriptHub {
  private readonly runs = new Map<string, RunReader>();
  private readonly read: (runId: string) => Promise<RunTurnsRead>;
  private readonly pollMs: number;
  private readonly maxRuns: number;
  private readonly maxReaders: number;
  private readonly maxMisses: number;
  private readonly schedule: (tick: () => void, ms: number) => Cancel;
  private closed = false;

  constructor(deps: TranscriptHubDeps = {}) {
    this.read = deps.read ?? fetchRunTurns;
    this.pollMs = deps.pollMs ?? FOLLOW_POLL_MS;
    this.maxRuns = deps.maxRuns ?? FOLLOW_MAX_RUNS;
    this.maxReaders = deps.maxReaders ?? FOLLOW_MAX_READERS;
    this.maxMisses = deps.maxMisses ?? FOLLOW_MAX_MISSES;
    this.schedule =
      deps.schedule ??
      ((tick, ms) => {
        tick(); // the first account is instant, not one interval late
        const h = setInterval(tick, ms);
        return () => clearInterval(h);
      });
  }

  /**
   * Watch one Run. The sink is called immediately with the current state — which on a Run this page
   * is already following is the account it already has, so opening a second view of one Run draws
   * instantly and reads nothing — and then once per ARRIVAL.
   *
   * Returns the release. Calling it removes this sink; the last release stops the reader, which is
   * the property the closed-reader test pins.
   */
  watch(runId: string, sink: (s: Following) => void): Cancel {
    if (this.closed) {
      sink(refused(runId, 'this page has stopped following runs'));
      return () => {};
    }
    const existing = this.runs.get(runId);
    if (!existing && this.runs.size >= this.maxRuns) {
      // LOUD, NOT SILENT. A reader that was declined and said nothing would leave the surface
      // showing "reading…" forever, which is the one lie this module is built to avoid telling.
      sink(
        refused(
          runId,
          `already following ${this.runs.size} runs on this page (cap ${this.maxRuns}) — close one to follow this`
        )
      );
      return () => {};
    }
    if (existing && existing.sinks.size >= this.maxReaders) {
      sink(refused(runId, `already ${existing.sinks.size} readers on ${runId} (cap ${this.maxReaders})`));
      return () => {};
    }

    const reader: RunReader = existing ?? {
      sinks: new Set(),
      state: { runId, turns: null, history: null, failure: null, phase: 'reading', reads: 0, arrivals: 0 },
      fingerprint: null,
      cancel: null,
      reading: false,
      misses: 0,
    };
    if (!existing) this.runs.set(runId, reader);
    reader.sinks.add(sink);
    sink(reader.state);

    // The first sink arms the reader. A later one is seeded above instead — it must not restart a
    // poller that is already running, and it must not re-read a run that has already settled.
    //
    // A READER THAT GAVE UP GETS A FRESH BUDGET, not the exhausted one. Somebody opening the run
    // again is somebody asking to try again, and inheriting the spent `misses` would answer them
    // with one read and another refusal.
    if (reader.cancel === null && reader.state.phase !== 'settled') {
      reader.misses = 0;
      reader.cancel = this.schedule(() => void this.poll(runId), this.pollMs);
    }

    return () => {
      const r = this.runs.get(runId);
      if (!r) return;
      r.sinks.delete(sink);
      if (r.sinks.size === 0) {
        r.cancel?.();
        r.cancel = null;
        this.runs.delete(runId);
      }
    };
  }

  /**
   * Read once and, if the account CHANGED, hand the new one to every sink.
   *
   * Public so a test drives it deterministically; the scheduled tick calls the same method. Overlap
   * is guarded the way `RowTailHub` guards its LIST: a read slower than the interval must not stack
   * a second read on the first, and a reader released mid-flight must not deliver into a sink that
   * has gone.
   */
  async poll(runId: string): Promise<void> {
    const reader = this.runs.get(runId);
    if (!reader || reader.reading) return;
    reader.reading = true;
    try {
      const read = await this.read(runId);
      // Released while the read was in flight: the sinks are gone and this answer is about a run
      // nobody is looking at. Delivering it would revive a torn-down reader.
      if (this.runs.get(runId) !== reader) return;
      if (read.ok) {
        reader.misses = 0;
        const settled = !read.turns.live;
        if (settled) this.stop(reader);
        const fingerprint = transcriptFingerprint(read.turns);
        const phase: FollowPhase = settled ? 'settled' : 'following';
        // COALESCED ON THE ACCOUNT, NOT THE CLOCK. An unchanged log emits nothing, so nothing on
        // screen moves — unless the PHASE moved, which is the run closing and is an arrival in its
        // own right: "still open" becoming "finished" is the most important turn there is.
        if (fingerprint === reader.fingerprint && phase === reader.state.phase) return;
        reader.fingerprint = fingerprint;
        this.emit(reader, {
          runId,
          turns: read.turns,
          history: read.history,
          failure: null,
          phase,
          reads: reader.state.reads + 1,
          arrivals: reader.state.arrivals + 1,
        });
        return;
      }

      reader.misses += 1;
      const spent = reader.misses >= this.maxMisses;
      if (spent) this.stop(reader);
      // ONE WORD FOR BOTH, AND IT IS THE HONEST ONE. A reader that has spent its patience and one
      // that will try again in two seconds are both NOT keeping the account true right now, which is
      // the only thing the surface can act on. What separates them is `detail`, which says which
      // read failed and how.
      const failure = { gone: read.gone, detail: read.detail };
      const phase: FollowPhase = 'stale';
      const same =
        reader.state.phase === phase &&
        reader.state.failure?.gone === failure.gone &&
        reader.state.failure?.detail === failure.detail;
      // A route that keeps failing the same way is one fact, not one every two seconds. The read is
      // still counted — that is what says the reader is alive and being refused.
      reader.state = { ...reader.state, reads: reader.state.reads + 1 };
      if (same) return;
      this.emit(reader, {
        ...reader.state,
        // THE ACCOUNT SURVIVES A FAILED RE-READ. Blanking it would lose a good reading of the run
        // still on screen the moment one poll of it failed — the same rule `WorkflowsPage` already
        // applies to a one-shot read, kept here so following cannot be the thing that breaks it.
        failure,
        phase,
        arrivals: reader.state.arrivals + 1,
      });
    } finally {
      reader.reading = false;
    }
  }

  /** Which Runs this page is following. The number the total cap is about. */
  activeRuns(): string[] {
    return [...this.runs.keys()];
  }

  /** Stop every reader — for a test, and for a page teardown that wants to prove it left nothing
   *  behind. */
  close(): void {
    this.closed = true;
    for (const reader of this.runs.values()) reader.cancel?.();
    this.runs.clear();
  }

  private emit(reader: RunReader, state: Following): void {
    reader.state = state;
    for (const sink of reader.sinks) sink(state);
  }

  /** Disarm a reader without forgetting what it read. A settled run keeps its account and its sinks
   *  — it simply stops asking, because a closed run's history is final. */
  private stop(reader: RunReader): void {
    reader.cancel?.();
    reader.cancel = null;
  }
}

function refused(runId: string, detail: string): Following {
  return {
    runId,
    turns: null,
    history: null,
    failure: { gone: false, detail },
    phase: 'refused',
    reads: 0,
    arrivals: 0,
  };
}

/**
 * The hub the app follows runs through.
 *
 * ONE PER PAGE, module-scoped, because {@link FOLLOW_MAX_RUNS} is a statement about the tab. A hub
 * per component would cap nothing: four components would be four hubs of four.
 */
export const transcriptHub = new TranscriptHub();

/**
 * Follow one Run from a component. `null` closes the conversation and releases the reader.
 *
 * THE STATE IS NOT CLEARED ON A SWITCH, and that is the same rule `transcriptFor` enforces one level
 * up: a reading carries the id it is about, so holding the previous run's state for the one frame
 * before the new reader seeds is harmless, and blanking it here would make an open transcript flicker
 * to empty every time an operator clicked a different run. What must never happen is DRAWING it, and
 * that decision belongs to the guard that already exists rather than to a second one here.
 */
export function useFollowedRun(runId: string | null, hub: TranscriptHub = transcriptHub): Following | null {
  const [state, setState] = useState<Following | null>(null);
  useEffect(() => {
    if (runId === null) {
      setState(null);
      return;
    }
    return hub.watch(runId, setState);
  }, [runId, hub]);
  // A state about the run we have SINCE left is not this hook's answer. One line, for the frame
  // between a switch and the new reader's seed.
  return state && state.runId === runId ? state : null;
}
