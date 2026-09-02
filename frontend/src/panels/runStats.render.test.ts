/**
 * The run's numbers, drawn — every state of them, as markup.
 *
 * NODE, NO JSDOM, NO TESTING-LIBRARY, as everywhere else here: `renderToStaticMarkup` and string
 * assertions about text and `data-testid`.
 *
 * WHAT IS ASSERTED IS WHAT AN OPERATOR CAN READ. Not that a component was handed a series — that
 * there is no `<svg>` on the page when one sample has been taken, that `0` and `—` and `623` are
 * three different strings, and that the sentence about a run which committed nothing is on screen
 * rather than encoded in a colour. Every one of those has a wrong version that renders happily.
 *
 * AND WHERE IT LANDS IS ASSERTED TOO. The bar is only a reading of "is this run moving" if it is on
 * screen while the operator is looking at something else, so the last block draws the whole thread
 * and pins the strip between the bar that names the run and the panel that is about to change.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import type { Concern } from '@core/vocabulary';

import type { DatasetInfo, MaterializationSummary, RunEvent, RunHistory, RunRow } from '../run/api';
import { RunStatsBar } from './RunStats';
import { runStats, type RunStatsInput } from './runStats';
import type { RunDatasets } from './runDatasets';
import { WorkflowThread, type WorkflowThreadProps } from './WorkflowThread';
import { threadOf } from './workflowThread';

const T0 = 1_787_084_868_000;
const NOW = T0 + 300_000;
const KEY = 'output:lame:v0.1.0:2026-08-27T09-00-00';

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

/** One partition this run alone wrote — the shape a rate may honestly be summed over. */
const MINE: RunDatasets = {
  wrote: [{ info: ds({ name: 'lame', version: 'v0.1.0', dt: '2026-08-27T09-00-00' }), how: 'sole' }],
  partial: false,
  read: true,
};

function draw(over: Partial<RunStatsInput> = {}): string {
  const stats = runStats({
    run: run({ materialization: ledger() }),
    history: history([ev(1, 'ActivityTaskScheduled')]),
    siblings: [],
    wrote: MINE,
    series: {},
    concerns: [],
    now: NOW,
    ...over,
  });
  return renderToStaticMarkup(createElement(RunStatsBar, { stats }));
}

/** The value one reading printed, read back out of the markup it was drawn into. */
function value(html: string, id: string): string {
  const found = new RegExp(`data-testid="stat-${id}" data-value="([^"]*)"`).exec(html);
  return found?.[1] ?? '';
}

describe('no run open', () => {
  it('draws nothing at all', () => {
    // NOT AN EMPTY STRIP. A row of em-dashes under a thread nobody opened a conversation in is
    // chrome dressed as a measurement — the same rule the spark follows for an empty series.
    const html = renderToStaticMarkup(createElement(RunStatsBar, { stats: null }));
    expect(html).toBe('');
  });
});

describe('a run that is going', () => {
  it('draws all four authorities, each under its own name', () => {
    const html = draw({ siblings: [run({ status: 'completed', closedAt: T0 + 60_000 })] });
    expect(html).toContain('data-testid="run-stats"');
    expect(html).toContain('data-run="sweep-1"');
    for (const source of ['ledger', 'lake', 'temporal', 'run-list']) {
      expect(html).toContain(`data-testid="run-stat-group-${source}"`);
    }
    // The source is a WORD IN THE SURFACE, not a tooltip: a scope that lives only on hover is
    // unlabelled to everyone who does not hover.
    expect(html).toContain('>ledger<');
    expect(html).toContain('>run list<');
  });

  it('prints the committed count with the scope it counted', () => {
    const html = draw();
    expect(value(html, 'committed')).toBe('623');
    expect(html).toContain('committed · this run · 2/2 datasets');
  });

  it('draws no alarm band when nothing is wrong', () => {
    expect(draw()).not.toContain('data-testid="run-stats-alarms"');
    expect(draw()).toContain('data-alarms="0"');
  });
});

describe('the throughput series', () => {
  it('draws NOTHING under two samples rather than a flat zero', () => {
    // A flat line is a measured zero and an empty slot is "nobody looked". One sample is not a
    // direction, so there is no polyline on the page — the slot keeps its width and stays empty.
    const html = draw({ series: { [KEY]: [4] } });
    expect(html).not.toContain('data-testid="spark"');
    expect(html).not.toContain('<polyline');
  });

  it('draws the line once two samples of one clock exist', () => {
    const html = draw({ series: { [KEY]: [4, 12] } });
    expect(html).toContain('data-testid="spark"');
    expect(html).toContain('data-samples="2"');
    expect(value(html, 'throughput')).toBe('12');
  });

  it('says which kind of nothing it has when there is no rate', () => {
    const html = draw({ series: {} });
    expect(value(html, 'throughput')).toBe('—');
    expect(html).toContain('one sample so far — a rate needs two');
  });
});

describe('a run that has finished', () => {
  const finished = { status: 'completed' as const, closedAt: T0 + 60_000 };

  it('keeps what it committed and refuses a rate', () => {
    const html = draw({ run: run({ ...finished, materialization: ledger() }), series: { [KEY]: [4, 12] } });
    expect(value(html, 'committed')).toBe('623');
    expect(value(html, 'throughput')).toBe('—');
    expect(html).toContain('settled — nothing is landing');
    expect(html).not.toContain('data-testid="spark"');
  });

  it('draws the streak of the workflow it belongs to', () => {
    const siblings = [
      run({ runId: 'r3', ...finished }),
      run({ runId: 'r2', ...finished }),
      run({ runId: 'r1', status: 'failed', closedAt: T0 + 30_000 }),
    ];
    const html = draw({ run: run(finished), siblings });
    expect(value(html, 'streak')).toBe('2');
    expect(html).toContain('data-testid="streak"');
    expect(html).toContain('data-bars="3"');
  });
});

describe('a run that failed', () => {
  it('draws Temporal’s failures beside the retries rather than instead of them', () => {
    const html = draw({
      run: run({ status: 'failed', closedAt: T0 + 60_000, materialization: ledger() }),
      history: history([
        ev(1, 'ActivityTaskScheduled'),
        ev(2, 'ActivityTaskStarted', { attempt: 2 }),
        ev(3, 'ActivityTaskFailed', { cat: 'failure' }),
      ]),
    });
    expect(value(html, 'retries')).toBe('1');
    expect(html).toContain('retries · 1 failed');
    // The one number to act on is the only one coloured. A bar that is always red is a bar nobody
    // reads.
    expect(html).toContain('text-rose-400">1<');
  });

  it('says the ledger could not be read rather than reporting a zero for it', () => {
    const html = draw({ run: run({ status: 'failed', closedAt: T0 + 1, materialization: null }) });
    expect(value(html, 'committed')).toBe('—');
    expect(html).toContain('ledger unread');
  });
});

describe('a run that produced nothing', () => {
  it('is a different reading from one that committed steadily, and says so in words', () => {
    const html = draw({
      run: run({ status: 'completed', closedAt: T0 + 60_000, materialization: ledger({ rows: 0 }) }),
    });
    expect(value(html, 'committed')).toBe('0');
    // AND THE SENTENCE IS ON SCREEN. `completed` is true and useless on its own: a dropped Unit
    // does not fail a workflow, so every other signal on this page reads as a perfect run.
    expect(html).toContain('data-testid="run-alarm-committed-nothing"');
    expect(html).toContain('committed no rows');
    expect(html).toContain('data-source="ledger"');
  });

  it('draws the isolated Units impossible to miss, in their own band', () => {
    const concern: Concern = {
      term: 'every-unit-isolated',
      label: 'every Unit was isolated — this call returned having done nothing',
      because: 'dropped=623 of in=623',
      events: [12],
      t: 4,
    };
    const html = draw({
      run: run({ status: 'completed', closedAt: T0 + 60_000, materialization: ledger({ rows: 0 }) }),
      concerns: [concern],
    });
    expect(html).toContain('data-testid="run-stats-alarms"');
    expect(html).toContain('data-testid="run-alarm-every-unit-isolated"');
    expect(html).toContain('every Unit was isolated');
    // TWO AUTHORITIES, TWO LINES. The ledger says nothing landed; the history says why. Neither is
    // folded into the other.
    expect(html).toContain('data-alarms="2"');
  });

  it('says an unrecorded ledger is not a zero', () => {
    const html = draw({
      run: run({ status: 'completed', closedAt: T0 + 1, materialization: ledger({ total: 0, complete: 0, rows: 0 }) }),
    });
    expect(value(html, 'committed')).toBe('—');
    expect(html).toContain('unrecorded');
    // Nothing to shout about: no writer recorded a Dataset, which is a statement about the LEDGER
    // and never about the run.
    expect(html).not.toContain('data-testid="run-alarm-committed-nothing"');
  });
});

describe('a run in retry backoff', () => {
  it('reads differently from one doing steady work, which is the whole reason for the bar', () => {
    // Both are `running`. The rate and the retries are what separate them, and neither is anywhere
    // else on the page.
    const backoff = draw({
      history: history([
        ev(1, 'ActivityTaskScheduled'),
        ev(2, 'ActivityTaskStarted', { attempt: 2 }),
        ev(3, 'ActivityTaskStarted', { attempt: 3 }),
        ev(4, 'ActivityTaskFailed', { cat: 'failure' }),
      ]),
      series: { [KEY]: [0, 0] },
    });
    const working = draw({
      history: history([ev(1, 'ActivityTaskScheduled'), ev(2, 'ActivityTaskScheduled')]),
      series: { [KEY]: [11, 12] },
    });
    expect(value(backoff, 'retries')).toBe('2');
    expect(value(backoff, 'throughput')).toBe('0.0');
    expect(value(working, 'retries')).toBe('0');
    expect(value(working, 'throughput')).toBe('12');
    // A MEASURED ZERO IS STILL A MEASUREMENT: two samples of nothing landing draw a flat line, and
    // that is a different statement from the empty slot above.
    expect(backoff).toContain('data-testid="spark"');
  });
});

describe('where the bar sits', () => {
  const EDITOR = createElement('div', { 'data-testid': 'editor' });
  const PANE = createElement('div', { 'data-testid': 'pane' });

  function thread(over: Partial<WorkflowThreadProps> = {}): string {
    const props: WorkflowThreadProps = {
      workflow: 'dnssweep',
      thread: threadOf('DnsSweep', [run()], 'sweep-1'),
      tab: 'monitor',
      onTab: () => undefined,
      onOpenRun: () => undefined,
      loaded: null,
      code: EDITOR,
      monitor: PANE,
      now: NOW,
      ...over,
    };
    return renderToStaticMarkup(createElement(WorkflowThread, props));
  }

  it('is between the bar that names the run and the panel that changes with the tab', () => {
    // ABOVE EVERY TAB, for the reason the scope bar is one bar and not five headings: "is this run
    // moving" is a property of the conversation, not of the panel somebody happens to be reading.
    // This draws the MONITOR tab deliberately — the numbers have to be there while an operator is
    // looking at the Machines, which is exactly when they ask.
    const html = thread({ stats: createElement(RunStatsBar, { stats: runStats(baseline()) }) });
    const scope = html.indexOf('data-testid="run-scope"');
    const stats = html.indexOf('data-testid="run-stats"');
    const panel = html.indexOf('data-testid="tab-panel"');
    expect(scope).toBeGreaterThan(-1);
    expect(stats).toBeGreaterThan(scope);
    expect(panel).toBeGreaterThan(stats);
  });

  it('leaves a thread with no run open exactly as it was', () => {
    // NOTHING REGRESSES WHEN NO RUN IS SELECTED: the page hands the bar a `null` reading and the
    // strip is absent, so the empty scope bar and the tab below it are untouched.
    const html = thread({
      thread: threadOf('DnsSweep', [run()], null),
      stats: createElement(RunStatsBar, { stats: runStats({ ...baseline(), run: null }) }),
    });
    expect(html).toContain('data-testid="run-none"');
    expect(html).not.toContain('data-testid="run-stats"');
  });

  function baseline(): RunStatsInput {
    return {
      run: run({ materialization: ledger() }),
      history: history([ev(1, 'ActivityTaskScheduled')]),
      siblings: [],
      wrote: MINE,
      series: { [KEY]: [4, 12] },
      concerns: [],
      now: NOW,
    };
  }
});
