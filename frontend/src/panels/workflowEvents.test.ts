import { describe, expect, it } from 'vitest';

import type { RunEvent } from '../run/api';
import {
  countByCategory,
  eventAside,
  eventName,
  eventPayload,
  eventStats,
  eventSummary,
  filterEvents,
  fmtClock,
  fmtEventDuration,
  fmtOffset,
  withoutBookkeeping,
} from './workflowEvents';

function ev(over: Partial<RunEvent> = {}): RunEvent {
  return {
    id: 1,
    type: 'ActivityTaskScheduled',
    cat: 'activity',
    t: 0,
    at: 0,
    detail: '',
    attempt: 1,
    dur: 0,
    ...over,
  };
}

describe('eventName', () => {
  // The activity type is what an operator is scanning for. It wins whenever it is there.
  it('prefers the activity type', () => {
    expect(
      eventName(ev({ detail: 'activityType=RunBatch · taskQueue=nscheck-0.1.0' }))
    ).toBe('RunBatch');
  });

  it('falls back to the workflow type, then to the queue', () => {
    expect(eventName(ev({ detail: 'workflowType=NsCheck · taskQueue=recon' }))).toBe('NsCheck');
    expect(eventName(ev({ detail: 'taskQueue=recon' }))).toBe('recon');
  });

  // A dispatch's endpoint names the actor AND its version; the `nexus=` pair beside it is the same
  // string on every dispatch kontra ever makes.
  it('names the endpoint on a Nexus dispatch, which has no activity type and no queue', () => {
    expect(
      eventName(ev({ detail: 'endpoint=kontra-nscheck-0-1-0 · nexus=kontra.actor/run' }))
    ).toBe('kontra-nscheck-0-1-0');
  });

  it('shows the raw detail when it names nothing structured', () => {
    expect(eventName(ev({ detail: 'ApplicationError: upstream 503' }))).toBe(
      'ApplicationError: upstream 503'
    );
  });

  it('trims a long detail rather than letting it wrap the row', () => {
    const name = eventName(ev({ detail: 'x'.repeat(200) }));
    expect(name).toHaveLength(72);
    expect(name.endsWith('…')).toBe(true);
  });

  // The server falls back to the type when it has no metadata; repeating it beside the type would
  // print the same word twice on one row.
  it('is empty when the detail is just the type again', () => {
    expect(eventName(ev({ type: 'WorkflowTaskCompleted', detail: 'WorkflowTaskCompleted' }))).toBe('');
  });
});

/**
 * The Summary, which is where the meaning was deliberately put.
 *
 * MEASURED ON REAL RUNS, both of these. A `speak` writes its sentence as the Summary of a
 * zero-duration timer, so a log that reads only `detail` prints `timerId=1` and drops the words; a
 * dispatch writes `<method> · <actor>@<ver> · <n> units` on its scheduling event (issue 03) so that
 * the Method is readable without opening a claim-checked payload, and a log that reads only `detail`
 * names the endpoint and never the Method. Same defect twice: the field a human chose the words of
 * is the field that was not drawn.
 */
describe('the Summary on a row', () => {
  const SPOKEN = 'sweeping 12 hosts for an open resolver';

  it('wins the name column over every reading of the machinery', () => {
    expect(eventName(ev({ type: 'TimerStarted', detail: 'timerId=1', summary: SPOKEN }))).toBe(SPOKEN);
    // Even against the activity type, which is the strongest of the guesses below it: a Summary is
    // the line somebody chose, and the rest of `eventName` is inference about what they'd want.
    expect(
      eventName(ev({ detail: 'activityType=RunBatch · taskQueue=probe-0.1.0', summary: 'head · probe@0.1.0 · 12 units' }))
    ).toBe('head · probe@0.1.0 · 12 units');
  });

  it('leaves an event with no Summary reading exactly as it did', () => {
    expect(eventSummary(ev({ detail: 'timerId=1' }))).toBe('');
    expect(eventName(ev({ type: 'TimerStarted', detail: 'timerId=1' }))).toBe('timerId=1');
    expect(eventAside(ev({ detail: 'timerId=1' }))).toBe('');
  });

  // A Summary is added IN FRONT OF the metadata, never in place of it: `endpoint=` is still the
  // fact that says which actor and which version, and it is what explains a dispatch sitting
  // forever on a queue nobody polls.
  it('hands the metadata it displaced to the space beside it', () => {
    const dispatch = ev({
      type: 'NexusOperationScheduled',
      detail: 'endpoint=kontra-probe-0-1-0 · nexus=kontra.actor/run',
      summary: 'head · probe@0.1.0 · 12 units',
    });
    expect(eventName(dispatch)).toBe('head · probe@0.1.0 · 12 units');
    expect(eventAside(dispatch)).toBe('endpoint=kontra-probe-0-1-0 · nexus=kontra.actor/run');
  });

  it('says nothing beside a Summary when there would be nothing to add', () => {
    // The server falls back to spelling the type when an event carries no metadata of its own.
    expect(eventAside(ev({ type: 'TimerFired', detail: 'TimerFired', summary: SPOKEN }))).toBe('');
    // …and a detail the Summary already repeats is one string, printed once.
    expect(eventAside(ev({ detail: SPOKEN, summary: SPOKEN }))).toBe('');
  });

  it('ignores a Summary that is only whitespace, rather than blanking the row for it', () => {
    expect(eventSummary(ev({ summary: '   ' }))).toBe('');
    expect(eventName(ev({ detail: 'taskQueue=recon', summary: '  ' }))).toBe('recon');
  });
});

describe('the filter chips', () => {
  const events = [
    ev({ id: 1, cat: 'workflow' }),
    ev({ id: 2, cat: 'activity' }),
    ev({ id: 3, cat: 'activity' }),
    ev({ id: 4, cat: 'failure' }),
    ev({ id: 5, cat: 'timer' }),
  ];

  it('counts per category, with all as the total', () => {
    const counts = countByCategory(events);
    expect(counts.all).toBe(5);
    expect(counts.activity).toBe(2);
    expect(counts.failure).toBe(1);
    expect(counts.task).toBe(0);
  });

  // The drillable set gets its own chip, because a run's children are where the minutes it looked
  // idle actually went — and under `all` they are four rows in three hundred.
  it('counts children, which is child workflows and Nexus dispatches together', () => {
    const counts = countByCategory([...events, ev({ id: 6, cat: 'child' }), ev({ id: 7, cat: 'child' })]);
    expect(counts.child).toBe(2);
    expect(filterEvents([...events, ev({ id: 6, cat: 'child' })], 'child').map((e) => e.id)).toEqual([6]);
  });

  it('filters to one category, and all is everything', () => {
    expect(filterEvents(events, 'activity').map((e) => e.id)).toEqual([2, 3]);
    expect(filterEvents(events, 'all')).toHaveLength(5);
  });

  it('does not alias the input on the all path', () => {
    const out = filterEvents(events, 'all');
    out.pop();
    expect(events).toHaveLength(5);
  });
});

describe('formatting', () => {
  it('keeps milliseconds in the offset — a fast queue finishes several inside one second', () => {
    expect(fmtOffset(4.2134)).toBe('+4.213s');
    expect(fmtOffset(0)).toBe('+0.000s');
  });

  it('prints a wall clock with milliseconds', () => {
    const at = new Date(2026, 0, 2, 14, 7, 32, 104).getTime();
    expect(fmtClock(at)).toBe('14:07:32.104');
  });

  it('prints no clock at all for an event with no timestamp', () => {
    expect(fmtClock(0)).toBe('');
  });

  it('drops a decimal once a duration is long enough not to need it', () => {
    expect(fmtEventDuration(1.238)).toBe('1.24s');
    expect(fmtEventDuration(42.31)).toBe('42.3s');
    expect(fmtEventDuration(0)).toBe('');
  });
});

describe('eventStats', () => {
  const events = [
    ev({ id: 1, type: 'WorkflowExecutionStarted', cat: 'workflow' }),
    ev({ id: 2, type: 'ActivityTaskScheduled' }),
    ev({ id: 3, type: 'ActivityTaskStarted' }),
    ev({ id: 4, type: 'ActivityTaskFailed', cat: 'failure' }),
    ev({ id: 5, type: 'ActivityTaskScheduled', attempt: 2 }),
    ev({ id: 6, type: 'ActivityTaskStarted', attempt: 2 }),
    ev({ id: 7, type: 'ActivityTaskCompleted', attempt: 2 }),
  ];

  // Counting COMPLETIONS would report zero for a run stuck against a queue nobody polls, which is
  // the state the number is most needed for.
  it('counts activities as scheduled, not as finished', () => {
    expect(eventStats(events).activities).toBe(2);
  });

  it('counts a retry once, on the start — not on every event of the attempt', () => {
    expect(eventStats(events).retries).toBe(1);
  });

  it('keeps failures separate from retries', () => {
    expect(eventStats(events).failures).toBe(1);
  });

  it('is all zeroes for an empty log', () => {
    expect(eventStats([])).toEqual({ activities: 0, retries: 0, failures: 0 });
  });
});

describe('eventPayload carries both lines', () => {
  // THE ESCAPE HATCH HAS TO HOLD BOTH. The row in front of it shows the Summary where there is
  // one, so an expanded card that carried only `detail` would be missing the half a reader just
  // came from — and one that carried only the Summary would have dropped the machinery.
  it('holds the Summary and the metadata it displaced', () => {
    const parsed = JSON.parse(
      eventPayload(
        ev({
          type: 'NexusOperationScheduled',
          detail: 'endpoint=kontra-probe-0-1-0 · nexus=kontra.actor/run',
          summary: 'head · probe@0.1.0 · 12 units',
        })
      )
    ) as Record<string, unknown>;
    expect(parsed.summary).toBe('head · probe@0.1.0 · 12 units');
    expect(parsed.detail).toBe('endpoint=kontra-probe-0-1-0 · nexus=kontra.actor/run');
  });
});

describe('eventPayload', () => {
  it('carries the whole record, so a question the summary did not anticipate is answerable', () => {
    const parsed = JSON.parse(
      eventPayload(ev({ id: 9, detail: 'activityType=RunBatch', dur: 1.5, at: 1_700_000_000_000 }))
    ) as Record<string, unknown>;
    expect(parsed.eventId).toBe(9);
    expect(parsed.detail).toBe('activityType=RunBatch');
    expect(parsed.duration).toBe('1.50s');
  });

  it('omits a duration and a timestamp it does not have, rather than printing zero', () => {
    const parsed = JSON.parse(eventPayload(ev())) as Record<string, unknown>;
    expect('duration' in parsed).toBe(false);
    expect('at' in parsed).toBe(false);
  });
});


/**
 * THE PAPERWORK, HELD BACK — the difference between a log of a run and a log of Temporal.
 *
 * MEASURED on `canary-1787842278`: 81 of 140 events are the workflow-task loop, three per decision
 * the workflow made, and a 40-row tail spent 30 rows on them. The run underneath had provisioned a
 * Machine, dispatched two halves of a sweep and parked on a question, and none of that was on
 * screen.
 */
describe('withoutBookkeeping', () => {
  it('holds back the workflow-task loop and says how many', () => {
    const events = [
      ev({ id: 1, type: 'WorkflowExecutionStarted', cat: 'workflow' }),
      ev({ id: 2, type: 'WorkflowTaskScheduled', cat: 'task' }),
      ev({ id: 3, type: 'WorkflowTaskStarted', cat: 'task' }),
      ev({ id: 4, type: 'WorkflowTaskCompleted', cat: 'task' }),
      ev({ id: 5, type: 'NexusOperationScheduled', cat: 'child' }),
    ];
    const { rows, held } = withoutBookkeeping(events);
    expect(held).toBe(3);
    expect(rows.map((e) => e.id)).toEqual([1, 5]);
  });

  // THE ONE WORKFLOW-TASK EVENT WORTH READING NEVER LEAVES. A workflow task that timed out is a
  // worker that stopped answering, which is the failure this whole log exists to make visible —
  // and it categorizes as `failure`, not `task`, so it survives on its own merit rather than by an
  // exception written here.
  it('keeps a workflow task that failed or timed out', () => {
    const events = [
      ev({ id: 1, type: 'WorkflowTaskScheduled', cat: 'task' }),
      ev({ id: 2, type: 'WorkflowTaskTimedOut', cat: 'failure' }),
    ];
    const { rows, held } = withoutBookkeeping(events);
    expect(held).toBe(1);
    expect(rows.map((e) => e.type)).toEqual(['WorkflowTaskTimedOut']);
  });

  it('holds nothing back on a log that has none, so the control never draws', () => {
    expect(withoutBookkeeping([ev({ cat: 'activity' })]).held).toBe(0);
  });
});
