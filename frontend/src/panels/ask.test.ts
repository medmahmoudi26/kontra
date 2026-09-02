/**
 * One ask, read: the ending the run declared, the clock it is measured on, and the form it is
 * answered with.
 *
 * NODE, NO JSDOM, NO CLOCK. Every `now` here is a literal, which is the whole reason this reading
 * lives apart from the component that draws it: "parked for four minutes" is an assertion rather
 * than a wait, and "waits indefinitely" is a string somebody can pin.
 *
 * WHAT THESE TESTS ARE REALLY GUARDING is that nothing here re-decides what the RUN already said.
 * The four endings are the workflow's own word (`hitl.ts`: "declared by the RUN, not inferred
 * here"), so an ask with no `answeredAt` is not automatically "waiting" and a deadline this browser
 * can see has passed is not automatically "expired" — the first would call a cancelled run's
 * question an unanswered one, and the second would let a browser overrule a workflow.
 */

import { describe, expect, it } from 'vitest';

import {
  ATTRIBUTION_NOTE,
  askForm,
  attributionWords,
  deadlineWords,
  parkedRun,
  parkedWords,
  readAsk,
  readAsks,
  shapeWords,
  standingWords,
  stillParked,
  waitedWords,
  waitingAsks,
  type ParkedRun,
} from './ask';
import { answerOf } from './ask';
import { setField } from './workflowInput';
import type { RunAsk } from '../run/turns';
import type { RunRow } from '../run/api';

const NOW = 1_700_000_000_000;
const MINUTE = 60_000;
const HOUR = 60 * MINUTE;

/** The shape `GET /api/runs/:id/asks` sends, built with only what a case is about. */
function ask(over: Partial<RunAsk> = {}): RunAsk {
  return {
    id: 'ask-1',
    prompt: 'Approve these 12 hosts?',
    askedAt: NOW - 4 * MINUTE,
    state: 'pending',
    waitedMs: 4 * MINUTE,
    ...over,
  };
}

const APPROVAL = {
  type: 'object',
  properties: {
    approve: { type: 'boolean' },
    note: { type: 'string' },
  },
  required: ['approve'],
};

/* ───────────────────────────── the ending is the run's ───────────────────────────── */

describe('how an ask stands', () => {
  it('a pending ask on an open run is waiting on the person reading', () => {
    const r = readAsk(ask(), NOW);
    expect(r.standing).toBe('waiting');
    expect(r.answerable).toBe(true);
    expect(standingWords(r).label).toBe('waiting on you');
  });

  it('an answered ask is answered, and stops its clock at the answer', () => {
    const r = readAsk(
      ask({ state: 'answered', askedAt: NOW - HOUR, answeredAt: NOW - 58 * MINUTE, by: 'mo' }),
      NOW
    );
    expect(r.standing).toBe('answered');
    expect(r.answerable).toBe(false);
    // TWO MINUTES, NOT AN HOUR. A settled ask measured against the wall clock would grow every
    // render on a run that finished last week.
    expect(r.waitedMs).toBe(2 * MINUTE);
    expect(waitedWords(r)).toBe('waited 2m 00s');
  });

  it('an expired ask reads as expired, and never as a human who was too slow to matter', () => {
    const r = readAsk(ask({ state: 'expired', deadlineAt: NOW - MINUTE }), NOW);
    expect(r.standing).toBe('expired');
    expect(r.answerable).toBe(false);
    expect(standingWords(r).label).toBe('expired');
    expect(deadlineWords(r)).toBe('the deadline passed with nobody answering');
  });

  it('an abandoned ask says the run ended, NOT that nobody answered in time', () => {
    const r = readAsk(ask({ state: 'abandoned' }), NOW);
    expect(r.standing).toBe('abandoned');
    const said = standingWords(r);
    expect(said.label).toBe('abandoned');
    // The distinction the four states exist for: a cancelled run must not read as a missed deadline.
    expect(said.because).toContain('nobody failed to answer in time');
    expect(said.because).not.toContain('deadline the author declared passed');
  });

  it('a state the memo carried but this release does not know falls back to waiting', () => {
    // The route already floors an unreadable `state` at `pending`; this is the second floor, so a
    // future fifth word cannot make a parked run render as nothing at all.
    const r = readAsk(ask({ state: 'sleeping' as RunAsk['state'] }), NOW);
    expect(r.standing).toBe('waiting');
  });

  it('a pending ask on a run that CLOSED is stranded — not expired, not abandoned, not answerable', () => {
    const r = readAsk(ask(), NOW, false);
    expect(r.stranded).toBe(true);
    expect(r.answerable).toBe(false);
    expect(r.overdue).toBe(false);
    const said = standingWords(r);
    expect(said.label).toBe('never answered');
    expect(said.because).toContain('nobody will answer it now');
  });
});

/* ───────────────────────────── the clock ───────────────────────────── */

describe('time parked, and time left', () => {
  it('a pending ask is measured to now and says it is parked', () => {
    expect(waitedWords(readAsk(ask({ askedAt: NOW - 4 * MINUTE - 12_000 }), NOW))).toBe('parked 4m 12s');
  });

  it('an ask with a deadline shows what is left of it', () => {
    const r = readAsk(ask({ deadlineAt: NOW + 2 * MINUTE + 48_000 }), NOW);
    expect(r.deadline).toEqual({ kind: 'left', ms: 2 * MINUTE + 48_000 });
    expect(deadlineWords(r)).toBe('2m 48s left');
  });

  it('an ask with NO deadline says it waits indefinitely rather than showing a blank', () => {
    const r = readAsk(ask(), NOW);
    expect(r.deadline).toEqual({ kind: 'none' });
    const said = deadlineWords(r);
    expect(said).toBe('no deadline — this run waits indefinitely');
    // The two failures this replaces: an empty slot, and a countdown stopped at zero.
    expect(said).not.toBe('');
    expect(said).not.toContain('0s');
  });

  it('a deadline of zero is absent, not an instant in 1970', () => {
    expect(readAsk(ask({ deadlineAt: 0 }), NOW).deadline).toEqual({ kind: 'none' });
  });

  it('a deadline gone by on a run that has not said so yet is OVERDUE, not expired', () => {
    // The gap is real and is exactly the case this surface exists for: the memo only becomes
    // `expired` when the workflow's timer fires, which needs a worker. A parked run whose worker is
    // down sits here indefinitely, and calling it expired on the browser's authority would hide it.
    const r = readAsk(ask({ deadlineAt: NOW - 3 * MINUTE }), NOW);
    expect(r.standing).toBe('waiting');
    expect(r.overdue).toBe(true);
    expect(r.answerable).toBe(true);
    expect(deadlineWords(r)).toBe('the deadline passed 3m 00s ago');
    expect(standingWords(r).because).toContain('worker is down');
  });

  it('an answered ask reports the room it had, measured to the answer', () => {
    const r = readAsk(
      ask({ state: 'answered', answeredAt: NOW - HOUR, deadlineAt: NOW + HOUR }),
      NOW
    );
    expect(deadlineWords(r)).toBe('answered with 2h 00m to spare');
  });

  it('an entry with no readable instant has no elapsed time, and says so', () => {
    const r = readAsk(ask({ askedAt: 0, malformed: true }), NOW);
    expect(r.waitedMs).toBeNull();
    // Neither `0s` nor fifty-six years.
    expect(waitedWords(r)).toBe('how long it has waited is not readable from this entry');
  });
});

/* ───────────────────────────── several at once ───────────────────────────── */

describe('a run with several asks', () => {
  const asks = [
    ask({ id: 'ask-1', prompt: 'Approve these 12 hosts?' }),
    ask({ id: 'ask-2', prompt: 'Which region?', state: 'answered', answeredAt: NOW - MINUTE, by: 'mo' }),
    ask({ id: 'ask-3', prompt: 'Raise the cap?' }),
  ];

  it('reads every one of them, in the order given', () => {
    expect(readAsks(asks, NOW).map((r) => r.ask.id)).toEqual(['ask-1', 'ask-2', 'ask-3']);
  });

  it('only the pending ones are waiting on a human — one answered leaves the others', () => {
    expect(waitingAsks(readAsks(asks, NOW)).map((r) => r.ask.id)).toEqual(['ask-1', 'ask-3']);
  });

  it('a closed run leaves NONE of them waiting, however the memo reads', () => {
    expect(waitingAsks(readAsks(asks, NOW, false))).toEqual([]);
  });
});

/* ───────────────────────────── the operator label ───────────────────────────── */

describe('the operator label is attribution', () => {
  it('names who said they decided it, and says that is what the name is', () => {
    expect(attributionWords({ by: 'mo' })).toBe('answered by mo, who said so themselves');
    expect(ATTRIBUTION_NOTE).toContain('Attribution, not authentication');
    expect(ATTRIBUTION_NOTE).toContain('checks nothing');
  });

  it('never implies the name was checked, verified, confirmed or authorised', () => {
    const said = `${attributionWords({ by: 'mo' })} ${ATTRIBUTION_NOTE}`.toLowerCase();
    for (const claim of ['verified', 'confirmed by', 'authenticated', 'signed in as', 'authorised', 'authorized']) {
      expect(said).not.toContain(claim);
    }
  });

  it('an unlabelled answer stays unlabelled — never `operator`, never a hostname', () => {
    const said = attributionWords({});
    expect(said).toBe('answered — whoever answered left no name');
    expect(said).not.toContain('operator');
    expect(attributionWords({ by: '   ' })).toBe(said);
  });
});

/* ───────────────────────────── the form ───────────────────────────── */

describe('an ask renders through the shared form renderer', () => {
  it('a declared shape becomes fields, through the same reading a run start uses', () => {
    const form = askForm(ask({ schema: APPROVAL }));
    expect(form.shape).toBe('fields');
    expect(form.draft.kind).toBe('fields');
    if (form.draft.kind !== 'fields') throw new Error('unreachable');
    expect(form.draft.fields.map((f) => f.name)).toEqual(['approve', 'note']);
    expect(form.draft.fields.find((f) => f.name === 'approve')?.required).toBe(true);
  });

  it('the collected answer is coerced by the declared type, not sent as strings', () => {
    const form = askForm(ask({ schema: APPROVAL }));
    const filled = setField(setField(form.draft, 'approve', 'true'), 'note', 'looks right');
    expect(answerOf(filled)).toEqual({ value: { approve: true, note: 'looks right' } });
  });

  it('a required field left blank refuses BEFORE anything is signalled', () => {
    const got = answerOf(askForm(ask({ schema: APPROVAL })).draft);
    expect('error' in got && got.error).toContain('approve is required');
  });

  it('an ask that declared no shape gets a JSON box, and says the author left it open', () => {
    const form = askForm(ask());
    expect(form.shape).toBe('free');
    expect(form.draft.kind).toBe('json');
    expect(shapeWords('free')).toContain('declares no shape');
  });

  it('an open shape is a different sentence from no shape at all', () => {
    const form = askForm(ask({ schema: { type: 'object', additionalProperties: true } }));
    expect(form.shape).toBe('open');
    expect(shapeWords('open')).not.toBe(shapeWords('free'));
  });

  it('something that is not a schema at all is refused here rather than after submitting', () => {
    const form = askForm(ask({ schema: 'Approval' }));
    expect(form.shape).toBe('unreadable');
    expect(shapeWords('unreadable')).toContain('cannot be answered from here');
    expect(shapeWords('unreadable')).toContain('still parked');
  });

  it('a settled ask has no form — the run drops the schema when it closes the ask', () => {
    expect(askForm(ask({ state: 'answered', answeredAt: NOW })).shape).toBe('settled');
    expect(askForm(ask({ state: 'expired' })).shape).toBe('settled');
  });

  it('a nested model nests, because the reading is `schemaTree` and not a second one', () => {
    const form = askForm(
      ask({
        schema: {
          type: 'object',
          properties: { retry: { $ref: '#/$defs/Retry' } },
          $defs: { Retry: { type: 'object', properties: { tries: { type: 'integer' } } } },
        },
      })
    );
    if (form.draft.kind !== 'fields') throw new Error('unreachable');
    const retry = form.draft.fields[0]!;
    expect(retry.kind).toBe('group');
    expect(retry.children?.map((c) => c.path)).toEqual(['retry.tries']);
  });
});

/* ───────────────────────────── the chrome ───────────────────────────── */

describe('what the chrome is told', () => {
  function run(runId: string, status: RunRow['status']): RunRow {
    return {
      runId,
      type: 'DnsSweep',
      status,
      tenant: 'default',
      startedAt: NOW - HOUR,
      closedAt: status === 'running' ? 0 : NOW,
      dispatches: 3,
    };
  }

  it('a run with a pending ask is parked, dated from the OLDEST question', () => {
    const got = parkedRun('sweep-1', 'dnssweep', [
      ask({ id: 'ask-1', askedAt: NOW - 10 * MINUTE }),
      ask({ id: 'ask-2', askedAt: NOW - 2 * MINUTE }),
      ask({ id: 'ask-0', state: 'answered', askedAt: NOW - HOUR, answeredAt: NOW - 30 * MINUTE }),
    ]);
    expect(got).toEqual({ runId: 'sweep-1', workflow: 'dnssweep', pending: 2, since: NOW - 10 * MINUTE });
  });

  it('a run whose asks are all settled is not parked at all', () => {
    expect(parkedRun('sweep-1', 'dnssweep', [ask({ state: 'expired' })])).toBeNull();
    expect(parkedRun('sweep-1', 'dnssweep', [])).toBeNull();
  });

  it('an unreadable instant contributes no date rather than dating the park to 1970', () => {
    expect(parkedRun('sweep-1', 'dnssweep', [ask({ askedAt: 0, malformed: true })])?.since).toBe(0);
  });

  it('a run the list says has CLOSED stops being signalled', () => {
    const parked: ParkedRun[] = [
      { runId: 'sweep-1', workflow: 'dnssweep', pending: 1, since: NOW },
      { runId: 'sweep-2', workflow: 'dnssweep', pending: 2, since: NOW },
    ];
    const kept = stillParked(parked, [run('sweep-1', 'failed'), run('sweep-2', 'running')]);
    expect(kept.map((p) => p.runId)).toEqual(['sweep-2']);
  });

  it('a run the list has NOT DISCOVERED yet is kept — absence is not evidence', () => {
    // `/api/runs` lists runs that have DISPATCHED, so a run parked before its first dispatch is
    // missing from the list. Dropping it would hide the one park that most needs a human.
    const parked: ParkedRun[] = [{ runId: 'brand-new', workflow: 'dnssweep', pending: 1, since: NOW }];
    expect(stillParked(parked, [run('sweep-1', 'running')])).toEqual(parked);
  });

  it('counts runs and questions apart, because they are two facts', () => {
    expect(
      parkedWords([
        { runId: 'a', workflow: 'w', pending: 2, since: NOW },
        { runId: 'b', workflow: 'w', pending: 1, since: NOW },
      ])
    ).toBe('2 runs are parked, waiting on a human — 3 questions unanswered');
    expect(parkedWords([{ runId: 'a', workflow: 'w', pending: 1, since: NOW }])).toBe(
      '1 run is parked, waiting on a human — 1 question unanswered'
    );
    expect(parkedWords([])).toBe('');
  });
});
