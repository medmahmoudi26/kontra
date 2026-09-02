/**
 * The raw view, drawn — and the transcript's own drill affordance.
 *
 * NODE, NO JSDOM, NO TESTING-LIBRARY, like every render suite here: `renderToStaticMarkup` and
 * string assertions about text and `data-testid`. The panel takes its level as a PROP, so every
 * state of it — the root, a child, a child Temporal dropped, an elided log, a turn with no event —
 * is a string in this file rather than a fetch in a browser.
 *
 * WHAT IS ASSERTED IS WHAT AN OPERATOR CAN READ. Not that a component received a prop: that the
 * Temporal type is on the page, that the event ids are on the page, and that the untranslated turn —
 * the one a reader most needs to check — has the same affordance as every other row.
 */

import { createElement, type ReactElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import { categorize } from '@core/history';
import { nameTranscript, readVocabulary, type NamedTurn } from '@core/vocabulary';
import { readTranscript, type Turn } from '@core/transcript';

import type { RunEvent, RunHistory, RunRow } from '../run/api';
import { readRunTurns } from '../run/turns';
import { TranscriptDrill, type DrillLevelView, type TranscriptDrillProps } from './TranscriptDrill';
import { TranscriptView } from './Transcript';
import { WorkflowThread, type WorkflowThreadProps } from './WorkflowThread';
import { EVENT_LOG_TAB, openDrill, threadOf } from './workflowThread';
import { rootLevel, type DrillLevel } from './eventDrill';
import { rowsFor, subjectOf, turnKey } from './transcriptDrill';

const BASE = 1_786_831_339_151;
const RUN = 'nscheck-1786831339';

function ev(id: number, type: string, ms: number, extra: Partial<RunEvent> = {}): RunEvent {
  return { id, type, cat: categorize(type), t: ms / 1000, at: BASE + ms, detail: type, attempt: 1, dur: 0, ...extra };
}

function closes(id: number, type: string, ms: number, openerMs: number, extra: Partial<RunEvent> = {}): RunEvent {
  return ev(id, type, ms, { dur: (ms - openerMs) / 1000, ...extra });
}

function history(events: RunEvent[], extra: Partial<RunHistory> = {}): RunHistory {
  return { events, scanned: events.length, elided: 0, truncated: false, ...extra };
}

const STARTED = ev(1, 'WorkflowExecutionStarted', 0, {
  detail: 'workflowType=NsCheck · taskQueue=wf-nscheck · identity=1@65520c20a6a4',
});

const FLEET_LINK = {
  workflowId: 'kontra-fleet/dns',
  execId: '01a00772-8296-7d33-8f8f-4b2e6f2f0001',
  type: 'stackWorkflow',
  via: 'child' as const,
};

/** The fleet child: MEASURED on `nscheck-1786831339`, four opaque rows covering 156 of 294 seconds. */
const FLEET: RunEvent[] = [
  ev(2, 'StartChildWorkflowExecutionInitiated', 1_000, { detail: 'workflowType=stackWorkflow', link: FLEET_LINK }),
  ev(3, 'ChildWorkflowExecutionStarted', 1_200, { detail: 'workflowType=stackWorkflow', link: FLEET_LINK }),
  closes(4, 'ChildWorkflowExecutionCompleted', 157_200, 1_200, {
    detail: 'workflowType=stackWorkflow',
    link: FLEET_LINK,
  }),
];

/** What the fleet child's OWN history holds — the detail the parent's four rows cannot carry,
 *  because it is inside claim-checked payloads the log refuses to decode (ADR 0007). */
const CHILD_EVENTS: RunEvent[] = [
  ev(1, 'WorkflowExecutionStarted', 0, { detail: 'workflowType=stackWorkflow · taskQueue=kontra-infra' }),
  ev(2, 'ActivityTaskScheduled', 500, { detail: 'activityType=pulumiUp' }),
  closes(3, 'ActivityTaskCompleted', 155_000, 500, { detail: 'activityType=pulumiUp' }),
  ev(4, 'WorkflowExecutionCompleted', 156_000),
];

/** One named turn of a given kind, FROM EITHER PANE. The account and the Event log are two halves of
 *  one reading — a `raw` turn is only ever in the second — and a drill is a total function over both,
 *  so a helper that looked in one would be asserting the split rather than the drill. */
function turnOf(events: RunEvent[], kind: Turn['kind']): NamedTurn {
  const named = nameTranscript(readTranscript(history(events)));
  const all = [...named.turns, ...named.untranslated];
  const found = all.find((t) => t.turn.kind === kind);
  if (!found) throw new Error(`no ${kind} turn — got ${all.map((t) => t.turn.kind).join(', ')}`);
  return found;
}

/** The root level's view, built the way the wired hook builds it: out of the log in hand. */
function rootView(events: RunEvent[], named: NamedTurn, extra: Partial<RunHistory> = {}): DrillLevelView {
  const { rows, missing } = rowsFor(history(events, extra), subjectOf(named.turn).events);
  return { rows, missing, loading: false, failure: null, span: 0, closed: true };
}

function draw(over: Partial<TranscriptDrillProps> & { named: NamedTurn }): string {
  const props: TranscriptDrillProps = {
    runId: RUN,
    stack: [rootLevel(RUN)] as DrillLevel[],
    view: { rows: [], missing: [], loading: false, failure: null, span: 0, closed: true },
    onDrill: () => undefined,
    onPop: () => undefined,
    onClose: () => undefined,
    ...over,
  };
  return renderToStaticMarkup(createElement(TranscriptDrill, props));
}

/* ───────────────────────────── the root level ───────────────────────────── */

describe('a domain turn opens onto the raw events it folded', () => {
  const events = [STARTED, ...FLEET];
  const named = turnOf(events, 'fleet');

  it('draws every event id the turn names, with Temporal’s own type on each row', () => {
    const html = draw({ named, view: rootView(events, named) });
    expect(html).toContain('data-testid="turn-drill"');
    // The three rows the friendly line was built from.
    expect(html).toContain('data-testid="drill-row-2"');
    expect(html).toContain('data-testid="drill-row-3"');
    expect(html).toContain('data-testid="drill-row-4"');
    expect(html).toContain('StartChildWorkflowExecutionInitiated');
    expect(html).toContain('ChildWorkflowExecutionCompleted');
    // The name AND what it rests on, together — the drill is where a disputed name gets settled.
    expect(html).toContain(named.label);
    expect(html).toContain('data-testid="drill-events"');
  });

  it('offers the child workflow as a destination, from the row that opened it', () => {
    const html = draw({ named, view: rootView(events, named) });
    expect(html).toContain('data-testid="drill-targets"');
    expect(html).toContain('data-testid="drill-into-kontra-fleet/dns"');
    // Reached from the initiation, which is the event `drillInto` records the level came from.
    expect(html).toContain('data-testid="drill-row-into-2"');
    expect(html).toContain('stackWorkflow');
  });

  it('draws no crumbs at the root — there is nowhere above it to go', () => {
    expect(draw({ named, view: rootView(events, named) })).not.toContain('data-testid="drill-crumbs"');
  });
});

/* ───────────────────────────── across a child ───────────────────────────── */

describe('a child is just another workflow id the same reducer reads', () => {
  const stack: DrillLevel[] = [
    rootLevel(RUN),
    { workflowId: FLEET_LINK.workflowId, execId: FLEET_LINK.execId, from: 2, type: 'stackWorkflow' },
  ];
  const view: DrillLevelView = {
    rows: CHILD_EVENTS,
    missing: [],
    loading: false,
    failure: null,
    span: 156_000,
    closed: true,
  };

  it('draws the child’s own events, which is where the parent’s opaque rows keep their detail', () => {
    const html = draw({ named: turnOf([STARTED, ...FLEET], 'fleet'), stack, view });
    expect(html).toContain('data-depth="1"');
    expect(html).toContain('activityType=pulumiUp');
    expect(html).toContain('data-testid="drill-row-3"');
    // MEASURED: the 156 seconds the run looked idle, on the clock the level measures for itself.
    expect(html).toContain('156.00s of wall time');
  });

  it('keeps both places to come back to, not one', () => {
    const html = draw({ named: turnOf([STARTED, ...FLEET], 'fleet'), stack, view });
    expect(html).toContain('data-testid="drill-crumbs"');
    expect(html).toContain('data-testid="drill-crumb-0"');
    expect(html).toContain('data-testid="drill-crumb-1"');
    expect(html).toContain('data-current="true"');
    // The crumb is shortened to fit; the execution that tells a bring-up from a teardown is in the
    // title, whole.
    expect(html).toContain(FLEET_LINK.execId);
  });

  it('tells a child that aged out apart from one it could not ask', () => {
    const named = turnOf([STARTED, ...FLEET], 'fleet');
    const gone = draw({
      named,
      stack,
      view: { ...view, rows: [], failure: { gone: true, detail: '404' } },
    });
    expect(gone).toContain('data-testid="drill-gone"');
    expect(gone).toContain('aged out at retention, or it never started');

    const broken = draw({
      named,
      stack,
      view: { ...view, rows: [], failure: { gone: false, detail: '502 Bad Gateway' } },
    });
    expect(broken).toContain('data-testid="drill-error"');
    expect(broken).toContain('502 Bad Gateway');
  });

  it('says a level is still being re-read while its own execution is open', () => {
    const html = draw({
      named: turnOf([STARTED, ...FLEET], 'fleet'),
      stack,
      view: { ...view, rows: CHILD_EVENTS.slice(0, 3), closed: false },
    });
    expect(html).toContain('data-testid="drill-open"');
  });
});

/* ───────────────────────────── the awkward turns ───────────────────────────── */

describe('the turns a drill would be easiest to leave out of', () => {
  it('an unrecognised event is reachable and shows its raw Temporal type', () => {
    const events = [STARTED, ev(2, 'WorkflowPropertiesModified', 500)];
    const named = turnOf(events, 'raw');
    expect(named.untranslated).toBe(true);
    const html = draw({ named, view: rootView(events, named) });
    expect(html).toContain('data-testid="drill-row-2"');
    // Twice over: as the turn's own label, and as the row's type. Nothing here guesses.
    expect(html).toContain('data-testid="drill-types"');
    expect(html).toContain('WorkflowPropertiesModified');
  });

  it('a park says it has no Temporal event rather than looking like a log that lost its rows', () => {
    const named = readVocabulary(history([STARTED]), {
      asks: [{ id: 'approve', prompt: 'Bring up 10 machines in sfo3?', askedAt: BASE + 1_500 }],
    }).turns.find((t) => t.turn.kind === 'parked')!;
    const html = draw({ named, view: { rows: [], missing: [], loading: false, failure: null, span: 0, closed: true } });
    expect(html).toContain('data-testid="drill-eventless"');
    expect(html).toContain('came from the run');
    expect(html).not.toContain('data-testid="drill-empty"');
  });

  it('names the rows an elided log no longer carries', () => {
    const full = [STARTED, ...FLEET];
    const named = turnOf(full, 'fleet');
    const short = [STARTED, FLEET[0]!, FLEET[2]!];
    const { rows, missing } = rowsFor(history(short, { elided: 1, truncated: true }), subjectOf(named.turn).events);
    const html = draw({ named, view: { rows, missing, loading: false, failure: null, span: 0, closed: true } });
    expect(html).toContain('data-testid="drill-missing"');
    expect(html).toContain('the history was elided to stay under its cap');
  });

  it('a level being read says which one', () => {
    const html = draw({
      named: turnOf([STARTED, ...FLEET], 'fleet'),
      stack: [rootLevel(RUN), { workflowId: FLEET_LINK.workflowId, execId: FLEET_LINK.execId }],
      view: { rows: [], missing: [], loading: true, failure: null, span: 0, closed: false },
    });
    expect(html).toContain('data-testid="drill-loading"');
    expect(html).toContain('kontra-fleet/dns');
  });
});

/* ───────────────────────────── the affordance on the transcript ───────────────────────────── */

describe('the transcript offers the check on every row', () => {
  const events = [
    STARTED,
    ...FLEET,
    ev(5, 'WorkflowPropertiesModified', 158_000),
    ev(6, 'WorkflowExecutionCompleted', 159_000),
  ];
  const turns = readRunTurns(RUN, history(events));

  function view(over: Parameters<typeof TranscriptView>[0]): string {
    return renderToStaticMarkup(createElement(TranscriptView, over));
  }

  it('every turn of the account drills', () => {
    const html = view({ turns, onDrill: () => undefined, openTurn: null });
    expect(turns.named.turns.length).toBeGreaterThan(0);
    for (const t of turns.named.turns) {
      expect(html).toContain(`data-testid="turn-drill-${turnKey(t.turn)}"`);
    }
  });

  // AND THE UNTRANSLATED ONE IS NOT A ROW HERE AT ALL. It has no word, so it has no place in the
  // account — but the pane still says how many of them there are and where they went, which is what
  // stops "the transcript is the log" from being an easy thing to believe.
  it('draws no row for an event it has no word for, and still counts it', () => {
    const html = view({ turns, onDrill: () => undefined, openTurn: null });
    const raw = turns.named.untranslated.find((t) => t.label === 'WorkflowPropertiesModified');
    expect(raw).toBeDefined();
    expect(html).not.toContain(`data-testid="turn-drill-${turnKey(raw!.turn)}"`);
    expect(html).not.toContain('data-kind="raw"');
    expect(html).toContain('data-chip="untranslated"');
    expect(html).toContain('data-pane="events"');
    expect(html).toContain('in the Event log');
  });

  it('marks the row whose raw events are open, and only that one', () => {
    const open = turnKey(turns.named.turns.find((t) => t.turn.kind === 'fleet')!.turn);
    const html = view({ turns, onDrill: () => undefined, openTurn: open });
    expect(html.match(/data-drilled="true"/g)).toHaveLength(1);
  });

  it('offers nothing to click when there is nowhere to put the raw view', () => {
    // A dead affordance teaches an operator not to trust the controls, which is the failure the
    // scope bar's stop buttons were written against one file over.
    expect(view({ turns, openTurn: null })).not.toContain('data-testid="turn-drill-');
  });
});

/* ───────────────────────────── where it lands on the thread ───────────────────────────── */

describe('the raw view is a sibling tab, and drilling still reaches it', () => {
  const events = [STARTED, ...FLEET, ev(5, 'WorkflowExecutionCompleted', 158_000)];
  const turns = readRunTurns('sweep-1', history(events));
  const row: RunRow = {
    runId: 'sweep-1',
    type: 'DnsSweep',
    status: 'completed',
    tenant: 'default',
    startedAt: BASE,
    closedAt: BASE + 158_000,
    dispatches: 3,
  };
  const thread = threadOf('DnsSweep', [row], 'sweep-1');
  const TAIL = createElement('div', { 'data-testid': 'the-tail' }, 'the compact tail');

  /** The turn that folded the fleet child — the one whose check goes one level deeper. */
  const fleetTurn = turns.named.turns.find((t) => t.turn.kind === 'fleet')!;

  /** The REAL panel, handed in where the page hands in `TurnDrill`. A stub would prove the tab
   *  renders something; this proves the events the turn folded are what is in view. */
  function rawPanel(named: NamedTurn, stack: DrillLevel[], view: DrillLevelView): ReactElement {
    return createElement(TranscriptDrill, {
      runId: 'sweep-1',
      named,
      stack,
      view,
      onDrill: () => undefined,
      onPop: () => undefined,
      onClose: () => undefined,
    });
  }

  function thread_(over: Partial<WorkflowThreadProps>): string {
    const props: WorkflowThreadProps = {
      workflow: 'dnssweep',
      thread,
      tab: 'transcript',
      onTab: () => undefined,
      onOpenRun: () => undefined,
      loaded: turns,
      code: createElement('div', null, 'editor'),
      monitor: createElement('div', null, 'pane'),
      now: BASE + 300_000,
      ...over,
    };
    return renderToStaticMarkup(createElement(WorkflowThread, props));
  }

  it('lands a drilled turn on the Event log, with the events that turn folded in view', () => {
    // THE WHOLE COST OF THE SPLIT, PAID. `openDrill` returns the turn AND the tab, so the click
    // that used to open a strip under the account now carries the reader to where the evidence is.
    const open = openDrill(fleetTurn);
    expect(open.tab).toBe(EVENT_LOG_TAB);
    const html = thread_({
      tab: open.tab,
      openTurn: open.openTurn,
      tail: TAIL,
      drill: rawPanel(fleetTurn, [rootLevel('sweep-1')], rootView(events, fleetTurn)),
    });
    expect(html).toContain('data-testid="event-log-drill"');
    expect(html).toContain('data-testid="turn-drill"');
    // The rows themselves, with Temporal's own type on each — not a panel that merely rendered.
    expect(html).toContain('data-testid="drill-row-2"');
    expect(html).toContain('StartChildWorkflowExecutionInitiated');
    expect(html).toContain('ChildWorkflowExecutionCompleted');
  });

  it('keeps the descent into a CHILD workflow on that tab, which is what the split threatened', () => {
    // A child is a different workflow with its own history, and it is the case the vocabulary most
    // needs: four opaque rows in the parent covering 156 seconds. Severing this at the tab boundary
    // would leave the friendly line unbacked exactly where it says least.
    const stack: DrillLevel[] = [
      rootLevel('sweep-1'),
      { workflowId: FLEET_LINK.workflowId, execId: FLEET_LINK.execId, from: 2, type: 'stackWorkflow' },
    ];
    const view: DrillLevelView = {
      rows: CHILD_EVENTS,
      missing: [],
      loading: false,
      failure: null,
      span: 156_000,
      closed: true,
    };
    const html = thread_({
      tab: EVENT_LOG_TAB,
      openTurn: openDrill(fleetTurn).openTurn,
      tail: TAIL,
      drill: rawPanel(fleetTurn, stack, view),
    });
    expect(html).toContain('data-testid="event-log-drill"');
    expect(html).toContain('data-depth="1"');
    expect(html).toContain('activityType=pulumiUp');
    // And both places to come back to are still on screen, inside the tab.
    expect(html).toContain('data-testid="drill-crumbs"');
    expect(html).toContain('data-testid="drill-crumb-1"');
  });

  it('takes the tail’s place rather than crowding it — two event logs is two answers', () => {
    const html = thread_({
      tab: EVENT_LOG_TAB,
      openTurn: openDrill(fleetTurn).openTurn,
      tail: TAIL,
      drill: rawPanel(fleetTurn, [rootLevel('sweep-1')], rootView(events, fleetTurn)),
    });
    expect(html).not.toContain('data-testid="the-tail"');
  });

  it('gives the pane back to the whole log when no turn is open', () => {
    const html = thread_({ tab: EVENT_LOG_TAB, tail: TAIL, openTurn: null });
    expect(html).toContain('data-testid="the-tail"');
    expect(html).toContain('data-testid="event-log-tail"');
    expect(html).not.toContain('data-testid="turn-drill"');
  });

  it('marks the drilled row on the transcript, and does not draw the raw view under it as well', () => {
    // The two tabs agree about the subject without sharing a pane: the row says which turn the log
    // beside it is about, and the account is not made to give up half its height to say so.
    const open = openDrill(fleetTurn);
    const html = thread_({
      tab: 'transcript',
      openTurn: open.openTurn,
      tail: TAIL,
      onDrillTurn: () => undefined,
      drill: rawPanel(fleetTurn, [rootLevel('sweep-1')], rootView(events, fleetTurn)),
    });
    expect(html.match(/data-drilled="true"/g)).toHaveLength(1);
    expect(html).not.toContain('data-testid="turn-drill"');
    expect(html).not.toContain('data-testid="the-tail"');
  });

  it('makes every row of the account drillable once there is somewhere to put it', () => {
    const html = thread_({ tail: TAIL, onDrillTurn: () => undefined, openTurn: null });
    expect(html).toContain(`data-testid="turn-drill-${turnKey(turns.named.turns[0]!.turn)}"`);
  });

  it('renders an unrecognised event as ITSELF, in the pane whose job that is', () => {
    const odd = [STARTED, ev(2, 'WorkflowPropertiesModified', 500), ev(3, 'WorkflowExecutionCompleted', 900)];
    const account = readRunTurns('sweep-1', history(odd));
    const untranslated = account.named.untranslated[0]!;
    expect(untranslated).toBeDefined();
    // It is not on the transcript, and the transcript did not lose it either: no row, no drill
    // button, and the count of it is on the chip bar with its address.
    expect(account.named.turns.some((t) => t.untranslated)).toBe(false);

    const readable = thread_({ tab: 'transcript', loaded: account, onDrillTurn: () => undefined, openTurn: null });
    expect(readable).not.toContain(`data-testid="turn-drill-${turnKey(untranslated.turn)}"`);
    // A COUNT WITH AN ADDRESS, and not the Temporal type: naming the type here would be this pane
    // speaking Temporal's language again, one chip instead of one row.
    expect(readable).toContain('data-chip="untranslated"');
    expect(readable).toContain('in the Event log');
    expect(readable).not.toContain('WorkflowPropertiesModified');

    // And on the Event log: the same type, on the row it was read from, verbatim.
    const raw = thread_({
      tab: EVENT_LOG_TAB,
      loaded: account,
      openTurn: openDrill(untranslated).openTurn,
      drill: rawPanel(untranslated, [rootLevel('sweep-1')], rootView(odd, untranslated)),
    });
    expect(raw).toContain('data-testid="drill-row-2"');
    expect(raw).toContain('WorkflowPropertiesModified');
  });
});

/* ───────────────────────────── which live run this is ───────────────────────────── */

describe('a live transcript says whether anything is watching it', () => {
  const live = readRunTurns(RUN, history([STARTED, ...FLEET]));

  function view(over: Parameters<typeof TranscriptView>[0]): string {
    return renderToStaticMarkup(createElement(TranscriptView, over));
  }

  it('says following when a reader is attached', () => {
    const html = view({ turns: live, follow: 'following' });
    expect(html).toContain('data-follow="following"');
    expect(html).toContain('still open - following');
  });

  it('says NOT following when the reader has been refused or has given up', () => {
    expect(view({ turns: live, follow: 'refused' })).toContain('still open - NOT following');
    expect(view({ turns: live, follow: 'stale' })).toContain('still open - NOT following');
  });

  it('with no reader at all, says so rather than assuming the good case', () => {
    const html = view({ turns: live });
    expect(html).toContain('data-follow="none"');
    expect(html).toContain('not following');
  });

  it('says nothing about following on a run that has closed', () => {
    const closed = readRunTurns(RUN, history([STARTED, ...FLEET, ev(5, 'WorkflowExecutionCompleted', 158_000)]));
    expect(view({ turns: closed, follow: 'settled' })).not.toContain('data-testid="transcript-live"');
  });
});
