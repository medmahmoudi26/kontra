/**
 * The Temporal event log for one Run, reduced to what an operator reads.
 *
 * WHY THIS EXISTS. The Workflows surface could already say a run's duration, its lifecycle and
 * the rows it wrote — the three things you ask AFTERWARDS. It could say nothing at all about the
 * middle, and the middle is where every real failure lives: a dispatch scheduled onto a queue
 * nobody polls, an activity that failed and is sitting in a retry backoff, a workflow task that
 * timed out. All three of those look identical from outside (`running`, no new rows), and all
 * three are one glance apart in the event history. This is that glance, without asking anyone to
 * open the Temporal Web UI in another tab.
 *
 * THE TYPE NAME COMES FROM THE ATTRIBUTE KEY, NOT FROM THE ENUM. Every `HistoryEvent` carries
 * exactly one `<something>EventAttributes` field, and its key is the event type in lowerCamel.
 * `eventType` itself is an enum whose wire form differs between the raw gRPC service (a number)
 * and the SDK's decoded objects (a `EVENT_TYPE_…` string), and a mapping table over that is a
 * thing that silently goes stale the way `RunBatch` did. The attribute key cannot: if it is
 * absent there is no event.
 *
 * PAYLOADS ARE NEVER DECODED. Every input, result and failure payload on this path may be a
 * claim-check `$ref` (ADR 0007), so decoding a history would fan out into the blob store — for a
 * 623-unit sweep, thousands of GETs to render a log. Everything below is read from the metadata
 * beside the payload: activity type, task queue, identity, attempt, and the failure MESSAGE,
 * which Temporal stores as a plain string on the failure proto rather than inside a payload.
 *
 * WHICH IS WHY EVENTS CARRY A LINK RATHER THAN A DETAIL. A dispatch and a fleet bring-up put every
 * fact worth reading inside those payloads, so the row for one can only ever say its type and its
 * timestamp. MEASURED on run `nscheck-1786831339` (305 events, 293.9s): events 11–16 are one
 * `kontra-fleet/dns` child covering 156.3s of it — 53% of the run as four opaque rows. The detail
 * is not in the payload; it is in the child's own history, and the child is just another workflow
 * id ({@link eventLink}), which this same reducer reads the same payload-free way.
 */

/** The categories the log filters by. Chosen so that "what went wrong" is one click:
 *  `failure` cuts across workflow, activity and task events, because an operator scanning for
 *  trouble does not care which layer produced it. */
export type EventCategory =
  | 'workflow'
  | 'task'
  | 'activity'
  | 'failure'
  | 'timer'
  | 'marker'
  | 'child'
  | 'signal';

/**
 * The workflow one event names — the thing an operator drills into.
 *
 * A CHILD IS JUST ANOTHER WORKFLOW ID, which is the whole trick: `/api/runs/:runId/history` takes
 * one and this reducer answers, so a level below the run costs no second read path, no second cap
 * and no second set of rules about payloads.
 */
export interface EventLink {
  /** The workflow id. What the history route takes, and the only identifier a **Run** has. */
  workflowId: string;
  /**
   * Temporal's own run id for that execution, when the event carries one.
   *
   * NEVER CALLED A RUN HERE. A **Run** is identified by its workflow id (CONTEXT.md), and this is
   * the other thing Temporal calls a run id. It is carried for exactly one reason: a workflow id
   * is REUSABLE, and asking for a history by id alone answers with the LATEST execution under it.
   * MEASURED on run `nscheck-1786831339`: fleet up (event 12, exec `01a00772-8296…`) and fleet down
   * (event 297, exec `01a00776-89dc…`) are both `kontra-fleet/dns`, so drilling into the 156-second
   * bring-up by id alone would show you the 29-second teardown instead — a plausible wrong answer,
   * which is the one thing worse than no answer.
   */
  execId?: string;
  /** The workflow type where the event names one, e.g. `stackWorkflow`. */
  type?: string;
  /** How the id was read: `child` from the child-execution attributes, `nexus` from the operation
   *  token. Kept because the two are different mechanisms with different failure modes. */
  via: 'child' | 'nexus';
  /** The namespace the event names, when it names one. See the guard in {@link mapHistory}. */
  namespace?: string;
}

/** One event, as the Workflows surface renders it. */
export interface RunEvent {
  /** Temporal's own event id. Stable, monotonic, and what you quote in a bug report. */
  id: number;
  /** e.g. `ActivityTaskScheduled`. Derived from the attribute key — see the module note. */
  type: string;
  cat: EventCategory;
  /** Seconds since the run's FIRST event. What "+4.213s" in the log means. */
  t: number;
  /** Epoch ms, for a wall clock beside the offset. */
  at: number;
  /** A one-line summary built from the event's own metadata. Never a decoded payload. */
  detail: string;
  /** Temporal's attempt counter where the event carries one; 1 otherwise. >1 is a retry. */
  attempt: number;
  /** Seconds this event closes, when it closes something with a known start (`1.24`). 0 when
   *  the event is not a closing one, or its opener fell outside the window we read. */
  dur: number;
  /** The workflow this event is about, when it is about one. Absent on every other event, and that
   *  absence is what makes a row navigable or not. */
  link?: EventLink;
  /**
   * Temporal's user-metadata Summary, when the event carries one. See {@link summaryOf}.
   *
   * ONE LINE OF METADATA, NOT A PAYLOAD READ, and the distinction is the whole reason it is here:
   * the facts a dispatch, a publish or an author's sentence would otherwise hide inside a
   * claim-checked payload are written a second time, small and inline, where a reducer can afford
   * them. `transcript.ts` turns this one into the Method a dispatch called.
   */
  summary?: string;
}

/** What {@link mapHistory} answers with. */
export interface RunHistory {
  events: RunEvent[];
  /** How many raw events were READ. Larger than `events.length` when the middle was elided, and
   *  BOUNDED BY THE READER'S CAP — see {@link RunHistory.historyLength}, which is not. */
  scanned: number;
  /**
   * How many events the history ACTUALLY has, as the server counts them.
   *
   * `scanned` IS NOT THIS, AND THE DIFFERENCE COST A NUMBER THAT WAS WRONG BY UP TO 61%. The reader
   * stops after `HISTORY_MAX_PAGES × HISTORY_PAGE` = 20,000 events and sets `truncated`; `scanned`
   * then reports what it fetched, which above that point is THE CAP AND NOT THE COUNT. Any run
   * between 20,001 and Temporal's 51,200 ceiling is legal, completes normally, and was recorded as
   * exactly 20,000 — a number that looks plausible and is wrong only for the largest runs, which
   * are the ones worth the most.
   *
   * IT IS FREE. `DescribeWorkflowExecution` answers it in one RPC without reading a single event,
   * so nothing here pages further to learn it.
   *
   * ABSENT WHERE NOBODY ASKED THE SERVER: an archived history (ADR 0025) is a recording, and a
   * reader built from a literal has no server behind it. Absent is "not established" and must never
   * be read as zero — `scanned` remains the honest floor.
   */
  historyLength?: number;
  /** What the history WEIGHS, as the server counts it. Rides along with
   *  {@link RunHistory.historyLength} because `describe` returns both and a byte-based meter wants
   *  it; absent on the same terms. */
  historySizeBytes?: number;
  /** Events dropped from the MIDDLE to stay under the cap. Never silently zero — the surface
   *  prints it, because a log with a hole nobody mentions is worse than no log. */
  elided: number;
  /** True when we stopped paging before the end of the history. */
  truncated: boolean;
  /**
   * Set only when this log came from the archive rather than from Temporal (ADR 0025).
   *
   * ABSENT MEANS LIVE, which is the correct default rather than a convenient one: a server older
   * than the archive really is serving a live history, and a console that read the absence as
   * "unknown" would label every run with a caveat. What it changes on screen is the affordances —
   * a follow cannot resume on a history that will never grow again.
   */
  archived?: boolean;
  /** Epoch ms the archive was written, so the console can say how old the account is. Absent on a
   *  live read, where the answer is "now". */
  archivedAt?: number;
}

/** How many events one response may carry. A long-lived sweep's history runs to tens of
 *  thousands of events (measured: ~1,700/hour for a blob cursor alone), and a browser asking for
 *  all of them every poll is the same mistake as an unbounded LIST. */
export const EVENT_CAP = 1000;
/** How much of the HEAD survives the cap. The first events are `WorkflowExecutionStarted` and the
 *  first dispatches — the "what did this run set out to do" that a tail-only window loses. */
export const HEAD_KEEP = 25;

/** The minimum shape this module needs. Deliberately structural: it is satisfied by the raw
 *  gRPC decode AND by the SDK's `fetchHistory()`, so tests need neither. */
export interface RawHistoryEvent {
  eventId?: unknown;
  eventTime?: { seconds?: unknown; nanos?: unknown } | null;
  [attributes: string]: unknown;
}

/** Attribute-key suffix → the type name, e.g. `activityTaskScheduledEventAttributes`. */
const ATTR_SUFFIX = 'EventAttributes';

/** The event families that are ABOUT another workflow, and so may carry (or inherit) a link. The
 *  test is on the type rather than on "did we find an id", so a completed dispatch inherits its
 *  scheduling event's link while an `ActivityTaskCompleted` — which shares the back-reference
 *  field — never can. */
const LINKABLE = /^(NexusOperation|ChildWorkflowExecution|StartChildWorkflowExecution)/;

/**
 * Reduce a raw history to the log.
 *
 * Pure, and that is the point: the entire mapping — categories, durations, the elision, the links —
 * is testable against literal objects, with no Temporal anywhere near it.
 *
 * `home` is the namespace these events were READ from, and is only ever used to refuse a link (see
 * below). Omitting it links everything, which is what a test wants.
 *
 * `server` IS WHAT THE SERVER SAID, and it is separate from anything this function can derive.
 * Everything else here is computed from `raw` — which is exactly why the authoritative length
 * cannot be: the reader that produced `raw` stopped at a cap, so counting it again would produce
 * the same wrong number by a second route. It is threaded in, or it is absent.
 *
 * IT TAKES AN ARRAY BECAUSE A TEST HAS ONE. A reader that pages does not, and must not build one —
 * see {@link HistoryReducer}, which this is a three-line wrapper over. There is ONE reduction here,
 * not two: the array form exists so literals stay the unit of test.
 */
export function mapHistory(
  raw: RawHistoryEvent[],
  truncated = false,
  home = '',
  server: { historyLength?: number; historySizeBytes?: number } = {}
): RunHistory {
  const reducer = new HistoryReducer();
  for (const ev of raw) reducer.push(ev);
  return reducer.finish(truncated, home, server);
}

/**
 * The same reduction, fed ONE EVENT AT A TIME so a pager never holds the history it is reducing.
 *
 * WHY THIS EXISTS. `fetchRunHistory` walks `HISTORY_MAX_PAGES × HISTORY_PAGE` = 20,000 RAW events
 * into a single array and hands it here — to produce at most {@link EVENT_CAP} = 1,000. The raw
 * events are the expensive object: a decoded `HistoryEvent` carries its whole attribute bag and its
 * payload metadata, where a {@link RunEvent} is a handful of numbers and two short strings. Peak
 * heap therefore scaled with the size of the RUN, on a path every open browser tab polls, for an
 * output whose size is fixed.
 *
 * WHAT MAKES IT SAFE, stated rather than assumed, because "just stream it" is wrong here:
 *
 *   - **Pass 1 is per-event already.** `dur` reads `startedAt.get(opener)`, and an opener always
 *     PRECEDES the event that closes it. Nothing in the first pass looks forward.
 *   - **Pass 2 (the subject) walks BACKWARDS** through `openerOf`, so it too only ever needs events
 *     already seen.
 *   - **Pass 3 (the link) genuinely looks FORWARD**, and is the reason this is a class and not a
 *     `map`. A `NexusOperationScheduled` learns its workflow id from the `Started` event AFTER it —
 *     `keepBest(linkFor, opener, link)` seeds the opener's id from its successor. So passes 2 and 3
 *     run in {@link HistoryReducer.finish}, once the maps are complete, over the ≤1,000 events that
 *     SURVIVED — never over the 20,000 that were read.
 *
 * WHAT IS STILL O(N): the four id-keyed maps. They hold numbers, short strings and small link
 * objects — kilobytes per thousand events against the megabytes a thousand raw events cost — and
 * they cannot be pruned without knowing which ids the retained tail will reference. This bounds the
 * big term, and says plainly that it does not bound every term.
 */
export class HistoryReducer {
  /** Event id → epoch ms, so a closing event can measure back to its opener. */
  private readonly startedAt = new Map<number, number>();
  /** Event id → the event that opened it, so `finish` can inherit a subject backwards. */
  private readonly openerOf = new Map<number, number>();
  /** Event id → the workflow that family of events is about. Seeded under an event's OWN id AND
   *  under its opener's, which is the forward reference that forces pass 3 into `finish`. */
  private readonly linkFor = new Map<number, EventLink>();
  /** Event id → what that event's family is ABOUT, e.g. `activityType=resolveBatch`. */
  private readonly subjectOf = new Map<number, string>();

  /** The first {@link HEAD_KEEP} reduced events — "what did this run set out to do". */
  private readonly head: RunEvent[] = [];
  /** A ring over everything AFTER the head, holding the last `EVENT_CAP - HEAD_KEEP`. Sized so that
   *  head + ring reproduces `[...all.slice(0, HEAD_KEEP), ...all.slice(-(EVENT_CAP - HEAD_KEEP))]`
   *  exactly — including the under-cap case, where the two together are simply everything. */
  private readonly ring: RunEvent[] = [];
  private ringAt = 0;

  /** Raw events READ — including any the reducer skipped, because that is what `scanned` means. */
  private scanned = 0;
  /** Events that actually REDUCED. `elided` is computed from this and not from `scanned`: an event
   *  with no attribute key never became a row, and counting it as elided would report a hole that
   *  was never there. */
  private reduced = 0;
  /** Epoch ms of the FIRST raw event, which is `t = 0`. Taken from the first event pushed whether or
   *  not it reduced, matching the array form's `raw[0]`. */
  private first = 0;

  /** Feed one raw event. Callers may discard it immediately afterwards — nothing here retains it. */
  push(ev: RawHistoryEvent): void {
    if (this.scanned === 0) this.first = tsToMs(ev.eventTime);
    this.scanned++;

    const key = attrKey(ev);
    if (!key) return;
    const type = typeName(key);
    const attrs = (ev[key] ?? {}) as Record<string, unknown>;
    const id = num(ev.eventId);
    const at = tsToMs(ev.eventTime);
    this.startedAt.set(id, at);

    // `initiatedEventId` is the child-workflow family's word for the same back-reference the
    // activity family spells `scheduledEventId`.
    const opener =
      num(attrs.startedEventId) || num(attrs.scheduledEventId) || num(attrs.initiatedEventId);
    const openedAt = opener ? this.startedAt.get(opener) : undefined;
    if (id > 0 && opener > 0) this.openerOf.set(id, opener);

    const link = eventLink(type, attrs);
    if (link) {
      if (id > 0) keepBest(this.linkFor, id, link);
      if (opener > 0) keepBest(this.linkFor, opener, link);
    }

    const detail = describe(type, attrs);
    const subject = SUBJECT.exec(detail)?.[0];
    if (id > 0 && subject) this.subjectOf.set(id, subject);

    this.retain({
      id,
      type,
      cat: categorize(type),
      t: this.first > 0 ? Math.max(0, (at - this.first) / 1000) : 0,
      at,
      detail,
      attempt: Math.max(1, num(attrs.attempt) || 1),
      dur: openedAt !== undefined && at >= openedAt ? (at - openedAt) / 1000 : 0,
      // A SIBLING OF THE ATTRIBUTE BAG. `userMetadata` hangs off the `HistoryEvent` itself rather
      // than off `attrs`, which is why it is read from `ev` and not from the bag everything else
      // above comes out of.
      ...summaryOf(ev),
    });
  }

  /** Head first, then the ring — the whole of the cap, applied as events arrive. */
  private retain(e: RunEvent): void {
    this.reduced++;
    if (this.head.length < HEAD_KEEP) {
      this.head.push(e);
      return;
    }
    const cap = EVENT_CAP - HEAD_KEEP;
    if (this.ring.length < cap) this.ring.push(e);
    else {
      this.ring[this.ringAt] = e;
      this.ringAt = (this.ringAt + 1) % cap;
    }
  }

  /** The retained events in history order — the ring unrolled from its oldest slot. */
  private retained(): RunEvent[] {
    const cap = EVENT_CAP - HEAD_KEEP;
    const tail =
      this.ring.length < cap
        ? this.ring
        : [...this.ring.slice(this.ringAt), ...this.ring.slice(0, this.ringAt)];
    return [...this.head, ...tail];
  }

  /**
   * Finish the reduction — the two passes that need the whole history, over only what survived.
   *
   * See the class note for why these are here and not in {@link HistoryReducer.push}.
   */
  finish(
    truncated = false,
    home = '',
    server: { historyLength?: number; historySizeBytes?: number } = {}
  ): RunHistory {
    /* SPREAD, SO ABSENT STAYS ABSENT. `historyLength: undefined` and no `historyLength` at all are
       the same to a reader in TypeScript and NOT the same over JSON — the first serialises to a key
       that is missing anyway, but it also defeats `'historyLength' in history`, which is how a caller
       asks whether the server was consulted. */
    const fromServer = {
      ...(server.historyLength === undefined ? {} : { historyLength: server.historyLength }),
      ...(server.historySizeBytes === undefined ? {} : { historySizeBytes: server.historySizeBytes }),
    };
    const events = this.retained();

    // THE NAME TRAVELS FORWARD TO THE EVENTS THAT CLOSE IT. Following the back-references rather
    // than pairing by type is what makes it work across all three families at once: an activity is
    // Scheduled→Started→Completed and a Nexus dispatch is Scheduled→Started→Completed, but the second
    // hop is spelled `startedEventId` on one and `scheduledEventId` on the other, and a child spells
    // it `initiatedEventId`. Three hops covers every chain Temporal writes; a family whose opener
    // named nothing (a workflow task) picks up nothing, which is the correct outcome rather than a
    // gap.
    for (const e of events) {
      const named = subjectFrom(e.id, this.subjectOf, this.openerOf);
      if (!named || e.detail.includes(named)) continue;
      e.detail = e.detail === e.type ? named : `${named} · ${e.detail}`;
    }

    for (const e of events) {
      if (!LINKABLE.test(e.type)) continue;
      const link = this.linkFor.get(e.id) ?? this.linkFor.get(this.openerOf.get(e.id) ?? 0);
      if (!link) continue;
      // A LINK INTO ANOTHER NAMESPACE IS NOT ONE THIS CONSOLE CAN FOLLOW. The history route reads
      // exactly one namespace (`KONTRA_NAMESPACE`), so a workflow id that also exists in ours would
      // render a DIFFERENT workflow's history under this row. Nexus endpoints are namespace-per-author
      // by design (ADR 0001), so this is reachable rather than theoretical. Say where it went instead
      // of offering a click that lies.
      if (home && link.namespace && link.namespace !== home) {
        e.detail = `${e.detail} · elsewhere in namespace=${link.namespace}`;
        continue;
      }
      e.link = link;
    }

    return {
      events,
      scanned: this.scanned,
      // Keep the head and the TAIL. The tail is where a live run is, and the head is what it set out
      // to do; the middle of a 20,000-event sweep is the same twenty lines repeating.
      elided: this.reduced > EVENT_CAP ? this.reduced - EVENT_CAP : 0,
      truncated,
      ...fromServer,
    };
  }
}


/**
 * Keep the better of two links for one event id.
 *
 * THE ONE THAT PINS AN EXECUTION WINS. `StartChildWorkflowExecutionInitiated` names only the id it
 * asked for — the execution does not exist yet — and the `ChildWorkflowExecutionStarted` that
 * answers it carries the exec id and points back at it. Letting the later event upgrade the earlier
 * one is what makes clicking the FIRST row of a reused id land on that row's child rather than on
 * whatever ran under the name last. Same workflow id only; otherwise first writer wins.
 */
function keepBest(into: Map<number, EventLink>, id: number, link: EventLink): void {
  const have = into.get(id);
  if (!have || (!have.execId && link.execId && have.workflowId === link.workflowId)) {
    into.set(id, link);
  }
}

/**
 * The piece of a `detail` that NAMES the thing an event's family is about.
 *
 * The three spellings of one question — which Method (`activityType`), which Actor and version
 * (`endpoint`, e.g. `kontra-nscheck-0-1-0`), which workflow (`workflowType`) — and deliberately not
 * `taskQueue` or `identity`: those say where and who, which every event in the family already
 * carries and which name nothing when repeated across three rows.
 */
const SUBJECT = /(?:activityType|endpoint|workflowType)=[\w.$-]+/;

/**
 * The subject of the event that opened this one, following the chain back.
 *
 * Bounded at three hops because that is the longest chain Temporal writes (schedule → start →
 * close) and an unbounded walk over a map an adversarial fixture could make cyclic is a hang in a
 * pure function.
 */
function subjectFrom(
  id: number,
  subjectOf: Map<number, string>,
  openerOf: Map<number, number>
): string {
  let at = openerOf.get(id) ?? 0;
  for (let hop = 0; hop < 3 && at > 0; hop += 1) {
    const found = subjectOf.get(at);
    if (found) return found;
    at = openerOf.get(at) ?? 0;
  }
  return '';
}

/** Which `…EventAttributes` field this event carries. `undefined` for an event with none, which
 *  is not a shape Temporal produces but is one a fixture can. */
export function attrKey(ev: RawHistoryEvent): string | undefined {
  for (const key of Object.keys(ev)) {
    if (key.endsWith(ATTR_SUFFIX) && ev[key] != null && typeof ev[key] === 'object') return key;
  }
  return undefined;
}

/** `activityTaskScheduledEventAttributes` → `ActivityTaskScheduled`. */
export function typeName(key: string): string {
  const stem = key.slice(0, -ATTR_SUFFIX.length);
  return stem.charAt(0).toUpperCase() + stem.slice(1);
}

/**
 * The workflow one event names, read from its attributes and from nothing else.
 *
 * THE THREE SHAPES, ALL RECORDED RATHER THAN ASSUMED (run `nscheck-1786831339`, 2026-08-15):
 *
 *   `StartChildWorkflowExecutionInitiated`  `workflowId` + `workflowType` — the id the parent ASKED
 *                                           for. No execution yet, so no exec id.
 *   `ChildWorkflowExecution*`               `workflowExecution: {workflowId, runId}` + `workflowType`
 *                                           — the id and the exact execution.
 *   `NexusOperationStarted`                 `operationToken` (and its deprecated twin `operationId`),
 *                                           base64url of `{"t":1,"ns":…,"wid":…}`.
 *
 * ALL THREE ARE PLAIN METADATA, NOT PAYLOADS. A Nexus token is a string field on the event, the same
 * as `taskQueue` or `identity` — reading it costs no blob GET, so drilling keeps the zero-read
 * property the whole log is built on (ADR 0007). Nothing here touches `input`, `result` or the
 * failure payload.
 */
export function eventLink(type: string, attrs: Record<string, unknown>): EventLink | undefined {
  if (type.startsWith('NexusOperation')) {
    // `operationToken` since Temporal 1.27; `operationId` is the same string under the older name,
    // and a server that still sends only the old one must still be drillable.
    const target = nexusTarget(str(attrs.operationToken) || str(attrs.operationId));
    if (!target) return undefined;
    const link: EventLink = { workflowId: target.workflowId, via: 'nexus' };
    if (target.namespace) link.namespace = target.namespace;
    return link;
  }
  if (!type.startsWith('ChildWorkflowExecution') && !type.startsWith('StartChildWorkflowExecution')) {
    return undefined;
  }
  const exec = attrs.workflowExecution as { workflowId?: unknown; runId?: unknown } | null;
  const workflowId = str(exec?.workflowId) || str(attrs.workflowId);
  if (!workflowId) return undefined;
  const link: EventLink = { workflowId, via: 'child' };
  const execId = str(exec?.runId);
  if (execId) link.execId = execId;
  const type_ = str((attrs.workflowType as { name?: unknown } | undefined)?.name);
  if (type_) link.type = type_;
  const ns = str(attrs.namespace);
  if (ns) link.namespace = ns;
  return link;
}

/**
 * The workflow behind a Nexus operation token.
 *
 * RECORDED, not guessed: the token on a dispatch's `NexusOperationStarted` decodes to
 * `{"t":1,"ns":"default","wid":"actor-nscheck-nscheck-1786831339-nscheck-60c3bfac"}` — Temporal's
 * workflow-run operation token, `t:1`, which is how a workflow-backed operation names the workflow
 * it started. Nothing else in the caller's history names it: the caller asked an endpoint for an
 * operation and got a token back, and without decoding this the twelve dispatches of that run are
 * twelve identical rows.
 *
 * Anything that is not that shape yields nothing rather than a guess — an operation whose handler
 * is not a workflow has no workflow to drill into, and inventing one would be the plausible wrong
 * answer this module refuses everywhere else.
 */
function nexusTarget(token: string): { workflowId: string; namespace: string } | undefined {
  if (!token) return undefined;
  try {
    const decoded = JSON.parse(Buffer.from(token, 'base64url').toString('utf8')) as {
      wid?: unknown;
      ns?: unknown;
    };
    const workflowId = str(decoded.wid);
    return workflowId ? { workflowId, namespace: str(decoded.ns) } : undefined;
  } catch {
    return undefined; // not base64, not JSON, or not a token we know — all mean "no link"
  }
}

/**
 * The category one type belongs to.
 *
 * FAILURE WINS OVER LAYER, always. `ActivityTaskFailed` is a failure, not an activity: the filter
 * exists so that one click answers "is anything wrong", and an event that hid under `activity`
 * because that is where it structurally belongs would defeat exactly that.
 */
export function categorize(type: string): EventCategory {
  if (/(Failed|TimedOut|Terminated|CancelRequested|Canceled|Cancelled)$/.test(type)) {
    // A cancel REQUEST is not a failure — it is an operator doing something deliberate — but a
    // completed cancellation of work is, in the only sense that matters here: output is missing.
    if (/CancelRequested$/.test(type)) return layer(type);
    // A CANCELLED TIMER IS A WAIT THAT ENDED EARLY, AND A WAIT IS NOT WORK. Every other Canceled
    // event names output that will now never arrive; a timer names only the clock, and cancelling
    // it is the SUCCESSFUL outcome of every race between "wait for the deadline" and "the thing
    // happened first". `ask` is precisely that race — the deadline timer is cancelled the moment a
    // person answers — so filing this under `failure` made every ANSWERED ask leave a failure in
    // its run. MEASURED on `canary-1787842278`: one `TimerCanceled`, the only failure in 140
    // events, and it flipped the run's whole verdict from "produced nothing" to "nothing worked"
    // on a run whose two halves agreed.
    if (type.startsWith('Timer')) return 'timer';
    return 'failure';
  }
  return layer(type);
}

function layer(type: string): EventCategory {
  if (type.startsWith('WorkflowTask')) return 'task';
  if (type.startsWith('ActivityTask')) return 'activity';
  if (type.startsWith('Timer')) return 'timer';
  if (type.startsWith('Marker') || type.startsWith('Upsert')) return 'marker';
  if (type.startsWith('ChildWorkflow') || type.startsWith('StartChildWorkflow')) return 'child';
  // A Nexus operation is a child in the only sense this log cares about: work THIS run caused to
  // happen in another workflow, whose story is one drill away. Filing it under `workflow` put a
  // dispatch beside the run's own start and completion, which are the two events it is least like.
  if (type.startsWith('Nexus')) return 'child';
  if (type.includes('Signal')) return 'signal';
  return 'workflow';
}

/**
 * One line of metadata for an event.
 *
 * The fields are chosen from what an operator actually chases: WHICH actor method
 * (`activityType`), on WHOSE queue (`taskQueue`) — the pair that explains a dispatch sitting
 * forever — and WHO picked it up (`identity`). Failures carry the message Temporal already
 * stringified; everything else falls back to naming nothing rather than to a decoded payload.
 *
 * A Nexus dispatch has neither an activity type nor a queue: it names an `endpoint` (which is the
 * actor AND its version — `kontra-nscheck-0-1-0`) and a `service`/`operation` pair. Those are the
 * same question in the Nexus spelling, so they are read the same way. The workflow it started is
 * NOT here — it is on the row's {@link EventLink}, because it is a thing to open rather than a
 * thing to read.
 */
export function describe(type: string, attrs: Record<string, unknown>): string {
  const bits: string[] = [];
  const activityType = str((attrs.activityType as { name?: unknown } | undefined)?.name);
  const workflowType = str((attrs.workflowType as { name?: unknown } | undefined)?.name);
  const queue = str((attrs.taskQueue as { name?: unknown } | undefined)?.name);
  const identity = str(attrs.identity);
  const failure = failureMessage(attrs.failure);

  if (workflowType) bits.push(`workflowType=${workflowType}`);
  if (activityType) bits.push(`activityType=${activityType}`);
  if (queue) bits.push(`taskQueue=${queue}`);
  if (str(attrs.endpoint)) bits.push(`endpoint=${str(attrs.endpoint)}`);
  const service = str(attrs.service);
  const operation = str(attrs.operation);
  if (service || operation) bits.push(`nexus=${[service, operation].filter(Boolean).join('/')}`);
  if (identity) bits.push(`identity=${identity}`);
  if (str(attrs.timerId)) bits.push(`timerId=${str(attrs.timerId)}`);
  if (str(attrs.markerName)) bits.push(`marker=${str(attrs.markerName)}`);
  // THE NAME OF A SIGNAL IS METADATA; its payload is not. Reading the name costs no blob GET, and
  // it is what pairs an answer with the question it answers: `sdk/python/kontra/hitl.py` puts
  // the ask's id IN the signal name (`kontra.answer/ask-1`) precisely so this line can.
  if (str(attrs.signalName)) bits.push(`signal=${str(attrs.signalName)}`);
  // A MEMO UPSERT NAMES ITS KEYS WITHOUT DECODING ITS VALUES — the fields are a map, and a map's
  // keys are structure rather than payload. That is how a run's ASKS reach the archived reduced
  // log at all: an ask is `upsert_memo({'kontra.ask.<id>': …})`, so this records that the run asked
  // `<id>` and, later, that a signal answered `<id>`. An archive holding an answer and not its
  // question would be a record of somebody approving something unspecified.
  const memoKeys = keysOf(attrs.upsertedMemo);
  if (memoKeys.length > 0) bits.push(`memo=${memoKeys.join(',')}`);
  const retryAfter = durationSeconds(attrs.startToFireTimeout ?? attrs.backoffStartInterval);
  if (retryAfter > 0) bits.push(`fires in ${retryAfter}s`);
  if (failure) bits.push(failure);
  // `retryState` names WHY a failed activity will not be retried again, which is the difference
  // between "wait" and "this run is over".
  const retryState = str(attrs.retryState);
  if (retryState && retryState !== 'RETRY_STATE_UNSPECIFIED' && failure) bits.push(retryState);
  if (bits.length === 0 && str(attrs.reason)) bits.push(str(attrs.reason));
  return bits.join(' · ') || type;
}

/**
 * The KEYS of a `Memo`'s field map, in a stable order and bounded.
 *
 * NOT A PAYLOAD READ. `upsertedMemo.fields` is `map<string, Payload>`; the keys are the map's own
 * structure and the values are the payloads this module never touches. Sorted so two emissions of
 * the same upsert produce the same line, and capped because `detail` is one line an operator reads
 * rather than a dump of whatever an author put in a memo.
 */
function keysOf(raw: unknown): string[] {
  const fields = (raw as { fields?: unknown } | null)?.fields;
  if (!fields || typeof fields !== 'object') return [];
  const keys = Object.keys(fields as Record<string, unknown>).sort();
  return keys.length > 4 ? [...keys.slice(0, 4), `+${keys.length - 4} more`] : keys;
}

/**
 * The user-metadata Summary on one event — `{ summary }`, or `{}` when it has none.
 *
 * IT IS A PAYLOAD, AND READING IT IS STILL NOT A PAYLOAD READ. The rule this module keeps is that
 * nothing here may fan out into the blob store, and a Summary cannot: the writer builds it to fit
 * 200 bytes (`SUMMARY_BUDGET` in `sdk/python/kontra/catalog.py`), some 650× under the codec's
 * 128 KiB offload threshold, so it is always stored inline. That bound is what makes this safe —
 * which is why the writer builds the line to fit rather than trimming it, and why the one case it
 * could not hold for is REFUSED below rather than guessed at.
 *
 * TWO ENCODINGS OF THE BYTES, because both reach here honestly: `Uint8Array` from the raw gRPC
 * decode this module is fed in production, and base64 from a history that travelled as JSON —
 * which is what `temporal workflow show -o json` emits, and therefore what a recorded fixture is.
 *
 * AND TWO OF THE STRING. Every SDK's default converter writes a string as JSON, so the bytes are
 * normally a quoted JSON string; one that was written plain is read as itself rather than refused,
 * because a Summary nobody can read is indistinguishable from a Summary nobody set.
 */
function summaryOf(ev: RawHistoryEvent): { summary?: string } {
  const payload = (ev.userMetadata as { summary?: unknown } | null | undefined)?.summary as
    | { metadata?: Record<string, unknown> | null; data?: unknown }
    | null
    | undefined;
  if (!payload?.data) return {};
  // A CLAIM-CHECKED SUMMARY IS A REF, NOT A SENTENCE. Unreachable for a dispatch, whose line is
  // capped; reachable in principle for an author who narrated a novel. Rendering the ref's JSON
  // as their sentence would be the confident wrong answer, and fetching it is the fan-out this
  // module does not do — so the event simply has no summary.
  if (bytesToText(payload.metadata?.encoding) === CLAIM_CHECK_ENCODING) return {};
  const text = bytesToText(payload.data);
  if (!text) return {};
  if (text.startsWith('"')) {
    try {
      const parsed: unknown = JSON.parse(text);
      return typeof parsed === 'string' && parsed ? { summary: parsed } : {};
    } catch {
      // Not JSON after all. Fall through and read the bytes as the line they look like.
    }
  }
  return { summary: text };
}

/** The marker the claim-check codec puts on a payload it offloaded (`codec/claimCheck.ts`).
 *  Spelled here rather than imported: this module is pure and must not pull in the store. */
const CLAIM_CHECK_ENCODING = 'binary/claim-check-v1';

/** Payload bytes as text. `Uint8Array` on the gRPC path, base64 when the history came as JSON. */
function bytesToText(raw: unknown): string {
  if (raw instanceof Uint8Array) return new TextDecoder().decode(raw);
  if (typeof raw === 'string') {
    try {
      return new TextDecoder().decode(Uint8Array.from(atob(raw), (c) => c.charCodeAt(0)));
    } catch {
      return '';
    }
  }
  return '';
}

/**
 * Temporal's failure proto keeps a human message beside the (possibly claim-checked) payload.
 * Nested causes are walked, because the useful sentence is usually the innermost one.
 *
 * EXPORTED BECAUSE A SECOND READER ARRIVED. `temporalClient.listServes` reads the close event of a
 * failed serve-dev execution to answer "why did Serve not start a Worker" — the same proto, on the
 * same raw gRPC path, wanting the same sentence. A local copy there would be a second walker over a
 * recursive shape, and the two would disagree on the day one of them learned about `cause` and the
 * other did not. That is the drift `index.ts` says this package exists to prevent.
 *
 * IT READS NO PAYLOAD, which is what makes it safe on both call sites: `message` is a plain string
 * field on the failure proto, beside the `details` payload that this never touches (ADR 0007).
 */
export function failureMessage(raw: unknown): string {
  const f = raw as { message?: unknown; cause?: unknown; applicationFailureInfo?: unknown } | null;
  if (!f || typeof f !== 'object') return '';
  const own = str(f.message);
  const cause = failureMessage(f.cause);
  if (own && cause && cause !== own) return `${own}: ${cause}`;
  return own || cause;
}

/** A proto Duration in whole-ish seconds, for "retrying in 2s". */
function durationSeconds(raw: unknown): number {
  const d = raw as { seconds?: unknown; nanos?: unknown } | null;
  if (!d || typeof d !== 'object') return 0;
  const s = num(d.seconds) + num(d.nanos) / 1e9;
  return s > 0 ? Math.round(s * 100) / 100 : 0;
}

/** Proto Timestamp → epoch ms. `seconds` arrives as a number or a protobufjs Long. */
function tsToMs(ts?: { seconds?: unknown; nanos?: unknown } | null): number {
  if (!ts) return 0;
  return num(ts.seconds) * 1000 + Math.floor(num(ts.nanos) / 1e6);
}

function num(raw: unknown): number {
  if (typeof raw === 'number') return Number.isFinite(raw) ? raw : 0;
  if (raw == null) return 0;
  const n = Number((raw as { toString(): string }).toString());
  return Number.isFinite(n) ? n : 0;
}

function str(raw: unknown): string {
  return typeof raw === 'string' ? raw : '';
}
