/**
 * "Is anything happening at all, anywhere" — the reading the rail draws, and the only place in the
 * browser allowed to interpret what `/api/pulse` said.
 *
 * WHY THE CHROME OWNS THIS QUESTION. Retiring the global Runs surface removed the one page that
 * answered it, and the answer is wanted from whichever surface an operator happens to be on — which
 * is exactly when they are not on a page that could tell them. So it lives in the frame, it is a
 * COUNT AND A WAY IN rather than a table, and the way in goes through the workflow: clicking reaches
 * a run the way every other route to a run now does, via the Workflows surface.
 *
 * FOUR READINGS, AND THEY ARE FOUR BECAUSE COLLAPSING ANY TWO IS A LIE SOMEBODY ACTS ON:
 *
 *   idle      Temporal reports nothing running. A resting appliance, stated as a state — not as a
 *             `0` beside a label, which reads as an empty page rather than a calm one.
 *   running   work is in flight and nothing is waiting on a person.
 *   parked    at least one run has stopped and is waiting on a HUMAN. Reported BESIDE `running`
 *             rather than instead of it: a run can be parked while three others grind, and a mark
 *             that replaced the other would make the busy fleet disappear the moment one question
 *             was asked. Same rule the two dots on the Workflows icon already follow.
 *   unknown   the appliance could not be asked. NEVER rendered as idle — "we could not look" and
 *             "nothing is happening" are the two readings this codebase keeps furthest apart, and a
 *             badge that quietly says "idle" because Temporal is down is the single worst sentence
 *             this rail can produce.
 *
 * A fifth value, `unread`, is the moment before the first answer arrives. It draws NOTHING rather
 * than a word, because every word available at that moment would be wrong for a fraction of a
 * second, and a rail that flashes "not known" on every page load is a rail nobody believes.
 *
 * IT DOES NOT REPORT `stalled`. That reading needs `DescribeWorkflowExecution`'s pending-work view,
 * which is one RPC per run — the cost this whole route exists to keep off every surface. A stall is
 * a fact about ONE run and it is answered where that run is open (`runActivity.ts`); this is a fact
 * about the cluster. Said out loud here because the omission is deliberate and looks like a gap.
 *
 * PURE. Props in, reading out, no store and no fetch — so idle, running, parked, mixed, capped and
 * unreachable are all assertable in node.
 */

import type { Pulse } from '../../run/api';
import { fmtDuration } from '../../run/api';
import { parkedWords } from '../ask';

/** What the whole cluster is doing, in one word. See the header for why there are five. */
export type PulseTone = 'unread' | 'idle' | 'running' | 'parked' | 'unknown';

/**
 * One run waiting on a human.
 *
 * STRUCTURAL ON PURPOSE. The pulse's `PulsePark` and the page-learned `ParkedRun` (`panels/ask.ts`)
 * are the same fact learned two ways, and this module merges them — so it names the shape rather
 * than importing either, and a test can drive it from a literal. The WORDS for a park are not
 * restated here: `parkedWords` already owns that sentence and one vocabulary is the rule.
 */
export interface PulsePark {
  runId: string;
  /** The caller's workflow type — which thread the conversation belongs to. */
  workflow: string;
  pending: number;
  /** Epoch ms the oldest waiting ask was asked. `0` when no entry carried a readable instant. */
  since: number;
}

/**
 * One line the rail prints. Two facts are two lines, never one merged phrase — "3 workflows of
 * which some are running is two facts and must read as two" is already this rail's rule for the
 * inventory counts, and it applies harder here, where the two facts are opposites.
 *
 * IT CARRIES BOTH WIDTHS. The rail collapses to 48 pixels and this is one of the two things that
 * must survive that (the other is `live`), so each fact knows its full sentence AND the two or
 * three characters it becomes — decided here rather than by the component, because the collapsed
 * wording is exactly the kind of thing a `renderToStaticMarkup` suite has to be able to state.
 */
export interface PulseFact {
  key: 'running' | 'parked' | 'idle' | 'unknown';
  /** What the expanded rail prints: `3 running`, `1 waiting on you`, `idle`, `not known`. */
  text: string;
  /** The micro-label the collapsed rail prints above the number. */
  short: string;
  /** The number, or `null` where there is none to give — `idle` and `not known` are words, and a
   *  `0` under either would be a count of something rather than the state it actually is. */
  value: number | null;
  tone: PulseTone;
}

/**
 * Where clicking the pulse goes.
 *
 * `run` ONLY WHEN THERE IS EXACTLY ONE SUBJECT. One run is waiting on you, so "in" means that
 * conversation and there is nothing to choose between. Two or more and the honest destination is
 * the surface that holds them — offering the first would silently pick one, and offering a list
 * would be the retired Runs page rebuilt inside a tooltip.
 *
 * `none` IS A REAL ANSWER. An idle appliance has nothing to open, and a control that navigates
 * nowhere is worse than a line of text that never pretended to.
 */
export type PulseEntry = { kind: 'run'; runId: string } | { kind: 'surface' } | { kind: 'none' };

export interface PulseReading {
  tone: PulseTone;
  /** What the rail prints — empty before the first answer, one line usually, two when a busy fleet
   *  is also waiting on somebody. */
  facts: PulseFact[];
  /** Runs Temporal reports as running. `null` means NOT KNOWN and is never 0. */
  running: number | null;
  /** Runs known to be waiting on a human. A FLOOR whenever the scan fell short. */
  parked: number;
  /** Those runs, longest-waiting first — the way in, capped by the server. */
  waiting: PulsePark[];
  /** The sentence behind the mark: what was read, when, and what it does not cover. */
  because: string;
  entry: PulseEntry;
}

export interface PulseEvidence {
  /** The server's last successful reading, or `null` if there has never been one. */
  pulse: Pulse | null;
  /** Why the last poll failed, or `null`. */
  error: string | null;
  /**
   * The parked runs a PAGE learned directly, from the transcript it had open.
   *
   * KEPT BESIDE THE SCAN RATHER THAN REPLACED BY IT. An open run's memo reaches Temporal's
   * visibility index asynchronously, so the cluster scan is a beat behind on a run that parked a
   * second ago — and the page watching that run knew immediately, off a describe. The page's entry
   * therefore WINS on a run both know about: it is the more direct read of the same memo.
   */
  known: readonly PulsePark[];
  now: number;
}

export function readPulse(evidence: PulseEvidence): PulseReading {
  const { pulse, error, known, now } = evidence;

  if (error !== null) {
    const waiting = sorted(known);
    return {
      tone: 'unknown',
      // THE PARK SURVIVES THE OUTAGE, and it is the one number that can. A page-learned entry did
      // not come from this route at all — it came from a describe of the run whose transcript was
      // open — so an unreachable cluster is no reason to stop telling somebody they are the
      // bottleneck. What it IS a reason to stop asserting is how much is running.
      facts: [
        { key: 'unknown', text: 'not known', short: 'runs', value: null, tone: 'unknown' },
        ...(waiting.length > 0 ? [parkedFact(waiting.length)] : []),
      ],
      // `null`, NOT the last count. A number the rail keeps printing after the appliance stopped
      // answering is a number that gets read as current — the staleness has to be in the value, not
      // only in a tooltip nobody opens.
      running: null,
      parked: waiting.length,
      waiting,
      because: unknownWords(error, pulse, now) + parkedTail(waiting),
      entry: entryFor(waiting, null),
    };
  }

  if (pulse === null) {
    // NOTHING DRAWN YET. Every word available before the first answer would be wrong for a fraction
    // of a second, and the rail is on screen for every page load in the app.
    return {
      tone: 'unread',
      facts: [],
      running: null,
      parked: 0,
      waiting: [],
      because: 'the appliance has not been asked yet',
      entry: { kind: 'none' },
    };
  }

  const waiting = merge(pulse, known);
  const facts: PulseFact[] = [];
  if (pulse.running > 0) {
    facts.push({
      key: 'running',
      text: `${pulse.running} running`,
      short: 'running',
      value: pulse.running,
      tone: 'running',
    });
  }
  if (waiting.length > 0) {
    facts.push(parkedFact(waiting.length));
  }
  if (facts.length === 0) {
    // THE WORD, NOT A ZERO. `in flight 0` beside `live panes 0` beside `units/s —` reads as a page
    // that failed to load; `idle` reads as an appliance at rest, which is what it is.
    facts.push({ key: 'idle', text: 'idle', short: 'idle', value: null, tone: 'idle' });
  }

  return {
    tone: waiting.length > 0 ? 'parked' : pulse.running > 0 ? 'running' : 'idle',
    facts,
    running: pulse.running,
    parked: waiting.length,
    waiting,
    because: because(pulse, waiting, now),
    entry: entryFor(waiting, pulse.running),
  };
}

/**
 * The parked runs the scan found and the ones a page already knew, as one list.
 *
 * NOTHING RUNNING MEANS NOTHING PARKED, and that guard is what keeps a page-learned entry from
 * outliving its run. A mark in the chrome survives the page that set it — deliberately, so an
 * operator on the Monitor is still told they are the bottleneck — which means the entry for a run
 * that has since failed would otherwise sit on the rail forever. `stillParked` prunes it against
 * the run list where that list is fresh; this prunes the whole set on the one statement that needs
 * no list at all.
 */
function merge(pulse: Pulse, known: readonly PulsePark[]): PulsePark[] {
  if (pulse.running === 0) return [];
  const byId = new Map<string, PulsePark>();
  for (const p of pulse.named) byId.set(p.runId, p);
  for (const p of known) byId.set(p.runId, p);
  return sorted([...byId.values()]);
}

function parkedFact(n: number): PulseFact {
  return { key: 'parked', text: `${n} waiting on you`, short: 'asked', value: n, tone: 'parked' };
}

/** Longest-waiting first. An entry with no readable instant is an UNKNOWN, not the oldest park on
 *  the cluster, so it sorts last rather than to 1970. */
function sorted(parks: readonly PulsePark[]): PulsePark[] {
  return [...parks].sort((a, b) => rank(a.since) - rank(b.since));
}

function rank(since: number): number {
  return since > 0 ? since : Number.POSITIVE_INFINITY;
}

function entryFor(waiting: readonly PulsePark[], running: number | null): PulseEntry {
  const only = waiting.length === 1 ? waiting[0] : undefined;
  if (only) return { kind: 'run', runId: only.runId };
  if (waiting.length > 1) return { kind: 'surface' };
  return running !== null && running > 0 ? { kind: 'surface' } : { kind: 'none' };
}

/**
 * Which runs, by name, at the end of the sentence.
 *
 * A TOOLTIP IS NOT THE RUNS LIST. This is capped by the SERVER (`NAMED_PARKED_CAP`) and it names
 * only the dimension that is addressed to a person — a question somebody has to answer. Naming the
 * running runs too would be the retired global list rebuilt one hover at a time, which is the one
 * thing this whole surface was paid for not doing.
 */
function runWords(waiting: readonly PulsePark[]): string {
  if (waiting.length === 0) return '';
  return ` ${waiting.map((p) => p.runId).join(', ')}.`;
}

/** The sentence behind a reading that was actually taken. */
function because(pulse: Pulse, waiting: readonly PulsePark[], now: number): string {
  if (pulse.running === 0) {
    return 'Nothing is running. Temporal reports no caller workflows in flight — this appliance is at rest, not unreachable.';
  }
  const runs = pulse.running === 1 ? '1 run is' : `${pulse.running} runs are`;
  const head = `${runs} in flight — Temporal reports them as running.`;
  if (waiting.length === 0) {
    // A CAPPED OR LAGGING SCAN MAY NOT SAY "nothing is waiting". It looked at some of the fleet, and
    // the difference between "no questions on the fleet" and "no questions on the part I opened" is
    // the difference between an all-clear and a partial look.
    return pulse.capped || pulse.scanned < pulse.running
      ? `${head} No question was found on the ${pulse.scanned} of them that were looked at; the rest were not opened.`
      : `${head} None of them is waiting on a person.`;
  }
  // ONE VOCABULARY. `parkedWords` is the sentence the rail already said about a park and the
  // Workflows page says about the same runs; a second phrasing invented here would be the same
  // fact in two dialects, in two places an operator reads within one glance of each other.
  const longest = waitedWords(waiting, now);
  const floor = pulse.capped ? ' At least — the scan did not reach every running run.' : '';
  return `${head} ${parkedWords(waiting)}${longest}.${floor}${runWords(waiting)}`;
}

/** How long the longest-waiting question has been open, as a clause. Empty when no entry carried a
 *  readable instant — inventing one would put "waiting 56 years" in the chrome. */
function waitedWords(waiting: readonly PulsePark[], now: number): string {
  const oldest = waiting.map((p) => p.since).filter((t) => t > 0);
  if (oldest.length === 0) return '';
  const ms = now - Math.min(...oldest);
  return ms > 0 ? `, the longest for ${fmtDuration(ms)}` : '';
}

/** What is still known about parks while the appliance cannot be reached — the one clause an
 *  outage does not invalidate, because a page-learned park came from a describe of the run whose
 *  transcript was open and not from this route. */
function parkedTail(waiting: readonly PulsePark[]): string {
  return waiting.length === 0 ? '' : ` ${parkedWords(waiting)}, as of when its transcript was last read.${runWords(waiting)}`;
}

/**
 * The sentence behind "not known", which has to do two jobs: say that the appliance could not be
 * asked, and refuse to let the last number stand in for the current one.
 */
function unknownWords(error: string, pulse: Pulse | null, now: number): string {
  const head = `The appliance could not be asked what is running: ${error}. This is not an idle appliance — it is one nothing could be read from.`;
  if (pulse === null) return head;
  const ago = now > pulse.at ? fmtDuration(now - pulse.at) : 'moments';
  const last = pulse.running === 0 ? 'nothing was running' : `${pulse.running} were running`;
  return `${head} Last read ${ago} ago, when ${last}.`;
}
