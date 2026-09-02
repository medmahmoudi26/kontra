/**
 * The thread's arithmetic: whose runs these are, which one is open, and what may be SAID about a
 * workflow whose type is not known yet.
 *
 * #13 ONE FLOOR DOWN. `workflowRow.test.ts` pins the status join — a running workflow must not draw
 * as NEVER RUN because the filename guess and the decorated type differ. This suite pins the same
 * failure where a SELECTION lives: switch threads and the status, the run list AND the open run all
 * have to move together, because a page that carried any one of them over is drawing one workflow's
 * conversation under another workflow's heading.
 *
 * Node, no jsdom, no component: every assertion below is against a value.
 */

import { describe, expect, it } from 'vitest';

import { categorize } from '@core/history';

import type { EventLink, RunEvent, RunHistory, RunRow } from '../run/api';
import { readRunTurns, type RunTurns } from '../run/turns';
import { turnKey } from './transcriptDrill';
import {
  DEFAULT_TAB,
  EVENT_LOG_TAB,
  TABS,
  drillHandler,
  isTab,
  openDrill,
  runWord,
  threadOf,
  transcriptFor,
  type TabId,
} from './workflowThread';
import { guessTypeFromFilename, typeFromSource } from './workflowSource';

const T0 = 1_787_084_868_000;

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

/** A reading, with only the field the anti-stale rule reads. The transcript itself is another
 *  module's business and is asserted in `turns.test.ts`. */
function turnsFor(runId: string): RunTurns {
  return { runId, named: {} as RunTurns['named'], live: false, asks: [] };
}

/* ───────────────────────────── the tabs ───────────────────────────── */

describe('the six tabs', () => {
  it('are six, in the order the questions get asked', () => {
    expect(TABS.map((t) => t.id)).toEqual([
      'transcript',
      'events',
      'code',
      'scratch',
      'monitor',
      'data',
    ]);
  });

  it('read Transcript and Event log — and nothing on the strip still says Chat', () => {
    // The word was doing double duty. What is on the first tab is an operator reading a machine's
    // account of itself, and a consumer-chat name for it imports an aesthetic the PRD ruled out.
    expect(TABS.map((t) => t.label)).toEqual([
      'Transcript',
      'Event log',
      'Code',
      'Scratch',
      'Monitor',
      'Data',
    ]);
    for (const tab of TABS) {
      expect(`${tab.id} ${tab.label} ${tab.hint}`.toLowerCase(), tab.id).not.toContain('chat');
    }
  });

  it('each carry a label and a sentence saying what they HOLD', () => {
    for (const tab of TABS) {
      expect(tab.label, tab.id).toBeTruthy();
      expect(tab.hint.length, tab.id).toBeGreaterThan(20);
    }
  });

  it('give the two readings a tab each, because neither substitutes for the other', () => {
    const ids = TABS.map((t) => t.id);
    expect(ids).toContain('transcript');
    expect(ids).toContain(EVENT_LOG_TAB);
    // Beside each other, and in that order: the check has to be next to the claim it settles.
    expect(ids.indexOf(EVENT_LOG_TAB)).toBe(ids.indexOf('transcript') + 1);
  });

  it('open on the Transcript — the reading an operator has never had, and where a deep link lands', () => {
    expect(DEFAULT_TAB).toBe('transcript');
    expect(TABS[0]?.id).toBe(DEFAULT_TAB);
  });

  it('refuse anything that is not one of them, so a remembered value cannot blank the page', () => {
    expect(isTab('transcript')).toBe(true);
    expect(isTab('events')).toBe(true);
    // LAST RELEASE'S WORD, REFUSED RATHER THAN ALIASED. A remembered `'chat'` is not an error — it
    // falls back to DEFAULT_TAB, which is the very tab it named, so nothing is lost by refusing it
    // and the word does not survive in the one place nobody would think to look.
    expect(isTab('chat')).toBe(false);
    expect(isTab(undefined)).toBe(false);
    expect(isTab(3)).toBe(false);
  });
});

/* ───────────────────────────── where a drill lands ───────────────────────────── */

describe('drilling a turn reaches the Event log, which is the whole cost of the split', () => {
  function ev(id: number, type: string, ms: number, detail = type, link?: EventLink): RunEvent {
    return {
      id,
      type,
      cat: categorize(type),
      t: ms / 1000,
      at: T0 + ms,
      detail,
      attempt: 1,
      dur: 0,
      ...(link ? { link } : {}),
    };
  }
  function history(events: RunEvent[]): RunHistory {
    return { events, scanned: events.length, elided: 0, truncated: false };
  }
  /** The fleet child every one of its three events names. The EXECUTION is pinned as well as the id,
   *  because `kontra-fleet/dns` is reused by the teardown and a drill by id alone lands on it. */
  const FLEET: EventLink = {
    workflowId: 'kontra-fleet/dns',
    execId: '01a00772-8296',
    type: 'stackWorkflow',
    via: 'child',
  };
  /** A run that opened a child workflow and closed — so the turns include a folded child and an
   *  event this release has no word for. */
  const account = readRunTurns(
    'sweep-1',
    history([
      ev(1, 'WorkflowExecutionStarted', 0, 'workflowType=DnsSweep · taskQueue=wf-dnssweep'),
      ev(2, 'StartChildWorkflowExecutionInitiated', 1_000, 'workflowType=stackWorkflow', FLEET),
      ev(3, 'ChildWorkflowExecutionStarted', 1_200, 'workflowType=stackWorkflow', FLEET),
      ev(4, 'ChildWorkflowExecutionCompleted', 157_200, 'workflowType=stackWorkflow', FLEET),
      ev(5, 'WorkflowPropertiesModified', 157_500),
      ev(6, 'WorkflowExecutionCompleted', 158_000),
    ])
  );

  it('answers with the tab as well as the turn, so a caller cannot set one and forget the other', () => {
    const named = account.named.turns[0]!;
    expect(openDrill(named)).toEqual({ openTurn: turnKey(named.turn), tab: EVENT_LOG_TAB });
  });

  // THE UNTRANSLATED ROW IS NOT ON THE TRANSCRIPT ANY MORE, AND THAT IS NOT IT BEING LEFT OUT. It
  // is already IN the Event log, verbatim — the pane a drill would have carried the reader to — so
  // there is nothing left for a drill from the account to do about it. What still has to hold is
  // that it exists, named and counted, in the other half of the reading.
  it('keeps Temporal\'s own rows out of the account and in the log', () => {
    expect(account.named.turns.some((t) => t.untranslated)).toBe(false);
    expect(account.named.untranslated.map((t) => t.label)).toEqual(['WorkflowPropertiesModified']);
  });

  it('lands EVERY turn there — the folded child and the untranslated row included', () => {
    // OVER BOTH HALVES, because the drill is a total function over turns and must stay one: the day
    // a row of the Event log offers the same descent, it costs nothing that this already works.
    for (const named of [...account.named.turns, ...account.named.untranslated]) {
      const open = openDrill(named);
      expect(open.tab, named.label).toBe('events');
      expect(open.openTurn, named.label).toBe(turnKey(named.turn));
    }
  });

  it('opens on the identity the transcript keys its rows by, so a growing loop cannot swap subjects', () => {
    // `turnKey` is anchored on the group's FIRST event, which is what survives a live loop gaining
    // members between the click and the next arrival.
    const child = account.named.turns.find((t) => t.turn.kind === 'fleet')!;
    expect(openDrill(child).openTurn).toBe(turnKey(child.turn));
  });

  it('does BOTH halves of the click, so no caller can mark the row and forget the tab', () => {
    // This is the assertion the split exists to keep true. The page that wires this imports
    // CodeMirror and cannot load under the runner, which is exactly why the pairing lives here.
    const marked: string[] = [];
    const tabs: TabId[] = [];
    const handler = drillHandler((named) => marked.push(turnKey(named.turn)), (id) => tabs.push(id));
    const named = account.named.turns.find((t) => t.turn.kind === 'fleet')!;
    handler!(named);
    expect(marked).toEqual([turnKey(named.turn)]);
    expect(tabs).toEqual(['events']);
  });

  it('offers nothing at all when there is nowhere to put the raw events', () => {
    // A row that changed tab to show an empty pane is worse than a row that does not click.
    let switched = 0;
    expect(drillHandler(undefined, () => (switched += 1))).toBeUndefined();
    expect(switched).toBe(0);
  });
});

/* ───────────────────────────── the thread, with no selection ───────────────────────────── */

describe('a thread with no run open', () => {
  it('says UNKNOWN, never never-run, before the type is resolved', () => {
    // The join key is the `@workflow.defn` type and the source has not been read. "No run of a type
    // I cannot name" is not "this has never run" — that is #13 in a different disguise.
    const thread = threadOf(undefined, [run('sweep-1', 'DnsSweep', 'running')], null);
    expect(thread.unresolved).toBe(true);
    expect(thread.neverRun).toBe(false);
    expect(thread.runs).toEqual([]);
    expect(thread.status.label).toBe('unknown');
    expect(thread.status.state).toBe('unknown');
  });

  it('says NEVER RUN only when the type is known and nothing carries it', () => {
    const thread = threadOf('Canary', [run('sweep-1', 'DnsSweep', 'running')], null);
    expect(thread.neverRun).toBe(true);
    expect(thread.unresolved).toBe(false);
    expect(thread.runs).toEqual([]);
    expect(thread.status.label).toBe('never run');
    expect(thread.status.state).toBe('idle');
  });

  it('reads NEVER RUN differently from a thread whose runs all failed', () => {
    // The distinction the acceptance criteria ask for, as two values rather than two paragraphs.
    const never = threadOf('Canary', [], null);
    const broken = threadOf('DnsSweep', [run('sweep-1', 'DnsSweep', 'failed')], null);
    expect(never.neverRun).toBe(true);
    expect(never.status.label).toBe('never run');
    expect(broken.neverRun).toBe(false);
    expect(broken.status.label).toBe('failed');
    expect(broken.runs).toHaveLength(1);
  });

  it('leaves the conversation closed — `/workflows/<w>` is a real place', () => {
    const thread = threadOf('DnsSweep', [run('sweep-1', 'DnsSweep', 'completed')], null);
    expect(thread.selected).toBeNull();
    // And nothing is MISSING: nobody asked for a run, so there is nothing to explain.
    expect(thread.missing).toBeNull();
  });
});

/* ───────────────────────────── one run, in each state ───────────────────────────── */

describe('a thread with one run', () => {
  it('running: the workflow is running, and the run is the one on screen', () => {
    const rows = [run('sweep-1', 'DnsSweep', 'running')];
    const thread = threadOf('DnsSweep', rows, 'sweep-1');
    expect(thread.status.state).toBe('running');
    expect(thread.status.label).toBe('running');
    expect(thread.selected).toBe(rows[0]);
    expect(thread.missing).toBeNull();
  });

  it('failed: the workflow is idle and the run says what it did', () => {
    const rows = [run('sweep-1', 'DnsSweep', 'failed')];
    const thread = threadOf('DnsSweep', rows, 'sweep-1');
    // IDLE IS ABOUT THE WORKFLOW, `failed` IS ABOUT THE RUN. A failed run does not leave the
    // workflow in a failed state — there is simply nothing open.
    expect(thread.status.state).toBe('idle');
    expect(thread.status.label).toBe('failed');
    expect(thread.selected?.status).toBe('failed');
  });

  it('parked: Temporal still says RUNNING, and only the ask makes it parked', () => {
    const parked = run('sweep-1', 'DnsSweep', 'running');
    const thread = threadOf('DnsSweep', [parked], 'sweep-1');
    // The list has no park signal and must not grow one by inference.
    expect(thread.status.state).toBe('running');
    expect(runWord(parked)).toBe('running');
    expect(runWord(parked, new Set(['sweep-1']))).toBe('parked');
  });

  it('many: every one of them, in the order given, and the newest is `last`', () => {
    const rows = [
      run('sweep-9', 'DnsSweep', 'completed', T0 + 900),
      run('sweep-8', 'DnsSweep', 'failed', T0 + 800),
      run('sweep-7', 'DnsSweep', 'cancelled', T0 + 700),
      run('sweep-6', 'DnsSweep', 'completed', T0 + 600),
      run('other-1', 'NsCheck', 'running', T0 + 500),
      run('sweep-5', 'DnsSweep', 'completed', T0 + 400),
    ];
    const thread = threadOf('DnsSweep', rows, 'sweep-7');
    expect(thread.runs.map((r) => r.runId)).toEqual([
      'sweep-9',
      'sweep-8',
      'sweep-7',
      'sweep-6',
      'sweep-5',
    ]);
    // The other workflow's run is not in this thread at any position.
    expect(thread.runs.some((r) => r.runId === 'other-1')).toBe(false);
    expect(thread.status.last?.runId).toBe('sweep-9');
    expect(thread.selected?.runId).toBe('sweep-7');
  });
});

/* ───────────────────────────── the switch ───────────────────────────── */

describe('switching workflows never shows a stale status', () => {
  const source = '@workflow.defn\nclass DockerLeakMonitor:\n    pass\n';
  const rows = [
    run('dhmonitor-1', 'DockerLeakMonitor', 'running'),
    run('sweep-1', 'DnsSweep', 'completed'),
  ];

  it('keys the status to the type of the workflow being VIEWED, not to a filename', () => {
    // The live bug, restated at thread level: `dhmonitor` is a RUNNING `DockerLeakMonitor`, and its
    // filename guesses to `Dhmonitor`, which is nobody's type.
    const real = typeFromSource(source, 'dhmonitor.py');
    expect(real).toBe('DockerLeakMonitor');
    expect(guessTypeFromFilename('dhmonitor.py')).toBe('Dhmonitor');

    expect(threadOf(real, rows, null).status.label).toBe('running');
    // And the guess is what produced the lie.
    expect(threadOf(guessTypeFromFilename('dhmonitor.py'), rows, null).status.label).toBe('never run');
  });

  it('carries NOTHING over: a second thread answers entirely from its own type', () => {
    const first = threadOf('DockerLeakMonitor', rows, 'dhmonitor-1');
    expect(first.status.state).toBe('running');
    expect(first.selected?.runId).toBe('dhmonitor-1');

    // The operator clicks another workflow. The run id is still in the address for an instant —
    // this is exactly the render where a page that remembered would draw the wrong thing.
    const second = threadOf('DnsSweep', rows, 'dhmonitor-1');
    expect(second.status.state).toBe('idle');
    expect(second.status.label).toBe('completed');
    expect(second.runs.map((r) => r.runId)).toEqual(['sweep-1']);
    expect(second.selected).toBeNull();
  });

  it('says out loud that the addressed run belongs to another workflow', () => {
    const thread = threadOf('DnsSweep', rows, 'dhmonitor-1');
    expect(thread.missing).toEqual({
      why: 'stray',
      runId: 'dhmonitor-1',
      type: 'DockerLeakMonitor',
    });
  });

  it('does not call an unlisted run stray — it has simply not dispatched yet', () => {
    // `/api/runs` lists runs that have DISPATCHED. A run started ten seconds ago is not there, and
    // reporting it as another workflow's would be a confident wrong answer about a fresh run.
    const thread = threadOf('DnsSweep', rows, 'sweep-99');
    expect(thread.missing).toEqual({ why: 'undiscovered', runId: 'sweep-99' });
  });

  it('still refuses to name a run while the type is unresolved', () => {
    // No type means no thread, so a run cannot be IN it — and the reason is the type, not the run.
    const thread = threadOf(undefined, rows, 'dhmonitor-1');
    expect(thread.selected).toBeNull();
    expect(thread.unresolved).toBe(true);
    expect(thread.missing?.why).toBe('stray');
  });
});

/* ───────────────────────────── what a row may say ───────────────────────────── */

describe('runWord is silent rather than inventive', () => {
  it('shows Temporal’s word for every run nothing has been read about', () => {
    for (const status of ['running', 'completed', 'failed', 'cancelled', 'pending'] as const) {
      expect(runWord(run('r', 'W', status))).toBe(status);
    }
  });

  it('never parks a run that has closed, whatever its asks say', () => {
    // An ask left pending on a run that failed is a question nobody will answer. Painting that run
    // as "waiting on a human" would put a live-looking row on a dead run.
    const parked = new Set(['r']);
    expect(runWord(run('r', 'W', 'failed'), parked)).toBe('failed');
    expect(runWord(run('r', 'W', 'completed'), parked)).toBe('completed');
    expect(runWord(run('r', 'W', 'cancelled'), parked)).toBe('cancelled');
    expect(runWord(run('r', 'W', 'running'), parked)).toBe('parked');
  });
});

/* ───────────────────────────── the anti-stale rule ───────────────────────────── */

describe('a transcript is drawn only for the run it is about', () => {
  const rows = [run('sweep-1', 'DnsSweep', 'completed'), run('sweep-2', 'DnsSweep', 'failed')];

  it('draws the reading when the ids agree', () => {
    const thread = threadOf('DnsSweep', rows, 'sweep-1');
    const loaded = turnsFor('sweep-1');
    expect(transcriptFor(loaded, thread.selected)).toBe(loaded);
  });

  it('refuses the PREVIOUS run’s reading rather than drawing it under this heading', () => {
    // The whole failure: the operator clicked sweep-2, the fetch for it is in flight, and the last
    // answer to arrive was sweep-1's. On an archived run that window is long enough to read.
    const thread = threadOf('DnsSweep', rows, 'sweep-2');
    expect(transcriptFor(turnsFor('sweep-1'), thread.selected)).toBeNull();
  });

  it('refuses everything when no run is open', () => {
    const thread = threadOf('DnsSweep', rows, null);
    expect(transcriptFor(turnsFor('sweep-1'), thread.selected)).toBeNull();
  });

  it('refuses a reading held across a workflow switch', () => {
    // Two failures compounding, which is how this reaches a screen: the thread no longer owns the
    // run, AND the reading is the old one. Either check alone would be enough; neither is skipped.
    const thread = threadOf('NsCheck', rows, 'sweep-1');
    expect(thread.selected).toBeNull();
    expect(transcriptFor(turnsFor('sweep-1'), thread.selected)).toBeNull();
  });
});

/* ───────────────────────────── no way to start anything ───────────────────────────── */

describe('a run is one execution of a caller’s workflow (ADR 0023 §12)', () => {
  it('exposes nothing that mints a run', async () => {
    // A thread READS runs. Nothing in this module starts, registers or interprets one — a page that
    // grew a way to launch an arbitrary graph would be re-introducing the interpreter through a tab.
    const module: Record<string, unknown> = await import('./workflowThread');
    const names = Object.keys(module).join(' ').toLowerCase();
    expect(names).not.toMatch(/start|launch|dispatch|interpret|graph/);
  });

  it('offers no tab that is a place to build one', () => {
    const ids: TabId[] = TABS.map((t) => t.id);
    expect(ids).not.toContain('run');
    expect(ids).not.toContain('start');
  });
});
