/**
 * The Summary on a row, and the sentence it carries — issue 30, drawn.
 *
 * WHY THIS SUITE EXISTS AT ALL. Both halves of issue 30 shipped green: every suite passed while the
 * one pane that claims to show everything drew `TimerStarted timerId=1` for a sentence somebody
 * wrote, and drew a dispatch without the Method name that was put on its Summary precisely so no
 * payload would have to be opened for it. Nothing asserted either property, so nothing broke when
 * neither held. These are those assertions.
 *
 * NODE, NO JSDOM, NO TESTING-LIBRARY, like every render suite here: `renderToStaticMarkup` and
 * string assertions about the text an operator can read. What is asserted is never that a component
 * received a prop — it is that the words are on the page, and that the rows which have no Summary
 * still carry exactly the metadata they carried before.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import { categorize } from '@core/history';
import { nameTranscript, type NamedTurn } from '@core/vocabulary';
import { readTranscript, type Turn } from '@core/transcript';

import type { RunEvent, RunHistory } from '../run/api';
import { readRunTurns } from '../run/turns';
import { TranscriptDrill, type DrillLevelView, type TranscriptDrillProps } from './TranscriptDrill';
import { TranscriptView, clockOf } from './Transcript';
import { rootLevel, type DrillLevel } from './eventDrill';
import { rowsFor, subjectOf } from './transcriptDrill';

const BASE = 1_786_787_691_000;
const RUN = 'probedemo-1787787691';

function ev(id: number, type: string, ms: number, extra: Partial<RunEvent> = {}): RunEvent {
  return { id, type, cat: categorize(type), t: ms / 1000, at: BASE + ms, detail: type, attempt: 1, dur: 0, ...extra };
}

function history(events: RunEvent[]): RunHistory {
  return { events, scanned: events.length, elided: 0, truncated: false };
}

const STARTED = ev(1, 'WorkflowExecutionStarted', 0, {
  detail: 'workflowType=probe_demo · taskQueue=wf-probedemo · identity=1@main-droplet',
});

/**
 * A `speak`: the sentence rides the user-metadata Summary of a zero-duration timer, and the timer's
 * OWN metadata is the whole of what `detail` can say about it.
 */
const SENTENCE = 'sweeping 12 hosts for an open resolver';
const SPOKE = ev(2, 'TimerStarted', 400, { detail: 'timerId=1', summary: SENTENCE });
const SPOKE_FIRED = ev(3, 'TimerFired', 400, { detail: 'timerId=1' });

/**
 * A dispatch: the Method rides the Summary of the SCHEDULING event and of no other (issue 03), which
 * is why the `Started` beside it below carries none — and is the case that proves a row with no
 * Summary is untouched.
 */
const NEXUS_LINK = { workflowId: 'actor-probe-probedemo-1787787691-head-60c3bfac', via: 'nexus' as const };
const DISPATCH_SUMMARY = 'head · probe@0.1.0 · 12 units';
const DISPATCHED = ev(4, 'NexusOperationScheduled', 900, {
  detail: 'endpoint=kontra-probe-0-1-0 · nexus=kontra.actor/run',
  summary: DISPATCH_SUMMARY,
  link: NEXUS_LINK,
});
const DISPATCH_STARTED = ev(5, 'NexusOperationStarted', 1_100, {
  detail: 'endpoint=kontra-probe-0-1-0 · nexus=kontra.actor/run · identity=2006879@main-droplet',
  link: NEXUS_LINK,
});

const EVENTS = [STARTED, SPOKE, SPOKE_FIRED, DISPATCHED, DISPATCH_STARTED];

/** One named turn of a given kind, from either pane — the account and the Event log are two halves
 *  of one reading, and a `raw` turn only ever lands in the second. */
function turnOf(events: RunEvent[], kind: Turn['kind']): NamedTurn {
  const named = nameTranscript(readTranscript(history(events)));
  const all = [...named.turns, ...named.untranslated];
  const found = all.find((t) => t.turn.kind === kind);
  if (!found) throw new Error(`no ${kind} turn — got ${all.map((t) => t.turn.kind).join(', ')}`);
  return found;
}

/** The rows one turn folded, built the way the wired hook builds them: out of the log in hand. */
function rootView(events: RunEvent[], named: NamedTurn): DrillLevelView {
  const { rows, missing } = rowsFor(history(events), subjectOf(named.turn).events);
  return { rows, missing, loading: false, failure: null, span: 0, closed: true };
}

function drawDrill(named: NamedTurn, events: RunEvent[] = EVENTS): string {
  const props: TranscriptDrillProps = {
    runId: RUN,
    named,
    stack: [rootLevel(RUN)] as DrillLevel[],
    view: rootView(events, named),
    onDrill: () => undefined,
    onPop: () => undefined,
    onClose: () => undefined,
  };
  return renderToStaticMarkup(createElement(TranscriptDrill, props));
}

/**
 * ONE ROW, SLICED OUT — never the whole panel.
 *
 * The panel's HEADER already names the turn it was opened from, so a narration's sentence is on the
 * page whatever the rows say. Asserting against the whole markup would have passed against the very
 * drawing this suite exists to prevent; the claim is about the ROW, so the assertion has to be.
 */
function drillRow(html: string, id: number): string {
  const at = html.indexOf(`data-testid="drill-row-${id}"`);
  if (at < 0) throw new Error(`no drill row ${id} on the page`);
  const start = html.lastIndexOf('<tr', at);
  const end = html.indexOf('</tr>', at);
  return html.slice(start, end < 0 ? html.length : end);
}

/* ──────────────── the Event log shows the Summary, because that is where the meaning is ──────────────── */

describe('an event carrying a Summary shows it', () => {
  it('draws a speak’s sentence on the row, which read `TimerStarted timerId=1` and nothing else', () => {
    const row = drillRow(drawDrill(turnOf(EVENTS, 'narration')), SPOKE.id);
    expect(row).toContain(SENTENCE);
    // The Temporal type is still on the row — the Summary is added in front of the machinery, not
    // in place of it, which is the promise the whole raw view rests on.
    expect(row).toContain('TimerStarted');
    expect(row).toContain('timerId=1');
  });

  it('draws a dispatch’s Method name, which is on the Summary and in no other metadata', () => {
    const row = drillRow(drawDrill(turnOf(EVENTS, 'dispatch')), DISPATCHED.id);
    // `head` is the fact issue 03 put there. Without it the row names the endpoint and the Method
    // is nowhere on the page.
    expect(row).toContain(DISPATCH_SUMMARY);
  });

  it('keeps the metadata the Summary displaced, beside it', () => {
    const row = drillRow(drawDrill(turnOf(EVENTS, 'dispatch')), DISPATCHED.id);
    // WHICH actor, WHICH version — the pair that explains a dispatch sitting forever on a queue
    // nobody polls, and the reason a Summary leads a row rather than replacing it.
    expect(row).toContain('endpoint=kontra-probe-0-1-0');
  });
});

describe('an event with NO Summary keeps the detail it has today', () => {
  it('leaves the row that carries only its own metadata exactly as it was', () => {
    const row = drillRow(drawDrill(turnOf(EVENTS, 'dispatch')), DISPATCH_STARTED.id);
    // `NexusOperationStarted` carries no Summary — only the Scheduled event does — so its row is
    // the one it has always been, identity and all.
    expect(row).toContain('identity=2006879@main-droplet');
    expect(row).not.toContain(DISPATCH_SUMMARY);
  });

  it('draws the timer’s own metadata on the TimerFired that closes a sentence', () => {
    const row = drillRow(drawDrill(turnOf(EVENTS, 'narration')), SPOKE_FIRED.id);
    expect(row).toContain('TimerFired');
    expect(row).toContain('timerId=1');
    expect(row).not.toContain(SENTENCE);
  });
});

/* ──────────────── `speak` reads like a log line ──────────────── */

describe('a narration reads `<human time> — <sentence>`', () => {
  function drawTranscript(events: RunEvent[] = EVENTS): string {
    const turns = readRunTurns(RUN, history(events), [], BASE + 60_000);
    return renderToStaticMarkup(createElement(TranscriptView, { turns, now: BASE + 60_000 }));
  }

  /** The narration row alone. Every other turn is asserted through the same slice, so "the other
   *  kinds keep their offset" is a statement about THEIR rows and not about the page as a whole. */
  function rowOf(html: string, kind: string): string {
    const at = html.indexOf(`data-kind="${kind}"`);
    if (at < 0) throw new Error(`no ${kind} row on the page`);
    const start = html.lastIndexOf('<', at);
    const end = html.indexOf(`data-kind="`, at + 1);
    return html.slice(start, end < 0 ? html.length : html.lastIndexOf('<', end));
  }

  const html = drawTranscript();

  it('prints the wall clock the sentence was said at, in the reader’s own timezone', () => {
    const row = rowOf(html, 'narration');
    // Compared against the same local formatting the row uses, because the answer legitimately
    // differs by machine — what is being asserted is that the WALL CLOCK is the number on the row.
    expect(clockOf(SPOKE.at)).not.toBe('');
    expect(row).toContain(clockOf(SPOKE.at));
  });

  it('drops the relative offset and the kind label, which stood in front of the words', () => {
    const row = rowOf(html, 'narration');
    expect(row).not.toContain('>+0.4s<');
    // `note` is the term's label. A sentence says what it says; naming its kind beside it adds
    // nothing an operator did not already have.
    expect(row).not.toContain('>note<');
    expect(row).toContain('—');
    expect(row).toContain(SENTENCE);
  });

  it('still drills, because a name nobody can check is a name nobody should trust', () => {
    const drawn = renderToStaticMarkup(
      createElement(TranscriptView, {
        turns: readRunTurns(RUN, history(EVENTS), [], BASE + 60_000),
        now: BASE + 60_000,
        onDrill: () => undefined,
      })
    );
    const row = rowOf(drawn, 'narration');
    expect(row).toContain('turn-drill-');
    expect(row).toContain('2ev');
  });

  it('leaves EVERY OTHER KIND on its relative offset, which is a deliberate exception of one', () => {
    const row = rowOf(html, 'dispatch');
    expect(row).toContain('+0.9s');
    expect(row).not.toContain(clockOf(DISPATCHED.at));
  });
});
