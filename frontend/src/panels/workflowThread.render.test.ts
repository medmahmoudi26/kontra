/**
 * The workflow thread, drawn — every state of it, as markup.
 *
 * NODE, NO JSDOM, NO TESTING-LIBRARY, as everywhere else here: `renderToStaticMarkup` and string
 * assertions about text and `data-testid`.
 *
 * THE HEAVY PANELS ARE PROPS, AND THAT IS THE WHOLE REASON THIS FILE CAN EXIST. The source viewer
 * is CodeMirror and the worker pane is xterm; both fail a test file before a test runs. `Shell.tsx`
 * made the same split for the same reason, and it buys the same thing here — the states that matter
 * get drawn honestly, including the two this page was briefed against: a status that belongs to the
 * workflow you just left, and a transcript that belongs to the run you just left.
 *
 * WHAT IS ASSERTED IS WHAT AN OPERATOR CAN READ. Not that a component was called with a prop — that
 * the words `never run`, `parked`, `is not a run of this workflow` and the run id under the heading
 * are on the page or are not. Every one of those was a wrong sentence somewhere in this app's
 * history.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import { categorize } from '@core/history';

import type { RunEvent, RunHistory, RunRow } from '../run/api';
import { readRunTurns, type RunAsk, type RunTurns } from '../run/turns';
import { WorkflowThread, type WorkflowThreadProps } from './WorkflowThread';
import { TABS, threadOf, type TabId } from './workflowThread';

const T0 = 1_787_084_868_000;
const NOW = T0 + 300_000;

function run(runId: string, type: string, status: RunRow['status'], startedAt = T0): RunRow {
  return {
    runId,
    type,
    status,
    tenant: 'default',
    startedAt,
    closedAt: status === 'running' ? 0 : startedAt + 60_000,
    dispatches: 3,
  };
}

/* ───────────────────────────── a reduced log to draw ───────────────────────────── */

function ev(id: number, type: string, ms: number, extra: Partial<RunEvent> = {}): RunEvent {
  return { id, type, cat: categorize(type), t: ms / 1000, at: T0 + ms, detail: type, attempt: 1, dur: 0, ...extra };
}

function closes(id: number, type: string, ms: number, openerMs: number, extra: Partial<RunEvent> = {}): RunEvent {
  return ev(id, type, ms, { dur: (ms - openerMs) / 1000, ...extra });
}

function history(events: RunEvent[], extra: Partial<RunHistory> = {}): RunHistory {
  return { events, scanned: events.length, elided: 0, truncated: false, ...extra };
}

const STARTED = ev(1, 'WorkflowExecutionStarted', 0, {
  detail: 'workflowType=DnsSweep · taskQueue=wf-dnssweep · identity=1@65520c20a6a4',
});

/** A run that dispatched three times to one Actor and published, then completed. Three dispatches
 *  in one barrier-free region is what makes the collapsed group real rather than contrived. */
function loop(runId: string): RunTurns {
  const events: RunEvent[] = [STARTED];
  let id = 2;
  let ms = 1_000;
  for (let i = 0; i < 3; i += 1) {
    events.push(ev(id, 'NexusOperationScheduled', ms, { detail: 'endpoint=kontra-nscheck-0-1-0' }));
    // `Started` measures back to `Scheduled` — that is the QUEUE time, and the reader keeps it out
    // of `dur` for it. The close then measures back to `Started`, because `mapHistory` prefers
    // `startedEventId` over `scheduledEventId`; a fixture that measured it from `Scheduled` would
    // be a closer the reader cannot place, and the loop would draw as three lines instead of one.
    events.push(closes(id + 1, 'NexusOperationStarted', ms + 100, ms, { detail: 'endpoint=kontra-nscheck-0-1-0' }));
    events.push(closes(id + 2, 'NexusOperationCompleted', ms + 900, ms + 100, { detail: 'endpoint=kontra-nscheck-0-1-0' }));
    id += 3;
    ms += 1_000;
  }
  events.push(ev(id, 'ActivityTaskScheduled', ms, { detail: 'activityType=publishBatch' }));
  events.push(closes(id + 1, 'ActivityTaskCompleted', ms + 200, ms, { detail: 'activityType=publishBatch' }));
  events.push(ev(id + 2, 'WorkflowExecutionCompleted', ms + 400));
  return readRunTurns(runId, history(events));
}

/** A run that ended badly, and said so. The other half of `hollow`: this one HAS a failure event. */
function broke(runId: string): RunTurns {
  return readRunTurns(
    runId,
    history([
      STARTED,
      ev(2, 'NexusOperationScheduled', 500, { detail: 'endpoint=kontra-nscheck-0-1-0' }),
      closes(3, 'NexusOperationFailed', 900, 500, { detail: 'endpoint=kontra-nscheck-0-1-0' }),
      ev(4, 'WorkflowExecutionFailed', 1_000, { detail: 'ApplicationError: nscheck exploded' }),
    ])
  );
}

/** A finished run whose account came from the archive rather than from Temporal (ADR 0025). An
 *  execution Temporal has dropped still reads on both tabs, and the transcript says which authority
 *  answered. */
function archived(runId: string): RunTurns {
  return readRunTurns(
    runId,
    history(
      [
        STARTED,
        ev(2, 'NexusOperationScheduled', 1_000, { detail: 'endpoint=kontra-nscheck-0-1-0' }),
        closes(3, 'NexusOperationCompleted', 1_900, 1_000, { detail: 'endpoint=kontra-nscheck-0-1-0' }),
        ev(4, 'ActivityTaskScheduled', 2_000, { detail: 'activityType=publishBatch' }),
        closes(5, 'ActivityTaskCompleted', 2_200, 2_000, { detail: 'activityType=publishBatch' }),
        ev(6, 'WorkflowExecutionCompleted', 2_400),
      ],
      { archived: true, archivedAt: T0 + 900_000 }
    )
  );
}

/** A run that completed having published nothing — the failure with no failure event. */
function hollow(runId: string): RunTurns {
  return readRunTurns(
    runId,
    history([
      STARTED,
      ev(2, 'ActivityTaskScheduled', 500, { detail: 'activityType=pageDataset' }),
      closes(3, 'ActivityTaskCompleted', 900, 500, { detail: 'activityType=pageDataset' }),
      ev(4, 'WorkflowExecutionCompleted', 1_200),
    ])
  );
}

/* ───────────────────────────── drawing one ───────────────────────────── */

const EDITOR = createElement('div', { 'data-testid': 'the-editor' }, 'the existing read-only viewer');
const PANE = createElement('div', { 'data-testid': 'the-pane' }, 'the worker pane');
/** The design tab, handed in like the other two — `WorkflowSketch.tsx` has its own render suite. */
const SKETCH = createElement('div', { 'data-testid': 'the-sketch' }, 'the workflow’s own sketch');

function draw(over: Partial<WorkflowThreadProps> = {}): string {
  const props: WorkflowThreadProps = {
    workflow: 'dnssweep',
    thread: threadOf('DnsSweep', [], null),
    tab: 'transcript',
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

/* ───────────────────────────── the run rail ───────────────────────────── */

describe('the runs beside the workflow', () => {
  it('says NEVER RUN for a workflow whose type is known and has no runs', () => {
    const html = draw({ thread: threadOf('Canary', [run('sweep-1', 'DnsSweep', 'completed')], null) });
    expect(html).toContain('data-testid="run-rail-never"');
    expect(html).toContain('Never run');
    expect(html).toContain('data-testid="run-rail-count">0<');
    expect(html).not.toContain('data-testid="run-sweep-1"');
  });

  it('never says NEVER RUN about a type it cannot name', () => {
    // #13. The source has not been read, so the join key is unknown — and "no run of a type I
    // cannot name" is not "this has never run".
    const html = draw({ thread: threadOf(undefined, [run('dhmonitor-1', 'DockerLeakMonitor', 'running')], null) });
    expect(html).toContain('data-testid="run-rail-unresolved"');
    expect(html).not.toContain('Never run');
    expect(html).not.toContain('data-testid="run-rail-never"');
  });

  it('draws one running run, with Temporal’s word for it', () => {
    const html = draw({ thread: threadOf('DnsSweep', [run('sweep-1', 'DnsSweep', 'running')], null) });
    expect(html).toContain('data-testid="run-sweep-1"');
    expect(html).toContain('data-word="running"');
    expect(html).toContain('data-testid="run-word-sweep-1"');
  });

  it('draws one failed run as failed, distinctly from a workflow that never ran', () => {
    const html = draw({ thread: threadOf('DnsSweep', [run('sweep-1', 'DnsSweep', 'failed')], null) });
    expect(html).toContain('data-word="failed"');
    expect(html).not.toContain('Never run');
  });

  it('draws a PARKED run as parked — but only one it has actually read an ask for', () => {
    const rows = [run('sweep-1', 'DnsSweep', 'running'), run('sweep-2', 'DnsSweep', 'running')];
    const html = draw({
      thread: threadOf('DnsSweep', rows, null),
      parked: new Set(['sweep-1']),
    });
    expect(html).toContain('data-testid="run-sweep-1" data-word="parked"');
    // The other run is running as far as anything here knows, and silence is the honest answer.
    expect(html).toContain('data-word="running"');
  });

  it('draws many runs, newest first, and counts them', () => {
    const rows = Array.from({ length: 12 }, (_, i) =>
      run(`sweep-${12 - i}`, 'DnsSweep', i === 0 ? 'running' : 'completed', T0 - i * 60_000)
    );
    const html = draw({ thread: threadOf('DnsSweep', rows, 'sweep-7') });
    expect(html).toContain('data-testid="run-rail-count">12<');
    for (const r of rows) expect(html, r.runId).toContain(`data-testid="run-${r.runId}"`);
    expect(html.indexOf('data-testid="run-sweep-12"')).toBeLessThan(html.indexOf('data-testid="run-sweep-1"'));
    // Exactly one is marked open.
    expect(html.match(/data-selected="true"/g)).toHaveLength(1);
    expect(html).toContain('data-testid="run-sweep-7" data-selected="true"');
  });
});

/* ───────────────────────────── switching workflows ───────────────────────────── */

describe('switching workflows never shows a stale status or a stale run', () => {
  const rows = [run('dhmonitor-1', 'DockerLeakMonitor', 'running'), run('sweep-1', 'DnsSweep', 'completed')];

  it('draws the OTHER workflow’s runs and none of this one’s', () => {
    const html = draw({ workflow: 'dnssweep', thread: threadOf('DnsSweep', rows, null) });
    expect(html).toContain('data-testid="run-sweep-1"');
    expect(html).not.toContain('data-testid="run-dhmonitor-1"');
    // And it does not inherit `running` from the workflow that is running.
    expect(html).not.toContain('data-word="running"');
  });

  it('refuses a run that belongs to another workflow, and says whose it is', () => {
    // The switch bug caught: the run id is still in the address for a render, and drawing it here
    // would put one workflow's conversation under another workflow's heading.
    const html = draw({ workflow: 'dnssweep', thread: threadOf('DnsSweep', rows, 'dhmonitor-1') });
    expect(html).toContain('data-testid="run-stray"');
    expect(html).toContain('is not a run of this workflow');
    expect(html).toContain('DockerLeakMonitor');
    expect(html).not.toContain('data-testid="run-scope"');
  });

  it('does not call an unlisted run stray — it may simply not have dispatched yet', () => {
    const html = draw({ thread: threadOf('DnsSweep', rows, 'sweep-99') });
    expect(html).toContain('data-testid="run-undiscovered"');
    expect(html).not.toContain('data-testid="run-stray"');
    expect(html).toContain('once it has dispatched');
  });

  it('refuses the previous run’s transcript rather than drawing it under this heading', () => {
    const html = draw({
      thread: threadOf('DnsSweep', [run('sweep-1', 'DnsSweep', 'completed'), run('sweep-2', 'DnsSweep', 'completed')], 'sweep-2'),
      loaded: loop('sweep-1'),
    });
    expect(html).toContain('data-testid="transcript-loading"');
    expect(html).toContain('Reading sweep-2');
    expect(html).not.toContain('data-testid="transcript"');
    expect(html).not.toContain('data-run="sweep-1"');
  });
});

/* ───────────────────────────── the tabs ───────────────────────────── */

describe('six tabs over one run', () => {
  const thread = threadOf('DnsSweep', [run('sweep-1', 'DnsSweep', 'completed')], 'sweep-1');

  it('offers all six, whichever one is open', () => {
    const html = draw({ thread });
    for (const t of TABS) {
      expect(html, t.id).toContain(`data-testid="tab-${t.id}"`);
      expect(html, t.id).toContain(`>${t.label}<`);
    }
  });

  it('marks exactly one of the six, never none', () => {
    for (const t of TABS) {
      const html = draw({ thread, tab: t.id });
      expect(html.match(/data-active="true"/g), t.id).toHaveLength(1);
      expect(html, t.id).toContain(`data-testid="tab-${t.id}" data-active="true"`);
      expect(html, t.id).toContain(`data-testid="tab-panel" data-tab="${t.id}"`);
    }
  });

  it('names the open run on every one of them — the scope is the page’s, not the tab’s', () => {
    for (const t of TABS) {
      const html = draw({ thread, tab: t.id, loaded: loop('sweep-1') });
      expect(html, t.id).toContain('data-testid="run-scope" data-run="sweep-1"');
      expect(html, t.id).toContain('sweep-1');
    }
  });

  it('changes what every tab shows when the run changes', () => {
    const two = threadOf(
      'DnsSweep',
      [run('sweep-2', 'DnsSweep', 'failed'), run('sweep-1', 'DnsSweep', 'completed')],
      'sweep-2'
    );
    for (const t of TABS) {
      const first = draw({ thread, tab: t.id, loaded: loop('sweep-1') });
      const second = draw({ thread: two, tab: t.id, loaded: loop('sweep-2') });
      expect(first, t.id).not.toBe(second);
      expect(second, t.id).toContain('sweep-2');
      expect(second, t.id).toContain('data-testid="run-scope" data-run="sweep-2"');
    }
  });

  it('shows the EXISTING editor in Code, handed in rather than rebuilt', () => {
    const html = draw({ thread, tab: 'code' });
    expect(html).toContain('data-testid="the-editor"');
    expect(html).toContain('the existing read-only viewer');
    // And it says what an operator has to know about reading today's source beside an old run.
    expect(html).toContain('as it is on this disk now');
  });

  it('shows the run’s Machines in Monitor, handed in for the same reason', () => {
    const html = draw({ thread, tab: 'monitor' });
    expect(html).toContain('data-testid="the-pane"');
    // AND THE HEADING SAYS WHOSE MACHINES. A tab headed "the worker serving this workflow" over a
    // panel scoped to one run is the sentence that sent an operator to the global wall.
    expect(html).toContain('The Machines sweep-1 ran on');
  });

  it('shows what the run wrote in Data, handed in like the rest', () => {
    const html = draw({
      thread,
      tab: 'data' as TabId,
      data: createElement('div', { 'data-testid': 'the-output' }),
    });
    expect(html).toContain('data-testid="the-output"');
  });

  it('scopes the Data tab and says it is unwired, rather than saying nothing, when nothing is handed in', () => {
    const html = draw({ thread, tab: 'data' as TabId });
    expect(html).toContain('data-testid="data-tab" data-run="sweep-1"');
    expect(html).toContain('Not built yet');
  });

  it('shows the workflow’s own sketch in Scratch, handed in like the other two', () => {
    expect(draw({ thread, tab: 'scratch', scratch: SKETCH })).toContain('data-testid="the-sketch"');
  });

  it('does not scope the sketch to the open run, because a sketch is not about one', () => {
    // The one reading on this page that outlives every run of the workflow. Handing it the run id
    // would be inventing a relationship the document does not have — and would make a sketch drawn
    // before anything ran look like it belonged to whichever conversation happened to be open.
    const html = draw({ thread, tab: 'scratch', scratch: SKETCH });
    expect(html).not.toContain('data-testid="scratch-tab" data-run=');
  });

  it('says the sketch is not wired rather than drawing an empty one, when nothing is handed in', () => {
    const html = draw({ thread, tab: 'scratch' });
    expect(html).toContain('data-testid="scratch-tab" data-run="sweep-1"');
    expect(html).toContain('Not built yet');
  });
});

/* ───────────────────────────── the transcript ───────────────────────────── */

describe('the Transcript tab is the run’s own account of itself', () => {
  const thread = threadOf('DnsSweep', [run('sweep-1', 'DnsSweep', 'completed')], 'sweep-1');

  it('draws the run in kontra’s words, not Temporal’s', () => {
    const html = draw({ thread, loaded: loop('sweep-1') });
    expect(html).toContain('data-testid="transcript" data-run="sweep-1"');
    expect(html).toContain('run started');
    expect(html).toContain('method returned');
    expect(html).toContain('batch published');
    expect(html).toContain('run finished');
  });

  it('folds a loop into one line, with the members still reachable', () => {
    const html = draw({ thread, loaded: loop('sweep-1') });
    expect(html).toContain('data-testid="turn-count"');
    expect(html).toContain('×3');
    // `<details>`, so expanding costs no state and the members render here as strings.
    expect(html).toContain('data-testid="turn-members"');
    expect(html.match(/data-testid="turn-member"/g)).toHaveLength(3);
  });

  it('shows the metadata every name rests on, beside the name', () => {
    const html = draw({ thread, loaded: loop('sweep-1') });
    expect(html).toContain('endpoint=kontra-nscheck-0-1-0');
  });

  it('raises the failure that has no failure event', () => {
    const html = draw({ thread, loaded: hollow('sweep-1') });
    expect(html).toContain('data-testid="transcript-verdict" data-term="run-produced-nothing"');
    expect(html).toContain('run finished, produced nothing');
    expect(html).toContain('data-testid="transcript-concerns"');
    expect(html).toContain('data-concern="produced-nothing"');
  });

  it('says a run is still open rather than letting it read as finished', () => {
    const live = readRunTurns('sweep-1', history([STARTED, ev(2, 'NexusOperationScheduled', 900, { detail: 'endpoint=kontra-nscheck-0-1-0' })]));
    const html = draw({ thread, loaded: live });
    expect(html).toContain('data-testid="transcript-live"');
    expect(html).toContain('not following');
  });

  it('counts what happened, in a chip per term', () => {
    const html = draw({ thread, loaded: loop('sweep-1') });
    expect(html).toContain('data-testid="transcript-chips"');
    expect(html).toContain('data-chip="method-returned"');
  });

  it('places a park in the middle of the account, with the question on it', () => {
    const ask: RunAsk = {
      id: 'approve',
      prompt: 'Bring up 10 machines in sfo3?',
      askedAt: T0 + 1_500,
      state: 'pending',
      waitedMs: 3_000,
    };
    const turns = readRunTurns(
      'sweep-1',
      history([
        STARTED,
        ev(2, 'NexusOperationScheduled', 1_000, { detail: 'endpoint=kontra-nscheck-0-1-0' }),
        closes(3, 'NexusOperationCompleted', 4_000, 1_000, { detail: 'endpoint=kontra-nscheck-0-1-0' }),
        ev(4, 'WorkflowExecutionCompleted', 5_000),
      ]),
      [ask]
    );
    const html = draw({ thread, loaded: turns });
    // THE QUESTION IS ON THE PAGE, not a row saying one was asked. A park renders as the ask
    // itself now (`AskTurn.tsx`), in place, between the turn that produced it and the close.
    expect(html).toContain('data-ask="approve"');
    expect(html).toContain('Bring up 10 machines in sfo3?');
    expect(html.indexOf('data-ask="approve"')).toBeGreaterThan(html.indexOf('method returned'));
    expect(html.indexOf('data-ask="approve"')).toBeLessThan(html.indexOf('data-kind="finished"'));
    // This run CLOSED with the question open, so nothing is waiting on anybody — and there is no
    // form, because a signal to a workflow that is not there is a button that does nothing.
    expect(html).toContain('data-stranded="true"');
    expect(html).not.toContain('data-testid="ask-approve-submit"');
  });

  it('a park on a run that is still open is answerable, and says the panel is waiting', () => {
    const ask: RunAsk = {
      id: 'approve',
      prompt: 'Bring up 10 machines in sfo3?',
      askedAt: T0 + 1_500,
      state: 'pending',
      waitedMs: 3_000,
      context: { region: 'sfo3', machines: 10 },
    };
    const turns = readRunTurns('sweep-1', history([STARTED]), [ask]);
    const html = draw({
      thread,
      loaded: turns,
      deck: { drafts: {}, onDraft: () => {}, by: '', onBy: () => {}, onAnswer: () => {}, sending: null, refused: {} },
    });
    expect(html).toContain('data-testid="transcript-waiting"');
    expect(html).toContain('This run is waiting for you to answer a question.');
    expect(html).toContain('data-testid="ask-approve-submit"');
    // The material the decision rests on, beside the question rather than in another tab.
    expect(html).toContain('data-testid="ask-approve-context"');
    expect(html).toContain('sfo3');
  });

  it('tells an account that aged out apart from one it could not read', () => {
    expect(draw({ thread, failure: { gone: true, detail: '404' } })).toContain('data-testid="transcript-gone"');
    const broken = draw({ thread, failure: { gone: false, detail: '502 Bad Gateway' } });
    expect(broken).toContain('data-testid="transcript-error"');
    expect(broken).toContain('502 Bad Gateway');
  });

  it('has words for a thread with nothing open, and for one with nothing to open', () => {
    expect(draw({ thread: threadOf('DnsSweep', [run('sweep-1', 'DnsSweep', 'completed')], null) })).toContain(
      'Pick a run on the left'
    );
    expect(draw({ thread: threadOf('Canary', [], null) })).toContain('no conversation to read yet');
  });
});

/* ───────────────────────────── the event log, beside it ───────────────────────────── */

describe('the Event log is a sibling reading and not a drawer under the account', () => {
  const closed = threadOf('DnsSweep', [run('sweep-1', 'DnsSweep', 'completed')], 'sweep-1');
  const TAIL = createElement('div', { 'data-testid': 'the-tail' }, 'the compact log');

  it('names the run it is about, and says which of the two readings this is', () => {
    const html = draw({ thread: closed, tab: 'events', loaded: loop('sweep-1'), tail: TAIL });
    expect(html).toContain('data-testid="event-log-tab" data-run="sweep-1"');
    expect(html).toContain('What Temporal recorded for sweep-1');
    // Neither reading substitutes for the other, and the pane says so rather than leaving an
    // operator to work out why there are two.
    expect(html).toContain('this is the rows');
  });

  it('has words for a thread with nothing open', () => {
    const html = draw({
      thread: threadOf('DnsSweep', [run('sweep-1', 'DnsSweep', 'completed')], null),
      tab: 'events',
    });
    expect(html).toContain('data-testid="event-log-no-run"');
    expect(html).not.toContain('data-testid="event-log-tab"');
  });

  it('says nothing is reading the events rather than drawing an empty log', () => {
    // A pane with no reader is not a run with no events, and "no events" would be the wrong
    // sentence about a run whose history has twenty thousand of them.
    const html = draw({ thread: closed, tab: 'events', loaded: loop('sweep-1') });
    expect(html).toContain('data-testid="event-log-absent"');
    expect(html).not.toContain('data-testid="event-log-tail"');
    expect(html).not.toContain('data-testid="event-log-drill"');
  });

  it('reads a run that FAILED on both tabs', () => {
    const failed = threadOf('DnsSweep', [run('sweep-1', 'DnsSweep', 'failed')], 'sweep-1');
    const account = draw({ thread: failed, loaded: broke('sweep-1') });
    expect(account).toContain('data-testid="transcript-verdict"');
    expect(account).toContain('nscheck exploded');
    const log = draw({ thread: failed, tab: 'events', loaded: broke('sweep-1'), tail: TAIL });
    expect(log).toContain('data-testid="event-log-tail"');
    expect(log).toContain('data-testid="the-tail"');
  });

  it('reads a finished run that came out of the archive on both tabs', () => {
    // Temporal has dropped the execution; the account was read from the archive (ADR 0025). Both
    // readings still work, and the transcript says which authority answered.
    const account = draw({ thread: closed, loaded: archived('sweep-1') });
    expect(account).toContain('data-testid="transcript-archived"');
    expect(account).toContain('from the archive');
    expect(account).not.toContain('data-testid="transcript-live"');
    const log = draw({ thread: closed, tab: 'events', loaded: archived('sweep-1'), tail: TAIL });
    expect(log).toContain('data-testid="event-log-tab" data-run="sweep-1"');
    expect(log).toContain('data-testid="the-tail"');
  });

  it('scopes both readings to the run, so switching run switches both', () => {
    const two = threadOf(
      'DnsSweep',
      [run('sweep-2', 'DnsSweep', 'running'), run('sweep-1', 'DnsSweep', 'completed')],
      'sweep-2'
    );
    const html = draw({ thread: two, tab: 'events', loaded: loop('sweep-2'), tail: TAIL });
    expect(html).toContain('data-testid="event-log-tab" data-run="sweep-2"');
    expect(html).not.toContain('data-run="sweep-1"');
  });
});

/* ───────────────────────────── what the page must not lose ───────────────────────────── */

describe('the live half is not traded away for the readable one', () => {
  const open = threadOf('DnsSweep', [run('sweep-1', 'DnsSweep', 'running')], 'sweep-1');
  const TAIL = createElement('div', { 'data-testid': 'the-tail' }, 'the compact live panel');

  it('keeps the compact live panel — one tab over now, rather than under the transcript', () => {
    // `RunTail` is the compact form and the transcript is the full account. The split moved it out
    // of the transcript's pane; it did not take it away, and what it still has that the transcript
    // does not — the per-Batch heartbeats and what landed — is the live half of this page.
    const html = draw({ thread: open, tab: 'events', loaded: loop('sweep-1'), tail: TAIL });
    expect(html).toContain('data-testid="event-log-tab" data-run="sweep-1"');
    expect(html).toContain('data-testid="event-log-tail"');
    expect(html).toContain('data-testid="the-tail"');
  });

  it('no longer folds it under the account, because two readings were sharing one pane', () => {
    const html = draw({ thread: open, loaded: loop('sweep-1'), tail: TAIL });
    expect(html).toContain('data-testid="transcript"');
    expect(html).not.toContain('data-testid="the-tail"');
  });

  it('still shows each while the other has nothing — neither reading waits on the other', () => {
    expect(draw({ thread: open, loaded: null, tail: TAIL })).toContain('data-testid="transcript-loading"');
    const log = draw({ thread: open, tab: 'events', loaded: null, tail: TAIL });
    expect(log).toContain('data-testid="the-tail"');
    expect(log).not.toContain('data-testid="transcript-loading"');
  });

  it('offers cancel and stop for a run that is OPEN, beside its own name', () => {
    const html = draw({ thread: open, onStop: () => undefined });
    expect(html).toContain('data-testid="cancel-sweep-1"');
    expect(html).toContain('data-testid="terminate-sweep-1"');
  });

  it('offers neither on a run that has closed', () => {
    // A stop button on a run that closed last week does nothing and says nothing, which is how an
    // operator learns not to trust the controls.
    const closed = threadOf('DnsSweep', [run('sweep-1', 'DnsSweep', 'completed')], 'sweep-1');
    const html = draw({ thread: closed, onStop: () => undefined });
    expect(html).not.toContain('data-testid="cancel-sweep-1"');
    expect(html).not.toContain('data-testid="terminate-sweep-1"');
  });

  it('refuses a second click while a stop is in flight', () => {
    const html = draw({ thread: open, onStop: () => undefined, stopping: 'sweep-1' });
    expect(html).toContain('data-testid="cancel-sweep-1" disabled=""');
    expect(html).toContain('data-testid="terminate-sweep-1" disabled=""');
  });
});

/* ───────────────────────────── what it must never grow ───────────────────────────── */

describe('a run is one execution of a caller’s workflow (ADR 0023 §12)', () => {
  it('offers no way to start anything from a thread', () => {
    const html = draw({
      thread: threadOf('DnsSweep', [run('sweep-1', 'DnsSweep', 'completed')], 'sweep-1'),
      loaded: loop('sweep-1'),
    });
    expect(html).not.toContain('data-testid="run-button"');
    expect(html).not.toContain('data-testid="serve-button"');
    // No button on this page is labelled with a starting verb. Checked per BUTTON rather than over
    // the whole document, so the scope bar's `Run` heading and a run id containing the word are not
    // mistaken for an affordance — and so that adding one is what fails.
    for (const button of html.match(/<button[\s\S]*?<\/button>/g) ?? []) {
      expect(button).not.toMatch(/>(Run|Start|Launch|Serve|Replay)</);
    }
  });

  it('reaches a run only through the workflow that produced it', () => {
    // Every run row on this page came out of `thread.runs`, which is `rowStatus`'s filter by the
    // decorated type. There is no listing here that could show somebody else's run.
    const rows = [run('sweep-1', 'DnsSweep', 'completed'), run('other-1', 'NsCheck', 'completed')];
    const html = draw({ thread: threadOf('DnsSweep', rows, null) });
    expect(html).toContain('data-testid="run-sweep-1"');
    expect(html).not.toContain('data-testid="run-other-1"');
    expect(html).not.toContain('other-1');
  });
});
