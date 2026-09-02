/**
 * One row per **Dataset**, not one row per dispatch.
 *
 * WHAT THE LISTING WAS SAYING. `/api/datasets` returns one row per `(name, version, dt)` — one
 * `version=…/dt=…` partition, which is one dispatch — so a workflow run three times put three rows
 * called `lame` in front of the operator, each reading `623 rows · sealed`, each looking like a
 * different thing. They are not different things. They are one Dataset that three Runs appended to,
 * and the surface said so nowhere.
 *
 * That is the same defect the count labelling fixed one layer up (`scope.ts`): a number whose scope
 * is not stated. Here the unstated scope was the ROW ITSELF. Labelling it "this dispatch" made the
 * row honest and left the list wrong — an operator scanning for "which Datasets do I have" was
 * reading a list of partitions.
 *
 * SO THE GRAIN OF THE LIST IS THE NAME, and the dispatches are what you open it to find. A group
 * carries its members verbatim, so nothing is lost: the per-dispatch rows are still there, still
 * addressable, still the thing the write-rate series is keyed on.
 *
 * THE TOTAL COMES FROM {@link datasetTotal}, which already existed and already summed exactly these
 * rows for the Catalog's panel. Two implementations of "the Dataset's total" is how the Catalog and
 * the Datasets page would come to disagree about `lame` while both polled the same response.
 */

import type { DatasetInfo } from '../run/api';
import { datasetTotal, type DatasetKind, type DatasetTotal } from './scope';

/** Every listing row that shares a name, and what they add up to. */
export interface DatasetGroup {
  kind: DatasetKind;
  name: string;
  /**
   * The dispatches, newest first — the listing rows verbatim.
   *
   * A standalone list has exactly one and it has no `dt`; an actor's output has one per
   * `(version, dt)`. Either way this is the array, not a count of it, because opening a dispatch
   * has to hand the console the SAME row the server described — its rows, its bytes, its lifecycle.
   */
  dispatches: DatasetInfo[];
  /** Rows across every dispatch, scoped `dataset` and stamped with the poll that measured it. */
  total: DatasetTotal;
  /** Bytes across every dispatch. */
  bytes: number;
  /** When the newest dispatch of this name was written. `undefined` when no row carries one. */
  updatedAt: number | undefined;
  /**
   * The **Actor** versions that wrote this name, newest dispatch first, deduplicated.
   *
   * More than one is a real and interesting state — the same Dataset written by two versions of an
   * Actor — and it is invisible from a total. It is NOT a defect to report: `version` is a partition
   * column, so this is what the lake holds.
   */
  versions: string[];
  /**
   * TRUE when this is a Run's TEMPORARY Dataset (temp-datasets slice 01). A temp is framework-named
   * (`tmp_…`) and owned by exactly one Run, so it is one name with one dispatch — but the flag is
   * read off any member rather than the name prefix, because the server's owner marker is the
   * authority on temp-ness and a durable Dataset that happens to start `tmp_` is not one.
   */
  temporary: boolean;
  /** The owning Run of a temporary Dataset; `undefined` for a durable one. */
  owner: string | undefined;
  /**
   * The **Run** whose record this group's tag/rename affordance ADDRESSES (ADR 0029 §4). A group can
   * span several Runs, each with its own record; the affordance acts on the NEWEST resolved one — the
   * same Run {@link groupDatasetName} names — because that is the Run
   * an operator watching a live Dataset is looking at. `undefined` when no dispatch resolved a Run (a
   * standalone list; a durable Dataset promoted in from a temp), and the affordance is then absent.
   */
  runId: string | undefined;
  /** The tag SET of {@link runId}'s Dataset (ADR 0029 §1), or empty. Read off the same newest
   *  resolved dispatch the derived name is. */
  tags: string[];
  /** The rename of {@link runId}'s Dataset (ADR 0029 §4), or `undefined` for the derived default. */
  renamedTo: string | undefined;
  /**
   * Every **Run** that contributed rows to this Dataset, ascending — the UNION over its dispatches.
   *
   * THIS IS THE GROUP'S HONEST ANSWER TO "WHICH RUN MADE THIS", and the group is exactly where the
   * plural becomes unavoidable: `lame` is ONE row on this page holding five **Runs**' output, so
   * {@link runId} — one Run, the newest resolved, which the tag/rename affordance addresses — is
   * emphatically not "the run that made this Dataset". Both are shown, and neither is a substitute
   * for the other (ADR 0017: two authorities, two fields).
   *
   * Empty for a Dataset whose rows carry no `run_id` (an operator-loaded list).
   */
  contributingRuns: string[];
  /** TRUE when some dispatch's statistics span several **Runs**, so {@link contributingRuns} is a
   *  lower bound. The surface says "at least N" rather than N. */
  contributingRunsPartial: boolean;
}

/**
 * Fold a listing into one row per Dataset.
 *
 * ORDER IS PRESERVED FROM THE RESPONSE, which arrives `ORDER BY dt DESC` — so the groups come out
 * newest-first by first appearance, and a name's dispatches stay in that order inside it. Sorting
 * here would be a second opinion about recency held by a layer that did not measure it.
 *
 * KIND IS PART OF THE IDENTITY, not just the name. An operator-loaded list and an actor's output
 * live in different schemas and can share a name; folding them together would produce one row whose
 * total spanned two tables and whose console could only open one of them.
 */
export function groupDatasets(
  catalog: readonly DatasetInfo[],
  measuredAt: number
): DatasetGroup[] {
  const groups = new Map<string, DatasetGroup>();
  for (const d of catalog) {
    const kind: DatasetKind = d.kind === 'standalone' ? 'standalone' : 'output';
    const key = `${kind}:${d.name}`;
    const existing = groups.get(key);
    if (existing) {
      existing.dispatches.push(d);
      continue;
    }
    // `total` is filled below, once every member is in. A partial total here would be one
    // dispatch's rows under the word "every run", which is the exact mislabelling being undone.
    groups.set(key, {
      kind,
      name: d.name,
      dispatches: [d],
      total: null as unknown as DatasetTotal,
      bytes: 0,
      updatedAt: undefined,
      versions: [],
      // Read off the member, never the name: the owner marker is the authority (see the field doc).
      temporary: d.temporary === true,
      owner: d.owner,
      // Filled below from the newest resolved dispatch — a partial value here would be one Run's
      // record before its newer sibling is in.
      runId: undefined,
      tags: [],
      renamedTo: undefined,
      // Filled below from EVERY member, for the same reason in the opposite direction: the union
      // is not knowable until the last dispatch of this name is in.
      contributingRuns: [],
      contributingRunsPartial: false,
    });
  }

  const out: DatasetGroup[] = [];
  for (const g of groups.values()) {
    // Summed from the group's OWN members rather than by re-filtering the catalog by name: two
    // kinds may share a name, and `datasetTotal` matches on the name alone.
    const total = datasetTotal(g.dispatches, g.name, measuredAt);
    if (!total) continue; // unreachable — a group exists only because it has a member
    const stamps = g.dispatches
      .map((d) => d.updatedAt)
      .filter((t): t is number => typeof t === 'number');
    // The record the group acts on is the NEWEST resolved Run's — dispatches arrive newest-first, so
    // the first with a runId is it. Its tags and rename come from the same row, keeping the affordance
    // and the derived name (which `groupDatasetName` reads off the same dispatch) describing one Run.
    const named = g.dispatches.find((d) => d.runId);
    out.push({
      ...g,
      total,
      bytes: total.bytes,
      updatedAt: stamps.length > 0 ? Math.max(...stamps) : undefined,
      versions: [...new Set(g.dispatches.map((d) => d.version).filter((v): v is string => !!v))],
      runId: named?.runId,
      tags: named?.tags ?? [],
      renamedTo: named?.renamedTo,
      // The UNION over every dispatch, sorted so two polls of one Dataset read the same. This is
      // the plural the single `runId` above cannot be: a name spans Runs, and that is the fact the
      // page could not state at all before.
      contributingRuns: [
        ...new Set(g.dispatches.flatMap((d) => d.contributingRuns ?? [])),
      ].sort(),
      contributingRunsPartial: g.dispatches.some((d) => d.contributingRunsPartial === true),
    });
  }
  return out;
}

/**
 * The row to hand the console when a GROUP is opened, rather than one of its dispatches.
 *
 * It carries no `version` and no `dt` on purpose. Those are what scope a listing row to one
 * partition, and their absence is what makes `listingScope` read `dataset` — so the console's header
 * says "every run" over a total, which is what the operator asked for by clicking the name instead
 * of a dispatch. The same absence is what makes `queryCommand` emit an unscoped
 * `kontra dataset query`, which is the right command for the same reason.
 *
 * Rows and bytes are the group's, so the header's number and the provenance panel below it are
 * finally describing the same rows. They differed before: the header counted one dispatch and the
 * panel counted the name.
 */
export function wholeDataset(g: DatasetGroup): DatasetInfo {
  return {
    kind: g.kind,
    name: g.name,
    rows: g.total.total.rows,
    bytes: g.bytes,
    ...(g.updatedAt === undefined ? {} : { updatedAt: g.updatedAt }),
    // The lifecycle is recorded per NAME server-side (one `_state.json` per Dataset), so every
    // dispatch of a name carries the same one and the group's is any member's.
    ...(g.dispatches[0]?.state ? { state: g.dispatches[0].state } : {}),
    // Carry temp-ness through so a temp opened from the list stays known to be one.
    ...(g.temporary ? { temporary: true } : {}),
    ...(g.owner === undefined ? {} : { owner: g.owner }),
    // THE RECORD TRAVELS WITH THE DATASET, because the Dataset's own page acts on it: the tag
    // affordance and the live row tail are both addressed by **Run** (ADR 0029 §4), so a console
    // opened from the listing without these can neither tag what it is showing nor watch it accrue.
    // `runId` keeps its group meaning exactly — the NEWEST resolved Run, the record's key, never
    // "the Run that made this" — which is why the plural travels beside it rather than instead of it.
    ...(g.runId === undefined ? {} : { runId: g.runId }),
    ...(g.tags.length === 0 ? {} : { tags: g.tags }),
    ...(g.contributingRuns.length === 0 ? {} : { contributingRuns: g.contributingRuns }),
    ...(g.contributingRunsPartial ? { contributingRunsPartial: true } : {}),
  };
}

/**
 * The run-grain name to show on a group row: the operator's RENAME when the record holds one (ADR
 * 0029 §4), else the DERIVED default (§2). Read off the newest dispatch that resolved a Run.
 *
 * A group can span dispatches from several Runs, each with its own name, so the row shows the most
 * recent — the Run an operator watching a live Dataset is looking at — and the rest are one grain
 * down, per dispatch. `undefined` when no dispatch resolved to a Run (a standalone list; a durable
 * Dataset promoted in from a temp). The DERIVED string is NEVER re-derived here — it is the
 * server's, byte-identical to `kontra dataset ls` — and `renamedTo ?? datasetName` is how the
 * default is used whenever no rename exists.
 *
 * It lives beside the group rather than in the page that draws it because two surfaces now read it:
 * the listing's name column and the row model behind it (`datasets/listing.ts`).
 */
export function groupDatasetName(g: Pick<DatasetGroup, 'dispatches'>): string | undefined {
  const d = g.dispatches.find((x) => x.datasetName);
  return d ? (d.renamedTo ?? d.datasetName) : undefined;
}

/** `v0.1.0, v0.2.0` — or nothing when no dispatch recorded a version. */
export function versionText(g: DatasetGroup): string {
  return g.versions.map((v) => `v${v}`).join(', ');
}

/** `3 dispatches` / `1 dispatch` / `standalone list`. The word a group's members are counted in. */
export function dispatchText(g: DatasetGroup): string {
  if (g.kind === 'standalone') return 'standalone list';
  const n = g.dispatches.length;
  return `${n} ${n === 1 ? 'dispatch' : 'dispatches'}`;
}

/**
 * WHICH **RUN** MADE THIS — `from nscheck-1787150959`, `from 5 runs`, or nothing.
 *
 * THE QUESTION THE PAGE COULD NOT ANSWER. A Dataset listed as `lame_demo · 430 rows · sealed` said
 * nothing about the run behind it, and `tmp_4e9b1b23` said nothing either — the operator had to go
 * and look somewhere else, having first guessed where. The **Run** is now on the row.
 *
 * ONE RUN IS NAMED, SEVERAL ARE COUNTED, and the difference is the whole design. A durable
 * **Dataset** accumulates across **Runs**; naming the first of five would be the lie the plural
 * exists to prevent, so several are counted and {@link runTitle} lists them on hover. "at least" is
 * for the case where a data file spans **Runs** and the statistics can only bound the set.
 *
 * EMPTY for a Dataset whose rows carry no **Run** at all — an operator-loaded list — because a
 * column reading `from 0 runs` invites the reader to wonder which ones went missing.
 */
export function runText(g: Pick<DatasetGroup, 'contributingRuns' | 'contributingRunsPartial'>): string {
  const n = g.contributingRuns.length;
  if (n === 0) return '';
  if (n === 1 && !g.contributingRunsPartial) return `from ${g.contributingRuns[0]}`;
  return `from ${g.contributingRunsPartial ? 'at least ' : ''}${n} runs`;
}

/** The **Runs** behind a Dataset, one per line — what {@link runText} counts, spelled out. */
export function runTitle(
  g: Pick<DatasetGroup, 'contributingRuns' | 'contributingRunsPartial'>
): string {
  if (g.contributingRuns.length === 0) return '';
  const head = g.contributingRunsPartial
    ? 'Runs that wrote this Dataset (at least — a data file spans several, so the set is a bound):'
    : 'Runs that wrote this Dataset:';
  return `${head}\n${g.contributingRuns.join('\n')}`;
}
