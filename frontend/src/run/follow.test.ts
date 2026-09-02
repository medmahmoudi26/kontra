/**
 * Following a run's transcript: turns arriving in place, a finished run reopening identically, and
 * the three caps that stop a reader becoming the defect it was written not to reproduce.
 *
 * NO NETWORK, NO CLOCK, NO BROWSER. The hub takes its read and its scheduler as dependencies, so
 * every poll below is a function call and every assertion is about what the sink was handed —
 * which is what makes "the transcript gained turns without a refresh" an assertion rather than a
 * wait for two seconds and a hope.
 *
 * `cat` IS NEVER HAND-WRITTEN, the same discipline `turns.test.ts` keeps: it comes from
 * `history.ts`'s own `categorize`, so a fixture cannot quietly disagree with the reducer about what
 * counts as a failure — and the reducer is the thing deciding whether a run is still live.
 */

import { describe, expect, it, vi } from 'vitest';

import { categorize } from '@core/history';

import type { RunEvent, RunHistory } from './api';
import {
  TranscriptHub,
  transcriptFingerprint,
  type Following,
  type FollowPhase,
} from './follow';
import { readRunTurns, type RunTurnsRead } from './turns';

const BASE = 1_786_831_339_151;

function ev(id: number, type: string, ms: number, extra: Partial<RunEvent> = {}): RunEvent {
  return { id, type, cat: categorize(type), t: ms / 1000, at: BASE + ms, detail: type, attempt: 1, dur: 0, ...extra };
}

function closes(id: number, type: string, ms: number, openerMs: number, extra: Partial<RunEvent> = {}): RunEvent {
  return ev(id, type, ms, { dur: (ms - openerMs) / 1000, ...extra });
}

function history(events: RunEvent[]): RunHistory {
  return { events, scanned: events.length, elided: 0, truncated: false };
}

const STARTED = ev(1, 'WorkflowExecutionStarted', 0, {
  detail: 'workflowType=DnsSweep · taskQueue=wf-dnssweep · identity=1@65520c20a6a4',
});

/** One dispatch to an Actor, opened at `ms` and closed 900ms later. */
function call(id: number, ms: number): RunEvent[] {
  return [
    ev(id, 'NexusOperationScheduled', ms, { detail: 'endpoint=kontra-nscheck-0-1-0' }),
    closes(id + 1, 'NexusOperationCompleted', ms + 900, ms, { detail: 'endpoint=kontra-nscheck-0-1-0' }),
  ];
}

/** The log as it stood after `n` calls — a RUNNING run: nothing has closed it. */
function afterCalls(n: number): RunHistory {
  const events = [STARTED];
  for (let i = 0; i < n; i += 1) events.push(...call(2 + i * 2, 1_000 + i * 1_000));
  return history(events);
}

/** The same run, closed. */
function finished(n: number): RunHistory {
  const h = afterCalls(n);
  return history([...h.events, ev(2 + n * 2, 'WorkflowExecutionCompleted', 1_000 + n * 1_000)]);
}

function ok(runId: string, h: RunHistory): RunTurnsRead {
  return { ok: true, turns: readRunTurns(runId, h), history: h };
}

/** A scheduler that records the tick and fires nothing, so the manual `poll` calls below are the
 *  only producer of reads. `fire()` is one interval elapsing. */
function manual(): {
  schedule: (tick: () => void, ms: number) => () => void;
  fire: () => void;
  armed: () => number;
  cancels: () => number;
} {
  const ticks: Array<() => void> = [];
  let cancels = 0;
  return {
    schedule: (tick) => {
      ticks.push(tick);
      return () => {
        cancels += 1;
        const at = ticks.indexOf(tick);
        if (at >= 0) ticks.splice(at, 1);
      };
    },
    fire: () => {
      for (const tick of [...ticks]) tick();
    },
    armed: () => ticks.length,
    cancels: () => cancels,
  };
}

/** Collect everything a sink is handed. */
function sink(): { fn: (s: Following) => void; seen: Following[]; phases: () => FollowPhase[] } {
  const seen: Following[] = [];
  return { fn: (s) => seen.push(s), seen, phases: () => seen.map((s) => s.phase) };
}

/* ───────────────────────────── it streams ───────────────────────────── */

describe('a running run gains turns without a refresh', () => {
  it('hands the sink a new account each time the log actually grew', async () => {
    const clock = manual();
    let calls = 1;
    const read = vi.fn(async (runId: string) => ok(runId, afterCalls(calls)));
    const hub = new TranscriptHub({ read, schedule: clock.schedule });
    const s = sink();

    hub.watch('sweep-1', s.fn);
    // The seed: a reader exists, nothing has answered yet. Saying "reading" is true; drawing an
    // empty transcript would say the run had done nothing.
    expect(s.seen[0]?.phase).toBe('reading');
    expect(s.seen[0]?.turns).toBeNull();

    await hub.poll('sweep-1');
    const first = s.seen[s.seen.length - 1]!;
    expect(first.phase).toBe('following');
    const openedWith = first.turns!.named.turns.find((t) => t.turn.kind === 'dispatch')!;
    expect(openedWith.turn.count).toBe(1);

    // The run makes another two calls. NOTHING is refetched by hand and nothing is remounted; the
    // next poll of the same reader is what puts them on screen.
    calls = 3;
    await hub.poll('sweep-1');
    const second = s.seen[s.seen.length - 1]!;
    expect(second.phase).toBe('following');
    expect(second.arrivals).toBe(2);
    expect(second.turns!.runId).toBe('sweep-1');

    const grown = second.turns!.named.turns.find((t) => t.turn.kind === 'dispatch')!;
    // IN PLACE, WHICH IS THE POINT. A loop is one line however many times it goes round, so the two
    // new calls arrive as `×3` on the row that was already there — same kind, same first event, so
    // the same React key and the same `<details>` left open. Nothing is inserted above it and
    // nothing re-mounts; the only thing that changed is the number.
    expect(grown.turn.count).toBe(3);
    expect(grown.turn.events[0]).toBe(openedWith.turn.events[0]);
    expect(`${grown.turn.kind}:${grown.turn.events[0]}`).toBe(
      `${openedWith.turn.kind}:${openedWith.turn.events[0]}`
    );
    // ...and the events the row can be checked against grew with it.
    expect(grown.turn.events.length).toBeGreaterThan(openedWith.turn.events.length);
  });

  it('a turn the run adds arrives as a NEW row, under the ones already there', async () => {
    const clock = manual();
    let closed = false;
    const read = vi.fn(async (runId: string) => ok(runId, closed ? finished(2) : afterCalls(2)));
    const hub = new TranscriptHub({ read, schedule: clock.schedule });
    const s = sink();
    hub.watch('sweep-1', s.fn);
    await hub.poll('sweep-1');
    const before = s.seen[s.seen.length - 1]!.turns!.named.turns.map((t) => t.term);

    closed = true;
    await hub.poll('sweep-1');
    const after = s.seen[s.seen.length - 1]!.turns!.named.turns.map((t) => t.term);
    // APPENDED, NOT REORDERED. Every row that was on screen is still on screen, in the same order,
    // with the new one after them — which is what lets the surface update without the reader losing
    // their place.
    expect(after.slice(0, before.length)).toEqual(before);
    expect(after.length).toBe(before.length + 1);
  });

  it('a poll that finds the log unchanged tells nobody — arrival is arrival, not a heartbeat', async () => {
    const clock = manual();
    const read = vi.fn(async (runId: string) => ok(runId, afterCalls(2)));
    const hub = new TranscriptHub({ read, schedule: clock.schedule });
    const s = sink();
    hub.watch('sweep-1', s.fn);

    await hub.poll('sweep-1');
    await hub.poll('sweep-1');
    await hub.poll('sweep-1');

    expect(read).toHaveBeenCalledTimes(3);
    // One seed and ONE arrival, from three reads. A surface driven by this cannot blink on a poll
    // that found nothing, because it is not told about one.
    expect(s.seen).toHaveLength(2);
    expect(s.seen[1]?.arrivals).toBe(1);
  });

  it('the run closing IS an arrival, and the reader disarms itself', async () => {
    const clock = manual();
    let closed = false;
    const read = vi.fn(async (runId: string) => ok(runId, closed ? finished(2) : afterCalls(2)));
    const hub = new TranscriptHub({ read, schedule: clock.schedule });
    const s = sink();
    hub.watch('sweep-1', s.fn);
    expect(clock.armed()).toBe(1);

    await hub.poll('sweep-1');
    expect(s.seen[s.seen.length - 1]?.phase).toBe('following');

    closed = true;
    await hub.poll('sweep-1');
    const last = s.seen[s.seen.length - 1]!;
    expect(last.phase).toBe('settled');
    expect(last.turns?.live).toBe(false);
    // A settled run is read once and never again: the interval is gone, so a tab left open on a run
    // that finished last week is not a request every two seconds about an answer that is final.
    expect(clock.armed()).toBe(0);
    clock.fire();
    expect(read).toHaveBeenCalledTimes(2);
  });

  it('two readers of one run share one read and see the same reading', async () => {
    const clock = manual();
    const read = vi.fn(async (runId: string) => ok(runId, afterCalls(2)));
    const hub = new TranscriptHub({ read, schedule: clock.schedule });
    const a = sink();
    const b = sink();
    hub.watch('sweep-1', a.fn);
    hub.watch('sweep-1', b.fn);
    expect(clock.armed()).toBe(1);

    await hub.poll('sweep-1');
    expect(read).toHaveBeenCalledTimes(1);
    // The SAME object, not two readings that happen to agree — which is what stops two panels on one
    // run showing visibly different ages.
    expect(a.seen[a.seen.length - 1]).toBe(b.seen[b.seen.length - 1]);
  });
});

/* ───────────────────────────── and it reopens ───────────────────────────── */

describe('a finished run reopens to the same transcript, from the same reader', () => {
  it('reads once, settles, and a reopen produces an identical account', async () => {
    const clock = manual();
    const read = vi.fn(async (runId: string) => ok(runId, finished(3)));
    const hub = new TranscriptHub({ read, schedule: clock.schedule });

    const first = sink();
    const release = hub.watch('sweep-9', first.fn);
    await hub.poll('sweep-9');
    const opened = first.seen[first.seen.length - 1]!;
    expect(opened.phase).toBe('settled');
    release();
    expect(hub.activeRuns()).toEqual([]);

    const again = sink();
    hub.watch('sweep-9', again.fn);
    await hub.poll('sweep-9');
    const reopened = again.seen[again.seen.length - 1]!;

    expect(reopened.phase).toBe('settled');
    // THE SAME TRANSCRIPT, not a similar one: same fingerprint, same rows, same words.
    expect(transcriptFingerprint(reopened.turns!)).toBe(transcriptFingerprint(opened.turns!));
    expect(reopened.turns!.named.turns.map((t) => t.label)).toEqual(
      opened.turns!.named.turns.map((t) => t.label)
    );
    expect(reopened.turns!.named.verdict.term).toBe(opened.turns!.named.verdict.term);
    // ONE READ EACH, and no interval left behind either time.
    expect(read).toHaveBeenCalledTimes(2);
    expect(clock.armed()).toBe(0);
  });

  it('the fingerprint moves when the run does, and not otherwise', () => {
    const two = readRunTurns('sweep-1', afterCalls(2));
    const twoAgain = readRunTurns('sweep-1', afterCalls(2));
    const three = readRunTurns('sweep-1', afterCalls(3));
    const done = readRunTurns('sweep-1', finished(2));
    expect(transcriptFingerprint(two)).toBe(transcriptFingerprint(twoAgain));
    expect(transcriptFingerprint(three)).not.toBe(transcriptFingerprint(two));
    expect(transcriptFingerprint(done)).not.toBe(transcriptFingerprint(two));
  });
});

/* ───────────────────────────── teardown ───────────────────────────── */

describe('a closed reader stops its stream', () => {
  it('the last release cancels the interval and the run stops being read', async () => {
    const clock = manual();
    const read = vi.fn(async (runId: string) => ok(runId, afterCalls(2)));
    const hub = new TranscriptHub({ read, schedule: clock.schedule });
    const s = sink();
    const release = hub.watch('sweep-1', s.fn);
    await hub.poll('sweep-1');
    expect(read).toHaveBeenCalledTimes(1);
    expect(clock.armed()).toBe(1);

    release();

    expect(clock.armed()).toBe(0);
    expect(clock.cancels()).toBe(1);
    expect(hub.activeRuns()).toEqual([]);
    // Every way the reader could still fire: the interval elapsing, and a poll called directly.
    clock.fire();
    await hub.poll('sweep-1');
    expect(read).toHaveBeenCalledTimes(1);
    const after = s.seen.length;
    clock.fire();
    expect(s.seen).toHaveLength(after);
  });

  it('one of two readers leaving does NOT stop the stream', async () => {
    const clock = manual();
    const read = vi.fn(async (runId: string) => ok(runId, afterCalls(2)));
    const hub = new TranscriptHub({ read, schedule: clock.schedule });
    const a = sink();
    const b = sink();
    const releaseA = hub.watch('sweep-1', a.fn);
    hub.watch('sweep-1', b.fn);
    releaseA();
    expect(hub.activeRuns()).toEqual(['sweep-1']);
    expect(clock.armed()).toBe(1);
    await hub.poll('sweep-1');
    expect(a.seen).toHaveLength(1); // the seed it got on the way in, and nothing after it left
    expect(b.seen).toHaveLength(2);
  });

  it('a read still in flight when the reader is released is never delivered', async () => {
    const clock = manual();
    let settle: (r: RunTurnsRead) => void = () => {};
    const read = vi.fn(
      () =>
        new Promise<RunTurnsRead>((resolve) => {
          settle = resolve;
        })
    );
    const hub = new TranscriptHub({ read, schedule: clock.schedule });
    const s = sink();
    const release = hub.watch('sweep-1', s.fn);
    const inFlight = hub.poll('sweep-1');
    release();
    settle(ok('sweep-1', afterCalls(2)));
    await inFlight;
    // The seed only. An answer about a run nobody is looking at must not revive a torn-down reader.
    expect(s.seen).toHaveLength(1);
    expect(hub.activeRuns()).toEqual([]);
  });

  it('close() stops every reader at once', async () => {
    const clock = manual();
    const read = vi.fn(async (runId: string) => ok(runId, afterCalls(1)));
    const hub = new TranscriptHub({ read, schedule: clock.schedule });
    hub.watch('a', sink().fn);
    hub.watch('b', sink().fn);
    expect(hub.activeRuns()).toHaveLength(2);
    hub.close();
    expect(hub.activeRuns()).toEqual([]);
    expect(clock.armed()).toBe(0);
    await hub.poll('a');
    expect(read).not.toHaveBeenCalled();
  });
});

/* ───────────────────────────── the caps ───────────────────────────── */

describe('readers are bounded, and a refusal is said out loud', () => {
  it('refuses a run past the total cap, and reads nothing for it', async () => {
    const clock = manual();
    const read = vi.fn(async (runId: string) => ok(runId, afterCalls(1)));
    const hub = new TranscriptHub({ read, schedule: clock.schedule, maxRuns: 2 });
    hub.watch('a', sink().fn);
    hub.watch('b', sink().fn);
    const third = sink();
    const release = hub.watch('c', third.fn);

    expect(third.seen[0]?.phase).toBe('refused');
    expect(third.seen[0]?.failure?.detail).toContain('cap 2');
    // NOT FOLLOWED AND NOT SILENTLY POLLED: no third reader, no third interval, no read.
    expect(hub.activeRuns().sort()).toEqual(['a', 'b']);
    expect(clock.armed()).toBe(2);
    await hub.poll('c');
    expect(read).not.toHaveBeenCalled();
    // The release of a refused reader is a no-op that must not tear down somebody else's.
    release();
    expect(hub.activeRuns().sort()).toEqual(['a', 'b']);
  });

  it('a run already being followed is admitted even when the total cap is reached', () => {
    const clock = manual();
    const hub = new TranscriptHub({
      read: async (runId) => ok(runId, afterCalls(1)),
      schedule: clock.schedule,
      maxRuns: 2,
    });
    hub.watch('a', sink().fn);
    hub.watch('b', sink().fn);
    const another = sink();
    hub.watch('a', another.fn);
    // The cap is on RUNS, not on readers: a second panel on a run this page is already following
    // costs nothing and must not be turned away.
    expect(another.seen[0]?.phase).not.toBe('refused');
    expect(hub.activeRuns().sort()).toEqual(['a', 'b']);
  });

  it('refuses a reader past the per-run cap', () => {
    const clock = manual();
    const hub = new TranscriptHub({
      read: async (runId) => ok(runId, afterCalls(1)),
      schedule: clock.schedule,
      maxReaders: 2,
    });
    hub.watch('a', sink().fn);
    hub.watch('a', sink().fn);
    const third = sink();
    hub.watch('a', third.fn);
    expect(third.seen[0]?.phase).toBe('refused');
    expect(third.seen[0]?.failure?.detail).toContain('cap 2');
  });

  it('gives up on a run that will not answer, and keeps the account it already had', async () => {
    const clock = manual();
    let broken = false;
    const read = vi.fn(
      async (runId: string): Promise<RunTurnsRead> =>
        broken ? { ok: false, runId, gone: false, detail: '502 Bad Gateway' } : ok(runId, afterCalls(2))
    );
    const hub = new TranscriptHub({ read, schedule: clock.schedule, maxMisses: 3 });
    const s = sink();
    hub.watch('sweep-1', s.fn);
    await hub.poll('sweep-1');
    const good = s.seen[s.seen.length - 1]!.turns;
    expect(good).not.toBeNull();

    broken = true;
    await hub.poll('sweep-1');
    await hub.poll('sweep-1');
    await hub.poll('sweep-1');

    const last = s.seen[s.seen.length - 1]!;
    expect(last.phase).toBe('stale');
    expect(last.failure?.detail).toContain('502');
    // THE ACCOUNT SURVIVES. A run whose re-read failed still has a transcript that was true when it
    // was read; blanking it would lose the only thing on screen worth having.
    expect(last.turns).toBe(good);
    // Three misses is the cap: the reader is disarmed, so a tab open on a dead route is not a
    // request every two seconds forever.
    expect(clock.armed()).toBe(0);
    const before = read.mock.calls.length;
    clock.fire();
    expect(read).toHaveBeenCalledTimes(before);
  });

  it('does not repeat one failure every poll', async () => {
    const clock = manual();
    const read = vi.fn(async (runId: string): Promise<RunTurnsRead> => ({
      ok: false,
      runId,
      gone: true,
      detail: 'Temporal has no history under that id',
    }));
    const hub = new TranscriptHub({ read, schedule: clock.schedule, maxMisses: 4 });
    const s = sink();
    hub.watch('never-started', s.fn);
    await hub.poll('never-started');
    await hub.poll('never-started');
    await hub.poll('never-started');
    // The seed, and ONE sentence about the failure — not one every two seconds.
    expect(s.seen).toHaveLength(2);
    expect(s.seen[1]?.failure?.gone).toBe(true);
    // ...but the reads are still counted, which is what says the reader is alive and being refused.
    expect(s.seen[1]?.reads).toBe(1);
    expect(read).toHaveBeenCalledTimes(3);
  });

  it('does not stack a second read on a slow one', async () => {
    const clock = manual();
    let settle: (r: RunTurnsRead) => void = () => {};
    const read = vi.fn(
      () =>
        new Promise<RunTurnsRead>((resolve) => {
          settle = resolve;
        })
    );
    const hub = new TranscriptHub({ read, schedule: clock.schedule });
    hub.watch('sweep-1', sink().fn);
    const first = hub.poll('sweep-1');
    void hub.poll('sweep-1');
    void hub.poll('sweep-1');
    expect(read).toHaveBeenCalledTimes(1);
    settle(ok('sweep-1', afterCalls(1)));
    await first;
  });
});
