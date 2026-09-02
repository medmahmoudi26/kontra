/**
 * The last rows of a Dataset, read inline — the ROW-LEVEL PEER of `rowTail.ts`.
 *
 * `rowTail.ts` answers "how many rows are there, and is that number still moving". This answers the
 * question the count cannot: WHAT DO THE ROWS LOOK LIKE. On the Data tab that question used to cost
 * a navigation to another surface, and the summary an operator already had — name, state, count,
 * link — is the right thing to keep as the default. So this is what an expander shows, ten rows at
 * a time, and the link out is still the way to the whole Dataset.
 *
 * IT READS THE UNGATED PREVIEW ROUTE, NOT THE CONSOLE'S QUERY ROUTE, and that is a decision rather
 * than an accident. `/api/datasets/query` is bearer-gated (`run/query.ts`) because it carries
 * OPERATOR SQL; `/api/datasets/:name/preview` composes its SQL server-side, checks the name against
 * the catalog and lets no object-store URL out, which is why it needs no credential. ADR 0031 §3
 * then removes the browser credential altogether on the appliance — "on loopback there is no browser
 * credential to bake" — so a peek built on the query route would be a peek that 401s on the topology
 * this repo is moving to, on the surface whose whole point is that you do not have to leave the run.
 * The console's bearer guards typed SQL. This is not typed SQL.
 *
 * "LAST" IS THE ORDER THE LAKE HANDS ROWS BACK, and nothing here pretends it is more than that.
 * No column in a materialized table stamps a per-row sequence (`data/parquet.ts` adds `version`,
 * `dt`, `node`, `run_id`, `run_started_at` — none of them per row), so a scan's order is the order
 * the data files hold, which for an append-only table is the order rows were written. It is the same
 * order `SELECT * FROM <name> LIMIT 100` shows an operator in the console, so the two windows onto
 * one Dataset agree. Where that assumption cannot hold — a read that hit its ceiling — this REFUSES
 * to call the rows a tail; see {@link readTail}.
 *
 * A READ THAT CAME BACK FULL IS NOT A TAIL. The plan asks for one row MORE than it needs. Getting
 * that extra row back means the Dataset is longer than the window, so what was read is the HEAD of
 * it and says so — "the first 10 rows" is a true sentence about a 223,378-row Dataset in a way that
 * "the last 10" would not be. That is the same discipline as `runDatasets.ts`'s `partial`: a bound
 * is not a measurement, and the surface says which one it is holding.
 *
 * AN OPEN DATASET'S TAIL FOLLOWS, AND FOLLOWING COSTS NO SECOND POLLER. The read is keyed on the
 * catalog's own row count, and the catalog is polled once for the whole app (`App.tsx` says why
 * there is exactly one poller). A count that moved is rows that COMMITTED, so the tail re-reads and
 * the new rows land; a count that did not move re-reads nothing, which is what keeps an expander off
 * the tick-driven-work path that issue #14 closed. Arrival is a row appearing — there is no pulse,
 * no flash and no spinner in any state this module names.
 */

import { BIG_TEXT, duckdbText } from './cells';
import type { DatasetBadgeState } from './state';

/** How many rows the expander shows. Ten is a LOOK — enough to see the shape of a row and the
 *  values in it — and deliberately not enough to be mistaken for a query. */
export const TAIL_ROWS = 10;

/**
 * The longest Dataset this will read end-to-end in order to find its last rows.
 *
 * THE COST OF A TRUE TAIL IS READING TO THE END. The preview route takes a `limit` and nothing else
 * — no offset, no ordering — so the only way to hold the last ten rows is to read the whole thing
 * and keep the tail of it. 500 is `PREVIEW_ROWS` on the server: the read the Datasets page already
 * pays once per opened Dataset, so a Dataset short enough to tail costs no more here than it does
 * there. Beyond it, this does NOT read 500 rows to show 10 of the middle — it asks for ten, calls
 * them the first ten, and points at the console for the end.
 */
export const TAIL_WINDOW = 500;

/** One column of the read, exactly as the preview route reports it. */
export interface TailColumn {
  name: string;
  type: string;
}

/** What a read can honestly claim about the rows it holds. */
export type TailReach = 'tail' | 'head';

/** What to ask the preview route for, and what the answer could be. */
export interface TailPlan {
  /** Rows to request. ZERO means do not ask at all — the catalog counts none, and a request that
   *  can only return nothing is a request not to make. */
  limit: number;
  /** The catalog's count this plan was made from. */
  known: number;
  /** The best this read could be. `head` when the Dataset is longer than {@link TAIL_WINDOW}. */
  attempt: TailReach;
}

/**
 * How to read one Dataset's tail, from the count the catalog already holds.
 *
 * ONE ROW MORE THAN IS NEEDED, always, so {@link readTail} can tell "this is the end of the
 * Dataset" from "this is where my window stopped". Without the extra row a Dataset of exactly 500
 * rows and one of 5,000 come back identical, and one of those two would be labelled a tail wrongly.
 */
export function tailPlan(known: number): TailPlan {
  if (!Number.isFinite(known) || known <= 0) return { limit: 0, known: 0, attempt: 'tail' };
  if (known <= TAIL_ROWS) return { limit: TAIL_ROWS + 1, known, attempt: 'tail' };
  if (known <= TAIL_WINDOW) return { limit: TAIL_WINDOW + 1, known, attempt: 'tail' };
  return { limit: TAIL_ROWS, known, attempt: 'head' };
}

/** Ten rows of a Dataset, and everything needed to say truthfully which ten they are. */
export interface TailRead {
  columns: readonly TailColumn[];
  /** At most {@link TAIL_ROWS}, in the order the lake returned them. */
  rows: readonly unknown[][];
  reach: TailReach;
  /** How many rows the read itself saw — bounded by the plan's limit, never a claim about the
   *  Dataset unless {@link TailRead.reach} is `tail`. */
  seen: number;
  /** The catalog's count at the moment the read was planned. */
  known: number;
  /** Every row this Dataset holds is on screen. The under-ten case, which says so rather than
   *  implying there is more above. */
  whole: boolean;
  /** When the read answered (epoch ms). */
  at: number;
}

/**
 * Turn one preview answer into a reading.
 *
 * THE CEILING TEST IS THE WHOLE OF THE HONESTY. `all.length >= plan.limit` means the answer was
 * clipped by the limit — the Dataset has at least one row past the window — so these rows are its
 * beginning, not its end. It also catches the case the catalog count could not: a Dataset that GREW
 * between the listing poll and this read. Planned as a tail, read as a head, labelled a head.
 */
export function readTail(
  preview: { columns: readonly TailColumn[]; rows: readonly unknown[][] },
  plan: TailPlan,
  at: number
): TailRead {
  const all = preview.rows;
  const clipped = plan.limit > 0 && all.length >= plan.limit;
  const reach: TailReach = plan.attempt === 'head' || clipped ? 'head' : 'tail';
  const rows =
    reach === 'tail'
      ? all.slice(Math.max(0, all.length - TAIL_ROWS))
      : all.slice(0, TAIL_ROWS);
  return {
    columns: preview.columns,
    rows,
    reach,
    seen: all.length,
    known: plan.known,
    whole: reach === 'tail' && all.length <= TAIL_ROWS,
    at,
  };
}

/** The reading for a Dataset the catalog counts no rows in — made without a request, because the
 *  answer is already known and a round trip that can only return nothing is one nobody should pay. */
export function emptyTail(plan: TailPlan, at: number): TailRead {
  return { columns: [], rows: [], reach: 'tail', seen: 0, known: plan.known, whole: true, at };
}

/**
 * Which ten rows these are, in one clause.
 *
 * THE DENOMINATOR CHANGES WITH THE REACH, on purpose. A tail read saw the whole Dataset, so `seen`
 * is the count it MEASURED and is better than the catalog's, which was taken earlier. A head read
 * saw only its window, so the only honest total is the catalog's — and it is named as a total, never
 * as something this read counted.
 */
export function tailWords(read: TailRead): string {
  if (read.seen === 0) return 'no rows';
  if (read.whole) {
    return `all ${read.seen} row${read.seen === 1 ? '' : 's'} — that is the whole of it`;
  }
  if (read.reach === 'tail') {
    return `the last ${read.rows.length} of ${read.seen.toLocaleString()} rows`;
  }
  return `the first ${read.rows.length} rows · ${read.known.toLocaleString()} in all`;
}

/**
 * Why a head read is a head read — the sentence that stops "the first ten" being read as a failure.
 * `null` when there is nothing to explain, so a caller can leave the line out entirely rather than
 * print an empty one.
 */
export function tailReachNote(read: TailRead): string | null {
  if (read.reach !== 'head') return null;
  return `Reading to the end of a Dataset longer than ${TAIL_WINDOW.toLocaleString()} rows is a query, not a peek — these are its first rows, and its last ones are one click away in the console.`;
}

/**
 * Whether this Dataset's tail FOLLOWS, and it is the lifecycle that decides.
 *
 * Only an `open` Dataset is still being written to, so only an `open` one's tail moves. `sealed`,
 * `abandoned` and no-lifecycle-at-all are three different reasons for the same fact — nothing is
 * appending — and `datasets/state.ts` is what keeps them three.
 */
export function tailFollows(state: DatasetBadgeState): boolean {
  return state === 'open';
}

/**
 * What the lifecycle means FOR THESE ROWS. Four states, four sentences — the same four
 * `datasetBadge` draws, said in terms of the tail rather than of the Dataset, because "these are its
 * last rows" is true of a sealed Dataset and false of an open one, and an expanded open Dataset that
 * read as finished would be the §11 failure with a table under it.
 */
export function tailStateWords(state: DatasetBadgeState): string {
  switch (state) {
    case 'open':
      return 'Still open — this tail follows as rows commit, so what is here is the newest, not the last.';
    case 'sealed':
      return 'Sealed — these are its last rows, and they will not change.';
    case 'abandoned':
      return 'Abandoned — its caller gave up, so these are the last rows it will ever have.';
    default:
      return 'No lifecycle was recorded for this Dataset, so nothing is appending to it — these rows are as final as the ones in a sealed one, and nothing declared them so.';
  }
}

/**
 * What the expander says BEFORE it is opened, from the catalog count alone.
 *
 * IT PROMISES ONLY WHAT IT CAN DELIVER. A Dataset longer than the window will show its FIRST rows,
 * and saying "the last 10 rows" on the control that opens it would be a lie the operator only
 * discovers after clicking.
 */
export function tailSummaryWords(known: number): string {
  if (!Number.isFinite(known) || known <= 0) return 'no rows to show';
  if (known <= TAIL_ROWS) return `all ${known} row${known === 1 ? '' : 's'}`;
  if (known <= TAIL_WINDOW) return `the last ${TAIL_ROWS} rows`;
  return `the first ${TAIL_ROWS} rows`;
}

/** One cell, ready to draw: what is shown, whether it was cut, and the whole of it for the title. */
export interface TailCell {
  text: string;
  clipped: boolean;
  full: string;
}

/**
 * One value, as DuckDB prints it — `datasets/cells.ts` owns that notation and this does not invent a
 * second one, because two spellings of one value across two windows onto the same Dataset is a thing
 * to translate rather than read.
 *
 * CLIPPED FOR A ROW, NEVER SILENTLY. Ten rows of a crawl carry markdown bodies thousands of
 * characters long; the ellipsis and the full value in the title are what keep the row a row without
 * hiding that there is more. Opening a value is the console's job (`cells.ts:inspect`), not this
 * expander's — ten rows is a look.
 */
export function tailCell(v: unknown, threshold = BIG_TEXT): TailCell {
  const full = duckdbText(v);
  return full.length > threshold
    ? { text: `${full.slice(0, threshold)}…`, clipped: true, full }
    : { text: full, clipped: false, full };
}

/**
 * EVERYTHING the tail's header depends on, as one value — the memo key, and the reason an arriving
 * row cannot rebuild the columns.
 *
 * THE SAME RULE THE RESULTS GRID RUNS ON (issue #14, `DatasetPage.schemaSignature`): a read that
 * changes rows but not columns must hand the header nothing new. It is spelled out here rather than
 * imported because `DatasetPage` is a lazy chunk carrying AG Grid and CodeMirror, and importing one
 * function out of it would pull both into the Workflows surface — a bundle regression to fix a
 * duplication that is three lines long.
 *
 * CONTROL CHARACTERS JOIN IT, because a DuckDB type carries spaces, commas and parens
 * (`DECIMAL(10, 2)`, `STRUCT(a VARCHAR)`) and a printable separator would let two different schemas
 * collide onto one signature — which would freeze the header on the wrong columns.
 */
export function columnSignature(columns: readonly TailColumn[]): string {
  return columns.map((c) => `${c.name}\u0001${c.type}`).join('\u0002');
}

/**
 * `unopened` — nobody has expanded this Dataset, so nothing has been read and nothing requested.
 * `reading`  — a read is in flight. Any rows from the previous read are KEPT (see below).
 * `ready`    — rows are on screen.
 * `failed`   — the read failed; the last rows it did get are still on screen, marked stale.
 */
export type TailPhase = 'unopened' | 'reading' | 'ready' | 'failed';

export interface TailState {
  phase: TailPhase;
  read: TailRead | null;
  /** The server's own sentence when a read failed — `run/api.ts` surfaces it verbatim, and it is
   *  the only part worth reading ("that dataset is gone" against "could not preview dataset: …"). */
  error: string | null;
}

export const TAIL_START: TailState = { phase: 'unopened', read: null, error: null };

export type TailInbound =
  | { type: 'read' }
  | { type: 'rows'; read: TailRead }
  | { type: 'failed'; error: string };

/**
 * Fold one step of the read into the state.
 *
 * A RE-READ KEEPS THE ROWS IT HAS. An open Dataset re-reads whenever its count moves, and blanking
 * the table for the duration of each read would turn "a row landed" into a flicker — the animation
 * this surface is explicitly not allowed to have. The same is true of a failure: rows that were
 * read are still rows that were read, so they stay and the failure is a SENTENCE beside them, the
 * same way `rowTail.ts` keeps its last count when its stream drops.
 */
export function tailReduce(state: TailState, ev: TailInbound): TailState {
  switch (ev.type) {
    case 'read':
      return { phase: 'reading', read: state.read, error: null };
    case 'rows':
      return { phase: 'ready', read: ev.read, error: null };
    case 'failed':
      return { phase: 'failed', read: state.read, error: ev.error };
    default:
      return state;
  }
}
