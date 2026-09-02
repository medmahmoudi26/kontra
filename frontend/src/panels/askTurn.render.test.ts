/**
 * HITL in the transcript, drawn — every state an operator can find a parked run in.
 *
 * NODE, NO JSDOM, NO TESTING-LIBRARY, like every render suite here: `renderToStaticMarkup` and
 * string assertions about text and `data-testid`. `AskTurn` takes props and returns markup, and the
 * draft lives one level up, precisely so that a form half-filled by an operator is a literal in this
 * file rather than a click in a browser.
 *
 * THE STATES THIS SLICE IS ACCOUNTABLE FOR, all of them here: one pending, several pending,
 * partially answered, answered, expired, an unreadable entry, and an ask whose author attached no
 * context. Every one of them is a real thing a run can be in, and the last two are the ones a
 * surface quietly breaks on — a run parked on a question nobody can read is exactly the run an
 * operator most needs a page for.
 *
 * AND THE ONE OUTCOME THAT IS NOT ALLOWED. A malformed ask must degrade to something legible and
 * must never blank the page: the run is parked either way, and a blank surface is the only outcome
 * that makes it unrecoverable from the UI. That is asserted as markup rather than as an intention —
 * the row is on the page, with a sentence on it, and the asks beside it still draw.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import { categorize } from '@core/history';

import type { RunEvent, RunHistory } from '../run/api';
import { readRunTurns, type RunAsk } from '../run/turns';
import { AskTurn, type AskDeck } from './AskTurn';
import { askForm, readAsk } from './ask';
import { NavRail, type NavRailProps } from './SideNav';
import { readPulse, type PulsePark } from './chrome/pulse';
import { TranscriptView } from './Transcript';
import { setField } from './workflowInput';
import type { View } from '../state/surfaces';

const BASE = 1_786_831_339_151;
const RUN = 'dnssweep-1786831339';
const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
/** The clock every reading here is taken against — four minutes into the run. */
const NOW = BASE + 4 * MINUTE;

function ev(id: number, type: string, ms: number, extra: Partial<RunEvent> = {}): RunEvent {
  return { id, type, cat: categorize(type), t: ms / 1000, at: BASE + ms, detail: type, attempt: 1, dur: 0, ...extra };
}

function history(events: RunEvent[], extra: Partial<RunHistory> = {}): RunHistory {
  return { events, scanned: events.length, elided: 0, truncated: false, ...extra };
}

const STARTED = ev(1, 'WorkflowExecutionStarted', 0, {
  detail: 'workflowType=DnsSweep · taskQueue=wf-dnssweep',
});
const CLOSED = ev(2, 'WorkflowExecutionCompleted', 3 * MINUTE);

function ask(over: Partial<RunAsk> = {}): RunAsk {
  return {
    id: 'ask-1',
    prompt: 'Approve these 12 hosts?',
    askedAt: BASE + 30_000,
    state: 'pending',
    waitedMs: 0,
    ...over,
  };
}

const APPROVAL = {
  type: 'object',
  properties: {
    approve: { type: 'boolean' },
    scope: { type: 'string', enum: ['listed', 'everything'] },
  },
  required: ['approve'],
};

const CONTEXT = { dataset: 'live-hosts', hosts: 12, sample: ['a.example', 'b.example'] };

/** A deck that collects nothing — the states below are about what is DRAWN, not what is typed. */
function deck(over: Partial<AskDeck> = {}): AskDeck {
  return {
    drafts: {},
    onDraft: () => {},
    by: '',
    onBy: () => {},
    onAnswer: () => {},
    sending: null,
    refused: {},
    ...over,
  };
}

function drawAsk(one: RunAsk, over: { deck?: AskDeck | null; now?: number; live?: boolean } = {}): string {
  const reading = readAsk(one, over.now ?? NOW, over.live ?? true);
  const withDeck = over.deck === null ? {} : { deck: over.deck ?? deck() };
  return renderToStaticMarkup(
    createElement(AskTurn, { reading, testId: `ask-${one.id}`, ...withDeck })
  );
}

/** The whole transcript, with the asks merged into it exactly as the page does it. */
function drawRun(asks: RunAsk[], events: RunEvent[] = [STARTED], over: { deck?: AskDeck } = {}): string {
  const turns = readRunTurns(RUN, history(events), asks, NOW);
  return renderToStaticMarkup(
    createElement(TranscriptView, { turns, now: NOW, deck: over.deck ?? deck() })
  );
}

/* ───────────────────────────── one pending ask ───────────────────────────── */

describe('one pending ask', () => {
  const html = drawAsk(ask({ schema: APPROVAL, context: CONTEXT }));

  it('says the run is waiting on the person reading, and asks the question', () => {
    expect(html).toContain('data-standing="waiting"');
    expect(html).toContain('data-answerable="true"');
    expect(html).toContain('waiting on you');
    expect(html).toContain('Approve these 12 hosts?');
  });

  it('renders the declared shape as fields, through the SHARED renderer', () => {
    // The ids are `FieldGroup`'s own, under this ask's prefix — which is the proof that the form
    // starting a run, the form calling a Method and this one are one component and not three.
    expect(html).toContain('data-testid="ask-ask-1-field-approve"');
    expect(html).toContain('data-testid="ask-ask-1-field-scope"');
    // A closed set is a `<select>` here exactly as it is everywhere else.
    expect(html).toContain('<option value="listed">listed</option>');
    expect(html).toContain('data-shape="fields"');
  });

  it('shows the context beside the question, so answering costs no second tab', () => {
    expect(html).toContain('data-testid="ask-ask-1-context"');
    expect(html).toContain('live-hosts');
    expect(html).toContain('data-testid="ask-ask-1-context-hosts"');
    expect(html).toContain('a.example');
  });

  it('shows time parked, and says it waits indefinitely where no deadline was declared', () => {
    expect(html).toContain('parked 3m 30s');
    expect(html).toContain('no deadline — this run waits indefinitely');
  });

  it('offers the answer under the operator label, worded as attribution', () => {
    expect(html).toContain('data-testid="ask-ask-1-submit"');
    expect(html).toContain('data-testid="ask-ask-1-by"');
    expect(html).toContain('answering as');
    expect(html).toContain('It is not a sign-in');
  });

  it('shows time remaining where a deadline exists', () => {
    const soon = drawAsk(ask({ schema: APPROVAL, deadlineAt: NOW + 2 * MINUTE + 48_000 }));
    expect(soon).toContain('2m 48s left');
    expect(soon).not.toContain('waits indefinitely');
  });

  it('refuses to submit an answer its own schema does not accept, before anything is signalled', () => {
    // `approve` is required and blank, so the button is disabled and the field is named. A signal
    // is durable and cannot be taken back, which is why this is caught on this side too.
    expect(html).toContain('data-testid="ask-ask-1-error"');
    expect(html).toContain('approve is required');
    // `disabled=""`, not `disabled` — the button's own class list carries `disabled:opacity-50`,
    // and a looser pattern would match that on an ENABLED button and never fail.
    expect(html).toMatch(/data-testid="ask-ask-1-submit"[^>]*disabled=""/);
  });

  it('a filled-in draft passes, and the button is live', () => {
    const form = setField(
      setField(askForm(ask({ schema: APPROVAL })).draft, 'approve', 'true'),
      'scope',
      'listed'
    );
    const html2 = drawAsk(ask({ schema: APPROVAL }), { deck: deck({ drafts: { 'ask-1': form } }) });
    expect(html2).not.toContain('data-testid="ask-ask-1-error"');
    expect(html2).not.toMatch(/data-testid="ask-ask-1-submit"[^>]*disabled=""/);
  });

  it('an answer already in flight refuses a second click rather than sending a second signal', () => {
    const html2 = drawAsk(ask({ schema: APPROVAL }), { deck: deck({ sending: 'ask-1' }) });
    expect(html2).toContain('answering…');
    expect(html2).toMatch(/data-testid="ask-ask-1-submit"[^>]*disabled=""/);
  });

  it('the appliance’s own refusal lands beside the form, naming the field', () => {
    const html2 = drawAsk(ask({ schema: APPROVAL }), {
      deck: deck({ refused: { 'ask-1': '/approve: must be boolean' } }),
    });
    expect(html2).toContain('data-testid="ask-ask-1-refused"');
    expect(html2).toContain('/approve: must be boolean');
  });
});

/* ───────────────────────────── several, and partially answered ───────────────────────────── */

describe('a run with several asks', () => {
  const asks = [
    ask({ id: 'ask-1', prompt: 'Approve these 12 hosts?', schema: APPROVAL, context: CONTEXT }),
    ask({
      id: 'ask-2',
      prompt: 'Which region?',
      askedAt: BASE + 40_000,
      state: 'answered',
      answeredAt: BASE + 70_000,
      by: 'mo',
      context: { regions: ['sfo3', 'nyc1'] },
    }),
    ask({ id: 'ask-3', prompt: 'Raise the machine cap to 20?', askedAt: BASE + 50_000, deadlineAt: NOW + HOUR }),
  ];
  const html = drawRun(asks);

  it('every ask is its own turn, and all three are on the page', () => {
    for (const id of ['ask-1', 'ask-2', 'ask-3']) {
      expect(html).toContain(`data-ask="${id}"`);
    }
    expect(html).toContain('Approve these 12 hosts?');
    expect(html).toContain('Which region?');
    expect(html).toContain('Raise the machine cap to 20?');
  });

  it('answering one leaves the others pending — two forms, one answer', () => {
    expect(html).toContain('data-testid="ask-ask-1-form"');
    expect(html).toContain('data-testid="ask-ask-3-form"');
    expect(html).not.toContain('data-testid="ask-ask-2-form"');
    expect(html).toContain('data-testid="ask-ask-2-answer"');
  });

  it('the panel says it is waiting, counts the questions, and lists them', () => {
    expect(html).toContain('data-testid="transcript-waiting"');
    expect(html).toContain('data-pending="2"');
    expect(html).toContain('This run is waiting for you to answer 2 questions.');
    expect(html).toContain('data-testid="waiting-ask-1"');
    expect(html).toContain('data-testid="waiting-ask-3"');
    // The one that is answered is not something anybody is waiting for.
    expect(html).not.toContain('data-testid="waiting-ask-2"');
  });

  it('each ask keeps its own clock', () => {
    expect(html).toContain('parked 3m 30s'); // ask-1, asked at +30s
    expect(html).toContain('parked 3m 10s'); // ask-3, asked at +50s
    expect(html).toContain('waited 30s'); // ask-2, answered 30s after it was asked
  });

  it('a run with one pending ask says so in the singular', () => {
    const one = drawRun([ask()]);
    expect(one).toContain('This run is waiting for you to answer a question.');
  });
});

/* ───────────────────────────── answered ───────────────────────────── */

describe('an answered ask', () => {
  const html = drawAsk(
    ask({ state: 'answered', answeredAt: BASE + 70_000, by: 'mo', context: CONTEXT })
  );

  it('carries the operator label as attribution — who SAID they decided it', () => {
    expect(html).toContain('data-testid="ask-ask-1-answer"');
    expect(html).toContain('data-by="mo"');
    expect(html).toContain('answered by mo, who said so themselves');
    expect(html).toContain('Attribution, not authentication');
  });

  it('never words it as verification', () => {
    const said = html.toLowerCase();
    for (const claim of ['verified by', 'authenticated', 'signed in as', 'authorised by', 'authorized by']) {
      expect(said).not.toContain(claim);
    }
  });

  it('keeps the question AND the material it was decided on', () => {
    expect(html).toContain('Approve these 12 hosts?');
    expect(html).toContain('live-hosts');
  });

  it('offers no form, because the run dropped the schema when it closed the ask', () => {
    expect(html).not.toContain('data-testid="ask-ask-1-form"');
    expect(html).not.toContain('data-testid="ask-ask-1-submit"');
  });

  it('an answer nobody labelled says so rather than inventing a name', () => {
    const bare = drawAsk(ask({ state: 'answered', answeredAt: BASE + 70_000 }));
    expect(bare).toContain('whoever answered left no name');
    expect(bare).not.toContain('answered by operator');
  });

  it('survives into a FINISHED run’s transcript, read from the archive', () => {
    const closed = readRunTurns(
      RUN,
      history([STARTED, CLOSED], { archived: true, archivedAt: BASE + 5 * MINUTE }),
      [ask({ state: 'answered', answeredAt: BASE + 70_000, by: 'mo', context: CONTEXT })],
      NOW
    );
    const drawn = renderToStaticMarkup(createElement(TranscriptView, { turns: closed, now: NOW }));
    expect(drawn).toContain('data-testid="transcript-archived"');
    // Both halves: the question and its answer. An archive holding one without the other is a
    // record of somebody approving something unspecified.
    expect(drawn).toContain('Approve these 12 hosts?');
    expect(drawn).toContain('answered by mo, who said so themselves');
    expect(drawn).toContain('live-hosts');
    // Nothing on a closed run is waiting for anybody.
    expect(drawn).not.toContain('data-testid="transcript-waiting"');
  });
});

/* ───────────────────────────── expired, overdue, stranded ───────────────────────────── */

describe('an ask that ended without an answer', () => {
  it('an expired ask reads as expired and offers nothing to fill in', () => {
    const html = drawAsk(ask({ state: 'expired', deadlineAt: BASE + 90_000 }));
    expect(html).toContain('data-standing="expired"');
    expect(html).toContain('>expired<');
    expect(html).toContain('the deadline passed with nobody answering');
    expect(html).not.toContain('data-testid="ask-ask-1-submit"');
    // The run's decision about what an expiry MEANT is the workflow's, and this does not guess.
    expect(html).toContain('the workflow’s own decision');
  });

  it('an expired ask is still in a finished run’s transcript, with its context', () => {
    const html = drawRun([ask({ state: 'expired', deadlineAt: BASE + 90_000, context: CONTEXT })], [
      STARTED,
      CLOSED,
    ]);
    expect(html).toContain('data-standing="expired"');
    expect(html).toContain('Approve these 12 hosts?');
    expect(html).toContain('live-hosts');
  });

  it('a deadline gone by that the RUN has not called expired says exactly that', () => {
    const html = drawAsk(ask({ schema: APPROVAL, deadlineAt: NOW - 3 * MINUTE }));
    expect(html).toContain('data-overdue="true"');
    expect(html).toContain('the deadline passed 3m 00s ago');
    // Still answerable: the memo says pending, and the appliance would still take it.
    expect(html).toContain('data-testid="ask-ask-1-submit"');
    expect(html).toContain('worker is down');
  });

  it('an abandoned ask does not read as a human who was too slow', () => {
    const html = drawAsk(ask({ state: 'abandoned' }));
    expect(html).toContain('data-standing="abandoned"');
    expect(html).toContain('nobody failed to answer in time');
  });

  it('a pending ask on a CLOSED run reads as never answered, with no form', () => {
    const html = drawRun([ask({ schema: APPROVAL })], [STARTED, CLOSED]);
    expect(html).toContain('data-stranded="true"');
    expect(html).toContain('never answered');
    expect(html).toContain('nobody will answer it now');
    expect(html).not.toContain('data-testid="ask-ask-1-submit"');
    expect(html).not.toContain('data-testid="transcript-waiting"');
  });
});

/* ───────────────────────────── the ones that break a surface ───────────────────────────── */

describe('a malformed ask', () => {
  const bad = ask({
    id: 'ask-9',
    prompt: '(this run published an ask that could not be read)',
    askedAt: 0,
    malformed: true,
  });

  it('degrades to a legible row rather than blanking anything', () => {
    const html = drawAsk(bad);
    expect(html).toContain('data-unreadable="true"');
    expect(html).toContain('could not be read');
    expect(html).toContain('kept and marked rather than dropped');
    // No invented clock over an entry with no readable instant.
    expect(html).toContain('not readable from this entry');
    expect(html).not.toContain('NaN');
    expect(html).not.toContain('Invalid Date');
  });

  it('is still answerable, because a stuck run has to be recoverable FROM THE UI', () => {
    const html = drawAsk(bad);
    expect(html).toContain('data-testid="ask-ask-9-json"');
    expect(html).toContain('data-testid="ask-ask-9-submit"');
  });

  it('does not take the readable asks beside it down with it', () => {
    const html = drawRun([bad, ask({ id: 'ask-1', schema: APPROVAL, context: CONTEXT })]);
    expect(html).toContain('data-ask="ask-9"');
    expect(html).toContain('data-testid="ask-ask-1-field-approve"');
    expect(html).toContain('live-hosts');
    // The whole account still drew — the run's own turns are untouched by an unreadable memo entry.
    expect(html).toContain('data-testid="transcript-turns"');
  });

  it('an ask carrying something that is not a schema says so instead of collecting a doomed answer', () => {
    const html = drawAsk(ask({ schema: 'Approval' }));
    expect(html).toContain('data-shape="unreadable"');
    expect(html).toContain('cannot be answered from here');
    expect(html).toContain('still parked');
    expect(html).not.toContain('data-testid="ask-ask-1-submit"');
  });
});

describe('an ask with no context', () => {
  it('says the author attached nothing rather than drawing an empty panel', () => {
    const html = drawAsk(ask({ schema: APPROVAL }));
    expect(html).toContain('data-testid="ask-ask-1-nocontext"');
    expect(html).toContain('the author attached nothing');
    expect(html).not.toContain('data-testid="ask-ask-1-context"');
    // The question and its form are entirely unaffected.
    expect(html).toContain('Approve these 12 hosts?');
    expect(html).toContain('data-testid="ask-ask-1-field-approve"');
  });

  it('a context that is a bare list or scalar still draws, as the JSON it is', () => {
    expect(drawAsk(ask({ context: ['a.example', 'b.example'] }))).toContain('a.example');
    expect(drawAsk(ask({ context: 42 }))).toContain('42');
  });
});

/* ───────────────────────────── read-only, and the chrome ───────────────────────────── */

describe('a surface with nowhere to send a signal', () => {
  it('draws the question and no button', () => {
    const html = drawAsk(ask({ schema: APPROVAL }), { deck: null });
    expect(html).toContain('Approve these 12 hosts?');
    expect(html).toContain('data-testid="ask-ask-1-readonly"');
    expect(html).not.toContain('data-testid="ask-ask-1-submit"');
  });
});

describe('the chrome says a run is waiting on you', () => {
  const counts: Record<View, number | null> = {
    workflows: 3,
    actors: 4,
    datasets: 11,
    monitor: 2,
    settings: null,
  };

  const PARKED: PulsePark[] = [{ runId: RUN, workflow: 'dnssweep', pending: 2, since: BASE + 30_000 }];

  /**
   * THE PAGE-LEARNED PATH, which is the one this file is about.
   *
   * The cluster scan behind `/api/pulse` reads open runs' memos off Temporal's VISIBILITY index,
   * and that index is eventually consistent — a run that parked a second ago reaches it a beat
   * later. The transcript that just fetched this run's asks knew immediately, off a describe. So
   * `known` is fed the page's entry and it WINS on a run both know about; the assertions below are
   * that the chrome carries it while the operator is somewhere else entirely.
   */
  function rail(known: PulsePark[] = [], over: Partial<NavRailProps> = {}): string {
    return renderToStaticMarkup(
      createElement(NavRail, {
        view: 'datasets',
        counts,
        pulse: readPulse({
          pulse: { running: 1, parked: 0, named: [], scanned: 1, capped: false, at: BASE },
          error: null,
          known,
          now: BASE + 300_000,
        }),
        collapsed: false,
        theme: 'dark',
        wall: { panes: 2, live: 0 },
        fleetSeries: [],
        unitsPerSec: 0,
        onView: () => {},
        onOpenRun: () => {},
        onCollapsed: () => {},
        onTheme: () => {},
        ...over,
      })
    );
  }

  it('marks it while the operator is on ANOTHER surface', () => {
    const html = rail(PARKED);
    expect(html).toContain('data-testid="nav-parked"');
    expect(html).toContain('data-value="1"');
    expect(html).toContain('2 questions unanswered');
    // Beside the running dot, never instead of it: a run can be parked while others grind.
    expect(html).toContain('data-testid="nav-running"');
  });

  it('names the run, and offers it as the way in', () => {
    const html = rail(PARKED);
    expect(html).toContain(RUN);
    expect(html).toContain('waiting on you');
    // One run waiting means there is nothing to choose between, so "in" is that conversation.
    expect(html).toContain('data-entry="run"');
  });

  it('survives collapsing, which is exactly the state you forget you are the bottleneck in', () => {
    const html = rail(PARKED, { collapsed: true });
    expect(html).toContain('data-testid="nav-parked"');
    expect(html).toContain('data-testid="nav-pulse-parked"');
  });

  it('says nothing at all when nothing is parked', () => {
    const html = rail();
    expect(html).not.toContain('data-testid="nav-parked"');
    expect(html).not.toContain('waiting on you');
  });
});
