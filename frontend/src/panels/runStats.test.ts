/**
 * The run's numbers, and the four authorities they must never be blended between.
 *
 * WHAT IS PINNED HERE IS THE PROVENANCE, NOT THE ARITHMETIC. Summing a series and counting an event
 * type are not what goes wrong; what goes wrong is a number appearing under the wrong authority's
 * name, or a silence being drawn as a zero. So every case below asserts which group a reading is
 * in, or which of the several kinds of nothing it says.
 *
 * THE THREE SILENCES ARE THREE ASSERTIONS, in both directions: a ledger nobody could read, a ledger
 * holding no record, and a record that committed zero rows are three different statements — and a
 * run that committed nothing must not be able to render as one that committed steadily, which is
 * the acceptance criterion this file exists to make mechanical.
 */

import { describe, expect, it } from 'vitest';

import type { Concern } from '@core/vocabulary';

import type { DatasetInfo, MaterializationSummary, RunEvent, RunHistory, RunRow } from '../run/api';
import { SPARK_MIN } from '../components/spark';
import type { RunDatasets } from './runDatasets';
import { ISOLATION_TERMS, perSecText, runStats, runThroughput, type RunStats } from './runStats';

const T0 = 1_787_084_868_000;
const NOW = T0 + 300_000;

function run(over: Partial<RunRow> = {}): RunRow {
  return {
    runId: 'sweep-1',
    type: 'DnsSweep',
    status: 'running',
    tenant: 'default',
    startedAt: T0,
    closedAt: 0,
    dispatches: 3,
    ...over,
  };
}

function ledger(over: Partial<MaterializationSummary> = {}): MaterializationSummary {
  return { total: 2, pending: 0, running: 0, complete: 2, failed: 0, rows: 623, bytes: 4096, ...over };
}

function ev(id: number, type: string, over: Partial<RunEvent> = {}): RunEvent {
  return { id, type, cat: 'activity', t: id, at: T0 + id * 1000, detail: type, attempt: 1, dur: 0, ...over };
}

function history(events: RunEvent[]): RunHistory {
  return { events, scanned: events.length, elided: 0, truncated: false };
}

function ds(over: Partial<DatasetInfo> & { name: string }): DatasetInfo {
  return { kind: 'output', rows: 0, bytes: 0, ...over };
}

/** A catalog reading with one partition this run alone wrote. `read` is the poll having answered. */
function wroteOne(name = 'lame', how: 'sole' | 'among' | 'owner' = 'sole'): RunDatasets {
  return { wrote: [{ info: ds({ name, version: 'v0.1.0', dt: '2026-08-27T09-00-00' }), how }], partial: false, read: true };
}

const NOTHING: RunDatasets = { wrote: [], partial: false, read: true };
const UNREAD: RunDatasets = { wrote: [], partial: false, read: false };

/** The key the store files a Dataset's rate series under — `kind:name:version:dt`. */
const KEY = 'output:lame:v0.1.0:2026-08-27T09-00-00';

function stats(over: Partial<Parameters<typeof runStats>[0]> = {}): RunStats {
  const got = runStats({
    run: run(),
    history: null,
    siblings: [],
    wrote: NOTHING,
    series: {},
    concerns: [],
    now: NOW,
    ...over,
  });
  if (got === null) throw new Error('expected stats for a run that is open');
  return got;
}

/** One reading, by id — and which authority it was drawn under, which is half of what is asserted. */
function reading(s: RunStats, id: string): { value: string; note: string; source: string; alarming?: boolean; samples: number | null; bars: number | null } {
  for (const g of s.groups) {
    for (const r of g.readings) {
      if (r.id !== id) continue;
      return {
        value: r.value,
        note: r.note,
        source: g.source,
        ...(r.alarming === undefined ? {} : { alarming: r.alarming }),
        samples: r.series ? r.series.length : null,
        bars: r.bars ? r.bars.length : null,
      };
    }
  }
  throw new Error(`no reading ${id}`);
}

describe('no run open', () => {
  it('has no stats at all rather than a row of em-dashes', () => {
    // A strip of four unmeasured slots under a thread nobody opened a conversation in is chrome
    // dressed as a measurement. The absence is the honest answer and the bar draws nothing.
    expect(
      runStats({ run: null, history: null, siblings: [], wrote: NOTHING, series: {}, concerns: [], now: NOW })
    ).toBeNull();
  });
});

describe('each number under its own authority', () => {
  it('files committed under the ledger, activities and retries under Temporal, the streak under the run list', () => {
    // THE WHOLE POINT OF THE GROUPING. These answer different questions from different sources and
    // are never blended into one "progress" number; the group is what says so in the surface.
    const s = stats({
      run: run({ materialization: ledger() }),
      history: history([ev(1, 'ActivityTaskScheduled')]),
      siblings: [run({ status: 'completed', closedAt: T0 + 60_000 })],
    });
    expect(reading(s, 'committed').source).toBe('ledger');
    expect(reading(s, 'throughput').source).toBe('lake');
    expect(reading(s, 'activities').source).toBe('temporal');
    expect(reading(s, 'retries').source).toBe('temporal');
    expect(reading(s, 'streak').source).toBe('run list');
  });

  it('draws no verdict over the four — there is no fifth blended number', () => {
    const s = stats({ run: run({ materialization: ledger() }) });
    const ids = s.groups.flatMap((g) => g.readings.map((r) => r.id));
    expect(ids).toEqual(['committed', 'throughput', 'activities', 'retries', 'streak']);
  });
});

describe('the ledger: three silences, three words', () => {
  it('says the ledger is unread rather than reporting zero', () => {
    // `materialization: null` is "the ledger could not be read" and is NOT a claim about the run.
    const r = reading(stats({ run: run({ materialization: null }) }), 'committed');
    expect(r.value).toBe('—');
    expect(r.note).toBe('ledger unread');
  });

  it('says unrecorded when the ledger holds no record for this run', () => {
    const r = reading(stats({ run: run({ materialization: ledger({ total: 0, complete: 0, rows: 0 }) }) }), 'committed');
    expect(r.value).toBe('—');
    expect(r.note).toBe('unrecorded');
  });

  it('reports the count with the scope it counted, never bare', () => {
    const r = reading(stats({ run: run({ materialization: ledger() }) }), 'committed');
    expect(r.value).toBe('623');
    // `this run` is the Datasets console's own word for the scope — one vocabulary, so a reader
    // cannot take it for the Dataset's total, which is the 623-vs-1,246 confusion.
    expect(r.note).toContain('this run');
    expect(r.note).toContain('2/2 datasets');
  });
});

describe('a run that committed nothing is not a run that committed steadily', () => {
  it('draws a settled empty commit as zero, alarming, with the sentence beside it', () => {
    const s = stats({
      run: run({ status: 'completed', closedAt: T0 + 60_000, materialization: ledger({ rows: 0 }) }),
    });
    const r = reading(s, 'committed');
    expect(r.value).toBe('0');
    expect(r.alarming).toBe(true);
    // AND IT IS SAID OUT LOUD, because `completed` is true and useless on its own: a dropped Unit
    // does not fail a workflow, so this run's row, its badge and `kontra runs` all read perfect.
    const alarm = s.alarms.find((a) => a.id === 'committed-nothing');
    expect(alarm?.source).toBe('ledger');
    expect(alarm?.text).toContain('committed no rows');
  });

  it('does not cry wolf over a running sweep that has not reached its first commit', () => {
    const s = stats({ run: run({ materialization: ledger({ rows: 0 }) }) });
    expect(reading(s, 'committed').alarming).toBe(false);
    expect(s.alarms.map((a) => a.id)).not.toContain('committed-nothing');
  });

  it('is a different string from a steady run, in every direction', () => {
    const empty = reading(stats({ run: run({ status: 'completed', closedAt: T0 + 1, materialization: ledger({ rows: 0 }) }) }), 'committed');
    const steady = reading(stats({ run: run({ materialization: ledger() }) }), 'committed');
    const unrecorded = reading(stats({ run: run({ materialization: ledger({ total: 0, rows: 0, complete: 0 }) }) }), 'committed');
    expect(new Set([empty.value, steady.value, unrecorded.value]).size).toBe(3);
  });
});

describe('Temporal: activities, retries and failures are separate numbers', () => {
  it('says the history is unread rather than counting zero of everything', () => {
    const s = stats({ history: null });
    expect(reading(s, 'activities').value).toBe('—');
    expect(reading(s, 'activities').note).toContain('history unread');
    expect(reading(s, 'retries').value).toBe('—');
  });

  it('counts a run whose history has answered with nothing in it as zero', () => {
    // A read history with no events is a MEASUREMENT — different from nobody having looked.
    const s = stats({ history: history([]) });
    expect(reading(s, 'activities').value).toBe('0');
    expect(reading(s, 'retries').value).toBe('0');
  });

  it('keeps retries and failures apart, and colours only the failures', () => {
    // One failing Batch retried three times is ONE problem and THREE retries. A single count hides
    // which, so the note carries the failures beside the number rather than replacing it.
    const s = stats({
      history: history([
        ev(1, 'ActivityTaskScheduled'),
        ev(2, 'ActivityTaskStarted', { attempt: 2 }),
        ev(3, 'ActivityTaskStarted', { attempt: 3 }),
        ev(4, 'ActivityTaskFailed', { cat: 'failure' }),
      ]),
    });
    expect(reading(s, 'retries').value).toBe('2');
    expect(reading(s, 'retries').note).toBe('retries · 1 failed');
    expect(reading(s, 'retries').alarming).toBe(true);
    expect(reading(s, 'activities').value).toBe('1');
    expect(reading(s, 'activities').alarming).toBeUndefined();
  });

  it('says none failed when none did, rather than leaving the reader to infer it', () => {
    const s = stats({ history: history([ev(1, 'ActivityTaskScheduled')]) });
    expect(reading(s, 'retries').note).toBe('retries · none failed');
    expect(reading(s, 'retries').alarming).toBe(false);
  });
});

describe('throughput: two samples of one clock, or nothing', () => {
  it('draws nothing and reports no rate under two samples', () => {
    // SPARK_MIN is the rule and it is not restated here — the series is handed over whatever its
    // length and the spark refuses it. What this asserts is that a single sample is not dressed up
    // as a rate, and that the reason is legible.
    const s = stats({ wrote: wroteOne(), series: { [KEY]: [4] } });
    const r = reading(s, 'throughput');
    expect(r.samples).toBe(1);
    expect(r.samples! < SPARK_MIN).toBe(true);
    expect(r.value).toBe('4.0');
    const none = stats({ wrote: wroteOne(), series: {} });
    expect(reading(none, 'throughput').value).toBe('—');
    expect(reading(none, 'throughput').note).toContain('one sample so far');
  });

  it('reports the newest sample once two exist', () => {
    const s = stats({ wrote: wroteOne(), series: { [KEY]: [4, 12] } });
    const r = reading(s, 'throughput');
    expect(r.value).toBe('12');
    expect(r.samples).toBe(2);
    expect(r.note).toContain('units/s');
  });

  it('tells the four silences apart', () => {
    // Only ONE of these resolves itself by waiting, and an operator who cannot tell them apart will
    // wait for all four.
    expect(reading(stats({ wrote: UNREAD }), 'throughput').note).toContain('has not been listed');
    expect(reading(stats({ wrote: NOTHING }), 'throughput').note).toContain('nothing in the lake carries this run');
    expect(reading(stats({ wrote: wroteOne('apexes', 'among') }), 'throughput').note).toContain('shared partitions');
    expect(reading(stats({ wrote: wroteOne(), series: {} }), 'throughput').note).toContain('a rate needs two');
  });

  it('refuses a rate for a settled run, and draws no line for one', () => {
    // A rate is a statement about NOW. The lake's current 0/s under a run that closed on Tuesday
    // answers a question nobody asked with a number that reads as a verdict on the run.
    const s = stats({
      run: run({ status: 'completed', closedAt: T0 + 60_000 }),
      wrote: wroteOne(),
      series: { [KEY]: [4, 12] },
    });
    const r = reading(s, 'throughput');
    expect(r.value).toBe('—');
    expect(r.samples).toBeNull();
    expect(r.note).toContain('settled');
  });

  it('sums only the partitions this run alone wrote, and says how many it left out', () => {
    // A partition several Runs appended to has no run-scoped rate: crediting this run with its
    // whole write rate would be attributing somebody else's rows.
    const wrote: RunDatasets = {
      wrote: [
        { info: ds({ name: 'lame', version: 'v0.1.0', dt: '2026-08-27T09-00-00' }), how: 'sole' },
        { info: ds({ name: 'apexes', version: 'v0.1.0', dt: '2026-08-27T09-00-00' }), how: 'among' },
      ],
      partial: false,
      read: true,
    };
    const flow = runThroughput(wrote, {
      [KEY]: [1, 2],
      'output:apexes:v0.1.0:2026-08-27T09-00-00': [100, 100],
    });
    expect(flow.series).toEqual([1, 2]);
    expect(flow.mine).toBe(1);
    expect(flow.shared).toBe(1);
  });

  it('right-aligns two partitions that started at different times', () => {
    // `sumSeries` aligns at the NEWEST end: a Dataset that first appeared thirty seconds in has a
    // shorter series, and those are the most RECENT samples, not the oldest.
    const wrote: RunDatasets = {
      wrote: [
        { info: ds({ name: 'lame', version: 'v0.1.0', dt: '2026-08-27T09-00-00' }), how: 'sole' },
        { info: ds({ name: 'tmp', version: 'v0.1.0', dt: '2026-08-27T09-00-00' }), how: 'owner' },
      ],
      partial: false,
      read: true,
    };
    const flow = runThroughput(wrote, {
      [KEY]: [1, 2, 3],
      'output:tmp:v0.1.0:2026-08-27T09-00-00': [10],
    });
    expect(flow.series).toEqual([1, 2, 13]);
  });
});

describe('the streak comes from the run list and from nowhere else', () => {
  it('says there is nothing to compare rather than a streak of zero', () => {
    const r = reading(stats({ siblings: [] }), 'streak');
    expect(r.value).toBe('—');
    expect(r.bars).toBeNull();
  });

  it('counts green from the newest end and draws a bar per sibling', () => {
    const siblings = [
      run({ runId: 'r3', status: 'completed', closedAt: T0 + 3 }),
      run({ runId: 'r2', status: 'completed', closedAt: T0 + 2 }),
      run({ runId: 'r1', status: 'failed', closedAt: T0 + 1 }),
    ];
    const r = reading(stats({ siblings }), 'streak');
    expect(r.value).toBe('2');
    expect(r.bars).toBe(3);
    expect(r.note).toBe('green · last 3');
  });
});

describe('dropped Units, which nothing else on the page will report', () => {
  function concern(term: string, label: string): Concern {
    return { term: term as Concern['term'], label, because: `dropped=12 of in=12`, events: [4], t: 1 };
  }

  it('repeats the isolation concerns and nothing else out of the transcript', () => {
    // A Method that isolated every Unit returns normally and the history is CLEAN, so this is the
    // one failure with no failure event — and the only reason the bar repeats the transcript at all.
    const s = stats({
      concerns: [
        concern('every-unit-isolated', 'every Unit was isolated — this call returned having done nothing'),
        concern('queue-unpolled', 'a queue nobody is polling'),
        concern('log-elided', 'the log was elided'),
      ],
    });
    expect(s.alarms.map((a) => a.id)).toEqual(['every-unit-isolated']);
    expect(s.alarms[0]!.source).toBe('temporal');
    expect(ISOLATION_TERMS).toContain('units-isolated');
  });

  it('collapses one term raised by many calls into one line with the count', () => {
    const s = stats({
      concerns: [
        concern('units-isolated', '3 Units were isolated and dropped'),
        concern('units-isolated', '4 Units were isolated and dropped'),
      ],
    });
    expect(s.alarms).toHaveLength(1);
    expect(s.alarms[0]!.text).toContain('in 2 calls');
  });

  it('is silent when the transcript has not arrived — silence is never evidence', () => {
    expect(stats({ concerns: [] }).alarms).toEqual([]);
  });
});

describe('a failed materialization', () => {
  it('is raised on its own, because no execution status reports it', () => {
    // The rows exist and are not queryable. `completed` says nothing about that (ADR 0017).
    const s = stats({ run: run({ materialization: ledger({ failed: 1, complete: 1 }) }) });
    const alarm = s.alarms.find((a) => a.id === 'output-failed');
    expect(alarm?.source).toBe('ledger');
    expect(alarm?.text).toContain('1 of 2 Datasets');
  });

  it('does not raise an alarm for ordinary retries, which every other reading already shows', () => {
    // The run's status, the transcript and the rose `n failed` beside the retries all report a
    // failed activity. A bar that shouts about them is a bar an operator learns to skip.
    const s = stats({ history: history([ev(1, 'ActivityTaskFailed', { cat: 'failure' })]) });
    expect(s.alarms).toEqual([]);
  });
});

describe('the rate, in words', () => {
  it('keeps a decimal under ten, where the fleet rail does not need one', () => {
    // A run committing a row every three seconds rounds to `0`, and reporting a moving run as
    // stopped is the confusion this reading exists to remove.
    expect(perSecText(0.4)).toBe('0.4');
    expect(perSecText(9.94)).toBe('9.9');
    expect(perSecText(12.4)).toBe('12');
    expect(perSecText(1200)).toBe('1,200');
    expect(perSecText(Number.NaN)).toBe('—');
  });
});
