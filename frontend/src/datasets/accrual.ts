/**
 * Whether a Dataset's row count is still MOVING, and what to say when it is not.
 *
 * THE QUESTION THIS ANSWERS IS NOT "WHAT LIFECYCLE IS THIS". `datasets/state.ts` owns that, and the
 * badge draws it. This owns the second question an operator asks with the first one — "is the number
 * beside it finished?" — and the two answers are not the same fact. A `sealed` Dataset's count is
 * final; an `open` one's may be climbing right now, or may have stopped forever when its producer
 * died; and an `open` Dataset with no Run resolved cannot be tailed AT ALL, so its number is static
 * for a reason that has nothing to do with its lifecycle.
 *
 * ARRIVAL, NOT ANIMATION. The house doctrine is that nothing blinks and live things update in place,
 * and the ask is that an operator sees output accruing. Those are reconciled by making the ROW and
 * the NUMBER the only things that move: a count lands, and the readout beside it says how old the
 * newest chunk is. There is no pulse, no flash and no spinner in any state this module names — a
 * degraded stream is a WORD ("stream lost") and not a colour that has to be noticed twice.
 *
 * THE COUNT AN OPEN DATASET SHOWS COMES FROM TWO PLACES, AND THEY ARE NOT THE SAME NUMBER. The
 * catalog total is what `/api/datasets` measured, and the console deliberately does not resubscribe
 * to that poll (`panels/DatasetPage.tsx`) — so while a console is open, that number is a fact about
 * the moment the console opened. The live tail is the durable object count of the Run's own path,
 * streamed per chunk. Drawing them as one number would be the merged-status-field mistake one seam
 * over: they answer different questions and are labelled separately, always.
 */

import type { DatasetBadgeState } from './state';

/**
 * What is happening to the count.
 *
 * FIVE, NOT TWO, and the extra one is the one that bites: `open-untailed`. A Dataset can be `open`
 * with no single **Run** behind it — a partition several Runs wrote, a row whose Run was never
 * stamped — and the row tail is addressed BY RUN. Collapsing that into `accruing` would open a
 * socket for a run id that does not exist and leave "watching for rows…" on screen forever, which
 * reads as "nothing has landed yet" when the truth is "nothing is watching".
 */
export type AccrualPhase =
  | 'accruing'
  | 'open-untailed'
  | 'sealed'
  | 'abandoned'
  | 'none';

/**
 * The phase for one Dataset, from its lifecycle and whether a **Run** addresses it.
 *
 * The `runId` is the record's key (ADR 0029 §4) and, on an `open` Dataset, the Run whose durable
 * path the tail LISTs. `state` is read through `datasets/state.ts`, so an unknown value from a newer
 * peer arrives here as `none` and this never has to guess between "done" and "not done".
 */
export function accrualPhase(d: { state: DatasetBadgeState; runId?: string }): AccrualPhase {
  if (d.state === 'sealed') return 'sealed';
  if (d.state === 'abandoned') return 'abandoned';
  if (d.state === 'open') return d.runId ? 'accruing' : 'open-untailed';
  return 'none';
}

/** Whether the page should hold a live subscription open at all. An idle Dataset opens no socket, so
 *  the server polls nothing for a Dataset nobody is watching (`datasets/rowTail.ts`). */
export function accruing(d: { state: DatasetBadgeState; runId?: string }): boolean {
  return accrualPhase(d) === 'accruing';
}

/** How one phase is said beside the count: a short word, and the sentence behind it. */
export interface AccrualWords {
  /** Drawn beside the number — never only in a tooltip. */
  label: string;
  title: string;
}

/**
 * What to say about the NUMBER in each phase.
 *
 * Every one of these is about whether the count is finished, never about the lifecycle the badge is
 * already showing — saying "sealed" twice on one line teaches an operator to read neither.
 */
export function accrualWords(phase: AccrualPhase): AccrualWords {
  switch (phase) {
    case 'accruing':
      return {
        label: 'accruing',
        title:
          'A Run is appending to this Dataset. The live count beside it is the durable object count ' +
          'of that Run’s own path, updated per chunk — not a per-row animation, and never larger ' +
          'than what is stored.',
      };
    case 'open-untailed':
      return {
        label: 'open · nothing to tail',
        title:
          'This Dataset is open — a producer may still be appending, or may have died before ' +
          'sealing — but no single Run resolved for it, and the live tail is addressed by Run. The ' +
          'count is the catalog’s, measured when this page opened; reopen the Dataset to remeasure it.',
      };
    case 'sealed':
      return {
        label: 'final',
        title: 'The caller declared this Dataset complete. Nothing is appending: this count is all of it.',
      };
    case 'abandoned':
      return {
        label: 'final · partial',
        title:
          'The caller gave up on this Dataset. What is here is partial AND final — the count will ' +
          'not grow, and it is not the whole of what was being written.',
      };
    default:
      return {
        label: 'not recorded',
        title:
          'No writer recorded a lifecycle for this Dataset, so nothing can say whether anything is ' +
          'still appending. The count is the catalog’s, measured when this page opened.',
      };
  }
}
