/**
 * What a count counted — the vocabulary every row count in the console states out loud.
 *
 * THE MEASURED CONFUSION. `SELECT count(*) FROM lame` returns 1,246: two **Runs** of one workflow
 * appending to one **Dataset** name. The Datasets listing shows 623 against that same name,
 * because a listing row is ONE dispatch's contribution. Both numbers are correct and neither was
 * labelled, so an operator comparing them concludes something is broken. Nothing was broken; the
 * console simply never said which rows each number was about.
 *
 * THREE SCOPES, AND EVERY COUNT NAMES ITS OWN:
 *
 *   dataset    every **Run** that ever wrote the name — the number `count(*)` over the name
 *              returns, and the rows the Dataset console's editor reads.
 *   run        one **Run**'s contribution to that name.
 *   dispatch   one `version=…/dt=…` partition. `version` is a PARTITION column, so a listing row
 *              holds exactly one **Actor** version: a Run that dispatched at two versions has two
 *              listing rows, which is why a dispatch is not a synonym for a Run.
 *
 * A COUNT OVER ONE RUN STILL SAYS SO. A Dataset written by exactly one Run reads unambiguously
 * today and ambiguously the second time the workflow runs — "unlabelled because it happens to be
 * unambiguous" is precisely how the 1,246-vs-623 confusion arrived.
 *
 * AND TWO NUMBERS FROM TWO QUERIES ARE NEVER DIVIDED — see {@link share}, which refuses. That is
 * the same discipline as the live-table snapshot rule: a Dataset a Run is still appending to grows
 * between two statements, so a share taken across them is wrong by whatever landed in between, and
 * this repo has shipped that bug before.
 */

import type { DatasetInfo } from '../run/api';

/** Which rows a number counted. */
export type CountScope = 'dataset' | 'run' | 'dispatch';

/** The two kinds of Dataset. A standalone list is loaded by an operator, never written by a Run. */
export type DatasetKind = 'output' | 'standalone';

/** How one scope is said, and what the sentence claims. */
export interface ScopeWords {
  /** Drawn in the surface beside the number — never only in a tooltip. */
  label: string;
  /** The long form, for the reader who needs to know exactly which rows. */
  title: string;
}

/**
 * The words for one scope.
 *
 * A STANDALONE LIST IS NOT SPOKEN OF IN RUNS. `kontra dataset create` loads it; no Run wrote it,
 * so "every run" would be a claim about a producer that does not exist. It is one table with no
 * partitions, so its listing row IS the whole list.
 */
export function scopeWords(scope: CountScope, kind: DatasetKind = 'output'): ScopeWords {
  if (scope === 'dataset') {
    return kind === 'standalone'
      ? {
          label: 'the whole list',
          title:
            'Every row under this name. An operator-loaded list has no dispatches and no Run ' +
            'wrote it, so this is the only count it has.',
        }
      : {
          label: 'every run',
          title:
            'Every Run that ever wrote this name, across every dispatch — the Dataset total. ' +
            '`SELECT count(*) FROM <name>` returns this number.',
        };
  }
  if (scope === 'run') {
    return {
      label: 'this run',
      title:
        "One Run's contribution to this Dataset — the rows carrying this run id. A Dataset " +
        'outlives the Run that wrote it and later Runs append to the same name, so this is ' +
        'smaller than the Dataset total whenever more than one Run has written.',
    };
  }
  return {
    label: 'this dispatch',
    title:
      'One dispatch — the rows in a single `version=…/dt=…` partition. `version` is a partition ' +
      'column, so a Run that dispatched at two Actor versions has two of these.',
  };
}

/** `1,246 rows · every run` — the number and its scope, in that order, everywhere. */
export function countText(rows: number, scope: CountScope, kind: DatasetKind = 'output'): string {
  return `${rows.toLocaleString()} rows · ${scopeWords(scope, kind).label}`;
}

/**
 * `as of 02:15:07` — when the statement behind a number answered.
 *
 * Printed for a count that may still be growing: a **Dataset** is `open` while a **Run** appends
 * to it, so a total read a minute ago is a fact about that minute. Time only, not the date: this
 * dates a number an operator is looking at now, and a full timestamp buries the part that changes.
 */
export function asOfText(measuredAt: number): string {
  return `as of ${new Date(measuredAt).toLocaleTimeString()}`;
}

/**
 * A number, the rows it counted, and the ONE statement that measured it.
 *
 * `statement` is an identity, not a description: two counts carry the same token only when one
 * statement produced both. That is what {@link share} checks, and it is why the token includes the
 * moment — a second read of the same Dataset is a different statement about a different moment.
 */
export interface Counted {
  rows: number;
  scope: CountScope;
  statement: string;
  /** When that statement answered (epoch ms). */
  measuredAt: number;
}

export function counted(
  rows: number,
  scope: CountScope,
  statement: string,
  measuredAt: number
): Counted {
  return { rows, scope, statement, measuredAt };
}

/** The `/api/datasets` listing poll — one catalog scan, so every row of one response shares it. */
export function listingStatement(measuredAt: number): string {
  return `datasets-listing@${measuredAt}`;
}

/** One `/api/datasets/:name/provenance` answer: buckets and total from a single scan. */
export function provenanceStatement(p: { name: string; measuredAt: number }): string {
  return `provenance:${p.name}@${p.measuredAt}`;
}

/**
 * One Run's materialization ledger, as of the last ledger row written for `subject` — the Dataset
 * the rows were committed to. A different authority from the listing and from a provenance scan,
 * which is exactly what {@link share} needs to be able to see.
 */
export function ledgerStatement(subject: string, measuredAt: number): string {
  return `ledger:${subject}@${measuredAt}`;
}

/**
 * What share one count is of another — `null` unless ONE statement measured both.
 *
 * THE REFUSAL IS THE POINT. "623 of 1,246 rows, from this run" is only true when both numbers came
 * out of the same scan; the ledger's count of what a Run committed and the catalog's total for the
 * name are two authorities answering at two moments, and dividing them produces a percentage that
 * looks measured and is not. The console shows both numbers, each labelled with its scope, and
 * draws no bar between them.
 */
export function share(part: Counted, whole: Counted): number | null {
  if (part.statement !== whole.statement) return null;
  if (whole.rows <= 0) return null;
  return part.rows / whole.rows;
}

/** The scope of one listing row: a partitioned output dispatch, or a whole standalone list. */
export function listingScope(d: Pick<DatasetInfo, 'dt'>): CountScope {
  return d.dt ? 'dispatch' : 'dataset';
}

/** Every listing row written under one name, added up — see {@link datasetTotal}. */
export interface DatasetTotal {
  /** Rows across every dispatch of the name, scoped `dataset` and stamped with the poll. */
  total: Counted;
  bytes: number;
  /** How many listing rows the total is made of — one per `(Actor version, dispatch)`. */
  dispatches: number;
  /** Which noun the total is said in: an operator-loaded list is never spoken of in Runs. */
  kind: DatasetKind;
}

/**
 * The Dataset's total from the LISTING, by summing every row that carries the name.
 *
 * `catalog.find(d => d.name === name)` is what the console did, and it returns ONE dispatch's rows
 * while the caption beside it said "all runs" — the mislabelled count this slice exists to end. A
 * name has one listing row per `(version, dt)` partition, so the total is the sum of them.
 *
 * ONE RESPONSE, ONE SNAPSHOT. Every row summed here came from the same `/api/datasets` poll, so
 * the total is a fact about that poll's moment — which is why the moment is carried with it rather
 * than being stamped by whoever renders it.
 */
export function datasetTotal(
  catalog: readonly DatasetInfo[],
  name: string,
  measuredAt: number
): DatasetTotal | null {
  const rows = catalog.filter((d) => d.name === name);
  if (rows.length === 0) return null;
  return {
    total: counted(
      rows.reduce((n, d) => n + d.rows, 0),
      'dataset',
      listingStatement(measuredAt),
      measuredAt
    ),
    bytes: rows.reduce((n, d) => n + d.bytes, 0),
    dispatches: rows.length,
    // From the rows themselves: a name is one kind or the other, and the two live in different
    // schemas, so the first row's kind is the name's.
    kind: rows[0]!.kind === 'standalone' ? 'standalone' : 'output',
  };
}
