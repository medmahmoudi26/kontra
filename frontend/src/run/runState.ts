/**
 * How a Run's two status dimensions become words on screen — and nothing else.
 *
 * PURE, and separate from the page for the reason ADR 0017 exists: the moment these two readings
 * are combined into one they stop being two. Keeping the derivation here means the run LIST and
 * the run DETAIL cannot drift into disagreeing about what `completed` means, and every rule below
 * is asserted in `runState.test.ts` rather than eyeballed in a browser.
 *
 *   EXECUTION       authority Temporal — did the caller's workflow finish?
 *   MATERIALIZATION authority the ledger — is what it wrote queryable?
 *
 * There is deliberately no function here that returns one verdict from both.
 */

import {
  fmtDuration,
  runDuration,
  type DatasetInfo,
  type MaterializationRecord,
  type RunDetail,
  type RunRow,
} from './api';
import type { StreakBar } from '../components/Spark';
import {
  counted,
  datasetTotal,
  ledgerStatement,
  type Counted,
  type DatasetTotal,
} from '../datasets/scope';

/** One dimension as a row prints it: the word, and what the word is a claim about. */
export interface Dimension {
  label: string;
  title: string;
}

/** The EXECUTION dimension — Temporal's own `RunStatus`, unchanged and unwrapped. */
export function executionOf(row: Pick<RunRow, 'status'>): Dimension {
  return {
    label: row.status,
    title: `execution — Temporal's verdict on the caller's workflow: ${row.status}`,
  };
}

/**
 * The MATERIALIZATION dimension, rolled up over a run's Datasets.
 *
 * THREE DIFFERENT SILENCES, AND THEY ARE THREE DIFFERENT WORDS. A ledger that could not be read
 * (`null`, or a server too old to send the field) is `unknown`; a ledger that holds no record for
 * this run is `unrecorded`; a record that committed zero rows is `complete`, because row count
 * zero is a successful empty result and saying otherwise would report a failure that did not
 * happen. Collapsing any two of those is the defect this dimension was added to end.
 */
export function materializationOf(row: Pick<RunRow, 'materialization'>): Dimension {
  const m = row.materialization;
  if (!m) {
    return {
      label: 'unknown',
      title:
        'materialization — the ledger could not be read. This is NOT a claim that the run wrote nothing.',
    };
  }
  if (m.total === 0) {
    return {
      label: 'unrecorded',
      title: 'materialization — no writer recorded a Dataset for this run',
    };
  }
  const of = `${m.complete}/${m.total} Datasets · ${m.rows.toLocaleString()} rows committed`;
  if (m.failed > 0) {
    return { label: 'failed', title: `materialization — ${m.failed} failed · ${of}` };
  }
  if (m.pending + m.running > 0) {
    return {
      label: 'writing',
      title: `materialization — ${m.pending + m.running} still being written · ${of}`,
    };
  }
  return { label: 'complete', title: `materialization — every Dataset is queryable · ${of}` };
}

/**
 * The state of the run a detail panel is SHOWING — never of a different one.
 *
 * THIS IS THE FIX, PINNED. In the two seconds between pressing Run and the first
 * `/api/runs/:id` answer there is no detail yet, and reading the newest KNOWN run's status there
 * printed `completed` over a run that had just started — the previous run's verdict, under the
 * new run's id. The fallback is therefore matched BY ID against the run list, and the detail is
 * only trusted when it is a detail of this run: `starting` is the honest word for that gap.
 */
export function watchedRunState(
  runId: string | null,
  detail: Pick<RunDetail, 'runId' | 'settled' | 'lifecycle'> | null,
  runs: readonly RunRow[]
): string {
  if (runId === null) return 'never run';
  if (detail && detail.runId === runId) return detail.settled ? detail.lifecycle : 'running';
  return runs.find((r) => r.runId === runId)?.status ?? 'starting';
}

/** Runs newest first. The server already sorts (SQLite visibility rejects `ORDER BY`, so it does
 *  it client-side there too) — this list does not inherit that promise, it keeps it. */
export function newestFirst(runs: readonly RunRow[]): RunRow[] {
  return [...runs].sort((a, b) => b.startedAt - a.startedAt);
}

/**
 * Whether the run being watched is one the list cannot see.
 *
 * `/api/runs` discovers runs by the Actor DISPATCHES they made, so a run that has not dispatched
 * yet is not in it. MEASURED on a four-Machine sweep: `fleet.up` took 156 of 294 seconds before
 * the first dispatch, so for 53% of that run the list had nothing to select — which is exactly
 * the window in which an operator is looking hardest. The rail says so instead of appearing to
 * have lost the run.
 */
export function isUnlisted(runs: readonly RunRow[], watching: string | null): boolean {
  return watching !== null && !runs.some((r) => r.runId === watching);
}

/** One Dataset a run recorded: what the ledger says about it, and what the lake holds under that
 *  name. `info` is `null` when the catalog has no such Dataset at all. */
export interface RunDataset {
  name: string;
  /** The ledger's worst state across this Dataset's records. */
  state: string;
  /** Rows THIS RUN committed, per the ledger — never the Dataset's total. */
  rows: number;
  /**
   * When the ledger last wrote a record for this Dataset in this run (epoch ms; `0` when no
   * record carried a stamp).
   *
   * The count above is true AS OF this moment and not afterwards — a run still dispatching will
   * commit more. Taken from the ledger's own `updatedAt` rather than from when the page fetched
   * it: the fetch time dates the request, and only the ledger knows when the rows landed.
   */
  committedAt: number;
  info: DatasetInfo | null;
}

/**
 * The Datasets a run's ledger records name, joined to the catalog listing.
 *
 * A record is keyed by `(actor, version, node)` and an output Dataset is addressed by the actor
 * that produced it, so the actor IS the Dataset name here. A record whose rows never landed leaves
 * `info` null, and that absence is drawn as absence rather than as a zero-row Dataset.
 */
export function datasetsOf(
  records: readonly MaterializationRecord[],
  catalog: readonly DatasetInfo[]
): RunDataset[] {
  const by = new Map<string, RunDataset>();
  for (const rec of records) {
    const name = typeof rec.actor === 'string' ? rec.actor : '';
    if (!name) continue;
    const prev = by.get(name);
    const at = typeof rec.updatedAt === 'number' ? rec.updatedAt : 0;
    by.set(name, {
      name,
      // The worst state wins, for the same reason the projection ranks a failure first: "some of
      // this Dataset will not arrive" is the fact to act on.
      state: worseState(prev?.state, typeof rec.state === 'string' ? rec.state : 'pending'),
      rows: (prev?.rows ?? 0) + (typeof rec.rows === 'number' ? rec.rows : 0),
      // The NEWEST record's stamp, because the summed count is only as old as its last addend.
      committedAt: Math.max(prev?.committedAt ?? 0, at),
      info: catalog.find((d) => d.name === name) ?? null,
    });
  }
  return [...by.values()];
}

/**
 * The two counts a run's Dataset row shows — and the reason they are never divided.
 *
 * TWO AUTHORITIES, TWO STATEMENTS, TWO MOMENTS. What this run committed comes from the
 * MATERIALIZATION LEDGER, which is run-scoped by construction; what the name holds comes from the
 * CATALOG LISTING, summed over every dispatch of it. They are fetched by different polls on
 * different clocks, so "623 of 1,246" formed from these two is a ratio across two moments of a
 * Dataset that may have grown in between — the live-table snapshot bug, wearing a label.
 * {@link share} refuses the division because the statements differ, and the surface prints both
 * numbers with their scopes instead.
 *
 * The mislabelling this replaces was measured: the row said `<info.rows> rows in the Dataset, all
 * runs` where `info` was `catalog.find(d => d.name === name)` — the FIRST listing row, which is
 * one dispatch. It reported one run's contribution under the words "all runs".
 */
export function runDatasetCounts(
  d: Pick<RunDataset, 'name' | 'rows' | 'committedAt'>,
  catalog: readonly DatasetInfo[],
  listedAt: number
): { committed: Counted; lake: DatasetTotal | null } {
  return {
    committed: counted(d.rows, 'run', ledgerStatement(d.name, d.committedAt), d.committedAt),
    lake: datasetTotal(catalog, d.name, listedAt),
  };
}

const STATE_RANK: Record<string, number> = { complete: 1, pending: 2, running: 2, failed: 3 };

function worseState(a: string | undefined, b: string): string {
  if (a === undefined) return b;
  return (STATE_RANK[b] ?? 0) >= (STATE_RANK[a] ?? 0) ? b : a;
}

/** How many past runs a streak draws. Twenty is about as many bars as read as bars at 100px, and
 *  far enough back to show a workflow that has been failing since Tuesday. */
export const STREAK_RUNS = 20;

/** The last N runs as streak bars, OLDEST FIRST — `runs` arrives newest-first. */
export function streakOf(rows: readonly RunRow[], now = Date.now()): StreakBar[] {
  return rows
    .slice(0, STREAK_RUNS)
    .map((r) => ({
      ms: runDuration(r, now),
      status: r.status,
      label: `${r.runId} · ${r.status} · ${fmtDuration(runDuration(r, now))}`,
    }))
    .reverse();
}
