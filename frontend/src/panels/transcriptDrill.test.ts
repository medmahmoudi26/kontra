/**
 * From a turn to the raw events behind it, and to the child workflow they open.
 *
 * WHAT THIS SUITE IS ACTUALLY DEFENDING is ADR 0027's condition on the vocabulary: a rename is only
 * affordable because it can be checked. So the cases below are the ones where a drill would be
 * easiest to leave out and most damaging to omit — a FOLDED group, whose members' events and child
 * links are reachable only through `expand`; an UNTRANSLATED turn, which is where a reader most
 * needs to see Temporal's own word; and a PARK, which has no event at all and must say so rather
 * than render as a log that lost its rows.
 *
 * Pure: turns and a reduced log in, rows out. Node, no browser. `cat` comes from `history.ts`'s own
 * `categorize`, so no fixture can quietly disagree with the reducer about what an event is.
 */

import { describe, expect, it } from 'vitest';

import { categorize } from '@core/history';
import { readTranscript, type Turn } from '@core/transcript';

import type { RunEvent, RunHistory } from '../run/api';
import { openerFor, openings, rowsFor, subjectOf, turnKey } from './transcriptDrill';

const BASE = 1_786_831_339_151;

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

/**
 * One dispatch to an Actor, with the backing workflow it names.
 *
 * THE LINK IS PER-MEMBER, which is the fixture's whole point: twelve dispatches of one run differ
 * only in the tail of their workflow id (`…-nscheck-60c3bfac`), and a drill that read the group's
 * head would offer one of the twelve.
 */
function dispatch(id: number, ms: number, n: number): RunEvent[] {
  const link = { workflowId: `actor-nscheck-nscheck-1786831339-nscheck-${n}`, via: 'nexus' as const };
  return [
    ev(id, 'NexusOperationScheduled', ms, { detail: 'endpoint=kontra-nscheck-0-1-0', link }),
    closes(id + 1, 'NexusOperationCompleted', ms + 900, ms, { detail: 'endpoint=kontra-nscheck-0-1-0', link }),
  ];
}

/** The `kontra-fleet/dns` child: four opaque rows in the parent covering most of a run. */
const FLEET_LINK = {
  workflowId: 'kontra-fleet/dns',
  execId: '01a00772-8296-7d33-8f8f-4b2e6f2f0001',
  type: 'stackWorkflow',
  via: 'child' as const,
};

function fleet(id: number, ms: number): RunEvent[] {
  return [
    ev(id, 'StartChildWorkflowExecutionInitiated', ms, { detail: 'workflowType=stackWorkflow', link: FLEET_LINK }),
    ev(id + 1, 'ChildWorkflowExecutionStarted', ms + 200, { detail: 'workflowType=stackWorkflow', link: FLEET_LINK }),
    closes(id + 2, 'ChildWorkflowExecutionCompleted', ms + 156_000, ms + 200, {
      detail: 'workflowType=stackWorkflow',
      link: FLEET_LINK,
    }),
  ];
}

function turnsOf(events: RunEvent[]): Turn[] {
  return readTranscript(history(events)).turns;
}

function ofKind(events: RunEvent[], kind: Turn['kind']): Turn {
  const found = turnsOf(events).find((t) => t.kind === kind);
  if (!found) throw new Error(`no ${kind} turn in this fixture — turns were ${turnsOf(events).map((t) => t.kind).join(', ')}`);
  return found;
}

/* ───────────────────────────── which turn ───────────────────────────── */

describe('turnKey', () => {
  it('does not move when a loop gains members', () => {
    const two = ofKind([STARTED, ...dispatch(2, 1_000, 1), ...dispatch(4, 2_000, 2)], 'dispatch');
    const four = ofKind(
      [STARTED, ...dispatch(2, 1_000, 1), ...dispatch(4, 2_000, 2), ...dispatch(6, 3_000, 3), ...dispatch(8, 4_000, 4)],
      'dispatch'
    );
    // A collapsed group is anchored at its FIRST member, so a live loop keeps its identity as it
    // grows — the row React reconciles and the turn the drill is open on stay the same thing.
    expect(turnKey(four)).toBe(turnKey(two));
  });

  it('gives a park a key even though it has no event', () => {
    const parked = readTranscript(history([STARTED]), {
      asks: [{ id: 'approve', prompt: 'Bring up 10 machines?', askedAt: BASE + 1_500 }],
    }).turns.find((t) => t.kind === 'parked')!;
    expect(turnKey(parked)).toBe(`parked:${BASE + 1_500}`);
  });
});

/* ───────────────────────────── what it drills to ───────────────────────────── */

describe('subjectOf', () => {
  it('gathers every member of a folded group, not just the head', () => {
    const events = [STARTED, ...dispatch(2, 1_000, 1), ...dispatch(4, 2_000, 2), ...dispatch(6, 3_000, 3)];
    const group = ofKind(events, 'dispatch');
    expect(group.count).toBe(3);

    const subject = subjectOf(group);
    // All six rows of the loop, in log order — the drill is the whole group, because the row is.
    expect(subject.events).toEqual([2, 3, 4, 5, 6, 7]);
    expect(subject.types).toEqual(['NexusOperationScheduled', 'NexusOperationCompleted']);
    // AND ALL THREE BACKING WORKFLOWS. `fold` builds a group by spreading its first member, so the
    // other two are reachable only through `expand` — a drill that read `turn.link` would offer one
    // of three and hide the rest.
    expect(subject.links.map((l) => l.workflowId)).toEqual([
      'actor-nscheck-nscheck-1786831339-nscheck-1',
      'actor-nscheck-nscheck-1786831339-nscheck-2',
      'actor-nscheck-nscheck-1786831339-nscheck-3',
    ]);
    expect(subject.eventless).toBe(false);
  });

  it('carries a child workflow through as one target, however many rows named it', () => {
    const subject = subjectOf(ofKind([STARTED, ...fleet(2, 1_000)], 'fleet'));
    expect(subject.events).toEqual([2, 3, 4]);
    // Three rows, ONE destination: the same execution, so one button rather than three that go to
    // the same place.
    expect(subject.links).toHaveLength(1);
    expect(subject.links[0]?.workflowId).toBe('kontra-fleet/dns');
    expect(subject.links[0]?.execId).toBe(FLEET_LINK.execId);
  });

  it('an untranslated turn drills to its own event and names its Temporal type', () => {
    const raw = ofKind([STARTED, ev(2, 'WorkflowPropertiesModified', 500)], 'raw');
    const subject = subjectOf(raw);
    expect(subject.events).toEqual([2]);
    // THE CASE THAT MATTERS MOST. This release has no word for the event, so the only honest thing
    // on screen is Temporal's own — and it must be reachable, not merely printed.
    expect(subject.types).toEqual(['WorkflowPropertiesModified']);
  });

  it('a park says it has no event rather than pretending to a hole in the log', () => {
    const parked = readTranscript(history([STARTED]), {
      asks: [{ id: 'approve', prompt: 'Bring up 10 machines?', askedAt: BASE + 1_500 }],
    }).turns.find((t) => t.kind === 'parked')!;
    const subject = subjectOf(parked);
    expect(subject.events).toEqual([]);
    expect(subject.eventless).toBe(true);
  });
});

/* ───────────────────────────── the rows behind it ───────────────────────────── */

describe('rowsFor', () => {
  const events = [STARTED, ...dispatch(2, 1_000, 1), ...dispatch(4, 2_000, 2)];

  it('answers out of the log the account was read from — no fetch, no second authority', () => {
    const group = ofKind(events, 'dispatch');
    const { rows, missing } = rowsFor(history(events), subjectOf(group).events);
    expect(rows.map((r) => r.id)).toEqual([2, 3, 4, 5]);
    expect(rows.map((r) => r.type)).toEqual([
      'NexusOperationScheduled',
      'NexusOperationCompleted',
      'NexusOperationScheduled',
      'NexusOperationCompleted',
    ]);
    expect(missing).toEqual([]);
  });

  it('names the rows an elided log no longer carries rather than showing four of six', () => {
    // `/history` drops events from the MIDDLE of a long log to stay under its cap. A turn that
    // names a row which is no longer there makes the check incomplete, and a check that is quietly
    // incomplete is worse than none.
    const elided = history(events.filter((e) => e.id !== 4 && e.id !== 5), { elided: 2, truncated: true });
    const { rows, missing } = rowsFor(elided, [2, 3, 4, 5]);
    expect(rows.map((r) => r.id)).toEqual([2, 3]);
    expect(missing).toEqual([4, 5]);
  });

  it('with no log in hand, every id is missing and none is invented', () => {
    expect(rowsFor(null, [2, 3])).toEqual({ rows: [], missing: [2, 3] });
  });
});

/* ───────────────────────────── across a child ───────────────────────────── */

describe('openings', () => {
  it('offers one button per destination, not one per row', () => {
    const rows = [STARTED, ...fleet(2, 1_000)];
    const found = openings(rows);
    expect(found).toHaveLength(1);
    // The OPENER is kept — `drillInto` records the event a level was reached from, and "where did
    // this come from" is better answered by the initiation than by the completion.
    expect(found[0]?.id).toBe(2);
    expect(found[0]?.type).toBe('StartChildWorkflowExecutionInitiated');
  });

  it('two different executions of one workflow id are two destinations', () => {
    // MEASURED on `nscheck-1786831339`: fleet up (exec `01a00772…`) and fleet down (exec
    // `01a00776…`) are both `kontra-fleet/dns`, so collapsing on the id alone would send a drill
    // into the 156-second bring-up to the 29-second teardown instead.
    const down = { ...FLEET_LINK, execId: '01a00776-89dc-7d33-8f8f-4b2e6f2f0002' };
    const rows = [
      ...fleet(2, 1_000),
      ev(9, 'StartChildWorkflowExecutionInitiated', 260_000, { detail: 'workflowType=stackWorkflow', link: down }),
    ];
    expect(openings(rows).map((r) => r.id)).toEqual([2, 9]);
  });

  it('skips every row the server put no link on', () => {
    expect(openings([STARTED, ev(2, 'ActivityTaskScheduled', 500)])).toEqual([]);
  });
});

describe('openerFor', () => {
  const rows = [STARTED, ...fleet(2, 1_000)];

  it('finds the event a turn-level button has to hand drillInto', () => {
    expect(openerFor(rows, FLEET_LINK)?.id).toBe(2);
  });

  it('answers nothing when the opener is not in the rows on hand', () => {
    // An elided log. The button is drawn disabled and says why, rather than doing nothing.
    expect(openerFor([STARTED], FLEET_LINK)).toBeUndefined();
  });

  it('will not match a different execution of the same workflow id', () => {
    expect(openerFor(rows, { ...FLEET_LINK, execId: 'another-one' })).toBeUndefined();
  });
});
