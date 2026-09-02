import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe as test, expect, it } from 'vitest';

import { categorize, type EventCategory, type RunHistory } from '@kontra/core/history';
import { expand, readTranscript, type DispatchTurn, type TranscriptEvent, type Turn } from './transcript';
import {
  DOMAINS,
  filtersOf,
  nameTranscript,
  nameTurn,
  neverPickedUp,
  readVocabulary,
  retryState,
  stalledBecause,
  TERMS,
  type Term,
  type Vocabulary,
} from '@kontra/core/vocabulary';
import { KONTRA_INTERNAL_WORKFLOW_TYPES } from './visibility';

/**
 * THE VOCABULARY IS NOT IN THE ARCHIVE, AND THIS SUITE IS THE PROOF.
 *
 * `vocabulary.fixture.json` is an archived run — the {@link ArchivedLog} envelope `historyArchive.ts`
 * writes, holding the reduced TEMPORAL log and nothing else. It is read off disk rather than built
 * in this file on purpose: the claim being made is about bytes that were written once and will
 * never be written again, and a fixture constructed in memory cannot make it. Every test below
 * names that same object; two of them name it with a DIFFERENT vocabulary and watch the same bytes
 * tell a different story.
 */
const FIXTURE = join(__dirname, 'vocabulary.fixture.json');

interface ArchivedFixture {
  v: number;
  runId: string;
  history: RunHistory;
}

function archive(): { bytes: string; log: ArchivedFixture } {
  const bytes = readFileSync(FIXTURE, 'utf8');
  return { bytes, log: JSON.parse(bytes) as ArchivedFixture };
}

/** The first event's instant, shared with `transcript.test.ts` so offsets read like the real run. */
const BASE = 1786831339151;

/** One reduced event. `cat` comes from `history.ts`'s own `categorize`, never hand-written. */
function ev(id: number, type: string, ms: number, extra: Partial<TranscriptEvent> = {}): TranscriptEvent {
  return { id, type, cat: categorize(type), t: ms / 1000, at: BASE + ms, detail: type, attempt: 1, dur: 0, ...extra };
}

/** A closing event, carrying the duration back to the event it closes — which is the join. */
function closes(id: number, type: string, ms: number, openerMs: number, extra: Partial<TranscriptEvent> = {}): TranscriptEvent {
  return ev(id, type, ms, { dur: (ms - openerMs) / 1000, ...extra });
}

function name(events: TranscriptEvent[], vocab?: Vocabulary) {
  return readVocabulary({ events }, {}, vocab);
}

function termOf(events: TranscriptEvent[], at = 0): Term {
  return name(events).turns[at]!.term;
}

/* ─────────────── the rule the whole module hangs on ─────────────── */

test('the vocabulary is a read-time function, never a field in the archive', () => {
  it('names a run Temporal has long since dropped, from the reduced log alone', () => {
    const { log } = archive();
    const named = readVocabulary(log.history);

    expect(named.transcript.archived).toBe(true);
    // THE DOMAIN ACCOUNT: what the run set out to do, built, called, wrote and how it ended.
    expect(named.turns.map((t) => t.term)).toEqual([
      'run-started',
      'fleet-ready',
      'dataset-read',
      'method-returned',
      'queue-unpolled',
      'dispatch-stalled',
      'run-worker-lost',
      'run-produced-nothing',
    ]);
    // AND WHAT THE EVENT LOG IS LEFT HOLDING. Not dropped — named, counted, and drawn verbatim in
    // the other pane. The two lists together are the whole reading, in the log's own order.
    expect(named.untranslated.map((t) => t.term)).toEqual([
      'temporal-plumbing',
      'untranslated',
      'temporal-plumbing',
    ]);
    expect(named.turns.length + named.untranslated.length).toBe(named.transcript.turns.length);
  });

  // THE CENTRAL CLAIM. Every stored event carries a Temporal type and Temporal's own category, and
  // not one of this module's words. If a label had been written at archive time it would be in
  // here, and today's mapping would be the only story this run could ever tell.
  it('holds not one word of the vocabulary in the stored bytes', () => {
    const { bytes } = archive();
    for (const term of Object.keys(TERMS)) expect(bytes).not.toContain(term);
    for (const phrase of [
      'queue nobody polled',
      'stalled, retrying',
      'worker stopped answering',
      'produced nothing',
      'method returned',
      'fleet ready',
    ]) {
      expect(bytes.toLowerCase()).not.toContain(phrase);
    }
    // What it holds instead: Temporal's own layers, which is exactly what this replaces.
    const cats = new Set(archive().log.history.events.map((e) => e.cat));
    expect([...cats].sort()).toEqual(['activity', 'child', 'failure', 'signal', 'task', 'workflow']);
  });

  it('reads the same stored run differently the moment the mapping improves', () => {
    const before = archive();
    const today = readVocabulary(before.log.history);
    // Tomorrow's release finds a better sentence for two terms it already had. Nothing is migrated
    // and nothing is rewritten — the run closed months ago.
    const tomorrow = readVocabulary(before.log.history, {}, {
      terms: {
        'fleet-ready': { label: 'machines up and reachable' },
        'run-nothing-worked': { label: 'finished having written nothing, and nothing worked' },
      },
    });

    expect(today.turns[1]!.label).toBe('fleet ready');
    expect(tomorrow.turns[1]!.label).toBe('machines up and reachable');
    expect(today.verdict.label).not.toBe(tomorrow.verdict.label);
    // The chips move with it, because they are built from the same table rather than from a second
    // list somebody has to keep in step.
    expect(tomorrow.filters.find((f) => f.term === 'fleet-ready')!.label).toBe('machines up and reachable');
    // AND THE ARCHIVE DID NOT MOVE. Byte-identical, before and after.
    expect(readFileSync(FIXTURE, 'utf8')).toBe(before.bytes);
  });

  it('gives a name to an event it had no word for yesterday, retroactively', () => {
    const { bytes, log } = archive();
    const today = readVocabulary(log.history);
    // Today it has no word for the signal, so the row is the Event log's.
    expect(today.untranslated[1]).toMatchObject({
      term: 'untranslated',
      label: 'WorkflowExecutionSignaled',
      untranslated: true,
    });
    expect(today.turns.some((t) => t.turn.types.includes('WorkflowExecutionSignaled'))).toBe(false);

    // A later revision learns that a signal on a caller's workflow is an ask coming back. The run it
    // is learning about is over; the improvement reaches it anyway.
    const later = readVocabulary(log.history, {}, {
      name: (turn) => (turn.kind === 'raw' && turn.type === 'WorkflowExecutionSignaled' ? 'ask-answered' : undefined),
    });
    // AND IT CHANGES PANES, which is the half a `kind === 'raw'` split would have got wrong. Learning
    // a word is what promotes a row into the account; the row was never anywhere else to promote.
    expect(later.turns[4]).toMatchObject({ term: 'ask-answered', label: 'ask answered', untranslated: false });
    expect(later.untranslated.map((t) => t.term)).toEqual(['temporal-plumbing', 'temporal-plumbing']);
    expect(readFileSync(FIXTURE, 'utf8')).toBe(bytes);
  });

  // The extension is tried LAST. A revision can teach a new word without any chance of quietly
  // re-pointing one that already reads right — the failure that would make every archived account
  // confidently wrong instead of merely incomplete.
  it('never lets a later revision re-point a term a built-in rule already answered', () => {
    const { log } = archive();
    // Its own doing that the indices are the log's here: a vocabulary that names EVERYTHING leaves
    // nothing for the Event log, so every turn is in the account and in Temporal's own order.
    const greedy = readVocabulary(log.history, {}, { name: () => 'author-note' });
    expect(greedy.untranslated).toEqual([]);
    expect(greedy.turns[2]!.term).toBe('fleet-ready');
    expect(greedy.turns[6]!.term).toBe('queue-unpolled');
    expect(greedy.turns[9]!.term).toBe('run-worker-lost');
  });
});

/* ─────────────── the three that look identical from outside ─────────────── */

/**
 * `history.ts` says it in its own header: a dispatch onto a queue nobody polls, an activity in retry
 * backoff, and a workflow task that timed out "all look identical from outside". They look identical
 * because none of them has a name. These are the three names.
 */
test('the three failures that look identical from outside', () => {
  const { log } = archive();
  const named = readVocabulary(log.history);

  it('names a dispatch nobody ever picked up, from the absence of a Started event', () => {
    const unpolled = named.turns[4]!;
    expect(unpolled.term).toBe('queue-unpolled');
    expect(unpolled.tone).toBe('wrong');
    expect(unpolled.because).toContain('no ActivityTaskStarted event');
    expect(unpolled.because).toContain('probe-0.1.0');
    expect(unpolled.turn.types).toEqual(['ActivityTaskScheduled', 'ActivityTaskTimedOut']);
  });

  it('names an activity between attempts as stalled, off Temporal\'s own attempt counter', () => {
    const stalled = named.turns[5]!;
    expect(stalled.term).toBe('dispatch-stalled');
    expect(stalled.tone).toBe('wrong');
    expect(stalled.because).toBe('attempt=4 and nothing has closed it');
  });

  it('names a workflow task that timed out as the run\'s own worker going quiet', () => {
    const lost = named.turns[6]!;
    expect(lost.term).toBe('run-worker-lost');
    expect(lost.tone).toBe('wrong');
    // Marked NOT plumbing by the reader, and named here — a failure is never bookkeeping.
    expect((lost.turn as { plumbing: boolean }).plumbing).toBe(false);
    // AND IT STAYS IN THE ACCOUNT, though its Temporal type is a `WorkflowTask*` like the three
    // bookkeeping rows either side of it. What sends a row to the Event log is having no word, not
    // having a type that usually means plumbing — this one has a word and it names a disaster.
    expect(named.untranslated.some((t) => t.term === 'run-worker-lost')).toBe(false);
    expect(lost.turn.types).toContain('WorkflowTaskTimedOut');
  });

  it('raises all three as concerns, so a surface showing headlines still shows them', () => {
    expect(named.concerns.map((c) => c.term)).toEqual([
      'queue-unpolled',
      'stalled',
      'open-at-close',
      'worker-lost',
      'produced-nothing',
    ]);
    // Every concern points back at the events it was read from — a concern is never a dead end.
    expect(named.concerns.find((c) => c.term === 'worker-lost')!.events).toEqual([19]);
  });
});

test('what the three are NOT — the distinctions that make them worth naming', () => {
  // 12c: a slow Method and an unpolled queue are the same two rows until you look at whether a
  // worker ever took the work.
  it('a Method that is merely slow reads as an activated Actor, not as an unpolled queue', () => {
    const slow = [
      ev(1, 'ActivityTaskScheduled', 0, { detail: 'activityType=RunBatch · taskQueue=nscheck-0.1.0' }),
      closes(2, 'ActivityTaskStarted', 300, 0),
    ];
    const named = name(slow).turns[0]!;
    expect(named.term).toBe('actor-activated');
    expect(named.tone).toBe('busy');
    expect(neverPickedUp(named.turn)).toBe(false);
  });

  // AND IT REFUSES TO GUESS. With nothing but a scheduling event, "no worker yet" and "no worker
  // ever" are the same two events. Naming it unpolled here would be the confident rename ADR 0027
  // exists to prevent, so it says exactly what it knows and no more.
  it('a dispatch that is only queued is not called unpolled, because nothing proves it yet', () => {
    const queued = [ev(1, 'NexusOperationScheduled', 0, { detail: 'endpoint=kontra-probe-0-1-0 · nexus=kontra.actor/run' })];
    const named = name(queued);
    expect(named.turns[0]!.term).toBe('method-called');
    expect(named.turns[0]!.because).toContain('no worker has taken it yet');
    expect(named.concerns.filter((c) => c.term === 'queue-unpolled')).toHaveLength(0);
  });

  it('separates a Method called through an endpoint from a Batch dispatched onto a queue', () => {
    expect(termOf([ev(1, 'NexusOperationScheduled', 0, { detail: 'endpoint=kontra-probe-0-1-0' })])).toBe('method-called');
    expect(termOf([ev(1, 'ActivityTaskScheduled', 0, { detail: 'activityType=RunBatch · taskQueue=probe-0.1.0' })])).toBe('batch-dispatched');
  });

  it('reads a retry that recovered as a Method that returned, and says how many attempts', () => {
    const recovered = [
      ev(1, 'ActivityTaskScheduled', 0, { detail: 'activityType=RunBatch · taskQueue=nscheck-0.1.0' }),
      closes(2, 'ActivityTaskStarted', 300, 0, { attempt: 3 }),
      closes(3, 'ActivityTaskCompleted', 2000, 300),
    ];
    const named = name(recovered).turns[0]!;
    expect(named.term).toBe('method-returned');
    expect(named.because).toContain('after 3 attempts');
    expect(stalledBecause(named.turn)).toBeUndefined();
  });

  it('reads Temporal\'s own retryState back, because "will retry" and "this is over" differ', () => {
    const inProgress = [
      ev(1, 'ActivityTaskScheduled', 0, { detail: 'activityType=RunBatch · taskQueue=nscheck-0.1.0' }),
      closes(2, 'ActivityTaskStarted', 100, 0),
      closes(3, 'ActivityTaskFailed', 900, 100, { detail: 'dial tcp: i/o timeout · RETRY_STATE_IN_PROGRESS' }),
    ];
    const named = name(inProgress).turns[0]!;
    expect(retryState(named.turn)).toBe('RETRY_STATE_IN_PROGRESS');
    expect(named.term).toBe('dispatch-stalled');

    const exhausted = [
      ev(1, 'ActivityTaskScheduled', 0, { detail: 'activityType=RunBatch · taskQueue=nscheck-0.1.0' }),
      closes(2, 'ActivityTaskStarted', 100, 0),
      closes(3, 'ActivityTaskFailed', 900, 100, { detail: 'dial tcp: i/o timeout · RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED' }),
    ];
    const failed = name(exhausted).turns[0]!;
    expect(failed.term).toBe('method-failed');
    expect(failed.because).toContain('RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED');
  });
});

/* ─────────────── the node whose Units all failed ─────────────── */

/**
 * THE DOCUMENTED FAILURE, AND THE WHOLE REASON THIS IS NOT COSMETIC. A node whose **Units** all
 * failed reports `completed` with empty output, with not one failure event anywhere in its history.
 * Temporal has exactly one word for that run and for a successful one. This module has three.
 */
test('a run that produced nothing never reads as a clean success', () => {
  const done = (extra: TranscriptEvent[] = []) => [
    ev(1, 'WorkflowExecutionStarted', 0, { detail: 'workflowType=NsCheck · taskQueue=recon' }),
    ...extra,
    ev(90, 'WorkflowExecutionCompleted', 10_000),
  ];

  it('gives the empty completion its own term and its own tone', () => {
    const named = name(done());
    expect(named.verdict.term).toBe('run-produced-nothing');
    expect(named.verdict.tone).toBe('wrong');
    expect(named.turns.at(-1)!.term).toBe('run-produced-nothing');
    expect(named.concerns.map((c) => c.term)).toContain('produced-nothing');
  });

  it('separates "published nothing" from "published nothing while everything broke"', () => {
    const broke = done([
      ev(2, 'ActivityTaskScheduled', 1000, { detail: 'activityType=RunBatch · taskQueue=probe-0.1.0' }),
      closes(3, 'ActivityTaskTimedOut', 61_000, 1000, { detail: 'activity ScheduleToStart timeout · RETRY_STATE_TIMEOUT' }),
    ]);
    const named = name(broke);
    expect(named.verdict.term).toBe('run-nothing-worked');
    expect(named.verdict.because).toContain('published=0');
  });

  it('calls a run that published something finished, and nothing else', () => {
    const wrote = done([
      ev(2, 'ActivityTaskScheduled', 1000, { detail: 'activityType=publishBatch · taskQueue=kontra-datasets' }),
      closes(3, 'ActivityTaskCompleted', 1100, 1000),
    ]);
    const named = name(wrote);
    expect(named.verdict.term).toBe('run-finished');
    expect(named.verdict.tone).toBe('ok');
    expect(named.concerns.filter((c) => c.term === 'produced-nothing')).toHaveLength(0);
  });

  it('keeps a failed run and a cancelled one two different words', () => {
    expect(name([ev(1, 'WorkflowExecutionFailed', 0, { detail: 'boom' })]).verdict.term).toBe('run-failed');
    expect(name([ev(1, 'WorkflowExecutionCanceled', 0)]).verdict.term).toBe('run-cancelled');
  });

  it('a run still going is neither, and a continued one is still going', () => {
    expect(name([ev(1, 'WorkflowExecutionStarted', 0)]).verdict.term).toBe('run-running');
    const chained = name([ev(1, 'WorkflowExecutionStarted', 0), ev(2, 'WorkflowExecutionContinuedAsNew', 5000)]);
    expect(chained.verdict.term).toBe('run-handed-over');
    // The reader files continue-as-new as raw; the vocabulary has a word for it anyway, which is
    // this module's job — the reader decides what becomes a turn, this decides what it is called.
    expect(chained.turns[1]!.term).toBe('run-handed-over');
    expect(chained.turns[1]!.untranslated).toBe(false);
  });

  // ADR 0023 §13. The counts ride inside the payload and are ABSENT today; the rule is written and
  // asserted against a turn carrying them, so the day a Summary sets them nothing has to be
  // rediscovered. `nameTurn` is pure over a turn, which is what lets this be tested at all.
  it('a call that returned having dropped every Unit is not a call that returned', () => {
    const call = (units: DispatchTurn['units']): Turn =>
      ({ ...(name([ev(1, 'ActivityTaskScheduled', 0, { detail: 'activityType=RunBatch · taskQueue=nscheck-0.1.0' }), closes(2, 'ActivityTaskStarted', 100, 0), closes(3, 'ActivityTaskCompleted', 900, 100)]).turns[0]!.turn as DispatchTurn), units }) as Turn;

    expect(nameTurn(call(undefined)).term).toBe('method-returned');
    expect(nameTurn(call({ in: 40, out: 40, dropped: 0 })).term).toBe('method-returned');
    expect(nameTurn(call({ in: 40, out: 37, dropped: 3 })).term).toBe('units-isolated');
    const wiped = nameTurn(call({ in: 40, out: 0, dropped: 40 }));
    expect(wiped.term).toBe('every-unit-isolated');
    expect(wiped.tone).toBe('wrong');
    expect(wiped.because).toContain('dropped=40 of in=40');
  });

  it('never invents a zero for counts the log does not carry', () => {
    const { log } = archive();
    for (const t of readVocabulary(log.history).turns) {
      if (t.turn.kind === 'dispatch') expect(t.turn.units).toBeUndefined();
      expect(t.because).not.toContain('dropped=0');
    }
  });
});

/* ─────────────── an unrecognised event renders as itself ─────────────── */

test('an event with no domain word renders as itself, never guessed at', () => {
  it('shows the raw Temporal type as the label and marks it untranslated', () => {
    const named = name([ev(1, 'ExternalWorkflowExecutionSignaled', 0, { detail: 'identity=cli' })]).untranslated[0]!;
    expect(named.term).toBe('untranslated');
    expect(named.label).toBe('ExternalWorkflowExecutionSignaled');
    expect(named.untranslated).toBe(true);
    expect(named.domain).toBe('temporal');
    expect(named.because).toBe('no rule in this release names this type');
  });

  it('files Temporal\'s own bookkeeping where an operator can find it rather than dropping it', () => {
    const named = name([ev(1, 'WorkflowTaskScheduled', 0), ev(2, 'WorkflowTaskStarted', 10), ev(3, 'WorkflowTaskCompleted', 20)]);
    // NOT IN THE ACCOUNT, AND NOT GONE. Three scheduler rows say nothing about what the run did, so
    // the Transcript has none of them — and every one is still named, counted and drilled from the
    // Event log, which is the pane whose job is Temporal's own record.
    expect(named.turns).toEqual([]);
    expect(named.untranslated[0]!.term).toBe('temporal-plumbing');
    expect(named.untranslated[0]!.untranslated).toBe(true);
    expect(named.untranslated[0]!.domain).toBe('temporal');
    expect(named.untranslated[0]!.turn.events).toEqual([1, 2, 3]);
  });

  it('never lets an untranslated failure read as calm', () => {
    const named = name([ev(1, 'ActivityTaskFailed', 0, { detail: 'boom', dur: 5 })]).untranslated[0]!;
    expect(named.untranslated).toBe(true);
    expect(named.tone).toBe('wrong');
  });

  it('names every member of a folded loop when an operator expands it', () => {
    const loop: TranscriptEvent[] = [];
    let id = 1;
    for (let i = 0; i < 3; i += 1) {
      const t = i * 1000;
      loop.push(ev(id++, 'NexusOperationScheduled', t, { detail: 'endpoint=kontra-probe-0-1-0 · nexus=kontra.actor/run' }));
      loop.push(closes(id++, 'NexusOperationCompleted', t + 400, t));
    }
    const head = name(loop).turns[0]!;
    expect(head.turn.count).toBe(3);
    expect(expand(head.turn).map((m) => nameTurn(m).term)).toEqual(['method-returned', 'method-returned', 'method-returned']);
  });
});

/* ─────────────── which pane each turn lands in ─────────────── */

/**
 * THE ASSERTION THAT WAS MISSING, AND THE REASON THE DEFECT SHIPPED.
 *
 * Every suite in this repo could say what a turn is CALLED and none of them said WHERE IT IS DRAWN,
 * so a reading that put Temporal's scheduler bookkeeping between the turns of a run's account was
 * green the whole way. The run below is the one an operator actually read: three workflow tasks, a
 * park, and one sentence a human wrote — which sat in the middle of the machinery and was missed.
 */
test('the account and the log are two panes, and each turn lands in exactly one', () => {
  const events = [
    ev(1, 'WorkflowExecutionStarted', 0, { detail: 'workflowType=Approve · taskQueue=wf-approve' }),
    ev(2, 'WorkflowTaskScheduled', 100),
    ev(3, 'WorkflowTaskStarted', 150),
    ev(4, 'WorkflowTaskCompleted', 200),
    // The author's own line, on the zero-duration timer `speak` writes it as.
    ev(5, 'TimerStarted', 300, { summary: '12 hosts resolved; asking before we touch them' }),
    closes(6, 'TimerFired', 304, 300),
    // The park: its memo, and the deadline timer whose Summary is the ask's own id.
    ev(7, 'WorkflowPropertiesModified', 400, { detail: 'memo=kontra.ask.approve-1' }),
    ev(8, 'TimerStarted', 410, { summary: 'kontra.ask/approve-1' }),
    ev(9, 'WorkflowTaskScheduled', 500),
    ev(10, 'WorkflowTaskStarted', 550),
  ];
  const named = readVocabulary(
    { events },
    { asks: [{ id: 'approve-1', prompt: 'Approve these 12 hosts?', askedAt: BASE + 400 }], now: BASE + 60_400 }
  );

  it('draws the domain account and nothing else on the Transcript', () => {
    expect(named.turns.map((t) => t.term)).toEqual(['run-started', 'author-note', 'run-parked']);
    for (const t of named.turns) {
      expect(t.untranslated, t.label).toBe(false);
      for (const type of t.turn.types) expect(type, t.label).not.toMatch(/^WorkflowTask|^WorkflowPropertiesModified$/);
    }
  });

  it('says what the author said, with their words and not the carrier\'s', () => {
    const note = named.turns.find((t) => t.term === 'author-note')!;
    expect(note.because).toBe('12 hosts resolved; asking before we touch them');
    // BOTH EVENTS OF THE SENTENCE, ONE ROW. The firing stays on its drill path rather than beside
    // it as a second line saying nothing — and it does NOT reappear in the other pane.
    expect(note.turn.events).toEqual([5, 6]);
  });

  it('reads the ask as the ask, not as a second sentence', () => {
    // The park is drawn once, from the ask route, and nothing here turns `kontra.ask/approve-1`
    // into an author's line underneath it.
    expect(named.turns.filter((t) => t.term === 'author-note')).toHaveLength(1);
    expect(named.turns.filter((t) => t.term === 'run-parked')).toHaveLength(1);
    expect(JSON.stringify(named.turns)).not.toContain('kontra.ask');
  });

  it('leaves every event it has no word for to the Event log, verbatim', () => {
    expect(named.untranslated.map((t) => t.label)).toEqual([
      'WorkflowTaskScheduled',
      'WorkflowPropertiesModified',
      'TimerStarted',
      'WorkflowTaskScheduled',
    ]);
    // The ask's deadline timer among them, with the id the SDK put on it — shown as itself rather
    // than promoted to prose.
    expect(named.untranslated.find((t) => t.turn.types.includes('TimerStarted'))!.turn.label).toBe(
      'kontra.ask/approve-1'
    );
  });

  it('drops nothing: every event of the log is in one pane or the other, exactly once', () => {
    const drawn = [...named.turns, ...named.untranslated].flatMap((t) => t.turn.events);
    expect([...drawn].sort((a, b) => a - b)).toEqual(events.map((e) => e.id));
  });
});

/* ─────────────── the log's own caveats ─────────────── */

test('an account with a hole in it says so', () => {
  it('raises the elision as a concern, because every name above it is a name for what survived', () => {
    const named = nameTranscript(readTranscript({ events: [ev(1, 'WorkflowExecutionStarted', 0)], elided: 412, truncated: true }));
    const hole = named.concerns.find((c) => c.term === 'log-elided')!;
    expect(hole.label).toContain('412 events');
    expect(hole.because).toContain('paging stopped');
  });

  it('names work the run stopped waiting for, which is not a Temporal failure at all', () => {
    const abandoned = [
      ev(1, 'WorkflowExecutionStarted', 0),
      ev(2, 'NexusOperationScheduled', 1000, { detail: 'endpoint=kontra-probe-0-1-0 · nexus=kontra.actor/run' }),
      ev(3, 'WorkflowExecutionTerminated', 9000, { detail: 'operator terminated' }),
    ];
    const named = name(abandoned);
    const open = named.concerns.find((c) => c.term === 'open-at-close')!;
    expect(open.events).toEqual([2]);
    expect(open.label).toBe('still outstanding when the run ended');
  });
});

/* ─────────────── the filter bar ─────────────── */

test('filters speak kontra, not Temporal', () => {
  const { log } = archive();
  const named = readVocabulary(log.history);

  it('offers no chip named after one of Temporal\'s layers', () => {
    const layers: EventCategory[] = ['workflow', 'task', 'activity', 'failure', 'timer', 'marker', 'child', 'signal'];
    for (const filter of named.filters) expect(layers).not.toContain(filter.term as EventCategory);
  });

  // THE POINT OF THE REPLACEMENT, IN ONE ASSERTION. Two events Temporal files identically — both
  // `failure` — are two different facts with two different names, and the operator reading them is
  // told which is which instead of being handed one bucket.
  it('cuts across Temporal\'s categories rather than renaming them', () => {
    const byCat = named.turns.filter((t) => t.turn.types.some((type) => /TimedOut$/.test(type)));
    expect(byCat.map((t) => t.term).sort()).toEqual(['queue-unpolled', 'run-worker-lost']);
  });

  it('offers only chips this run actually produced, and counts the calls behind each', () => {
    expect(named.filters.map((f) => f.term)).toEqual([
      'run-started',
      'run-worker-lost',
      'run-produced-nothing',
      'fleet-ready',
      'method-returned',
      'queue-unpolled',
      'dispatch-stalled',
      'dataset-read',
      'temporal-plumbing',
      'untranslated',
    ]);
    // The three folded workflow-task events plus the lone one: a chip counts the CALLS, not the rows,
    // because "4 events of bookkeeping" is the number and "2 rows" is not.
    expect(named.filters.find((f) => f.term === 'temporal-plumbing')!.count).toBe(4);
  });

  it('groups the chips in the order an operator reads a run', () => {
    const seen = named.filters.map((f) => DOMAINS.indexOf(f.domain));
    expect(seen).toEqual([...seen].sort((a, b) => a - b));
  });

  it('labels a chip from the term, even where the row shows a raw Temporal type', () => {
    const named2 = filtersOf(name([ev(1, 'ExternalWorkflowExecutionSignaled', 0)]).untranslated);
    expect(named2[0]!.label).toBe('untranslated');
    expect(named2[0]!.count).toBe(1);
  });

  // NOTHING IS DROPPED, AND THE BAR IS WHERE THAT IS CHECKABLE. The chips are built over BOTH
  // panes, so their counts still sum to every event the log carried — a bar that summed only the
  // rows on screen would quietly teach an operator that the account is the log.
  it('still accounts for the events it is no longer drawing', () => {
    const drawn = named.turns.reduce((n, t) => n + Math.max(1, t.turn.count), 0);
    const elsewhere = named.untranslated.reduce((n, t) => n + Math.max(1, t.turn.count), 0);
    expect(named.filters.reduce((n, f) => n + f.count, 0)).toBe(drawn + elsewhere);
    expect(elsewhere).toBeGreaterThan(0);
  });
});

/* ─────────────── the lake ─────────────── */

test('the lake, in the lake\'s own words', () => {
  const lake = (activity: string, close = 'ActivityTaskCompleted', detail?: string) =>
    name([
      ev(1, 'ActivityTaskScheduled', 0, { detail: `activityType=${activity} · taskQueue=kontra-datasets` }),
      closes(2, close, 500, 0, detail ? { detail } : {}),
    ]).turns[0]!;

  it('names each lake activity by what it did to the Dataset', () => {
    expect(lake('pageDataset').term).toBe('dataset-read');
    expect(lake('openTempDataset').term).toBe('dataset-opened');
    expect(lake('publishBatch').term).toBe('batch-published');
    expect(lake('promoteDataset').term).toBe('dataset-promoted');
    expect(lake('tagDataset').term).toBe('dataset-tagged');
  });

  // SEALED IS READ FROM THE ACT, NOT FROM THE STATE FIELD. `closeDataset` takes its state inside a
  // payload the log never sees, so what this witnesses is whether the caller's declaration landed —
  // "sealed when the caller declares it complete" (CONTEXT.md) — and never the payload's own word.
  it('calls a Dataset sealed when the close landed, and unsealed when it did not', () => {
    expect(lake('closeDataset').term).toBe('dataset-sealed');
    expect(lake('closeDataset').because).toContain('closeDataset completed');
    const failed = lake('closeDataset', 'ActivityTaskFailed', 'lake unreachable');
    expect(failed.term).toBe('dataset-unsealed');
    expect(failed.tone).toBe('wrong');
    const inflight = name([ev(1, 'ActivityTaskScheduled', 0, { detail: 'activityType=closeDataset · taskQueue=kontra-datasets' })]).turns[0]!;
    expect(inflight.term).toBe('dataset-sealing');
  });
});

/* ─────────────── the fleet ─────────────── */

test('a fleet operation, by what happened to it', () => {
  const child = (type: string, extra: Partial<TranscriptEvent> = {}) =>
    name([
      ev(1, 'StartChildWorkflowExecutionInitiated', 0, {
        detail: 'workflowType=stackWorkflow',
        link: { workflowId: 'kontra-fleet/dns', type: 'stackWorkflow', via: 'child' },
      }),
      closes(2, type, 5000, 0, { link: { workflowId: 'kontra-fleet/dns', execId: 'abc', type: 'stackWorkflow', via: 'child' }, ...extra }),
    ]).turns[0]!;

  it('names the four states a fleet operation can be read in', () => {
    expect(termOf([ev(1, 'StartChildWorkflowExecutionInitiated', 0, { detail: 'workflowType=stackWorkflow', link: { workflowId: 'kontra-fleet/dns', type: 'stackWorkflow', via: 'child' } })])).toBe('fleet-requested');
    expect(child('ChildWorkflowExecutionStarted').term).toBe('fleet-building');
    expect(child('ChildWorkflowExecutionCompleted').term).toBe('fleet-ready');
    expect(child('ChildWorkflowExecutionFailed').term).toBe('fleet-failed');
  });

  it('says which child workflow it read that from', () => {
    expect(child('ChildWorkflowExecutionCompleted').because).toContain('kontra-fleet/dns');
  });
});

/* ─────────────── the rule this module lives by ─────────────── */

test('every name comes from metadata, and every name says where from', () => {
  it('carries a reason on every turn and every concern', () => {
    const { log } = archive();
    const named = readVocabulary(log.history);
    for (const turn of named.turns) expect(turn.because.length).toBeGreaterThan(0);
    for (const concern of named.concerns) expect(concern.because.length).toBeGreaterThan(0);
    expect(named.verdict.because.length).toBeGreaterThan(0);
  });

  it('reads nothing that is not in the reduced log — no payload key exists to read', () => {
    const { log } = archive();
    for (const event of log.history.events) {
      expect(Object.keys(event).sort()).toEqual(
        expect.arrayContaining(['at', 'attempt', 'cat', 'detail', 'dur', 'id', 't', 'type'])
      );
      for (const forbidden of ['input', 'result', 'payload', 'payloads', 'failure']) {
        expect(event).not.toHaveProperty(forbidden);
      }
    }
  });

  it('agrees with the reducer about what counts as a failure', () => {
    const { log } = archive();
    for (const event of log.history.events) expect(event.cat).toBe(categorize(event.type));
  });

  it('has a spec for every term it can produce', () => {
    for (const term of Object.keys(TERMS) as Term[]) {
      expect(TERMS[term].label.length).toBeGreaterThan(0);
      expect(DOMAINS).toContain(TERMS[term].domain);
    }
  });
});

test('a narration carries the author\'s words, not a description of the carrier', () => {
  // THE BUG THIS PINS. `nameTurn` used to answer 'a Summary the author set on their own event'
  // for every narration — which describes the MECHANISM and discards the cargo, so two different
  // sentences rendered as the same row and a reader learned nothing from the second. `speak`
  // exists to put words on this line.
  it("uses the author's sentence as the detail", () => {
    const named = nameTurn({
      kind: 'narration',
      text: '12 hosts staged; holding for approval',
      open: false,
      at: 0,
      ids: [1],
    } as never);
    expect(named.because).toBe('12 hosts staged; holding for approval');
    expect(named.label).toBe('note');
  });

  it('says an empty sentence is empty rather than drawing a blank row', () => {
    // A narration that reached the log carrying nothing is an author's bug, and should look
    // like one rather than like a row that failed to render.
    const named = nameTurn({ kind: 'narration', text: '', open: false, at: 0, ids: [1] } as never);
    expect(named.because).toBe('(an empty sentence)');
  });

  it('gives two different sentences two different rows', () => {
    const a = nameTurn({ kind: 'narration', text: 'batch 1 of 12', open: false, at: 0, ids: [1] } as never);
    const b = nameTurn({ kind: 'narration', text: 'batch 2 of 12', open: false, at: 1, ids: [2] } as never);
    expect(a.because).not.toBe(b.because);
  });
});

/**
 * A MACHINE'S LIFECYCLE, IN THE ACCOUNT RATHER THAN IN THE LOG.
 *
 * `cli/warden/warden_workflow.go` runs one blocked workflow per **Machine** and writes each decision as a
 * Temporal user-metadata Summary on the event that arms the next watch. Nothing about that reaches a
 * component: `transcript.ts` already folds a Summary onto its turn as `label`, and what this release
 * adds is a WORD for it — which is precisely the change {@link nameTranscript} was built to absorb
 * with no surface moving.
 *
 * Every event below is the real shape, taken from a history this repo measured: an
 * `ActivityTaskScheduled` for an activity type this module does not know, carrying a Summary.
 * Nothing here is invented from a payload.
 */
test("a Machine's lifecycle reads as an account, not as ActivityTaskScheduled", () => {
  const watch = (id: number, ms: number, summary: string): TranscriptEvent =>
    ev(id, 'ActivityTaskScheduled', ms, { detail: 'activityType=wardenWatch', summary });

  it('names each of the five things a Warden can decide', () => {
    const named = name([
      watch(1, 0, 'kontra.machine · watching · wdn-abc123 · attached from kf-dns-01 via podman'),
      watch(2, 1000, 'kontra.machine · assignment changed · wdn-abc123 · +nscheck@0.1.0'),
      watch(3, 2000, 'kontra.machine · worker started · nscheck@0.1.0'),
      watch(4, 3000, 'kontra.machine · worker exited · nscheck@0.1.0 · only its handler half is running'),
      watch(5, 4000, "kontra.machine · machine unreachable · wdn-abc123 · nothing on this Machine's queue took the watch (miss 1; next watch waits 2m0s)"),
    ]);
    expect(named.turns.map((t) => t.term)).toEqual([
      'machine-watching',
      'machine-assignment-changed',
      'machine-worker-started',
      'machine-worker-exited',
      'machine-unreachable',
    ]);
    // NONE OF THEM IS LEFT IN THE EVENT LOG. Before this release every one of these rows was an
    // `untranslated` ActivityTaskScheduled and drew in the other pane; that is the whole of what
    // "visible in the Transcript" means here.
    expect(named.untranslated).toEqual([]);
  });

  it("quotes the Warden's own words rather than paraphrasing them", () => {
    const named = name([
      watch(1, 0, 'kontra.machine · worker exited · nscheck@0.1.0 · only its handler half is running'),
    ]);
    expect(named.turns[0]!.because).toBe('nscheck@0.1.0 · only its handler half is running');
    expect(named.turns[0]!.label).toBe('worker exited');
  });

  // A WORKER THAT STOPPED WITHOUT BEING ASKED TO IS NOT A GREEN ROW, and a Machine that went quiet
  // is not either. The Warden restarts the Worker, so the FIX is invisible from outside; a surface
  // that toned the recovery and not the fault would hide a crash loop behind a healthy Fleet.
  it('tones a fault as a fault and an ordinary change as ordinary', () => {
    const tones = (summary: string) => name([watch(1, 0, summary)]).turns[0]!.tone;
    expect(tones('kontra.machine · worker exited · a@1')).toBe('wrong');
    expect(tones('kontra.machine · machine unreachable · wdn-abc123')).toBe('wrong');
    expect(tones('kontra.machine · assignment changed · wdn-abc123 · +a@1')).toBe('ok');
    expect(tones('kontra.machine · worker started · a@1')).toBe('ok');
  });

  // VERSION SKEW IS A PERMANENT COST (ADR 0037), so a control plane reading a NEWER Warden must not
  // guess. An unknown kind stays exactly what it was — an untranslated event, verbatim, in the Event
  // log — which is this module's first principle and the opposite of a confident wrong name.
  it('leaves a decision it has no word for in the Event log, verbatim', () => {
    const named = name([watch(1, 0, 'kontra.machine · egress blocked · nscheck@0.1.0')]);
    expect(named.turns).toEqual([]);
    expect(named.untranslated.map((t) => t.label)).toEqual(['ActivityTaskScheduled']);
    expect(named.untranslated[0]!.untranslated).toBe(true);
  });

  it('does not claim a Summary that merely mentions a machine', () => {
    for (const summary of [
      'kontra.machine',
      'kontra.machines · worker exited · a@1',
      'crawl · crawler@0.1.0[acme.com] · 12 units',
      'a note about kontra.machine · worker exited',
    ]) {
      expect(name([watch(1, 0, summary)]).turns.map((t) => t.term)).toEqual([]);
    }
  });

  // ONE ROW PER DECISION, IN ORDER — a `transcript.ts` property, and it was a real defect found
  // against the real shape of a Warden's watcher (one `ActivityTaskScheduled` per decision, forever).
  // A raw turn's collapse key was its Temporal TYPE alone, so a Machine's whole life folded into a
  // single line wearing the FIRST sentence and a count: `watching ×4`.
  //
  // AND THE SECOND HALF IS THE ORDERING. A group is anchored at its first member within a
  // barrier-free region, so `started, exited, started` folded the second start UP into the first —
  // rendering a crash loop as one start ×2 above the exit that came between them.
  it('gives every decision its own row, in the order they were decided', () => {
    const named = name([
      watch(1, 0, 'kontra.machine · watching · wdn-abc123'),
      watch(2, 1000, 'kontra.machine · worker started · a@1'),
      watch(3, 2000, 'kontra.machine · worker exited · a@1 · both halves are gone'),
      watch(4, 3000, 'kontra.machine · worker started · a@1'),
    ]);
    expect(named.turns.map((t) => t.term)).toEqual([
      'machine-watching',
      'machine-worker-started',
      'machine-worker-exited',
      'machine-worker-started',
    ]);
    expect(named.turns.every((t) => t.turn.count === 1)).toBe(true);

    // Even two decisions that read identically stay two rows: they happened at two instants and
    // each one is a sentence about its own. Same rule a narration has had all along.
    const twice = name([
      watch(1, 0, 'kontra.machine · worker exited · a@1'),
      watch(2, 1000, 'kontra.machine · worker exited · a@1'),
    ]);
    expect(twice.turns.map((t) => t.turn.count)).toEqual([1, 1]);
  });

  /**
   * THE CROSS-LANGUAGE LITERAL, PINNED AGAINST THE GO THAT WRITES IT.
   *
   * There is no import to make — that side is Go, this side is bundled into the browser — so the
   * house rule is the one `transcript.test.ts` already applies to the ask's memo prefix: write the
   * literal on each side and read the other side's BYTES. A rename in `warden_workflow.go` is a red
   * test here rather than a Fleet that silently stops appearing in anybody's account.
   */
  it('spells every kind the way cli/warden/warden_workflow.go spells it', () => {
    const go = readFileSync(join(__dirname, '..', '..', '..', 'cli', 'warden', 'warden_workflow.go'), 'utf8');
    expect(go).toContain('wardenSummaryPrefix = "kontra.machine"');
    expect(go).toContain('wardenSummarySep = " · "');
    for (const kind of [
      'watching',
      'assignment changed',
      'worker started',
      'worker exited',
      'machine unreachable',
    ]) {
      expect(go).toContain(`wardenDecisionKind = "${kind}"`);
    }
    // …and the Go side has no kind this release cannot name, which is the direction a test that
    // only checked the five above would miss.
    expect([...go.matchAll(/wardenDecisionKind = "([^"]+)"/g)].map((m) => m[1]!)).toHaveLength(5);

    // THE WORKFLOW TYPE IS THE SAME KIND OF LITERAL, and it is load-bearing somewhere else: a
    // Warden's watcher never closes, so `visibility.ts` has to subtract it from run discovery or a
    // ten-Machine Fleet is ten rows on the Runs page that never finish.
    expect(go).toContain('wardenWorkflowType = "wardenWorkflow"');
    expect(KONTRA_INTERNAL_WORKFLOW_TYPES).toContain('wardenWorkflow');
  });
});
