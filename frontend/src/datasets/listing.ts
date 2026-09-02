/**
 * The Datasets LISTING as a datagrid's row model — one row per Dataset, every fact the listing
 * knows, as VALUES rather than as markup.
 *
 * WHY THIS MODULE EXISTS AT ALL. The listing was a hand-rolled `<table>`: the facts lived inside
 * JSX, so "sort by size", "filter to the temporaries", "widen the tags column" were each a feature
 * to write by hand, and none of them were written. A grid gives all of it for free — but only if
 * every column has a VALUE to sort and filter on, which is what this builds. The rendering (badges,
 * chips, the spark, the buttons) stays in `panels/DatasetPage.tsx`; what is here is the answer to
 * "what does this column hold", once, testable in node.
 *
 * THE SORT VALUE AND THE DRAWN STRING ARE DIFFERENT FIELDS, ON PURPOSE, and that is not cosmetic:
 *
 *   - THE DERIVED NAME IS STORED UTC AND RENDERED LOCAL (ADR 0029 §2, "The UI renders local").
 *     {@link DatasetListingRow.name} is the CANONICAL string — the one the server rendered, the one
 *     `kontra dataset ls` prints — and {@link DatasetListingRow.nameLocal} is the same name with its
 *     instant re-drawn in the viewer's zone. The column sorts on `name`, so two operators in two
 *     zones get the SAME order out of one Dataset list, and each reads their own clock.
 *   - `rows`, `bytes`, `updatedAt` and the write rate are NUMBERS. `4.5 MB` and `512 KB` sort as
 *     text with the smaller one first; a grid that sorts sizes wrongly is worse than a list that
 *     does not sort at all, because it looks like an answer.
 *
 * THE SINGULAR RUN IS REFUSED WHERE IT WOULD BE A LIE (ADR 0029 §2, ADR 0017). A partition several
 * **Runs** wrote has no singular `runId`, so {@link DatasetListingRow.runId} is EMPTY there and the
 * plural {@link DatasetListingRow.runs} carries the set. The cell says "from 5 runs"; it never shows
 * the first of five dressed as the one. The empty singular is also what the column sorts on, which
 * is the honest ordering: a Dataset that no single Run owns has no id to be ordered by.
 *
 * A ROW'S IDENTITY IS `kind:name` AND NOTHING ELSE, so it is the SAME id on every 2 s catalog poll
 * even as rows, bytes and lifecycle move. That is what `getRowId` needs to update rows in place —
 * without it AG Grid replaces the whole row set on each tick and the operator's scroll, selection
 * and expansion go with it, which is the listing-side of the reset bug issue #14 fixed for the
 * results grid.
 */

import { sumSeries } from '../components/spark';
import { datasetKey } from '../state/store';
import { datasetExpiry, type ExpiryReading } from './expiry';
import { groupDatasetName, versionText, wholeDataset, type DatasetGroup } from './grouped';
import { localDatasetName, type LocalNameOptions } from './localName';
import { datasetState, type DatasetBadgeState } from './state';
import type { DatasetKind } from './scope';

/**
 * One Dataset, flattened to the values its columns sort, filter and draw from.
 *
 * The {@link DatasetListingRow.group} is carried whole because the row's ACTIONS act on a Dataset,
 * not on a projection of one — opening the console, deleting a temp, tagging a Run all need the
 * dispatches, the owner and the record key that only the group has.
 */
export interface DatasetListingRow {
  /** `kind:name` — stable across polls, which is the whole point. See the module header. */
  id: string;
  /** The Dataset itself, for the cells that act on it rather than read it. */
  group: DatasetGroup;
  /** The STORAGE name — the identity, the thing `SELECT * FROM <it>` names. */
  dataset: string;
  /**
   * The run-grain name (ADR 0029): the operator's RENAME when the record holds one (§4), else the
   * DERIVED default (§2), CANONICAL — UTC, exactly as the server rendered it. This is the sort and
   * filter value. Empty when no dispatch resolved a Run to name.
   */
  name: string;
  /** The same name with its instant in the VIEWER'S zone — what is drawn. Equal to {@link name}
   *  for a rename (an operator's own words have no instant to localise). */
  nameLocal: string;
  /** TRUE when {@link name} is an operator RENAME rather than the derived default. */
  renamed: boolean;
  /** TRUE when {@link nameLocal} differs from {@link name} by having been re-zoned — so the cell
   *  knows whether there is a canonical form worth showing on hover. */
  localized: boolean;
  kind: DatasetKind;
  /** A Run's TEMPORARY Dataset (ADR 0028): framework-named, owned, explicitly deleted. */
  temporary: boolean;
  /** The owning Run of a temporary Dataset; empty for a durable one. */
  owner: string;
  /** The §11 lifecycle, or `none` when no writer ever recorded one. */
  state: DatasetBadgeState;
  /** Rows under this name, across every dispatch and every Run — the `dataset` scope. */
  rows: number;
  bytes: number;
  /** The write-rate samples the app-level poll measured, summed over the dispatches. */
  rate: number[];
  /** The NEWEST sample — rows/second right now, and the value the rate column sorts on. Zero for a
   *  Dataset nothing is writing to, which is also what an unmeasured one reads as; the spark itself
   *  stays empty until there are two samples, so an unmeasured Dataset draws nothing rather than a
   *  flat line claiming "actively writing zero rows". */
  rateNow: number;
  /** `v0.1.0, v0.2.0` — every **Actor** version that wrote this name. */
  version: string;
  /** The NEWEST dispatch partition, `YYYY-MM-DDTHH-MM-SS`, verbatim: it is a directory on disk and
   *  UTC by construction, so unlike the name's instant it is NOT re-zoned. Empty for a standalone
   *  list, which has no dispatch. */
  dt: string;
  /** How many `version=…/dt=…` partitions are under this name. */
  dispatches: number;
  /**
   * The ONE **Run** this Dataset's record is keyed on (ADR 0029 §4) — the key a tag or a rename
   * addresses, and NOT an answer to "which Run made this".
   *
   * It is the group's NEWEST resolved Run. On a Dataset five **Runs** appended to, that is one of
   * the five: the tag affordance has to address a single record, and the newest is the Run an
   * operator watching a live Dataset is looking at (`datasets/grouped.ts`). {@link soleRun} is the
   * attribution, {@link runs} is the plural, and this is the KEY — three facts, three fields, so no
   * one of them can be read as another (ADR 0017).
   */
  runId: string;
  /**
   * The Run this Dataset can honestly be ATTRIBUTED to, or EMPTY when no single Run can be.
   *
   * The `run` column sorts and filters on this, never on {@link runId}: showing the newest of five
   * under a heading that reads "run" is the first-of-five lie ADR 0029 §2 refuses. Empty means the
   * cell counts instead — "from 5 runs" — and empty is also the honest sort key, because a Dataset
   * no single Run owns has no id to be ordered by.
   */
  soleRun: string;
  /**
   * Every **Run** this Dataset is known to be written by, ascending — the honest plural.
   *
   * THE UNION OF BOTH AUTHORITIES, which the group's `contributingRuns` alone is not. The lake's
   * per-file `run_id` statistics answer for anything written since rows carried a Run; the ledger's
   * DispatchRef (already resolved onto each dispatch as its `runId`) is the only authority for
   * output written before that, and for a temp before any row lands. Counting only the first would
   * report "from 0 runs" for a Dataset whose Runs are perfectly well known.
   */
  runs: string[];
  /** TRUE when a data file spans several **Runs**, so {@link runs} is a lower bound. */
  runsPartial: boolean;
  /** The tag SET of {@link runId}'s Dataset (ADR 0029 §1). */
  tags: string[];
  /** The table's column names, from the schema call. Empty while it is still reading. */
  columns: string[];
  /** When the newest file under this name was committed (epoch ms); 0 when nothing recorded one. */
  updatedAt: number;
  /**
   * WHAT RETENTION WOULD DO WITH THIS DATASET, and when — the sweep's reading, drawn on the row.
   *
   * IT IS ON THE LISTING and not only on the Dataset's own page because retention has to be visible
   * BEFORE it bites: the sweep is armed and hourly, and an operator scanning a hundred Datasets for
   * the ones about to age out will not open a hundred pages. Computed once per poll rather than in a
   * cell, so the grid's sort and filter act on the same reading the cell draws.
   */
  expiry: ExpiryReading;
  /**
   * The Run whose rows are STREAMING into this Dataset right now — set only while the Dataset is
   * `open` and a Run is resolved, and otherwise empty.
   *
   * IT IS DELIBERATELY NOT A COUNT. The live-tail cell renders an SSE subscription, and a cell whose
   * VALUE moved every two seconds would be refreshed by the grid every two seconds — tearing down
   * and re-opening the stream on each catalog poll. Keyed on the Run, the value is constant for as
   * long as the tail should live, so the grid leaves the cell alone and the stream survives.
   */
  live: string;
}

/** What the page knows that a group does not: the measured write rates, and the schema. */
export interface ListingInputs {
  /** The app-level poll's per-dispatch rate samples, keyed the way the store keys them. */
  series: Record<string, number[]>;
  /** Column names per Dataset name, or NULL while `/api/datasets/schema` is still answering —
   *  "reading…" and "this table has no columns" are different statements. */
  columns: Record<string, string[]> | null;
  /** Test seam for the name's local rendering; the viewer's own locale and zone by default. */
  name?: LocalNameOptions;
  /** The clock the expiry reading is measured against. `Date.now()` by default; injected so a test
   *  can age a Dataset without waiting, exactly as the sweep's own `now` is. */
  now?: number;
}

/**
 * Fold the page's groups into grid rows.
 *
 * ORDER IS PRESERVED — the response arrives newest-first and the grid's own sort is the operator's
 * to set. Re-ordering here would be a second opinion about recency held by a layer that did not
 * measure it, and it would fight the sort the operator picked.
 */
export function listingRows(
  groups: readonly DatasetGroup[],
  input: ListingInputs
): DatasetListingRow[] {
  return groups.map((g) => {
    // The canonical name the server rendered, then the same string in the viewer's zone. Never
    // re-derived here: the identity is the server's (ADR 0029 §2), and this only re-draws its
    // instant.
    const canonical = groupDatasetName(g) ?? '';
    const local = localDatasetName(canonical || undefined, input.name);
    const state = datasetState(wholeDataset(g));
    // Every Run either authority can name, as ONE set — see `runs`. The singular is filled only
    // when that set has exactly one member and no data file straddles Runs, which is ADR 0029 §2's
    // rule applied at the group's grain rather than the partition's.
    const runs = [
      ...new Set([
        ...g.contributingRuns,
        ...g.dispatches.map((d) => d.runId).filter((id): id is string => typeof id === 'string'),
      ]),
    ].sort();
    const sole = runs.length === 1 && !g.contributingRunsPartial ? runs[0]! : '';
    // The Dataset's rate is its dispatches' rates added up — usually only one of them is moving,
    // so this is the name's throughput without knowing which partition the live Run writes into.
    const rate = sumSeries(g.dispatches.map((d) => input.series[datasetKey(d)]).filter(Array.isArray));
    return {
      id: `${g.kind}:${g.name}`,
      group: g,
      dataset: g.name,
      name: canonical,
      nameLocal: local?.text ?? '',
      renamed: g.renamedTo !== undefined,
      localized: local?.localized === true,
      kind: g.kind,
      temporary: g.temporary,
      owner: g.owner ?? '',
      state,
      rows: g.total.total.rows,
      bytes: g.bytes,
      rate,
      rateNow: rate.length > 0 ? (rate[rate.length - 1] ?? 0) : 0,
      version: versionText(g),
      dt: g.dispatches[0]?.dt ?? '',
      dispatches: g.dispatches.length,
      runId: g.runId ?? '',
      soleRun: sole,
      runs,
      runsPartial: g.contributingRunsPartial,
      tags: g.tags,
      columns: input.columns?.[g.name] ?? [],
      updatedAt: g.updatedAt ?? 0,
      // The same five dispositions the sweep uses, in its precedence — `datasets/expiry.ts` holds
      // the one implementation and the contract with `backend/src/data/retention.ts`. `runs`
      // and not `sole`: the sweep addresses every contributing Run, so a Dataset several Runs wrote
      // is a candidate even though no singular answer exists for the `run` column beside it.
      expiry: datasetExpiry(
        {
          temporary: g.temporary,
          tags: g.tags,
          state,
          updatedAt: g.updatedAt ?? 0,
          kind: g.kind,
          runId: g.runId ?? '',
          runs,
        },
        input.now ?? Date.now()
      ),
      live: state === 'open' && g.runId ? g.runId : '',
    };
  });
}

/** The grid's row identity. Extracted so `getRowId` and the row builder cannot drift — an id that
 *  disagreed with the row it identifies would make the grid update the wrong row in place. */
export function listingRowId(row: Pick<DatasetListingRow, 'kind' | 'dataset'>): string {
  return `${row.kind}:${row.dataset}`;
}

/** `2026-08-19T14-49-20 · +2` — the newest dispatch, and how many more are under this name. The
 *  count is on the cell rather than in a column of its own because a dispatch is a GRAIN of this
 *  row, not a fact beside it. */
export function dispatchCell(row: Pick<DatasetListingRow, 'dt' | 'dispatches' | 'kind'>): string {
  if (row.kind === 'standalone') return 'standalone list';
  if (!row.dt) return '';
  return row.dispatches > 1 ? `${row.dt} · +${row.dispatches - 1}` : row.dt;
}

/**
 * What a tag on THIS Dataset means for how long it lives — the sentence the add-tag affordance
 * carries, and the one place the two answers are spelled out together.
 *
 * A DURABLE Dataset: untagged output expires after the repo's TTL and tagged output is KEPT (ADR
 * 0029 §3). That is the whole meaning of the tag, and it is the operator's judgement rather than
 * the author's policy — "this run turned out to matter" (§4).
 *
 * A TEMPORARY Dataset: the tag does NOT change its lifetime, because a temp has no clock to change.
 * The retention sweep never collects a temporary Dataset, tagged or not (`data/retention.ts`,
 * `kept-temporary`), and the explicit `deleteTemporaryDataset` still removes it — ADR 0028's
 * ownership is not overridden by a label. What the tag DOES do is real and worth saying: the record
 * is keyed by the **Run** (§4), and the Run's DURABLE output is kept by it. Tagging the temp of a
 * Run that promoted into `lame` keeps `lame`'s rows past the TTL.
 */
export function tagMeaning(row: Pick<DatasetListingRow, 'temporary'>): string {
  return row.temporary
    ? 'Tag this Run’s output — a tag is kept output (ADR 0029 §3).\n' +
        'On a TEMPORARY Dataset it does not extend a lifetime: a temp is never swept on a clock, ' +
        'and the delete button still removes it. What it keeps is the DURABLE output of the same ' +
        'Run — the record is keyed by the Run, not by the table.'
    : 'Tag this Dataset — untagged output expires after the retention TTL, tagged output is kept ' +
        '(ADR 0029 §3). The record is keyed by the Run, so the tag covers everything that Run wrote.';
}
