import { describe as test, expect, it } from 'vitest';

import {
  EVENT_CAP,
  HEAD_KEEP,
  attrKey,
  categorize,
  describe as describeEvent,
  eventLink,
  mapHistory,
  typeName,
  type RawHistoryEvent,
} from './history';

/** A raw event at `t` seconds past the epoch, with one attribute bag. */
function ev(id: number, key: string, seconds: number, attrs: Record<string, unknown> = {}): RawHistoryEvent {
  return { eventId: id, eventTime: { seconds, nanos: 0 }, [key]: attrs };
}

test('the type name comes from the attribute key', () => {
  it('reads the key rather than the enum, which differs between transports', () => {
    expect(typeName('activityTaskScheduledEventAttributes')).toBe('ActivityTaskScheduled');
    expect(typeName('workflowExecutionStartedEventAttributes')).toBe('WorkflowExecutionStarted');
  });

  it('finds the one attribute bag among the scalar fields', () => {
    expect(attrKey(ev(1, 'timerStartedEventAttributes', 0))).toBe('timerStartedEventAttributes');
  });

  it('ignores a null attribute bag — a proto decode sets every field, most to null', () => {
    const raw: RawHistoryEvent = {
      eventId: 1,
      eventTime: { seconds: 1, nanos: 0 },
      activityTaskFailedEventAttributes: null,
      timerFiredEventAttributes: { timerId: 'retry' },
    };
    expect(attrKey(raw)).toBe('timerFiredEventAttributes');
  });

  it('skips an event with no attributes at all rather than inventing one', () => {
    expect(mapHistory([{ eventId: 1, eventTime: { seconds: 1, nanos: 0 } }]).events).toEqual([]);
  });
});

/**
 * EIGHT EVENTS OFF A REAL RUN, not eight events somebody imagined.
 *
 * Captured 2026-08-15 with `temporal workflow show -w nscheck-1786831339 -o json` against the local
 * cluster — the 294-second, 305-event NsCheck run the monitoring plane exists because of. The
 * attribute bags are verbatim except that `input`/`result` (claim-checked payloads, ADR 0007) are
 * dropped and the uninteresting middle of the history is not here. The ONE transformation is on
 * `eventTime`: the CLI renders it as RFC3339 and the raw gRPC decode this module actually reads
 * delivers `{seconds, nanos}`, so the recorded instants are written in the shape the reducer sees.
 * `eventId` is left as the CLI's string, which is a third spelling `num()` has to survive.
 *
 * What it holds, and why each is here:
 *   1        the run starts
 *   11–16    ONE child — `kontra-fleet/dns` bringing the fleet up. 156.31 seconds, four opaque rows
 *   38–43    ONE dispatch — a Nexus operation whose token names the backing workflow
 *   296–297  the SAME child workflow id again, a different execution: the teardown
 *   305      the run completes, at +293.947s
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
      namespaceId: '019f6aa0-0d8c-74f9-b989-815eb57de734',
      workflowId: 'kontra-fleet/dns',
      workflowType: { name: 'stackWorkflow' },
      taskQueue: { name: 'kontra-infra', kind: 'TASK_QUEUE_KIND_NORMAL' },
      parentClosePolicy: 'PARENT_CLOSE_POLICY_TERMINATE',
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
      endpointId: '855ba9e5-9061-4574-aa25-47175a9333cc',
    },
  },
  {
    eventId: '39',
    eventTime: { seconds: 1786831498, nanos: 991823074 },
    nexusOperationStartedEventAttributes: {
      scheduledEventId: '38',
      // Both spellings, byte-identical, exactly as the server sent them. base64url of
      // {"t":1,"ns":"default","wid":"actor-nscheck-nscheck-1786831339-nscheck-60c3bfac"}.
      operationId:
        'eyJ0IjoxLCJucyI6ImRlZmF1bHQiLCJ3aWQiOiJhY3Rvci1uc2NoZWNrLW5zY2hlY2stMTc4NjgzMTMzOS1uc2NoZWNrLTYwYzNiZmFjIn0',
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

/** The one event of the recorded run with this id. */
function recorded(id: number): (typeof RECORDED)[number] {
  const found = RECORDED.find((e) => Number(e.eventId) === id);
  if (!found) throw new Error(`no recorded event ${id}`);
  return found;
}

/** Its attribute bag, whichever `…EventAttributes` field carries it. */
function bagOf(id: number): Record<string, unknown> {
  const ev = recorded(id);
  return ev[attrKey(ev)!] as Record<string, unknown>;
}

test('the id a row drills into', () => {
  it('reads the child workflow id the parent ASKED for, before any execution exists', () => {
    const link = eventLink('StartChildWorkflowExecutionInitiated', bagOf(11));
    expect(link).toEqual({
      workflowId: 'kontra-fleet/dns',
      type: 'stackWorkflow',
      via: 'child',
      namespace: 'default',
    });
    // No exec id, and none invented: Temporal has not started the child at this event.
    expect(link?.execId).toBeUndefined();
  });

  it('reads the execution the child actually got, not just its id', () => {
    expect(eventLink('ChildWorkflowExecutionStarted', bagOf(12))).toEqual({
      workflowId: 'kontra-fleet/dns',
      execId: '01a00772-8296-7c0e-ae18-7aaca6721266',
      type: 'stackWorkflow',
      via: 'child',
      namespace: 'default',
    });
  });

  // THE REASON THE EXEC ID IS CARRIED AT ALL. Both fleet children of this run are `kontra-fleet/dns`
  // — asking Temporal for that id alone answers with the teardown, so drilling into the 156-second
  // bring-up would show you the 29-second destroy.
  it('separates two executions that share one workflow id', () => {
    expect(eventLink('ChildWorkflowExecutionStarted', bagOf(12))?.execId).not.toBe(
      eventLink('ChildWorkflowExecutionStarted', bagOf(297))?.execId
    );
  });

  it('decodes the workflow a Nexus dispatch started out of its operation token', () => {
    expect(eventLink('NexusOperationStarted', bagOf(39))).toEqual({
      workflowId: 'actor-nscheck-nscheck-1786831339-nscheck-60c3bfac',
      via: 'nexus',
      namespace: 'default',
    });
  });

  it('still decodes a server that sends only the deprecated operationId', () => {
    const { operationId } = bagOf(39);
    expect(eventLink('NexusOperationStarted', { operationId })?.workflowId).toBe(
      'actor-nscheck-nscheck-1786831339-nscheck-60c3bfac'
    );
  });

  // An operation whose handler is not a workflow has no workflow to drill into, and a token this
  // module does not understand is exactly that case. Nothing is guessed.
  it('links nothing for a token it cannot read', () => {
    expect(eventLink('NexusOperationStarted', { operationToken: 'not-base64-json' })).toBeUndefined();
    expect(eventLink('NexusOperationStarted', { operationToken: btoa('{"t":9,"ns":"x"}') })).toBeUndefined();
    expect(eventLink('NexusOperationStarted', {})).toBeUndefined();
  });

  it('links nothing for an event that is not about another workflow', () => {
    expect(eventLink('ActivityTaskScheduled', bagOf(1))).toBeUndefined();
    expect(eventLink('WorkflowExecutionStarted', bagOf(1))).toBeUndefined();
  });
});

test('the recorded run, reduced', () => {
  const { events } = mapHistory(RECORDED);
  const byId = new Map(events.map((e) => [e.id, e]));

  it('measures the provisioning window this whole plane exists because of', () => {
    // 156.31 of the run's 293.947 seconds, in ONE child. Every number here is off the recording.
    expect(byId.get(16)!.dur).toBeCloseTo(156.31, 2);
    expect(byId.get(305)!.t).toBeCloseTo(293.947, 3);
  });

  // THE TRAP THIS CLOSES. The initiated event names only the id it asked for, and that id is used
  // twice in this run. Clicking it must open THIS child, not the teardown that reused the name.
  it('pins every row of a child to the execution that row is about', () => {
    for (const id of [11, 12, 16]) {
      expect(byId.get(id)!.link?.workflowId).toBe('kontra-fleet/dns');
      expect(byId.get(id)!.link?.execId).toBe('01a00772-8296-7c0e-ae18-7aaca6721266');
    }
    for (const id of [296, 297]) {
      expect(byId.get(id)!.link?.execId).toBe('01a00776-89dc-75da-a0a2-97d86a09a2a3');
    }
  });

  // THE PROPAGATION. Only event 39 carries the token; 38 does not know the workflow id yet and 43
  // no longer repeats it. An operator clicking any row of a dispatch means the same dispatch.
  it('carries a dispatch link across all three of its events', () => {
    for (const id of [38, 39, 43]) {
      expect(byId.get(id)!.link).toEqual({
        workflowId: 'actor-nscheck-nscheck-1786831339-nscheck-60c3bfac',
        via: 'nexus',
        namespace: 'default',
      });
    }
  });

  it('names the actor AND its version on the dispatch row, from metadata only', () => {
    expect(byId.get(38)!.detail).toBe('endpoint=kontra-nscheck-0-1-0 · nexus=kontra.actor/run');
  });

  it('files a dispatch beside the child that provisioned for it, not beside the run itself', () => {
    expect(byId.get(38)!.cat).toBe('child');
    expect(byId.get(12)!.cat).toBe('child');
    expect(byId.get(1)!.cat).toBe('workflow');
  });

  it('leaves every other row unlinked, so what is navigable is exactly what is', () => {
    expect(events.filter((e) => e.link).map((e) => e.id)).toEqual([11, 12, 16, 38, 39, 43, 296, 297]);
  });

  it('measures the dispatch back to when it was SCHEDULED, not to when it started', () => {
    expect(byId.get(43)!.dur).toBeCloseTo(3.479, 3); // 43 → 38, across the started event
  });
});

test('links that must not be followed', () => {
  const activity = [
    ev(5, 'activityTaskScheduledEventAttributes', 1000, { activityType: { name: 'RunBatch' } }),
    ev(6, 'activityTaskCompletedEventAttributes', 1001, { scheduledEventId: 5 }),
  ];

  // Activities share the `scheduledEventId` back-reference with dispatches. Inheriting on that alone
  // would hang a drill-through off every completed Batch, pointing at nothing.
  it('does not let an activity inherit a link from its own family', () => {
    expect(mapHistory(activity).events.every((e) => e.link === undefined)).toBe(true);
  });

  it('refuses a link out of the namespace it was read from, and says where it went', () => {
    const foreign = [
      ev(1, 'nexusOperationStartedEventAttributes', 1000, {
        scheduledEventId: 1,
        // {"t":1,"ns":"tenant-b","wid":"actor-elsewhere"}
        operationToken: Buffer.from('{"t":1,"ns":"tenant-b","wid":"actor-elsewhere"}').toString(
          'base64url'
        ),
      }),
    ];
    const [event] = mapHistory(foreign, false, 'default').events;
    expect(event!.link).toBeUndefined();
    expect(event!.detail).toContain('elsewhere in namespace=tenant-b');
    // Same events read from THAT namespace are drillable — the refusal is about reach, not shape.
    expect(mapHistory(foreign, false, 'tenant-b').events[0]!.link?.workflowId).toBe(
      'actor-elsewhere'
    );
  });
});

test('categories', () => {
  it('puts each layer where it belongs', () => {
    expect(categorize('WorkflowExecutionStarted')).toBe('workflow');
    expect(categorize('WorkflowTaskScheduled')).toBe('task');
    expect(categorize('ActivityTaskStarted')).toBe('activity');
    expect(categorize('TimerStarted')).toBe('timer');
    expect(categorize('MarkerRecorded')).toBe('marker');
    expect(categorize('UpsertWorkflowSearchAttributes')).toBe('marker');
    expect(categorize('ChildWorkflowExecutionStarted')).toBe('child');
    expect(categorize('WorkflowExecutionSignaled')).toBe('signal');
  });

  // THE POINT OF THE FILTER. "Is anything wrong" must be one click, and an ActivityTaskFailed
  // filed under `activity` because that is its structural layer would defeat exactly that.
  it('files every failure under failure, whatever layer produced it', () => {
    expect(categorize('ActivityTaskFailed')).toBe('failure');
    expect(categorize('ActivityTaskTimedOut')).toBe('failure');
    expect(categorize('WorkflowTaskTimedOut')).toBe('failure');
    expect(categorize('WorkflowExecutionFailed')).toBe('failure');
    expect(categorize('WorkflowExecutionTerminated')).toBe('failure');
    expect(categorize('ChildWorkflowExecutionFailed')).toBe('failure');
  });

  it('leaves a cancel REQUEST on its own layer — that is an operator, not a fault', () => {
    expect(categorize('ActivityTaskCancelRequested')).toBe('activity');
    expect(categorize('WorkflowExecutionCancelRequested')).toBe('workflow');
  });

  /**
   * A CANCELLED TIMER IS THE HAPPY PATH OF `ask`, AND IT USED TO READ AS A FAULT.
   *
   * Every other Canceled event names output that will never arrive. A timer names the clock, and
   * cancelling it is how a race between "wait for the deadline" and "it happened first" ends when
   * the second one wins — which is what answering a question does to the ask's deadline timer.
   * MEASURED on `canary-1787842278`: one `TimerCanceled`, the only failure among 140 events, on a
   * run whose local and fleet halves agreed. It cost the run its verdict.
   */
  it('does not call a cancelled WAIT a failure — a timer is not work', () => {
    expect(categorize('TimerCanceled')).toBe('timer');
    // The work-shaped cancels are untouched: those really are output that is now missing.
    expect(categorize('ActivityTaskCanceled')).toBe('failure');
    expect(categorize('ChildWorkflowExecutionCanceled')).toBe('failure');
    expect(categorize('WorkflowExecutionCanceled')).toBe('failure');
  });
});

/**
 * WHAT A ROW IS ABOUT, ON THE ROWS THAT DO NOT SAY IT.
 *
 * Temporal writes the name once, on the event that SCHEDULES the work; the two that follow carry a
 * back-reference and an identity. So two rows in every three say somebody picked something up and
 * finished it without ever saying what — and the three families spell the back-reference three
 * different ways, which is why this follows the chain instead of pairing by type.
 */
test('the name travels to the events that close the work', () => {
  it('names the Method on the rows that only carried an identity', () => {
    const events = mapHistory([
      ev(5, 'activityTaskScheduledEventAttributes', 1000, {
        activityType: { name: 'resolveBatch' },
        taskQueue: { name: 'kontra-infra' },
      }),
      ev(6, 'activityTaskStartedEventAttributes', 1001, {
        scheduledEventId: 5,
        identity: '1@13c18db2fb13',
      }),
      // Two hops from the name: Completed points at Started, which points at Scheduled.
      ev(7, 'activityTaskCompletedEventAttributes', 1002, {
        scheduledEventId: 5,
        startedEventId: 6,
        identity: '1@13c18db2fb13',
      }),
    ]).events;
    expect(events[1]!.detail).toBe('activityType=resolveBatch · identity=1@13c18db2fb13');
    expect(events[2]!.detail).toBe('activityType=resolveBatch · identity=1@13c18db2fb13');
  });

  it('names the Actor and version on a dispatch that closed', () => {
    const events = mapHistory([
      ev(38, 'nexusOperationScheduledEventAttributes', 1000, {
        endpoint: 'kontra-nscheck-0-1-0',
        service: 'kontra.actor',
        operation: 'run',
      }),
      ev(43, 'nexusOperationCompletedEventAttributes', 1004, { scheduledEventId: 38 }),
    ]).events;
    // It said its own type and nothing else before: the row for a finished dispatch named neither
    // the Actor nor the version, on the one surface that claims to show everything.
    expect(events[1]!.detail).toBe('endpoint=kontra-nscheck-0-1-0');
  });

  it('does not repeat a name the row already carries', () => {
    const events = mapHistory([
      ev(11, 'startChildWorkflowExecutionInitiatedEventAttributes', 1000, {
        workflowId: 'kontra-fleet/nscheck-0.1.0',
        workflowType: { name: 'stackWorkflow' },
      }),
      ev(12, 'childWorkflowExecutionCompletedEventAttributes', 1160, {
        initiatedEventId: 11,
        workflowType: { name: 'stackWorkflow' },
        workflowExecution: { workflowId: 'kontra-fleet/nscheck-0.1.0' },
      }),
    ]).events;
    expect(events[1]!.detail).toBe('workflowType=stackWorkflow');
  });

  it('picks up nothing from an opener that named nothing — a workflow task stays as it was', () => {
    const events = mapHistory([
      ev(2, 'workflowTaskScheduledEventAttributes', 1000, { taskQueue: { name: '2139228' } }),
      ev(3, 'workflowTaskStartedEventAttributes', 1000, {
        scheduledEventId: 2,
        identity: '2139228@main-droplet',
      }),
    ]).events;
    // `taskQueue` is deliberately not a subject: it is on every event of the family already, and a
    // column printing one sticky-queue id down forty rows names nothing.
    expect(events[1]!.detail).toBe('identity=2139228@main-droplet');
  });
});

test('detail lines', () => {
  it('names the method and the queue — the pair that explains a stuck dispatch', () => {
    const line = describeEvent('ActivityTaskScheduled', {
      activityType: { name: 'RunBatch' },
      taskQueue: { name: 'nscheck-0.1.0' },
    });
    expect(line).toBe('activityType=RunBatch · taskQueue=nscheck-0.1.0');
  });

  it('names who picked the work up', () => {
    expect(describeEvent('ActivityTaskStarted', { identity: 'kf-node-1@worker' })).toContain(
      'identity=kf-node-1@worker'
    );
  });

  it('carries the failure message Temporal already stringified, without touching a payload', () => {
    const line = describeEvent('ActivityTaskFailed', {
      failure: { message: 'RunBatch failed', cause: { message: 'dial tcp: i/o timeout' } },
      retryState: 'RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED',
    });
    expect(line).toBe('RunBatch failed: dial tcp: i/o timeout · RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED');
  });

  it('does not repeat a cause identical to its own message', () => {
    expect(describeEvent('WorkflowExecutionFailed', { failure: { message: 'boom', cause: { message: 'boom' } } })).toBe(
      'boom'
    );
  });

  it('says how long a retry timer waits', () => {
    expect(describeEvent('TimerStarted', { timerId: '3', startToFireTimeout: { seconds: 2, nanos: 0 } })).toBe(
      'timerId=3 · fires in 2s'
    );
  });

  it('falls back to the type rather than to an empty cell', () => {
    expect(describeEvent('WorkflowTaskCompleted', {})).toBe('WorkflowTaskCompleted');
  });
});

test('mapHistory', () => {
  const raw = [
    ev(1, 'workflowExecutionStartedEventAttributes', 1000, {
      workflowType: { name: 'NsCheck' },
      taskQueue: { name: 'recon' },
    }),
    ev(2, 'activityTaskScheduledEventAttributes', 1002, {
      activityType: { name: 'RunBatch' },
      taskQueue: { name: 'nscheck-0.1.0' },
    }),
    ev(3, 'activityTaskStartedEventAttributes', 1003, { scheduledEventId: 2, attempt: 2 }),
    ev(4, 'activityTaskCompletedEventAttributes', 1007, { startedEventId: 3, scheduledEventId: 2 }),
  ];

  it('offsets every event from the FIRST one, which is what +4.000s means', () => {
    const { events } = mapHistory(raw);
    expect(events.map((e) => e.t)).toEqual([0, 2, 3, 7]);
  });

  it('keeps the wall clock too, so an offset can be read as a time of day', () => {
    expect(mapHistory(raw).events[0]!.at).toBe(1_000_000);
  });

  it('measures a closing event back to the event that opened it', () => {
    const { events } = mapHistory(raw);
    expect(events[3]!.dur).toBe(4); // started at +3, completed at +7
  });

  it('reports the attempt, so a retry is visible without reading the failure', () => {
    expect(mapHistory(raw).events[2]!.attempt).toBe(2);
    expect(mapHistory(raw).events[0]!.attempt).toBe(1); // no counter on the event ⇒ first
  });

  it('leaves dur at zero when the opener is outside the window', () => {
    const orphan = [ev(9000, 'activityTaskCompletedEventAttributes', 5000, { startedEventId: 8999 })];
    expect(mapHistory(orphan).events[0]!.dur).toBe(0);
  });

  it('is empty, not broken, for a history with no events yet', () => {
    expect(mapHistory([])).toEqual({ events: [], scanned: 0, elided: 0, truncated: false });
  });
});

test('the cap', () => {
  const long = Array.from({ length: EVENT_CAP + 500 }, (_, i) =>
    ev(i + 1, 'activityTaskScheduledEventAttributes', 1000 + i, { activityType: { name: 'RunBatch' } })
  );

  it('keeps the head AND the tail — the middle of a long sweep repeats', () => {
    const { events } = mapHistory(long);
    expect(events).toHaveLength(EVENT_CAP);
    expect(events[0]!.id).toBe(1);
    expect(events[HEAD_KEEP - 1]!.id).toBe(HEAD_KEEP);
    expect(events[events.length - 1]!.id).toBe(long.length);
  });

  // A log with a hole nobody mentions is worse than no log: it reads as a complete history in
  // which nothing happened between event 25 and the end.
  it('says how many it dropped', () => {
    const got = mapHistory(long);
    expect(got.elided).toBe(500);
    expect(got.scanned).toBe(EVENT_CAP + 500);
  });

  it('does not elide anything at exactly the cap', () => {
    expect(mapHistory(long.slice(0, EVENT_CAP)).elided).toBe(0);
  });

  it('passes truncation through — we stopped paging, which is a different fact from eliding', () => {
    expect(mapHistory(long.slice(0, 3), true).truncated).toBe(true);
  });
});

/**
 * `scanned` IS WHAT WAS READ; `historyLength` IS WHAT THERE IS — issue F3.
 *
 * The reader stops at 20,000 events and sets `truncated`, and `scanned` then reported the CAP as if
 * it were the count. Any run between 20,001 and Temporal's 51,200 ceiling was recorded as exactly
 * 20,000 — an undercount of up to 61%, invisible because the number looks plausible and is only
 * wrong for the largest runs.
 *
 * EVERY CASE HERE ASSERTS THE TWO NUMBERS DISAGREE, and says which is which. "The count is right"
 * passes if both are wrong together, which is precisely the state this was in.
 */
test('what was read, and what there is', () => {
  const read = Array.from({ length: 3 }, (_, i) =>
    ev(i + 1, 'activityTaskScheduledEventAttributes', 1000 + i, { activityType: { name: 'RunBatch' } })
  );

  it('carries the server’s length beside the reader’s count, and they differ', () => {
    const got = mapHistory(read, true, '', { historyLength: 31_402, historySizeBytes: 9_001_234 });
    expect(got.scanned).toBe(3);
    expect(got.historyLength).toBe(31_402);
    expect(got.historySizeBytes).toBe(9_001_234);
    // THE ASSERTION THAT MATTERS. A reader that quietly reused `scanned` for both would pass every
    // "is the length right" check ever written against a history shorter than the cap.
    expect(got.historyLength).not.toBe(got.scanned);
  });

  it('is ABSENT, not zero, when nobody asked the server', () => {
    // An archived history is a recording and a fixture has no server behind it. `0` would be a
    // claim that the run had no events, which is the one thing it cannot mean.
    const got = mapHistory(read, true);
    expect('historyLength' in got).toBe(false);
    expect(got.historyLength).toBeUndefined();
    expect(got.scanned).toBe(3);
  });

  it('is absent per field — a describe that answered one and not the other says so', () => {
    const got = mapHistory(read, false, '', { historyLength: 12 });
    expect(got.historyLength).toBe(12);
    expect('historySizeBytes' in got).toBe(false);
  });

  it('carries through the ELIDING return too, which is the long-run path', () => {
    // Two return sites, and the one that matters for a big run is the one that drops the middle.
    // A field added to only the short path would be absent on exactly the runs it exists for.
    const long = Array.from({ length: EVENT_CAP + 500 }, (_, i) =>
      ev(i + 1, 'activityTaskScheduledEventAttributes', 1000 + i, { activityType: { name: 'RunBatch' } })
    );
    const got = mapHistory(long, true, '', { historyLength: 48_000 });
    expect(got.elided).toBe(500);
    expect(got.scanned).toBe(EVENT_CAP + 500);
    expect(got.historyLength).toBe(48_000);
  });

  it('leaves the display path exactly as it was', () => {
    // The elision is honest and is not what was wrong: head, tail and `elided` are the reader's
    // contract with the screen. Only the TOTAL was wrong.
    const long = Array.from({ length: EVENT_CAP + 500 }, (_, i) =>
      ev(i + 1, 'activityTaskScheduledEventAttributes', 1000 + i, { activityType: { name: 'RunBatch' } })
    );
    const without = mapHistory(long, true);
    const with_ = mapHistory(long, true, '', { historyLength: 48_000 });
    expect(with_.events).toEqual(without.events);
    expect(with_.elided).toBe(without.elided);
    expect(with_.truncated).toBe(without.truncated);
  });
});

test('protobufjs Longs', () => {
  // `seconds` and `eventId` arrive as Longs from the raw gRPC decode, not as numbers. Reading
  // them with a bare Number() would yield NaN and every offset would render as "+NaNs".
  it('reads a Long-shaped seconds and eventId', () => {
    const long = (n: number) => ({ toString: () => String(n) });
    const raw: RawHistoryEvent = {
      eventId: long(7),
      eventTime: { seconds: long(1_700_000_000), nanos: 500_000_000 },
      timerFiredEventAttributes: { timerId: '1' },
    };
    const { events } = mapHistory([raw]);
    expect(events[0]!.id).toBe(7);
    expect(events[0]!.at).toBe(1_700_000_000_500);
  });
});
