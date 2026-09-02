/**
 * Reading a Temporal event log: the pure part.
 *
 * The server already reduced the history to `RunEvent` (see `backend/src/history.ts`). What is
 * left is presentation with real decisions in it — which line names the event, which category a
 * filter chip counts, how a run's activity total is derived — and each of those is a rule worth
 * pinning rather than a formatting detail.
 */

import type { RunEvent } from '../run/api';

/** The filter chips, in the order they appear. `all` first because it is the resting state, and
 *  `failure` early because "is anything wrong" is the question this log is opened to answer.
 *
 *  `Children` is the drillable set — every child workflow and every Nexus dispatch, which are the
 *  rows whose story is one level down rather than on the row. Its count doubles as "how many places
 *  this run caused something to happen", which no other number on the page says. */
export const EVENT_FILTERS = [
  { id: 'all', label: 'All' },
  { id: 'failure', label: 'Failures' },
  { id: 'activity', label: 'Activity' },
  { id: 'child', label: 'Children' },
  { id: 'task', label: 'Workflow task' },
  { id: 'timer', label: 'Timers' },
  { id: 'workflow', label: 'Workflow' },
] as const;

export type EventFilter = (typeof EVENT_FILTERS)[number]['id'];

/** How many events the TIMELINE draws. The table has no window — it is a table — but a timeline is
 *  a card per event with an expandable body, and a thousand of those is a scroll nobody reads and a
 *  layout the browser re-flows on every poll. */
export const TIMELINE_WINDOW = 120;

/**
 * Temporal's user-metadata Summary on one event, trimmed — `''` when it carries none.
 *
 * THE ONE FIELD ON A `RunEvent` A HUMAN CHOSE THE WORDS OF. Everything in `detail` is read off the
 * event's own machinery; this is what somebody wrote FOR this row. Two writers put things here
 * today and neither is reachable any other way without decoding a payload: `speak` writes its
 * sentence as the Summary of a zero-duration timer, and a dispatch writes `<method> · <actor>@<ver>
 * · <n> units` on its scheduling event (`sdk/python/actorkit/catalog.py`, `dispatch_summary`) —
 * precisely so the Method is readable without opening the input (ADR 0007).
 */
export function eventSummary(e: Pick<RunEvent, 'summary'>): string {
  return e.summary?.trim() ?? '';
}

/**
 * The event's OWN metadata, for the space BESIDE a Summary that took the front of the row.
 *
 * IT EXISTS SO THAT SHOWING THE MEANING COSTS NOTHING. A dispatch's Summary names the Method and a
 * timer's is a sentence, but `endpoint=kontra-probe-0-1-0 · nexus=kontra.actor/run` is still the
 * fact that says WHICH actor and WHICH queue — the pair that explains a dispatch sitting forever.
 * So a Summary is added in front of the detail rather than in place of it, and a row that has no
 * Summary keeps exactly the line it has today.
 *
 * Empty when there is nothing extra to say: no Summary (the detail already leads the row), a detail
 * the server fell back to spelling as the type again, or a detail the Summary already repeats.
 */
export function eventAside(e: Pick<RunEvent, 'detail' | 'type' | 'summary'>): string {
  if (!eventSummary(e)) return '';
  if (!e.detail || e.detail === e.type || e.detail === eventSummary(e)) return '';
  return e.detail;
}

/**
 * The one-line name beside the event type.
 *
 * THE SUMMARY WINS OVER EVERY READING OF THE MACHINERY, and that is the whole ordering below in one
 * sentence: the rest of this function GUESSES which piece of an event's metadata a human would have
 * wanted, and a Summary is the piece a human actually chose. MEASURED on real runs: without it a
 * `speak` reads `TimerStarted timerId=1` — the sentence absent from the one pane that claims to
 * show everything — and a dispatch reads as its endpoint, with the Method name nowhere on the page.
 * What it displaces is not lost: {@link eventAside} carries it, and {@link eventPayload} holds both.
 *
 * ORDER MATTERS AND IS THE POINT, below that. The activity type is what an operator is looking for —
 * which Method this event is about — so it wins whenever it is present. The task queue is the
 * fallback because the second question is always "whose queue", and a scheduled activity sitting
 * forever on a queue nobody polls is the failure this log exists to make visible. Only with neither
 * does the raw detail get used, trimmed rather than wrapped.
 *
 * A Nexus dispatch has no activity type and no queue; its `endpoint` is both, and it names the
 * actor AND its version (`kontra-nscheck-0-1-0`). It ranks above the queue because the
 * service/operation pair beside it (`kontra.actor/run`) is identical on every dispatch kontra ever
 * makes, and a column that prints one string for all twelve rows names nothing.
 */
export function eventName(e: Pick<RunEvent, 'detail' | 'type' | 'summary'>): string {
  const said = eventSummary(e);
  if (said) return said;
  const activity = /activityType=([\w.$-]+)/.exec(e.detail);
  if (activity) return activity[1]!;
  const workflow = /workflowType=([\w.$-]+)/.exec(e.detail);
  if (workflow) return workflow[1]!;
  const endpoint = /endpoint=([\w.$-]+)/.exec(e.detail);
  if (endpoint) return endpoint[1]!;
  const queue = /taskQueue=([\w.$-]+)/.exec(e.detail);
  if (queue) return queue[1]!;
  if (e.detail === e.type) return '';
  return e.detail.length > 72 ? `${e.detail.slice(0, 71)}…` : e.detail;
}

/**
 * The rows a reader came for, and the count of Temporal's own paperwork held back.
 *
 * MEASURED ON `canary-1787842278`: 81 of its 140 events — 58% — are `WorkflowTaskScheduled`,
 * `WorkflowTaskStarted` and `WorkflowTaskCompleted`, three per decision the workflow made. Not one
 * of them says which Method ran, which Actor answered, which Machine took the work or what the
 * author narrated. Drawn in line they push every event that DOES say those things off a 40-row
 * tail, which is how the log of a run that provisioned a Machine, dispatched two halves of a sweep
 * and asked a human a question reads as a list of `WorkflowTaskScheduled`.
 *
 * HELD, NOT DROPPED, and the count comes back so the surface can say how many and put them back.
 * An event log that quietly omits a category is the exact shape of thing this console exists not to
 * be — and one class of workflow-task event never leaves at all: `WorkflowTaskTimedOut` and
 * `WorkflowTaskFailed` categorize as `failure` rather than `task`, so the paperwork worth reading
 * is the paperwork that stays.
 */
export function withoutBookkeeping(events: readonly RunEvent[]): {
  rows: RunEvent[];
  held: number;
} {
  const rows = events.filter((e) => e.cat !== 'task');
  return { rows, held: events.length - rows.length };
}

/** How many events fall in each category, for the chip counts. `all` is the total, so the chips
 *  add up to more than the list length and are meant to. */
export function countByCategory(events: readonly RunEvent[]): Record<EventFilter, number> {
  const counts = Object.fromEntries(EVENT_FILTERS.map((f) => [f.id, 0])) as Record<
    EventFilter,
    number
  >;
  counts.all = events.length;
  for (const e of events) {
    if (e.cat in counts) counts[e.cat as EventFilter] += 1;
  }
  return counts;
}

export function filterEvents(events: readonly RunEvent[], filter: EventFilter): RunEvent[] {
  return filter === 'all' ? [...events] : events.filter((e) => e.cat === filter);
}

/** `+4.213s`. Three decimals because activities on a fast queue finish inside one, and a log that
 *  rounded them all to `+4s` would show a dozen events at the same instant. */
export function fmtOffset(seconds: number): string {
  return `+${seconds.toFixed(3)}s`;
}

/** `14:07:32.104` — the wall clock, for correlating with a tmux pane or another system's log. */
export function fmtClock(epochMs: number): string {
  if (!epochMs) return '';
  const d = new Date(epochMs);
  const pad = (n: number, w = 2) => String(n).padStart(w, '0');
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${pad(d.getMilliseconds(), 3)}`;
}

/** `1.24s` for a closing event, empty for one that closes nothing. */
export function fmtEventDuration(seconds: number): string {
  if (!seconds) return '';
  return seconds >= 10 ? `${seconds.toFixed(1)}s` : `${seconds.toFixed(2)}s`;
}

/**
 * The numbers the stat cards take from the log.
 *
 * `activities` counts SCHEDULED, not started or completed: a dispatch that was scheduled and never
 * picked up is exactly the case worth counting, and counting completions instead would report zero
 * for a run stuck against an unserved queue — the state the number is most needed for.
 *
 * `retries` counts events whose attempt is past the first. Temporal stamps the attempt on the
 * schedule and start of each retry, so this is the count of re-attempts rather than of failures;
 * `failures` is kept separately because one failing Batch that retries five times is one problem
 * and five retries, and collapsing them hides which.
 */
export function eventStats(events: readonly RunEvent[]): {
  activities: number;
  retries: number;
  failures: number;
} {
  let activities = 0;
  let retries = 0;
  let failures = 0;
  for (const e of events) {
    if (e.type === 'ActivityTaskScheduled') activities += 1;
    if (e.attempt > 1 && e.type.endsWith('Started')) retries += 1;
    if (e.cat === 'failure') failures += 1;
  }
  return { activities, retries, failures };
}

/** The CSS colour token for a category's timeline node and type label. Kept as literals rather than
 *  Tailwind classes because they are also used as SVG `fill`/`stroke` and as an inline border. */
export function eventColor(cat: RunEvent['cat']): string {
  switch (cat) {
    case 'failure':
      return '#fda4af';
    case 'activity':
      return '#6ee7b7';
    case 'task':
      return '#7dd3fc';
    case 'timer':
      return '#fde047';
    case 'marker':
      return '#d8b4fe';
    case 'child':
      return '#f0abfc';
    case 'signal':
      return '#fdba74';
    default:
      return '#34d399';
  }
}

/** The expandable body of one event card: everything the server sent, as JSON. Deliberately the
 *  WHOLE record — this is the escape hatch for a question the summary line did not anticipate, and
 *  a curated subset would be the thing that omitted the field somebody needed. */
export function eventPayload(e: RunEvent): string {
  return JSON.stringify(
    {
      eventId: e.id,
      eventType: e.type,
      category: e.cat,
      elapsed: fmtOffset(e.t),
      at: e.at ? new Date(e.at).toISOString() : undefined,
      attempt: e.attempt,
      duration: e.dur ? fmtEventDuration(e.dur) : undefined,
      // BOTH LINES, NEVER ONE. The row in front of this shows the Summary where there is one; this
      // is the escape hatch, so it must hold the metadata that displaced as well as the metadata
      // that displaced it.
      summary: e.summary,
      detail: e.detail,
      // The workflow this row drills into, spelled out. The button beside it shows a shortened id;
      // this is where you copy the whole one from, which is what `kontra runs`, `temporal workflow
      // show` and a bug report all want.
      target: e.link,
    },
    null,
    2
  );
}
