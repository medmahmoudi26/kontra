/**
 * What one run wrote — the Datasets side of "the output and the run that made it are not two
 * searches".
 *
 * IT READS THE CATALOG THE APP ALREADY HOLDS. `store.datasets` is polled once for the whole app
 * (`App.tsx` says why there is exactly one poller), and ADR 0029 §2 put the Run on the row: `runId`
 * is present wherever the server could name exactly ONE Run behind a partition, and
 * `contributingRuns` is the honest plural beside it. So "which Datasets did this run write" is a
 * filter over a listing that is already in memory, not a fetch and not a ledger read.
 *
 * WHY NOT THE MATERIALIZATION LEDGER. `RunDetail.materializationRecords` is the other authority and
 * it answers a different question — what the materializer RECORDED, which is empty for a Dataset a
 * caller published itself; `runStats.ts` prints that as `unrecorded` rather than `0` for exactly
 * this reason. The catalog answers "what is in the lake with this run's id on it", which is what a
 * link has to be true about: a name that is not in the catalog cannot be opened.
 *
 * WHAT DRAWS IT: `RunMonitor.tsx` on the open run, `WorkflowsPage.tsx` on a settled one, and
 * `DatasetTail.tsx` for the rows themselves. Nothing here renders.
 *
 * A PARTIAL ANSWER SAYS IT IS PARTIAL. `contributingRunsPartial` marks a row whose run list is a
 * LOWER BOUND — a data file spanning several Runs — so a run missing from it is not evidence that
 * the run wrote nothing. {@link runDatasets} carries that up rather than letting an empty list read
 * as a measurement.
 */

import type { DatasetInfo } from '../run/api';

/** One Dataset this run put rows into, and how the catalog knows. */
export interface RunDataset {
  info: DatasetInfo;
  /**
   * How the row was attributed to the run.
   *
   *   sole    `runId` — the server named exactly one Run behind this partition (ADR 0029 §4).
   *   among   `contributingRuns` — this Dataset accumulates, and this run is one of the writers.
   *   owner   a temporary Dataset the Run itself owns.
   */
  how: 'sole' | 'among' | 'owner';
}

export interface RunDatasets {
  wrote: RunDataset[];
  /**
   * At least one row in the catalog cannot list its Runs exhaustively, so an empty `wrote` is a
   * floor rather than a finding. Never used to soften a row that IS there — only to stop "none"
   * from being printed as a measurement.
   */
  partial: boolean;
  /**
   * The catalog has been read at all. FALSE is "nobody looked": the first poll has not answered, and
   * a run drawn as having written nothing before its lake was ever listed is the same lie one level
   * down from a run drawn as never having run.
   */
  read: boolean;
}

/**
 * The Datasets carrying this run's id, newest first.
 *
 * NEWEST FIRST BY `updatedAt`, falling back to the name, because a run that wrote four partitions
 * of one Actor wants the one still filling at the top — and a listing whose order changed between
 * two polls would move the link out from under the pointer.
 */
export function runDatasets(
  catalog: readonly DatasetInfo[],
  runId: string | null,
  listedAt: number
): RunDatasets {
  const read = listedAt > 0;
  if (runId === null || runId === '') return { wrote: [], partial: false, read };

  const wrote: RunDataset[] = [];
  let partial = false;
  for (const info of catalog) {
    if (info.contributingRunsPartial) partial = true;
    const how =
      info.runId === runId
        ? 'sole'
        : info.owner === runId
          ? 'owner'
          : info.contributingRuns?.includes(runId)
            ? 'among'
            : null;
    if (how === null) continue;
    wrote.push({ info, how });
  }

  wrote.sort(
    (a, b) =>
      (b.info.updatedAt ?? 0) - (a.info.updatedAt ?? 0) || (a.info.name < b.info.name ? -1 : 1)
  );
  return { wrote, partial, read };
}

/** What a row says about how it was attributed — one clause, because the difference decides whether
 *  a row count on the Datasets page is this run's or everybody's. */
export function attributionWords(how: RunDataset['how']): string {
  switch (how) {
    case 'sole':
      return 'this run is the only one behind this partition';
    case 'owner':
      return 'a temporary Dataset this run owns';
    default:
      return 'this run is one of several that wrote it — the total there is not this run’s';
  }
}
