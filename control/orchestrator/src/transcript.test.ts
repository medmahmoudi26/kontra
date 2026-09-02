import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe as test, expect, it } from 'vitest';

import { MemoryStore, ObjectStore } from './codec/objectStore';
import { categorize, mapHistory, type RawHistoryEvent } from '@kontra/core/history';
import { ASK_MEMO_PREFIX as SERVER_ASK_MEMO_PREFIX } from './hitl';
import { HistoryArchive } from './historyArchive';
import {
  actorOf,
  actorOfQueue,
  expand,
  metaOf,
  methodOf,
  readTranscript,
  type Ask,
  type DispatchTurn,
  type FailedTurn,
  type FinishedTurn,
  type FleetTurn,
  type ParkedTurn,
  type StartedTurn,
  type TranscriptEvent,
  type Turn,
} from '@kontra/core/transcript';

/**
 * THE READER NEVER SEES A PAYLOAD, AND THESE FIXTURES ARE THE PROOF.
 *
 * Everything below is a REDUCED event — the shape `mapHistory` produces and the archive stores.
 * There is no `input`, no `result` and no failure payload anywhere in this file, because there is
 * none in the thing the reader reads. Any fact a turn carries here was therefore read from
 * metadata: the type, the category, the two timestamps, `dur`, `attempt`, the workflow the event
 * names, and the one line `history.ts` already built from `activityType`, `taskQueue`, `endpoint`
 * and the failure MESSAGE. A test that had to hand the reader a payload to get a turn out of it
 * would be the first sign this module had broken its one rule.
 */

/** The first event's instant on the recorded run, so offsets read like the real thing. */
const BASE = 1786831339151;

/** One reduced event. `cat` is derived by `history.ts`'s own `categorize`, never hand-written —
 *  a fixture that disagreed with the reducer about what counts as a failure would test nothing. */
function ev(id: number, type: string, ms: number, extra: Partial<TranscriptEvent> = {}): TranscriptEvent {
  return {
    id,
    type,
    cat: categorize(type),
    t: ms / 1000,
    at: BASE + ms,
    detail: type,
    attempt: 1,
    dur: 0,
    ...extra,
  };
}

/**
 * A closing event, with the duration Temporal recorded back to the event it closes.
 *
 * `dur` IS THE JOIN. The reduced log drops `scheduledEventId`, so this is how the reader rejoins a
 * family — and writing fixtures this way is what keeps the test honest about it: a closer whose
 * `dur` does not point at a real opener is a closer the reader cannot place, exactly as in
 * production.
 */
function closes(
  id: number,
  type: string,
  ms: number,
  openerMs: number,
  extra: Partial<TranscriptEvent> = {}
): TranscriptEvent {
  return ev(id, type, ms, { dur: (ms - openerMs) / 1000, ...extra });
}

/* ───────────────────────────── the recorded run ───────────────────────────── */

/**
 * THE SAME EIGHT EVENTS OFF THE SAME REAL RUN, and read the same way the console reads them.
 *
 * Captured 2026-08-15 with `temporal workflow show -w nscheck-1786831339 -o json` — the 294-second,
 * 305-event NsCheck run (`history.test.ts` holds the same recording and explains each event). It is
 * here in its RAW form and put through `mapHistory` below, so this suite asserts the whole path a
 * browser actually takes: Temporal's own bytes → the reduced log → turns. Payloads are dropped from
 * the recording because the reducer drops them.
 */
const RECORDED: RawHistoryEvent[] = [
  {
    eventId: '1',
    eventTime: { seconds: 1786831339, nanos: 151182504 },
    workflowExecutionStartedEventAttributes: {
      workflowType: { name: 'NsCheck' },
      taskQueue: { name: 'recon', kind: 'TASK_QUEUE_KIND_NORMAL' },
      identity: '1@65520c20a6a4',
      attempt: 1,
      workflowId: 'nscheck-1786831339',
    },
  },
  {
    eventId: '11',
    eventTime: { seconds: 1786831340, nanos: 164897318 },
    startChildWorkflowExecutionInitiatedEventAttributes: {
      namespace: 'default',
      workflowId: 'kontra-fleet/dns',
      workflowType: { name: 'stackWorkflow' },
      taskQueue: { name: 'kontra-infra' },
      workflowTaskCompletedEventId: '10',
    },
  },
  {
    eventId: '12',
    eventTime: { seconds: 1786831340, nanos: 220203004 },
    childWorkflowExecutionStartedEventAttributes: {
      namespace: 'default',
      initiatedEventId: '11',
      workflowExecution: {
        workflowId: 'kontra-fleet/dns',
        runId: '01a00772-8296-7c0e-ae18-7aaca6721266',
      },
      workflowType: { name: 'stackWorkflow' },
    },
  },
  {
    eventId: '16',
    eventTime: { seconds: 1786831496, nanos: 530188615 },
    childWorkflowExecutionCompletedEventAttributes: {
      namespace: 'default',
      workflowExecution: {
        workflowId: 'kontra-fleet/dns',
        runId: '01a00772-8296-7c0e-ae18-7aaca6721266',
      },
      workflowType: { name: 'stackWorkflow' },
      initiatedEventId: '11',
      startedEventId: '12',
    },
  },
  {
    eventId: '38',
    eventTime: { seconds: 1786831498, nanos: 930265396 },
    nexusOperationScheduledEventAttributes: {
      endpoint: 'kontra-nscheck-0-1-0',
      service: 'kontra.actor',
      operation: 'run',
      workflowTaskCompletedEventId: '37',
      requestId: '4623e62b-6d76-461b-8dfc-63c72afaee94',
    },
  },
  {
    eventId: '39',
    eventTime: { seconds: 1786831498, nanos: 991823074 },
    nexusOperationStartedEventAttributes: {
      scheduledEventId: '38',
      operationToken:
        'eyJ0IjoxLCJucyI6ImRlZmF1bHQiLCJ3aWQiOiJhY3Rvci1uc2NoZWNrLW5zY2hlY2stMTc4NjgzMTMzOS1uc2NoZWNrLTYwYzNiZmFjIn0',
      requestId: '4623e62b-6d76-461b-8dfc-63c72afaee94',
    },
  },
  {
    eventId: '43',
    eventTime: { seconds: 1786831502, nanos: 409108469 },
    nexusOperationCompletedEventAttributes: {
      scheduledEventId: '38',
      requestId: '4623e62b-6d76-461b-8dfc-63c72afaee94',
    },
  },
  {
    eventId: '296',
    eventTime: { seconds: 1786831604, nanos: 171224122 },
    startChildWorkflowExecutionInitiatedEventAttributes: {
      namespace: 'default',
      workflowId: 'kontra-fleet/dns',
      workflowType: { name: 'stackWorkflow' },
      taskQueue: { name: 'kontra-infra' },
      workflowTaskCompletedEventId: '295',
    },
  },
  {
    eventId: '297',
    eventTime: { seconds: 1786831604, nanos: 219583540 },
    childWorkflowExecutionStartedEventAttributes: {
      namespace: 'default',
      initiatedEventId: '296',
      workflowExecution: {
        workflowId: 'kontra-fleet/dns',
        runId: '01a00776-89dc-75da-a0a2-97d86a09a2a3',
      },
      workflowType: { name: 'stackWorkflow' },
    },
  },
  {
    eventId: '305',
    eventTime: { seconds: 1786831633, nanos: 98551306 },
    workflowExecutionCompletedEventAttributes: { workflowTaskCompletedEventId: '304' },
  },
];

test('the recorded run, as turns', () => {
  const transcript = readTranscript(mapHistory(RECORDED));
  const kinds = transcript.turns.map((t) => t.kind);

  // FIVE TURNS OUT OF TEN EVENTS, and the three-event families are the reason: one fleet bring-up,
  // one dispatch and one teardown are three things that happened, not eight rows to scroll.
  it('folds each family of events into the one thing that happened', () => {
    expect(kinds).toEqual(['started', 'fleet', 'dispatch', 'fleet', 'finished']);
    expect(transcript.turns.map((t) => t.events)).toEqual([
      [1],
      [11, 12, 16],
      [38, 39, 43],
      [296, 297],
      [305],
    ]);
  });

  it('reads what the run set out to do off the start event, from metadata', () => {
    const started = transcript.turns[0] as StartedTurn;
    expect(started.workflowType).toBe('NsCheck');
    expect(started.queue).toBe('recon');
    expect(started.identity).toBe('1@65520c20a6a4');
    // Nothing was submitted to this reader, so nothing is claimed to have been submitted.
    expect(started.input).toBeUndefined();
  });

  // THE WINDOW THIS WHOLE PLANE EXISTS BECAUSE OF: 156.31 of the run's 293.947 seconds, in one
  // child, which the run surface used to render as "nothing dispatched yet". The number is the same
  // one the console's fleet window prints — two surfaces must not disagree about one measurement.
  it('measures the provisioning window and separates it from the wait to start it', () => {
    const fleet = transcript.turns[1] as FleetTurn;
    expect(fleet.fleet).toBe('kontra-fleet/dns');
    expect(fleet.state).toBe('ready');
    expect(fleet.dur).toBeCloseTo(156.31, 2);
    expect(fleet.queued).toBeCloseTo(0.056, 3);
    expect(fleet.open).toBe(false);
  });

  // THE TRAP THIS CLOSES. `kontra-fleet/dns` is the id of the bring-up AND of the teardown, so a
  // turn that carried only the id would drill into whichever ran last.
  it('pins each fleet turn to its own execution, not to the id they share', () => {
    const [bringUp, teardown] = [transcript.turns[1] as FleetTurn, transcript.turns[3] as FleetTurn];
    expect(bringUp.link?.execId).toBe('01a00772-8296-7c0e-ae18-7aaca6721266');
    expect(teardown.link?.execId).toBe('01a00776-89dc-75da-a0a2-97d86a09a2a3');
    // The teardown never closed inside the events we read, and is not reported as if it had.
    expect(teardown.state).toBe('running');
    expect(teardown.open).toBe(true);
  });

  it('names the Actor and its version off the endpoint, and drills into the workflow it started', () => {
    const dispatch = transcript.turns[2] as DispatchTurn;
    expect(dispatch.actor).toBe('nscheck');
    expect(dispatch.version).toBe('0.1.0');
    expect(dispatch.endpoint).toBe('kontra-nscheck-0-1-0');
    expect(dispatch.state).toBe('done');
    expect(dispatch.link?.workflowId).toBe('actor-nscheck-nscheck-1786831339-nscheck-60c3bfac');
    // THE OLDER RUN. This recording is from before the Summary carried the Method name, which is
    // what every archived run is and what every Go dispatch still is. The Method is not here, it is
    // not guessed, and nothing about the turn is worse for it: the Actor, the version, the state
    // and the drill link all read exactly as they always did.
    expect(dispatch.method).toBeUndefined();
    expect(dispatch.units).toBeUndefined();
  });

  it('ends the run where its history ends it', () => {
    const finished = transcript.turns[4] as FinishedTurn;
    expect(finished.outcome).toBe('completed');
    expect(finished.t).toBeCloseTo(293.947, 3);
    expect(transcript.live).toBe(false);
    expect(transcript.outcome).toBe('completed');
    expect(transcript.span).toBeCloseTo(293.947, 3);
  });

  // NOT ONE EVENT DROPPED. Anything this module has no word for renders as itself; anything it does
  // have a word for still names the events it folded. Both are the same guarantee.
  it('accounts for every event it was given, exactly once', () => {
    const seen = transcript.turns.flatMap((t) => expand(t)).flatMap((t) => t.events);
    expect([...seen].sort((a, b) => a - b)).toEqual([1, 11, 12, 16, 38, 39, 43, 296, 297, 305]);
    expect(new Set(seen).size).toBe(seen.length);
  });
});

/* ───────────────────────────── every turn kind ───────────────────────────── */

test('every turn kind, from the shapes the reduced log actually carries', () => {
  it('started — the input the operator submitted, supplied rather than decoded', () => {
    const { turns } = readTranscript(
      { events: [ev(1, 'WorkflowExecutionStarted', 0, { detail: 'workflowType=DnsSweep · taskQueue=recon' })] },
      { input: { dataset: 'targets', into: 'live' } }
    );
    const started = turns[0] as StartedTurn;
    expect(started.kind).toBe('started');
    expect(started.workflowType).toBe('DnsSweep');
    expect(started.input).toEqual({ dataset: 'targets', into: 'live' });
  });

  it('fleet — requested before Temporal has an execution to pin', () => {
    const { turns } = readTranscript({
      events: [
        ev(2, 'StartChildWorkflowExecutionInitiated', 1000, {
          detail: 'workflowType=stackWorkflow · taskQueue=kontra-infra',
          link: { workflowId: 'kontra-fleet/dns', type: 'stackWorkflow', via: 'child' },
        }),
      ],
    });
    const fleet = turns[0] as FleetTurn;
    expect(fleet.kind).toBe('fleet');
    expect(fleet.state).toBe('requested');
    // How many machines were asked for is in the child's input. Absent, never 0 — a fleet reported
    // as "0 ready" while four are booting is the plausible wrong answer.
    expect(fleet.machines).toBeUndefined();
  });

  it('fleet — a child that failed is a fleet that failed', () => {
    const { turns } = readTranscript({
      events: [
        ev(2, 'StartChildWorkflowExecutionInitiated', 1000, {
          link: { workflowId: 'kontra-fleet/dns', type: 'stackWorkflow', via: 'child' },
        }),
        closes(3, 'ChildWorkflowExecutionFailed', 9000, 1000, {
          detail: 'droplet create failed: no capacity in sfo3',
          link: { workflowId: 'kontra-fleet/dns', type: 'stackWorkflow', via: 'child' },
        }),
      ],
    });
    const fleet = turns[0] as FleetTurn;
    expect(fleet.state).toBe('failed');
    expect(fleet.failures).toBe(1);
    expect(fleet.error).toBe('droplet create failed: no capacity in sfo3');
    expect(fleet.dur).toBe(8);
  });

  it('dispatch — the RunBatch path names the Actor off the task queue, exactly', () => {
    const { turns } = readTranscript({
      events: [
        ev(2, 'ActivityTaskScheduled', 0, {
          detail: 'activityType=RunBatch · taskQueue=nscheck-0.1.0',
          summary: 'addrs',
        }),
        closes(3, 'ActivityTaskStarted', 400, 0),
        closes(4, 'ActivityTaskCompleted', 2400, 400),
      ],
    });
    const dispatch = turns[0] as DispatchTurn;
    expect(dispatch.kind).toBe('dispatch');
    expect(dispatch.actor).toBe('nscheck');
    expect(dispatch.version).toBe('0.1.0');
    expect(dispatch.queue).toBe('nscheck-0.1.0');
    // The Summary is the decided route for the Method name, and it is read as one.
    expect(dispatch.method).toBe('addrs');
    expect(dispatch.state).toBe('done');
    expect(dispatch.dur).toBe(2);
    expect(dispatch.queued).toBe(0.4);
  });

  // 12c: A DISPATCH ONTO A QUEUE NOBODY POLLS. It is scheduled, it never starts, and it is the
  // failure that reads identically to a run that is merely slow — so it has its own word.
  it('dispatch — one that was never picked up stays queued and open', () => {
    const { turns } = readTranscript({
      events: [
        ev(2, 'NexusOperationScheduled', 0, { detail: 'endpoint=kontra-probe-0-1-0 · nexus=kontra.actor/run' }),
      ],
    });
    const dispatch = turns[0] as DispatchTurn;
    expect(dispatch.state).toBe('queued');
    expect(dispatch.open).toBe(true);
    expect(dispatch.queued).toBeUndefined();
  });

  it('narration — an author-written sentence is a turn of its own', () => {
    const { turns } = readTranscript({
      events: [
        ev(2, 'MarkerRecorded', 4000, { detail: 'marker=kontra.say', summary: '119 apexes in scope, 37k subs expected' }),
      ],
    });
    expect(turns[0]!.kind).toBe('narration');
    expect((turns[0] as { text: string }).text).toBe('119 apexes in scope, 37k subs expected');
  });

  it('dataset — the lake activities, by what each one did', () => {
    const { turns } = readTranscript({
      events: [
        ev(2, 'ActivityTaskScheduled', 0, { detail: 'activityType=openTempDataset · taskQueue=kontra-datasets' }),
        closes(3, 'ActivityTaskCompleted', 200, 0),
        ev(4, 'ActivityTaskScheduled', 400, { detail: 'activityType=publishBatch · taskQueue=kontra-datasets' }),
        closes(5, 'ActivityTaskCompleted', 900, 400),
        ev(6, 'ActivityTaskScheduled', 1000, { detail: 'activityType=closeDataset · taskQueue=kontra-datasets' }),
        closes(7, 'ActivityTaskCompleted', 1100, 1000),
      ],
    });
    expect(turns.map((t) => t.kind)).toEqual(['dataset', 'dataset', 'dataset']);
    expect(turns.map((t) => (t as { action: string }).action)).toEqual(['open', 'publish', 'close']);
    // `closeDataset` cannot say SEALED or ABANDONED — the state is a payload field — and the reader
    // says `close` rather than picking one. Sealed and abandoned are the difference between "there
    // was nothing to find" and "the producer died".
    expect((turns[2] as { dataset?: string }).dataset).toBeUndefined();
  });

  it('parked — the pending ask, placed where the run stopped to ask it', () => {
    const ask: Ask = {
      id: 'approve-1',
      prompt: 'Approve these 12 hosts?',
      askedAt: BASE + 5000,
      schema: { type: 'object', properties: { approved: { type: 'boolean' } } },
      context: { dataset: 'live', n: 12 },
    };
    const { turns } = readTranscript(
      { events: [ev(1, 'WorkflowExecutionStarted', 0), ev(2, 'MarkerRecorded', 9000)] },
      { asks: [ask], now: BASE + 245_000 }
    );
    const parked = turns.find((t) => t.kind === 'parked') as ParkedTurn;
    expect(parked.pending).toBe(true);
    expect(parked.waited).toBe(240);
    expect(parked.ask.context).toEqual({ dataset: 'live', n: 12 });
    // It belongs where it was asked, not at the end of the list.
    expect(turns.map((t) => t.kind)).toEqual(['started', 'parked', 'raw']);
  });

  it('parked — an answered ask is preserved beside the question it answered', () => {
    const { turns } = readTranscript(
      { events: [ev(1, 'WorkflowExecutionStarted', 0)] },
      {
        asks: [{ id: 'a', prompt: 'Approve?', askedAt: BASE + 1000, answeredAt: BASE + 61_000, by: 'mohamed' }],
        now: BASE + 600_000,
      }
    );
    const parked = turns[1] as ParkedTurn;
    expect(parked.pending).toBe(false);
    expect(parked.waited).toBe(60);
    expect(parked.ask.by).toBe('mohamed');
  });

  it('parked — a deadline that passed unanswered is reported, and nothing more', () => {
    const { turns } = readTranscript(
      { events: [ev(1, 'WorkflowExecutionStarted', 0)] },
      { asks: [{ id: 'a', prompt: 'Approve?', askedAt: BASE, deadlineAt: BASE + 60_000 }], now: BASE + 90_000 }
    );
    expect((turns[1] as ParkedTurn).expired).toBe(true);
    // What an expiry MEANS is the author's decision — `ask` raises inside the workflow. The reader
    // reports the deadline passing; it never decides that the run failed.
    expect(turns.some((t) => t.kind === 'failed')).toBe(false);
  });

  it('raw — an event with no domain word renders as its Temporal type, never dropped', () => {
    const { turns } = readTranscript({ events: [ev(2, 'WorkflowExecutionSignaled', 1000, { detail: 'identity=cli' })] });
    expect(turns[0]!.kind).toBe('raw');
    expect((turns[0] as { type: string }).type).toBe('WorkflowExecutionSignaled');
    expect(turns[0]!.types).toEqual(['WorkflowExecutionSignaled']);
  });

  it('raw — Temporal\'s own scheduler bookkeeping is marked plumbing, and a failure never is', () => {
    const { turns } = readTranscript({
      events: [ev(2, 'WorkflowTaskScheduled', 0), ev(3, 'WorkflowTaskTimedOut', 10_000)],
    });
    expect(turns.map((t) => (t as { plumbing: boolean }).plumbing)).toEqual([true, false]);
  });
});

/* ───────────── finished, failed, and the run that produced nothing ───────────── */

test('a run that produced nothing is not a run that failed', () => {
  /** A complete, clean run that wrote not one row. */
  const empty = readTranscript({
    events: [
      ev(1, 'WorkflowExecutionStarted', 0, { detail: 'workflowType=DnsSweep' }),
      ev(2, 'ActivityTaskScheduled', 1000, { detail: 'activityType=pageDataset · taskQueue=kontra-datasets' }),
      closes(3, 'ActivityTaskCompleted', 1200, 1000),
      ev(4, 'WorkflowExecutionCompleted', 2000),
    ],
  });

  /** The same run, ended by an Actor that could not be reached. */
  const failed = readTranscript({
    events: [
      ev(1, 'WorkflowExecutionStarted', 0, { detail: 'workflowType=DnsSweep' }),
      ev(2, 'NexusOperationScheduled', 1000, { detail: 'endpoint=kontra-probe-0-1-0 · nexus=kontra.actor/run' }),
      closes(3, 'NexusOperationFailed', 4000, 1000, {
        detail: 'RunBatch failed: dial tcp: i/o timeout · RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED',
      }),
      ev(4, 'WorkflowExecutionFailed', 4100, { detail: 'workflow execution error' }),
    ],
  });

  it('reports the empty run as finished, and says it published nothing', () => {
    const last = empty.turns[empty.turns.length - 1] as FinishedTurn;
    expect(last.kind).toBe('finished');
    expect(last.outcome).toBe('completed');
    expect(last.published).toBe(0);
    expect(last.empty).toBe(true);
    expect(empty.empty).toBe(true);
    // The distinction the whole module turns on: reading a Dataset is not writing one, and a run
    // that read 200 pages and wrote nothing still wrote nothing.
    expect(empty.turns.some((t) => t.kind === 'failed')).toBe(false);
  });

  it('reports the failed run as failed, and never as empty output', () => {
    const last = failed.turns[failed.turns.length - 1] as FailedTurn;
    expect(last.kind).toBe('failed');
    expect(last.outcome).toBe('failed');
    expect(failed.turns.some((t) => t.kind === 'finished')).toBe(false);
    expect(failed.outcome).toBe('failed');
  });

  it('says WHERE the run failed, not only that it did', () => {
    const last = failed.turns[failed.turns.length - 1] as FailedTurn;
    expect(last.because).toBe('workflow execution error');
    // A workflow's own failure event repeats itself at best. What an operator needs is the dispatch
    // that broke — an earlier event, with its own metadata.
    expect(last.where?.event).toBe(3);
    expect(last.where?.detail).toContain('dial tcp: i/o timeout');
  });

  it('counts a publish as output, so a run that wrote rows is not called empty', () => {
    const wrote = readTranscript({
      events: [
        ev(1, 'WorkflowExecutionStarted', 0),
        ev(2, 'ActivityTaskScheduled', 1000, { detail: 'activityType=publishBatch · taskQueue=kontra-datasets' }),
        closes(3, 'ActivityTaskCompleted', 1500, 1000),
        ev(4, 'WorkflowExecutionCompleted', 2000),
      ],
    });
    expect(wrote.published).toBe(1);
    expect(wrote.empty).toBe(false);
    expect((wrote.turns[wrote.turns.length - 1] as FinishedTurn).empty).toBe(false);
  });

  it('keeps a cancelled run its own word rather than folding it into failed', () => {
    const cancelled = readTranscript({
      events: [ev(1, 'WorkflowExecutionStarted', 0), ev(2, 'WorkflowExecutionCanceled', 500, { detail: 'operator stopped it' })],
    });
    expect(cancelled.outcome).toBe('cancelled');
    expect((cancelled.turns[1] as FailedTurn).outcome).toBe('cancelled');
  });
});

/* ───────────────────────────── live versus finished ───────────────────────────── */

test('a live run and a finished one are different things', () => {
  const open = [
    ev(1, 'WorkflowExecutionStarted', 0),
    ev(2, 'NexusOperationScheduled', 1000, { detail: 'endpoint=kontra-probe-0-1-0' }),
  ];

  it('a run with no closing event is live, and claims no outcome', () => {
    const t = readTranscript({ events: open });
    expect(t.live).toBe(true);
    expect(t.outcome).toBeUndefined();
    expect(t.turns.some((x) => x.kind === 'finished' || x.kind === 'failed')).toBe(false);
  });

  it('the same run with its closing event is finished', () => {
    const t = readTranscript({ events: [...open, ev(3, 'WorkflowExecutionCompleted', 5000)] });
    expect(t.live).toBe(false);
    expect(t.outcome).toBe('completed');
  });

  // A CONTINUED CHAIN IS STILL RUNNING. Each leg closes as it hands over, so reading the handover
  // as an ending would report a sweep that is still going as done.
  it('a run that continued as new is still live', () => {
    const t = readTranscript({ events: [...open, ev(3, 'WorkflowExecutionContinuedAsNew', 5000)] });
    expect(t.continued).toBe(true);
    expect(t.live).toBe(true);
    expect(t.outcome).toBeUndefined();
  });

  it('an empty log reads as live rather than as a run that finished', () => {
    const t = readTranscript({ events: [] });
    expect(t.turns).toEqual([]);
    expect(t.live).toBe(true);
    expect(t.span).toBe(0);
  });

  it('carries the log\'s own caveats through rather than swallowing them', () => {
    const t = readTranscript({
      events: [ev(1, 'WorkflowExecutionStarted', 0)],
      elided: 19_231,
      truncated: true,
      archived: true,
      archivedAt: 1786840000000,
    });
    expect(t.elided).toBe(19_231);
    expect(t.truncated).toBe(true);
    expect(t.archived).toBe(true);
    expect(t.archivedAt).toBe(1786840000000);
  });

  // THE JOIN IS CONSUMING, so anything that asks it when it should not have silently steals another
  // family's close. An opener's `dur` is 0, which points the arithmetic straight at its own instant.
  it('never lets a dispatch scheduled in the same millisecond steal another family\'s close', () => {
    const { turns } = readTranscript({
      events: [
        // One dispatch, still open: scheduled, then started 5s later.
        ev(2, 'NexusOperationScheduled', 0, { detail: 'endpoint=kontra-probe-0-1-0' }),
        closes(3, 'NexusOperationStarted', 5000, 0),
        // A second dispatch scheduled at the exact instant the first one STARTED, with no
        // operation token to key off. Its own `dur` is 0, so the arithmetic points at 5000 —
        // the instant the first family is waiting under.
        ev(4, 'NexusOperationScheduled', 5000, { detail: 'endpoint=kontra-dnsfacts-0-1-0' }),
        closes(5, 'NexusOperationCompleted', 8000, 5000),
      ],
    });
    // The first dispatch keeps its close. If the opener had consumed the join, that close would
    // have found nothing to attach to and rendered as a loose raw row under a dispatch that
    // appeared to still be running.
    expect(turns.map((t) => t.events)).toEqual([[2, 3, 5], [4]]);
    expect(turns.some((t) => t.kind === 'raw')).toBe(false);
    expect((turns[0] as DispatchTurn).state).toBe('done');
    expect((turns[1] as DispatchTurn).state).toBe('queued');
    expect(turns[1]!.open).toBe(true);
  });

  it('draws a fleet window from its closing event alone when the opener was elided', () => {
    const { turns } = readTranscript({
      events: [
        closes(900, 'ChildWorkflowExecutionCompleted', 160_000, 3_690, {
          link: {
            workflowId: 'kontra-fleet/dns',
            execId: '01a00772-8296-7c0e-ae18-7aaca6721266',
            type: 'stackWorkflow',
            via: 'child',
          },
        }),
      ],
      elided: 880,
    });
    const fleet = turns[0] as FleetTurn;
    expect(fleet.kind).toBe('fleet');
    expect(fleet.state).toBe('ready');
    expect(fleet.dur).toBeCloseTo(156.31, 2);
    // Its start is not in the log and is not back-dated from a guess.
    expect(fleet.events).toEqual([900]);
  });

  it('places a family whose opener was elided rather than back-dating it', () => {
    // `dur` points at an event that is not in the window, so there is nothing to join to. The close
    // is real and is shown; no start is invented for it.
    const t = readTranscript({ events: [closes(900, 'ActivityTaskCompleted', 60_000, 200), ev(901, 'WorkflowExecutionCompleted', 61_000)], elided: 850 });
    expect(t.turns[0]!.kind).toBe('raw');
    expect(t.turns[0]!.events).toEqual([900]);
  });
});

/* ───────────────────────────── collapsing ───────────────────────────── */

/**
 * A LOOP THE WAY A LOOP ACTUALLY ARRIVES.
 *
 * `dnssweep` pages a Dataset, dispatches to one Actor, dispatches to a second, publishes, and goes
 * round again — so two hundred iterations INTERLEAVE four kinds of turn. A collapse rule that only
 * folded strictly-adjacent turns would fold nothing here while passing happily against a fixture of
 * two hundred identical events in a row. This fixture is the interleaved shape on purpose.
 */
function sweep(iterations: number, failAt: number[] = []): TranscriptEvent[] {
  const events: TranscriptEvent[] = [ev(1, 'WorkflowExecutionStarted', 0)];
  let id = 2;
  let ms = 1000;
  for (let i = 0; i < iterations; i += 1) {
    const page = ms;
    events.push(ev(id++, 'ActivityTaskScheduled', page, { detail: 'activityType=pageDataset · taskQueue=kontra-datasets' }));
    events.push(closes(id++, 'ActivityTaskCompleted', page + 50, page));
    const dns = page + 60;
    events.push(ev(id++, 'NexusOperationScheduled', dns, { detail: 'endpoint=kontra-dnsfacts-0-1-0 · nexus=kontra.actor/run' }));
    const broke = failAt.includes(i);
    events.push(
      closes(id++, broke ? 'NexusOperationFailed' : 'NexusOperationCompleted', dns + 400, dns, {
        detail: broke ? 'RunBatch failed: dial tcp: i/o timeout' : 'NexusOperationCompleted',
      })
    );
    const probe = dns + 500;
    events.push(ev(id++, 'NexusOperationScheduled', probe, { detail: 'endpoint=kontra-probe-0-1-0 · nexus=kontra.actor/run' }));
    events.push(closes(id++, 'NexusOperationCompleted', probe + 300, probe));
    const publish = probe + 400;
    events.push(ev(id++, 'ActivityTaskScheduled', publish, { detail: 'activityType=publishBatch · taskQueue=kontra-datasets' }));
    events.push(closes(id++, 'ActivityTaskCompleted', publish + 100, publish));
    ms = publish + 200;
  }
  events.push(ev(id++, 'WorkflowExecutionCompleted', ms));
  return events;
}

test('two hundred batches read as one line', () => {
  const { turns, published } = readTranscript({ events: sweep(200) });

  it('folds a whole interleaved loop into one turn per repeated call', () => {
    expect(turns.map((t) => t.kind)).toEqual(['started', 'dataset', 'dispatch', 'dispatch', 'dataset', 'finished']);
    expect(turns.map((t) => t.count)).toEqual([1, 200, 200, 200, 200, 1]);
  });

  it('separates the two Actors, because they are two different calls', () => {
    expect((turns[2] as DispatchTurn).endpoint).toBe('kontra-dnsfacts-0-1-0');
    expect((turns[3] as DispatchTurn).endpoint).toBe('kontra-probe-0-1-0');
    expect((turns[2] as DispatchTurn).actor).toBe('dnsfacts');
  });

  it('carries the totals, so the one line says what the two hundred did', () => {
    const dns = turns[2] as DispatchTurn;
    // SUMMED, not spanned: 200 calls of 0.4s is 80 seconds of dispatching, inside a run that took
    // very much longer. The span is beside it and says something different.
    expect(dns.dur).toBeCloseTo(80, 6);
    expect(dns.t).toBeCloseTo(1.06, 6);
    expect(dns.endT).toBeCloseTo(sweepEndT(200), 6);
    expect(dns.state).toBe('done');
    expect(published).toBe(200);
  });

  it('expands to every member, each keeping its own events', () => {
    const dns = turns[2] as DispatchTurn;
    const members = expand(dns);
    expect(members).toHaveLength(200);
    expect(members[0]!.events).toEqual([4, 5]);
    expect(new Set(members.flatMap((m) => m.events)).size).toBe(400);
    // A single turn expands to itself, so a caller never has to branch on `folded`.
    expect(expand(turns[0]!)).toEqual([turns[0]]);
    expect(turns[0]!.folded).toBeUndefined();
  });

  it('accounts for every one of the loop\'s events exactly once', () => {
    const all = turns.flatMap((t) => expand(t)).flatMap((t) => t.events);
    expect(new Set(all).size).toBe(all.length);
    expect(all.length).toBe(sweep(200).length);
  });

  // A SWEEP WHERE ONE UNIT BROKE MUST NOT SHATTER INTO TWO HUNDRED LINES TO SAY SO.
  it('keeps a loop with a failure in it one line, and says how many broke', () => {
    const broken = readTranscript({ events: sweep(200, [37, 91]) });
    const dns = broken.turns[2] as DispatchTurn;
    expect(dns.count).toBe(200);
    expect(dns.failures).toBe(2);
    expect(dns.state).toBe('failed');
    expect(dns.error).toContain('dial tcp: i/o timeout');
    expect(expand(dns).filter((m) => m.failures > 0)).toHaveLength(2);
  });
});

/** Where the LAST dnsfacts dispatch of an `n`-iteration sweep closes, in seconds. One iteration of
 *  {@link sweep} is 1160 ms wide, and its dnsfacts call closes 460 ms into it. */
function sweepEndT(n: number): number {
  return (1000 + (n - 1) * 1160 + 460) / 1000;
}

test('what collapsing refuses to fold', () => {
  it('a milestone between two loops splits them, because something happened in between', () => {
    const { turns } = readTranscript({
      events: [
        ev(2, 'NexusOperationScheduled', 0, { detail: 'endpoint=kontra-probe-0-1-0' }),
        closes(3, 'NexusOperationCompleted', 100, 0),
        ev(4, 'ActivityTaskScheduled', 200, { detail: 'activityType=closeDataset · taskQueue=kontra-datasets' }),
        closes(5, 'ActivityTaskCompleted', 300, 200),
        ev(6, 'NexusOperationScheduled', 400, { detail: 'endpoint=kontra-probe-0-1-0' }),
        closes(7, 'NexusOperationCompleted', 500, 400),
      ],
    });
    expect(turns.map((t) => t.kind)).toEqual(['dispatch', 'dataset', 'dispatch']);
    expect(turns.map((t) => t.count)).toEqual([1, 1, 1]);
  });

  it('an author\'s sentence splits a loop, because it is what they wrote it for', () => {
    const { turns } = readTranscript({
      events: [
        ev(2, 'NexusOperationScheduled', 0, { detail: 'endpoint=kontra-probe-0-1-0' }),
        closes(3, 'NexusOperationCompleted', 100, 0),
        ev(4, 'MarkerRecorded', 150, { summary: 'first pass done, retrying the drops' }),
        ev(5, 'NexusOperationScheduled', 200, { detail: 'endpoint=kontra-probe-0-1-0' }),
        closes(6, 'NexusOperationCompleted', 300, 200),
      ],
    });
    expect(turns.map((t) => t.kind)).toEqual(['dispatch', 'narration', 'dispatch']);
  });

  it('a Summary that names the Method separates two Methods on one Actor', () => {
    const { turns } = readTranscript({
      events: [
        ev(2, 'ActivityTaskScheduled', 0, { detail: 'activityType=RunBatch · taskQueue=probe-0.1.0', summary: 'head' }),
        closes(3, 'ActivityTaskCompleted', 100, 0),
        ev(4, 'ActivityTaskScheduled', 200, { detail: 'activityType=RunBatch · taskQueue=probe-0.1.0', summary: 'get' }),
        closes(5, 'ActivityTaskCompleted', 300, 200),
        ev(6, 'ActivityTaskScheduled', 400, { detail: 'activityType=RunBatch · taskQueue=probe-0.1.0', summary: 'head' }),
        closes(7, 'ActivityTaskCompleted', 500, 400),
      ],
    });
    // Two groups, not three turns and not one: `head` twice and `get` once.
    expect(turns.map((t) => (t as DispatchTurn).method)).toEqual(['head', 'get']);
    expect(turns.map((t) => t.count)).toEqual([2, 1]);
  });

  // A SUMMARY ON A RAW TURN IS A SENTENCE, AND SENTENCES DO NOT FOLD. The narration rule two cases
  // up already says this for the turns this module names; it has to be true of the ones it does not,
  // because a workflow whose whole account is carried on repeated events of ONE type is a real shape
  // — `cli/warden_workflow.go` puts a Machine's lifecycle on one `ActivityTaskScheduled` per
  // decision, forever.
  //
  // THE ORDERING IS THE SHARPER HALF. A group is anchored at its FIRST member within a barrier-free
  // region, so `started, exited, started` folded the second start up into the first and rendered it
  // ABOVE the exit that came between them: a crash loop, told backwards, as `started ×2`.
  it('never folds two raw events that carry different sentences, or reorders them', () => {
    const watch = (id: number, ms: number, summary: string) =>
      ev(id, 'ActivityTaskScheduled', ms, { detail: 'activityType=wardenWatch', summary });
    const { turns } = readTranscript({
      events: [
        watch(2, 0, 'kontra.machine · worker started · a@1'),
        watch(3, 100, 'kontra.machine · worker exited · a@1'),
        watch(4, 200, 'kontra.machine · worker started · a@1'),
      ],
    });
    expect(turns.map((t) => t.label)).toEqual([
      'kontra.machine · worker started · a@1',
      'kontra.machine · worker exited · a@1',
      'kontra.machine · worker started · a@1',
    ]);
    expect(turns.map((t) => t.count)).toEqual([1, 1, 1]);
  });

  it('still folds raw events that nobody named', () => {
    // The control for the rule above: without a Summary, two events of one type are one line, which
    // is what keeps an unrecognised loop from filling the log.
    const { turns } = readTranscript({
      events: [
        ev(2, 'ActivityTaskScheduled', 0, { detail: 'activityType=whatever' }),
        closes(3, 'ActivityTaskCompleted', 100, 0),
        ev(4, 'ActivityTaskScheduled', 200, { detail: 'activityType=whatever' }),
        closes(5, 'ActivityTaskCompleted', 300, 200),
      ],
    });
    expect(turns).toHaveLength(1);
    expect(turns[0]!.count).toBe(2);
  });

  it('folds all of Temporal\'s bookkeeping into one line however its types alternate', () => {
    const { turns } = readTranscript({
      events: [
        ev(2, 'WorkflowTaskScheduled', 0),
        ev(3, 'WorkflowTaskStarted', 10),
        ev(4, 'WorkflowTaskCompleted', 20),
        ev(5, 'WorkflowTaskScheduled', 30),
        ev(6, 'WorkflowTaskStarted', 40),
        ev(7, 'WorkflowTaskCompleted', 50),
      ],
    });
    expect(turns).toHaveLength(1);
    expect(turns[0]!.count).toBe(6);
    expect(turns[0]!.types).toEqual(['WorkflowTaskScheduled', 'WorkflowTaskStarted', 'WorkflowTaskCompleted']);
  });
});

/* ───────────────────────────── ordering ───────────────────────────── */

test('ordering', () => {
  it('turns come out in the order the events happened', () => {
    const { turns } = readTranscript({ events: sweep(3) });
    const offsets = turns.map((t) => t.t);
    expect([...offsets].sort((a, b) => a - b)).toEqual(offsets);
  });

  it('a turn opens where its opening event is, not where it closed', () => {
    const { turns } = readTranscript({
      events: [
        ev(2, 'NexusOperationScheduled', 1000, { detail: 'endpoint=kontra-probe-0-1-0' }),
        closes(3, 'NexusOperationCompleted', 9000, 1000),
      ],
    });
    expect(turns[0]!.t).toBe(1);
    expect(turns[0]!.endT).toBe(9);
    expect(turns[0]!.dur).toBe(8);
  });

  it('asks are merged into the run\'s own order, not appended to it', () => {
    const { turns } = readTranscript(
      { events: sweep(2) },
      { asks: [{ id: 'a', prompt: 'Approve?', askedAt: BASE + 2000 }], now: BASE + 9000 }
    );
    const at = turns.findIndex((t) => t.kind === 'parked');
    expect(at).toBeGreaterThan(0);
    expect(turns[at - 1]!.at).toBeLessThanOrEqual(turns[at]!.at);
    expect(turns[at + 1]!.at).toBeGreaterThanOrEqual(turns[at]!.at);
  });
});

/* ───────────────────────────── reading the metadata back ───────────────────────────── */

test('reading the metadata back out of the line the reducer built', () => {
  it('splits the reducer\'s own key=value line', () => {
    expect(metaOf('activityType=RunBatch · taskQueue=nscheck-0.1.0')).toEqual({
      activityType: 'RunBatch',
      taskQueue: 'nscheck-0.1.0',
    });
  });

  // A failure message is free text the reducer put on the same line. It must not be mistaken for a
  // key, and a key it does not recognise must not become one either.
  it('reads no key out of a failure message that happens to contain an equals sign', () => {
    expect(metaOf('RunBatch failed: exit status=137 · RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED')).toEqual({});
    expect(metaOf('ActivityTaskCompleted')).toEqual({});
  });

  it('restores an Actor and a version from an endpoint whose dots were collapsed', () => {
    expect(actorOf('kontra-nscheck-0-1-0')).toEqual({ actor: 'nscheck', version: '0.1.0' });
    expect(actorOf('kontra-dns-facts-0-1-0')).toEqual({ actor: 'dns-facts', version: '0.1.0' });
    expect(actorOf('kontra-probe-10-2-0')).toEqual({ actor: 'probe', version: '10.2.0' });
    expect(actorOf('kontra-probe-1-0-0-rc1')).toEqual({ actor: 'probe', version: '1.0.0-rc1' });
  });

  it('yields the whole stem rather than a guessed split when nothing looks like a version', () => {
    expect(actorOf('kontra-probe')).toEqual({ actor: 'probe', version: '' });
    expect(actorOf('something-else')).toEqual({ actor: 'something-else', version: '' });
  });

  it('takes the Method name off the first field of a Summary', () => {
    expect(methodOf('crawl · crawler@0.1.0[acme.com] · 12 units')).toBe('crawl');
    expect(methodOf('extract_links · crawler@0.1.0 · 623 units')).toBe('extract_links');
    // A Summary that is only the Method — all any writer is obliged to put there.
    expect(methodOf('crawl')).toBe('crawl');
  });

  // THE GUARD, and the whole reason a shape test exists rather than "take the first field". Each
  // line below is one a real reader meets, and each would put a confident wrong name on a dispatch.
  it('reads no Method out of a line that does not carry one', () => {
    // What this SDK wrote before the Method name travelled at all.
    expect(methodOf('nscheck@0.1.0 (12 units)')).toBeUndefined();
    // A dispatch that named no Method: the Actor takes the first field, `@` and all.
    expect(methodOf('crawler@0.1.0 · 12 units')).toBeUndefined();
    expect(methodOf('echo@ · 5 units')).toBeUndefined();
    // An author's own sentence, on an event this reader would otherwise call a dispatch.
    expect(methodOf('first pass done, retrying the drops')).toBeUndefined();
    // A field the writer had to shorten says so, and a name that was cut is not a name.
    expect(methodOf('an_extremely_long_method_na… · crawler@0.1.0')).toBeUndefined();
    expect(methodOf('')).toBeUndefined();
    expect(methodOf(undefined)).toBeUndefined();
  });

  it('takes the version off a task queue exactly, because its dots survive there', () => {
    expect(actorOfQueue('nscheck-0.1.0')).toEqual({ actor: 'nscheck', version: '0.1.0' });
    expect(actorOfQueue('dns-facts-0.1.0')).toEqual({ actor: 'dns-facts', version: '0.1.0' });
    expect(actorOfQueue('probe-1.0.0-rc1')).toEqual({ actor: 'probe', version: '1.0.0-rc1' });
    expect(actorOfQueue('kontra-datasets')).toEqual({ actor: 'kontra-datasets', version: '' });
  });
});

/* ───────────────────────────── the Method name, as metadata ───────────────────────────── */

/**
 * A `userMetadata` Summary on one raw event, in the two encodings a history really arrives in.
 *
 * NOT A CONVENIENCE SHAPE. `userMetadata` is a sibling of the attribute bag rather than part of it,
 * and its `summary` is a Payload — so the fixtures below put the reducer through the same two steps
 * production does. `bytes` is what the raw gRPC decode hands `mapHistory` on this controller; `b64`
 * is what a history that travelled as JSON carries, which is what `temporal workflow show -o json`
 * emits and therefore what any future recording is made of.
 *
 * The bytes are a QUOTED JSON string because that is what every SDK's default converter writes.
 */
function summarised(text: string, as: 'bytes' | 'b64' = 'bytes'): Record<string, unknown> {
  const data = new TextEncoder().encode(JSON.stringify(text));
  const encoding = new TextEncoder().encode('json/plain');
  return {
    userMetadata: {
      summary:
        as === 'bytes'
          ? { metadata: { encoding }, data }
          : {
              metadata: { encoding: Buffer.from(encoding).toString('base64') },
              data: Buffer.from(data).toString('base64'),
            },
    },
  };
}

/** The same dispatch the recorded run made, as the Python SDK writes it today. Three events, one
 *  Summary — set on the SCHEDULED event, which is the only one a writer can put metadata on. */
function dispatched(summary: string | undefined, as: 'bytes' | 'b64' = 'bytes'): RawHistoryEvent[] {
  return [
    {
      eventId: '38',
      eventTime: { seconds: 1786831498, nanos: 930265396 },
      nexusOperationScheduledEventAttributes: {
        endpoint: 'kontra-nscheck-0-1-0',
        service: 'kontra.actor',
        operation: 'run',
        workflowTaskCompletedEventId: '37',
      },
      ...(summary === undefined ? {} : summarised(summary, as)),
    },
    {
      eventId: '43',
      eventTime: { seconds: 1786831502, nanos: 409108469 },
      nexusOperationCompletedEventAttributes: { scheduledEventId: '38' },
    },
  ];
}

/** One scheduled-and-completed dispatch of `probe`, at event `id`, naming `method`. */
function call(id: number, method: string): RawHistoryEvent[] {
  return [
    {
      eventId: String(id),
      eventTime: { seconds: 1786831498 + id, nanos: 0 },
      nexusOperationScheduledEventAttributes: { endpoint: 'kontra-probe-0-1-0', operation: 'run' },
      ...summarised(`${method} · probe@0.1.0 · 40 units`),
    },
    {
      eventId: String(id + 1),
      eventTime: { seconds: 1786831499 + id, nanos: 0 },
      nexusOperationCompletedEventAttributes: { scheduledEventId: String(id) },
    },
  ];
}

test('the Method a dispatch called, off the event and not out of the payload', () => {
  // THE WHOLE PATH, which is the only way this is worth asserting: Temporal's own bytes → the
  // reduced log → turns. A test that handed the reader a `summary` string would prove the half of
  // this that was never in doubt.
  it('reads the Method name off the scheduling event\'s own metadata', () => {
    const { turns } = readTranscript(mapHistory(dispatched('crawl · nscheck@0.1.0[acme.com] · 12 units')));
    const dispatch = turns[0] as DispatchTurn;
    expect(dispatch.method).toBe('crawl');
    // And the facts the endpoint already carried are unchanged by it.
    expect(dispatch.actor).toBe('nscheck');
    expect(dispatch.version).toBe('0.1.0');
    expect(dispatch.state).toBe('done');
  });

  it('reads it the same way out of a history that travelled as JSON', () => {
    const { turns } = readTranscript(mapHistory(dispatched('crawl · nscheck@0.1.0 · 12 units', 'b64')));
    expect((turns[0] as DispatchTurn).method).toBe('crawl');
  });

  // NOTHING IS THROWN AWAY BY BEING UNDERSTOOD. The line Temporal's own UI puts on the bar is the
  // line this reader keeps, so an operator comparing the two surfaces sees one sentence, not two.
  it('keeps the whole line the writer wrote, beside the name it read out of it', () => {
    const { turns } = readTranscript(mapHistory(dispatched('crawl · nscheck@0.1.0[acme.com] · 12 units')));
    expect(turns[0]!.label).toBe('crawl · nscheck@0.1.0[acme.com] · 12 units');
  });

  // THE CASE EVERY ARCHIVED RUN IS IN. Absent is an answer; a blank standing in for a name is not.
  it('reads a dispatch with no Summary at all exactly as it always did', () => {
    const { turns } = readTranscript(mapHistory(dispatched(undefined)));
    const dispatch = turns[0] as DispatchTurn;
    expect(dispatch.kind).toBe('dispatch');
    expect(dispatch.actor).toBe('nscheck');
    expect(dispatch.state).toBe('done');
    expect('method' in dispatch).toBe(false);
    expect(dispatch.label).toBeUndefined();
  });

  it('puts no Method on a dispatch whose Summary named none', () => {
    const { turns } = readTranscript(mapHistory(dispatched('nscheck@0.1.0 · 12 units')));
    const dispatch = turns[0] as DispatchTurn;
    expect(dispatch.method).toBeUndefined();
    // The line is still shown. It says something true; it just does not say a Method name.
    expect(dispatch.label).toBe('nscheck@0.1.0 · 12 units');
  });

  // A SUMMARY THE CODEC OFFLOADED IS A REF, NOT A SENTENCE. Unreachable for a dispatch, whose line
  // is built to 200 bytes; reachable for an author who narrates at length. Rendering the ref's JSON
  // as their words would be the confident wrong answer, and fetching it is the fan-out this plane
  // does not do.
  it('reads no summary out of a claim-checked one', () => {
    const raw = dispatched(undefined);
    raw[0]!.userMetadata = {
      summary: {
        metadata: { encoding: new TextEncoder().encode('binary/claim-check-v1') },
        data: new TextEncoder().encode('{"sha256":"deadbeef","size":900000,"meta":{}}'),
      },
    };
    const { turns } = readTranscript(mapHistory(raw));
    expect((turns[0] as DispatchTurn).method).toBeUndefined();
    expect(turns[0]!.label).toBeUndefined();
  });

  // WHAT THE NAME BUYS, and the reason it is worth a byte budget: a sweep that called two Methods
  // of one Actor used to fold into one line, because the endpoint was all there was to fold on.
  it('separates two Methods of one Actor that the endpoint alone could not', () => {
    const events = ['head', 'get', 'head'].flatMap((m, i) => call(100 + i * 2, m));
    const { turns } = readTranscript(mapHistory(events));
    const dispatches = turns.filter((t) => t.kind === 'dispatch') as DispatchTurn[];
    expect(dispatches.map((d) => d.method)).toEqual(['head', 'get']);
    // Two groups, not three turns and not one: `head` twice and `get` once, each expandable.
    expect(dispatches.map((d) => d.count)).toEqual([2, 1]);
  });

  // THE POINT OF THE WHOLE SLICE, stated as an assertion rather than as a comment: the Method name
  // is also inside the dispatch payload, and there is no payload anywhere in this fixture.
  it('gets there with no payload in the history at all', () => {
    const raw = dispatched('crawl · nscheck@0.1.0 · 12 units');
    const json = JSON.stringify(raw);
    expect(json).not.toContain('"input"');
    expect(json).not.toContain('"result"');
    expect(json).not.toContain('$ref');
    expect((readTranscript(mapHistory(raw)).turns[0] as DispatchTurn).method).toBe('crawl');
  });
});

/* ───────────────────────────── an author's own sentence ───────────────────────────── */

/**
 * One narration, as `sdk/python/actorkit/narrate.py` actually writes it.
 *
 * A ZERO-DURATION TIMER AND ITS FIRING, because a timer is the only command a workflow can issue
 * that carries user metadata and runs nothing. Written in Temporal's own raw shape and put through
 * `mapHistory`, so what is asserted below is the path a browser takes rather than a `summary`
 * string handed straight to the reader.
 */
function said(id: number, text: string | undefined, sec: number): RawHistoryEvent[] {
  return [
    {
      eventId: String(id),
      // The same second `call` counts from, so a sentence beside a dispatch has an offset that
      // means what it says.
      eventTime: { seconds: 1786831500 + sec, nanos: 0 },
      timerStartedEventAttributes: { timerId: String(id), startToFireTimeout: { seconds: 0, nanos: 0 } },
      ...(text === undefined ? {} : summarised(text)),
    },
    {
      eventId: String(id + 1),
      eventTime: { seconds: 1786831500 + sec, nanos: 4_000_000 },
      timerFiredEventAttributes: { timerId: String(id), startedEventId: String(id) },
    },
  ];
}

test('narration — the one turn nothing derives', () => {
  it('reads a sentence off the timer that carried it', () => {
    const { turns } = readTranscript(mapHistory(said(2, '119 apexes in scope, 37k subs expected', 0)));
    expect(turns).toHaveLength(1);
    const turn = turns[0]!;
    expect(turn.kind).toBe('narration');
    expect((turn as { text: string }).text).toBe('119 apexes in scope, 37k subs expected');
    // BOTH EVENTS, ONE ROW. The firing is real and stays on the drill path; it is not a second
    // line beside the sentence saying nothing.
    expect(turn.events).toEqual([2, 3]);
    expect(turn.types).toEqual(['TimerStarted', 'TimerFired']);
  });

  // `TimerStarted` ENDS IN `Started`, so the default would leave every sentence in a transcript
  // permanently open — and a run whose author narrated would read as one with unfinished business.
  it('is an instant, never something left open', () => {
    const { turns } = readTranscript(mapHistory(said(2, 'first pass done, retrying the drops', 0)));
    expect(turns[0]!.open).toBe(false);
  });

  // NARRATION IS THE ONLY THING THIS BRANCH TRANSLATES. An author who slept for four hours did it
  // on purpose, and a wait that reads as a sentence would be the confident wrong answer.
  it('leaves a timer that said nothing as the wait it is', () => {
    const { turns } = readTranscript(mapHistory(said(2, undefined, 0)));
    expect(turns.map((t) => t.kind)).toEqual(['raw', 'raw']);
    expect(turns.map((t) => (t as { type: string }).type)).toEqual(['TimerStarted', 'TimerFired']);
  });

  it('lands among the derived turns, at the point in the run where it was written', () => {
    const { turns } = readTranscript(
      mapHistory([...call(2, 'crawl'), ...said(10, 'first pass done, retrying the drops', 60), ...call(20, 'crawl')])
    );
    expect(turns.map((t) => t.kind)).toEqual(['dispatch', 'narration', 'dispatch']);
    // AND IT SPLITS THE LOOP, which is what it was written for: the same Method twice around a
    // sentence is two groups, because something happened in between an operator was meant to read.
    expect(turns.map((t) => t.count)).toEqual([1, 1, 1]);
    expect(turns[1]!.t).toBe(60);
  });

  // A SENTENCE IS PROSE, AND TWO OF THEM ARE TWO SENTENCES. Nothing here folds them: collapsing a
  // narration would hide the words, which is the only thing it carries.
  it('never folds one sentence into another', () => {
    const { turns } = readTranscript(
      mapHistory([...said(2, 'seeding from the paid-programs scope', 0), ...said(10, 'scope resolved, dispatching', 5)])
    );
    expect(turns.map((t) => (t as { text: string }).text)).toEqual([
      'seeding from the paid-programs scope',
      'scope resolved, dispatching',
    ]);
  });

  // THE READER DOES NOT DEPEND ON THE TIMER. What makes a narration is a Summary on an event that
  // is about nothing else, so a sentence written some other way — another SDK, a marker — reads
  // identically. This is the case the Go SDK lands in when it grows the surface.
  it('reads a sentence carried some other way exactly the same', () => {
    const { turns } = readTranscript({
      events: [ev(2, 'MarkerRecorded', 4000, { detail: 'marker=kontra.say', summary: 'scope resolved' })],
    });
    expect(turns[0]!.kind).toBe('narration');
    expect((turns[0] as { text: string }).text).toBe('scope resolved');
  });

  // THE POINT OF PUTTING IT IN HISTORY AT ALL. Temporal holds 24 hours on this controller; the
  // reduced log is what ADR 0025 keeps, and a Summary is part of a reduced event — so the author's
  // own words are still there when the execution that spoke them is gone. Through the real archive
  // rather than a hand-built shape, because JSON is what the object store round-trips.
  it('survives into the archived log, and reads as the same sentence out of it', async () => {
    const archive = new HistoryArchive(new ObjectStore({ backing: new MemoryStore() }));
    const raw = [...call(2, 'crawl'), ...said(10, '119 apexes in scope, 37k subs expected', 60)];
    await archive.write(
      { runId: 'sweep-1786831339', startedAt: BASE, closedAt: BASE + 90_000 },
      mapHistory(raw),
      1786840000000
    );
    const restored = await archive.read('sweep-1786831339');
    const transcript = readTranscript(restored!);
    expect(transcript.archived).toBe(true);
    const narration = transcript.turns.find((t) => t.kind === 'narration')!;
    expect((narration as { text: string }).text).toBe('119 apexes in scope, 37k subs expected');
    expect(narration.open).toBe(false);
  });
});

/* ───────────────────────────── an ask is not a sentence ───────────────────────────── */

/**
 * How an ask spells itself, WRITTEN HERE INDEPENDENTLY of `transcript.ts`'s own copy.
 *
 * That is the house rule for a literal crossing a boundary, and it is what makes these tests worth
 * running: every fixture below is built from these bytes, so a rename inside the reader does not
 * quietly rename the fixture with it — the ask starts reading as a note again and the assertions
 * below say so.
 */
const ASK_MEMO_PREFIX = 'kontra.ask.';
const ASK_TIMER_PREFIX = 'kontra.ask/';

/**
 * The two events a park writes ABOUT ITSELF, as `sdk/python/actorkit/hitl.py` writes them.
 *
 * THE SECOND ONE IS THE BUG. `upsert_memo({'kontra.ask.<id>': …})` is one
 * `WorkflowPropertiesModified`, and the wait that follows carries
 * `timeout_summary='kontra.ask/<id>'` so Temporal's own UI can label the timer bar — which is a
 * Summary on an event about nothing else, the exact shape of a narration. Read as one it becomes a
 * note whose text is an id, drawn under the parked turn that already asks the question properly.
 *
 * Written raw and put through `mapHistory` for the same reason `said` is: what these assert is the
 * path a browser takes, memo keys and user metadata included, not a `summary` string handed
 * straight to the reader.
 */
function asked(id: number, askId: string, sec: number): RawHistoryEvent[] {
  return [
    {
      eventId: String(id),
      eventTime: { seconds: 1786831500 + sec, nanos: 0 },
      workflowPropertiesModifiedEventAttributes: {
        upsertedMemo: { fields: { [`${ASK_MEMO_PREFIX}${askId}`]: { metadata: {}, data: 'BASE64-NOBODY-DECODES' } } },
      },
    },
    {
      eventId: String(id + 1),
      eventTime: { seconds: 1786831500 + sec, nanos: 1_000_000 },
      timerStartedEventAttributes: { timerId: String(id + 1), startToFireTimeout: { seconds: 3600, nanos: 0 } },
      ...summarised(`${ASK_TIMER_PREFIX}${askId}`),
    },
  ];
}

/** The instant `asked` writes its memo at, in epoch ms — what the ask route would report. */
const askedAt = (sec: number): number => (1786831500 + sec) * 1000;

test('an ask is not a sentence', () => {
  const ask: Ask = {
    id: 'approve-1',
    prompt: 'Approve these 12 hosts?',
    askedAt: askedAt(0),
    deadlineAt: askedAt(3600),
  };

  // CROSS-LANGUAGE LITERALS, pinned on both routes. A rename on either side is not a type error:
  // it is an ask that silently reads as a note again, which is what shipped.
  it('spells the ask exactly as the two emitters do', () => {
    const py = readFileSync(join(__dirname, '..', '..', '..', 'sdk', 'python', 'actorkit', 'hitl.py'), 'utf8');
    expect(py).toContain(`ASK_MEMO_PREFIX = "${ASK_MEMO_PREFIX}"`);
    expect(py).toContain(`timeout_summary=f"${ASK_TIMER_PREFIX}{ask_id}"`);
    // And the server's own spelling of the memo prefix, which `hitl.ts` exports for the ask route.
    expect(SERVER_ASK_MEMO_PREFIX).toBe(ASK_MEMO_PREFIX);
  });

  it('draws the park, and no note underneath it', () => {
    const { turns } = readTranscript(mapHistory(asked(2, 'approve-1', 0)), {
      asks: [ask],
      now: askedAt(240),
    });
    // THE DEFECT, ASSERTED DIRECTLY. There is one question here and it is drawn once.
    expect(turns.some((t) => t.kind === 'narration')).toBe(false);
    const park = turns.find((t) => t.kind === 'parked') as ParkedTurn;
    expect(park.ask.prompt).toBe('Approve these 12 hosts?');
    expect(park.pending).toBe(true);
  });

  // NOTHING IS DROPPED. The memo write and the deadline timer are real events and stay in the
  // reading as themselves — untranslated, so the Event log draws them, with their ids intact.
  it('keeps the ask\'s own two events, as themselves', () => {
    const { turns } = readTranscript(mapHistory(asked(2, 'approve-1', 0)), { asks: [ask], now: askedAt(240) });
    const raw = turns.filter((t) => t.kind === 'raw');
    expect(raw.map((t) => (t as { type: string }).type)).toEqual(['WorkflowPropertiesModified', 'TimerStarted']);
    expect(raw.flatMap((t) => t.events)).toEqual([2, 3]);
    // The memo KEY reached the reduced log and its value never did — which is how a reader tells
    // this event is the ask at all.
    expect(raw[0]!.detail).toBe(`memo=${ASK_MEMO_PREFIX}approve-1`);
    expect(raw[0]!.detail).not.toContain('BASE64');
  });

  // THE HALF THAT MUST NOT REGRESS. A real `speak` beside a real ask is still a note with its words
  // — the ask's machinery is what stops being one, not every Summary on a timer.
  it('still reads the author\'s own sentence written beside it', () => {
    const { turns } = readTranscript(
      mapHistory([...said(2, 'first pass done, retrying the drops', 0), ...asked(10, 'approve-1', 1)]),
      { asks: [{ ...ask, askedAt: askedAt(1), deadlineAt: askedAt(3601) }], now: askedAt(240) }
    );
    const notes = turns.filter((t) => t.kind === 'narration');
    expect(notes).toHaveLength(1);
    expect((notes[0] as { text: string }).text).toBe('first pass done, retrying the drops');
    expect(turns.some((t) => t.kind === 'parked')).toBe(true);
  });

  // A run with no ask route to ask — an archived run, a surface with no `asks` — loses the park and
  // must still not gain a note. The memo event is the one thing left saying a question was asked,
  // and reading it as prose would be the confident wrong answer.
  it('never invents a note from an ask nobody handed it', () => {
    const { turns } = readTranscript(mapHistory(asked(2, 'approve-1', 0)));
    expect(turns.map((t) => t.kind)).toEqual(['raw', 'raw']);
  });
});

/* ───────────────────────────── the rule this module lives by ───────────────────────────── */

test('nothing here needs a payload', () => {
  it('produces a full transcript from events that carry no payload at all', () => {
    // Every fixture in this file is payload-free, which is what the reduced log is. This asserts the
    // consequence directly: eight of the nine turn kinds come out of metadata alone, and the ninth
    // (parked) comes from the ask route rather than from a decode.
    const { turns } = readTranscript(
      { events: [...sweep(2), ev(9_000, 'MarkerRecorded', 100_000, { summary: 'done sweeping' })] },
      { input: { dataset: 'targets' }, asks: [{ id: 'a', prompt: 'Approve?', askedAt: BASE + 500 }], now: BASE + 200_000 }
    );
    const kinds = new Set(turns.map((t: Turn) => t.kind));
    for (const kind of ['started', 'dispatch', 'dataset', 'parked', 'narration', 'finished']) {
      expect(kinds.has(kind as Turn['kind'])).toBe(true);
    }
  });
});
