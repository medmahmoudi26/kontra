/**
 * The RETENTION SWEEPER — untagged Datasets expire after a TTL this repo owns; tagged ones are
 * kept (ADR 0029 §3, §5).
 *
 * FIVE WAYS TO GET THIS WRONG, ALL FIXED HERE — the first four because the ADR named each as a way
 * to delete something an operator wanted, the fifth because a measured defect turned the TTL off for
 * the biggest Datasets on the box:
 *
 *   1. IT READS THE RECORD, NEVER THE SEARCH ATTRIBUTE. "Is this kept?" is a read of the Dataset
 *      RECORD (`DatasetRecordStore`) — the durable authority for tags — NOT a Temporal query for
 *      `KontraTag IS NULL`. `KontraTag` is a projection over the LIVE window that cannot be written
 *      after the execution closes, so it reports "untagged" for every Dataset tagged after its Run
 *      ended — which is MOST kept Datasets, since the operator path exists precisely for the run
 *      that "turned out to matter" (§4). A collector reading the search attribute would delete them
 *      all. So this reads the record, and if the record store is unreachable it does NOT sweep
 *      (below) rather than sweep blind.
 *
 *   2. THE CLOCK RUNS FROM LAST WRITE, NOT CREATION. Age is `now - lastWriteAt`, and `lastWriteAt`
 *      is the newest data-file commit the catalog holds for the Run (`DatasetInfo.updatedAt`), never
 *      the Run's start. A Run that appends for thirty hours would otherwise have its earliest chunks
 *      collected while it is still writing to them.
 *
 *   3. A GRACE WINDOW, NOT A DELETE AT EXACTLY T+TTL. Tagging is routinely retroactive — an operator
 *      tags at hour 23 because that is when the run turned out to be interesting — so the cutoff is
 *      `TTL + GRACE` behind now, not `TTL`, and the sweep does not race a tag landing at the edge.
 *
 *   4. A DATASET STILL BEING WRITTEN IS NEVER COLLECTED, REGARDLESS OF AGE. An `open` Dataset (§11
 *      of ADR 0023 — "while its producer appends, and equally after a producer died mid-append") is
 *      kept unconditionally. The lifecycle cannot tell a live appender from a crashed one, and the
 *      safe reading is to never race a live append: a crashed `open` Dataset lingering is not data
 *      loss, collecting a live one IS. It shows as `open` ("producer died") for a human to judge.
 *
 *   5. IT IS KEYED ON EVERY CONTRIBUTING RUN, NOT ON A SOLE ONE. A **Dataset** accrues — several
 *      **Runs** append to one `(actor, version, dt)` partition — and the sweep used to key its
 *      candidates on `DatasetInfo.runId`, the LISTING's singular field, which is deliberately absent
 *      the moment two Runs share a partition. That failed in both directions at once (reproduced by
 *      hand: `.scratch/post-merge-review/TRIAGE-2026-08-25.md` §2): a shared partition was exempt
 *      from the TTL and absent from the report entirely, and where the ledger DID name one of its
 *      Runs the sweep deleted under that Run alone, leaving the other's rows and ledger row behind
 *      permanently while reporting the Dataset collected. A tag on the arbitrary winner shielded
 *      rows nobody had tagged. Candidates are now one per (**Run**, partition), each reading its own
 *      record, each deleting only `WHERE run_id = <its own>`.
 *
 *      READ THIS BEFORE THE FIRST COLLECTING DEPLOYMENT: it makes the sweep collect strictly MORE
 *      than it used to, and what it newly reaches is the oldest, most-appended-to output on the box
 *      — including a durable Dataset built by promotion, whose rows carry their SOURCE Run's id. That
 *      is the policy working as ADR 0029 §3 states it, and it is exactly why collection is opt-in per
 *      deployment (`KONTRA_RETENTION_COLLECT`) and why the preview route exists. Read a preview
 *      before turning it on.
 *
 * THE TEMPORARY-DATASET RULE (ADR 0029 status block, ADR 0028): a temporary Dataset is the SECOND
 * thing that can remove a Dataset, and the two must AGREE on who owns a temp's lifetime. A temporary
 * Dataset is deleted EXPLICITLY (`deleteTemporaryDataset`, temp-datasets slice 03) and NEVER swept on
 * a clock — the whole point of a temp outliving its fleet is that triage happens later. So this sweep
 * KEEPS every temporary Dataset (the owner marker `DatasetInfo.temporary`), whatever its age, tag or
 * lifecycle. A temp a Run is still writing to is doubly kept — temporary AND `open`.
 *
 * THE TTL IS A CONSTANT THIS REPO OWNS, deliberately NOT the namespace's execution-retention config.
 * That the dev server's retention is also 24h is a coincidence (ADR 0025's standing warning): coupling
 * Dataset lifetime to an ops setting nobody connected to storage would make it change silently. This
 * module reads no namespace config; the number lives here.
 *
 * BUCKET-LIFECYCLE GC BY PREFIX IS REJECTED (§3): it would need tagging to move objects to a permanent
 * prefix, which fights the `run=`-first blob layout and its measured 50× LIST win. The sweep costs
 * code; the prefix scheme would cost the read path.
 */

import type { ObjectStore } from '../codec/objectStore';
import { datasetName } from './datasetName';
import {
  datasetRunIds,
  listDatasets,
  withDatasetNames,
  type DatasetInfo,
} from './datasets';
import type { DatasetDeviation } from './datasetRecords';
import type { MaterializationState } from './materialization';
import type { DispatchRef } from './materializationStore';
import type { RunWorkflow } from './runWorkflows';
import {
  LAKE,
  OUTPUT_SCHEMA,
  lakeConnection,
  lakeEnabled,
  parseDtPartition,
  resolveLakeConfig,
  safeName,
  type LakeConfig,
} from './parquet';

// THE TTL AND ITS GRACE ARE THE CONTRACT'S, NOT THIS MODULE'S. The browser computes a Dataset's
// expiry from the same two numbers, and it cannot import this file — `retention.ts` pulls DuckDB
// and the object store in behind it. Re-exported rather than moved out of reach, so every caller
// here reads exactly as it did.
import { DATASET_RETENTION_TTL_MS, DATASET_RETENTION_GRACE_MS } from '../../contract/datasets';

export { DATASET_RETENTION_TTL_MS, DATASET_RETENTION_GRACE_MS };


/**
 * One Run's Dataset as the sweep weighs it — the whole of what the KEEP/COLLECT decision reads. Built
 * by {@link gatherCandidates} from three authorities keyed on the **Run**: the lake (age, lifecycle,
 * temp marker, rows, and WHICH Runs wrote each partition), the ledger (which Run wrote which output),
 * and the Dataset record (tags). The pure {@link classifySweep} takes ONLY this — no store, no clock
 * of its own — so the decision is testable in isolation.
 *
 * ONE PER **RUN**, WHICH IS NOT ONE PER DATASET. A Run writing several tables is one candidate; a
 * partition several Runs wrote is one candidate EACH (fix #5).
 */
export interface SweepCandidate {
  /** The **Run** that wrote this output — the caller workflow's id (ADR 0023 §12), the record's key. */
  runId: string;
  /** What to CALL it in the report: the rename, else the derived name, else the bare table. */
  name: string;
  /**
   * The newest data-file commit the catalog holds for this Run (epoch ms) — the LAST WRITE, which is
   * the clock (fix #2), never the Run's creation. `0` means the catalog reported none, which is
   * treated as unknown-and-kept rather than infinitely old (fix against guessing, as `sweepUnits`
   * does with a missing mtime).
   */
  lastWriteAt: number;
  /** Total output rows the Run holds across its tables — reported as what a collection would free. */
  rows: number;
  /** The Dataset record carries at least one tag — KEPT (fix #1, read from the record not Temporal). */
  tagged: boolean;
  /**
   * THIS RUN may still be appending — KEPT (fix #4), and read from the LEDGER, not the lifecycle
   * marker. See {@link groupByRun} for the measurement that forced the change.
   */
  open: boolean;
  /** The Run owns this as a temporary Dataset — KEPT; temps are deleted explicitly, never swept. */
  temporary: boolean;
  /** The `output` schema tables this Run wrote, `safeName`d — what a collection DELETEs from. */
  tables: string[];
  /**
   * TRUE when at least one partition folded into this candidate was written by MORE THAN ONE **Run**
   * — so {@link SweepCandidate.rows} is an UPPER BOUND on what collecting this Run frees, not a count.
   *
   * The catalog counts rows per PARTITION, and the only thing that counts them per **Run** is
   * `datasetProvenance`, a row-grain scan (`data/datasets.ts`). A sweep must not open data files, so
   * the honest move is to say the number is a bound rather than to divide a partition's rows between
   * its contributors and report a fiction. The DELETE itself is unaffected: it is `WHERE run_id =`,
   * exactly this Run's rows, whatever the count said.
   */
  shared: boolean;
}

/** Why a candidate was kept or collected — the reason travels into the report so a dry run explains
 *  itself. Every value but `collect` is a keep, each naming which of the four safeguards applied. */
export type SweepDisposition =
  | 'collect'
  | 'kept-tagged'
  | 'kept-open'
  | 'kept-temporary'
  | 'kept-fresh';

/** What the sweep decided about one Run's Dataset. */
export interface SweepDecision {
  runId: string;
  name: string;
  disposition: SweepDisposition;
  /** `now - lastWriteAt` at decision time, so the report can show how far past (or short of) the
   *  cutoff each Dataset sat. */
  ageMs: number;
  lastWriteAt: number;
  rows: number;
  /** Present (and TRUE) only when {@link SweepCandidate.shared} was — `rows` is then an upper bound.
   *  Omitted rather than `false` so the common decision stays the small object a bounded sample of
   *  them can afford. */
  shared?: boolean;
}

/** What one sweep did — or, in dry-run, WOULD do. Serializable: it is an activity return value and a
 *  route response. */
export interface SweepReport {
  /** Candidate Runs weighed (every resolvable output Dataset). */
  scanned: number;
  /** Decisions with `disposition === 'collect'`. In a dry run these are what WOULD be collected. */
  collected: SweepDecision[];
  /** Every kept decision, each carrying which safeguard kept it. */
  kept: SweepDecision[];
  /** TRUE when nothing was deleted — the default, so a first invocation can never delete (fix, and
   *  the same posture `sweepUnits` takes). */
  dryRun: boolean;
  /** The constants the decision used, echoed so the report is self-describing. */
  ttlMs: number;
  graceMs: number;
  /** Runs actually purged (0 in a dry run). */
  purgedRuns: number;
  /** Rows those purges removed, by the catalog's pre-collection count (0 in a dry run). */
  purgedRows: number;
}

/**
 * How many decisions a {@link SweepSummary} may carry — the whole reason the summary exists.
 *
 * THE FULL REPORT CANNOT RIDE IN TEMPORAL HISTORY. `SweepReport` holds one {@link SweepDecision} per
 * **Run** in the lake, and it was returned as BOTH the activity result and the workflow result — so
 * an hourly schedule wrote the entire catalog into history twice per tick, forever. This repo has
 * measured that wall: a workflow accumulating references dies at roughly 8k with
 * `GrpcMessageTooLarge`, and the history a durable execution carries is charged on every replay, not
 * only on the write. So the durable path returns COUNTS plus a bounded sample, and the full list
 * stays where it can be paged and thrown away: `GET /api/datasets/retention/preview`.
 *
 * Twenty of each: enough that an operator reading a tick sees which Datasets are actually going and
 * whether the reasons look right, small enough that a tick's footprint is a few kilobytes whether the
 * lake holds ten Datasets or ten thousand.
 */
export const SWEEP_SAMPLE_SIZE = 20;

/**
 * What a SCHEDULED sweep reports — the same facts as {@link SweepReport}, at a size that does not
 * grow with the catalog.
 *
 * Every count the report could be asked for is here in full; only the per-Dataset detail is sampled,
 * and `truncated` says when the sample is one. The preview route returns the whole
 * {@link SweepReport}, because an HTTP response is read once and dropped, whereas a workflow result
 * is durable for the namespace's retention and replayed with the execution.
 */
export interface SweepSummary {
  /** Candidate Runs weighed — the same number {@link SweepReport.scanned} carries. */
  scanned: number;
  /** How many Runs landed on each disposition. Every candidate is in exactly one bucket, so these
   *  sum to {@link SweepSummary.scanned}. */
  counts: Record<SweepDisposition, number>;
  /** Rows under the collect decisions — what this tick freed, or in a dry run WOULD free. An upper
   *  bound where a shared partition contributed (see {@link SweepCandidate.shared}). */
  collectedRows: number;
  /** TRUE when nothing was deleted. */
  dryRun: boolean;
  ttlMs: number;
  graceMs: number;
  /** Runs actually purged (0 in a dry run). */
  purgedRuns: number;
  /** Rows those purges removed, by the catalog's pre-collection count (0 in a dry run). */
  purgedRows: number;
  /** At most {@link SweepSummary.sampleSize} of each, largest first by rows — the biggest blast
   *  radius is the part of a long list worth carrying. */
  sample: { collected: SweepDecision[]; kept: SweepDecision[] };
  /** The cap that produced the sample, echoed so a reader knows a 20-long list is a cap and not a
   *  count. */
  sampleSize: number;
  /** TRUE when either list was longer than the cap — "there is more, ask the preview route". */
  truncated: boolean;
}

/**
 * Reduce a full sweep report to the bounded thing a workflow may return.
 *
 * Pure and total, so the history-footprint property is a unit test rather than a claim: summarize a
 * report of ten and a report of ten thousand, and the serialized bytes differ only by the digits of
 * the counts.
 */
export function summarizeSweep(report: SweepReport, sampleSize = SWEEP_SAMPLE_SIZE): SweepSummary {
  const counts: Record<SweepDisposition, number> = {
    collect: 0,
    'kept-tagged': 0,
    'kept-open': 0,
    'kept-temporary': 0,
    'kept-fresh': 0,
  };
  for (const d of report.collected) counts[d.disposition] += 1;
  for (const d of report.kept) counts[d.disposition] += 1;
  const cap = Math.max(0, sampleSize);
  // Largest first: on a list too long to carry, the Datasets worth seeing are the ones whose loss
  // would cost the most. `slice` before `sort` is not an option — the biggest may be last.
  const biggest = (ds: readonly SweepDecision[]): SweepDecision[] =>
    [...ds].sort((a, b) => b.rows - a.rows || a.runId.localeCompare(b.runId)).slice(0, cap);
  return {
    scanned: report.scanned,
    counts,
    collectedRows: report.collected.reduce((n, d) => n + d.rows, 0),
    dryRun: report.dryRun,
    ttlMs: report.ttlMs,
    graceMs: report.graceMs,
    purgedRuns: report.purgedRuns,
    purgedRows: report.purgedRows,
    sample: { collected: biggest(report.collected), kept: biggest(report.kept) },
    sampleSize: cap,
    truncated: report.collected.length > cap || report.kept.length > cap,
  };
}

/**
 * The stores the sweep reads and, on collection, purges. `Pick`ed to the exact methods used so a test
 * hands minimal fakes and this module pulls in no singletons.
 *
 * `records` and `materialization` are LOAD-BEARING for correctness, not best-effort: a sweep that
 * cannot read the record store must not run (it would treat every Dataset as untagged), so their
 * failures PROPAGATE rather than being swallowed the way the listing route swallows them. `summaries`
 * is the third retention arm, touched only on collection.
 */
export interface RetentionDeps {
  store: ObjectStore;
  lake?: Partial<LakeConfig>;
  materialization: {
    listDispatches(sel?: {
      actor?: string;
      version?: string;
      dt?: string;
      limit?: number;
    }): Promise<DispatchRef[]>;
    purgeRun(runId: string): Promise<number>;
  };
  records: {
    list(runIds: readonly string[]): Promise<DatasetDeviation[]>;
    purgeRun(runId: string): Promise<number>;
  };
  /**
   * The Runs' caller-workflow identities (ADR 0029 §2). OPTIONAL, and the only optional READ here:
   * unlike the tags, this decides nothing — it only decides what a Dataset is CALLED in the report,
   * and its absence falls back to the producing Actor exactly as the listing does. It is wired all
   * the same so an operator reading a sweep sees the same string the Datasets page shows them.
   */
  workflows?: {
    list(runIds: readonly string[]): Promise<RunWorkflow[]>;
    purgeRun(runId: string): Promise<number>;
  };
  summaries?: { purgeRun(runId: string): Promise<number> };
}

export interface SweepOptions {
  /** Untagged TTL. Defaults to {@link DATASET_RETENTION_TTL_MS} — the repo constant, not the
   *  namespace config. */
  ttlMs?: number;
  /** Grace behind the TTL. Defaults to {@link DATASET_RETENTION_GRACE_MS}. */
  graceMs?: number;
  /** The clock. Defaults to `Date.now()`; injected so a test can age a Dataset without waiting. */
  now?: number;
  /** DEFAULTS TRUE — a dry run reports what WOULD be collected and deletes nothing. A sweep that
   *  deleted on its first call is one typo from a run's output, so collection is opt-in. */
  dryRun?: boolean;
  /** Liveness for the heartbeating activity that wraps this — called as candidates gather and as each
   *  Run is collected, so the workflow that AWAITS the sweep can report progress and Temporal sees the
   *  work is alive. */
  heartbeat?: (progress: { phase: 'gather' | 'collect'; runId?: string }) => void;
  /** How often the gather beats while ONE read is still in flight. Defaults to
   *  {@link GATHER_HEARTBEAT_MS}; a test shortens it rather than waiting thirty seconds. */
  heartbeatIntervalMs?: number;
}

/**
 * The KEEP/COLLECT decision, pure and total over its input — no store, no wall clock. This is the
 * whole of the policy, and every acceptance criterion of ADR 0029 §3/§5 is a property of THIS
 * function, so it is the one tested directly.
 *
 * Precedence is deliberate: TEMPORARY first (the temp/sweep agreement — a temp is never swept), then
 * TAGGED (the record says keep), then OPEN (still being written), then the age gate. The order only
 * shapes which reason a kept Dataset reports; the outcome (kept) is the same for the first three
 * whatever its age. A Dataset falls to `collect` only when it is durable, untagged, not open, and its
 * LAST WRITE is older than `TTL + GRACE`.
 */
export function classifySweep(
  candidates: readonly SweepCandidate[],
  opts: { ttlMs: number; graceMs: number; now: number }
): SweepDecision[] {
  const cutoff = opts.now - (opts.ttlMs + opts.graceMs);
  return candidates.map((c) => {
    const base = {
      runId: c.runId,
      name: c.name,
      ageMs: opts.now - c.lastWriteAt,
      lastWriteAt: c.lastWriteAt,
      rows: c.rows,
      ...(c.shared ? { shared: true as const } : {}),
    };
    if (c.temporary) return { ...base, disposition: 'kept-temporary' as const };
    if (c.tagged) return { ...base, disposition: 'kept-tagged' as const };
    if (c.open) return { ...base, disposition: 'kept-open' as const };
    // No known last write is unknown age, not infinite age — keep it rather than guess, exactly as
    // `sweepUnits` retains an object whose store reported no mtime.
    if (!c.lastWriteAt || c.lastWriteAt >= cutoff) return { ...base, disposition: 'kept-fresh' as const };
    return { ...base, disposition: 'collect' as const };
  });
}

/**
 * How often the gather beats while a single read is in flight.
 *
 * THE HEARTBEAT TIMEOUT IS TWO MINUTES (`workflows/retention.ts`) AND `maximumAttempts` IS 1, so a
 * read that outlives the timeout does not retry — the tick fails and the next SCHEDULED firing is the
 * only retry. This used to beat exactly once, after the first of four reads, which meant a catalog
 * scan longer than two minutes failed the tick before the first beat ever fired (measured:
 * `.scratch/post-merge-review/TRIAGE-2026-08-25.md` §3 — one beat at 392 ms and nothing across the
 * three reads after it). So every read is now wrapped: one beat when it starts, and one every
 * {@link GATHER_HEARTBEAT_MS} for as long as it runs. Thirty seconds is a quarter of the timeout,
 * which leaves three missed beats of slack before Temporal calls the activity dead.
 */
export const GATHER_HEARTBEAT_MS = 30_000;

/**
 * Run one read with the heartbeat held open for its whole duration.
 *
 * The timer is `unref`ed so it can never hold the process open, and every call is guarded: a
 * heartbeat is LIVENESS, and a throwing heartbeat must not be the thing that fails a sweep.
 */
async function reading<T>(
  heartbeat: SweepOptions['heartbeat'],
  everyMs: number,
  read: () => Promise<T>
): Promise<T> {
  const beat = (): void => {
    try {
      heartbeat?.({ phase: 'gather' });
    } catch {
      /* liveness only — never fail the sweep on a heartbeat */
    }
  };
  beat();
  const timer = setInterval(beat, everyMs);
  timer.unref?.();
  try {
    return await read();
  } finally {
    clearInterval(timer);
  }
}

/**
 * EVERY **Run** whose output this row holds — the key set the sweep works in, and NOT the listing's
 * singular `runId`.
 *
 * WHY THE SWEEP DIVERGES FROM THE LISTING HERE, deliberately. `DatasetInfo.runId` is filled only when
 * a partition has exactly ONE contributor, because a surface that has to print one name must not pick
 * a winner. A sweep has no such constraint: it deletes `WHERE run_id = <run>`, one Run at a time, so
 * it can act on a shared partition perfectly precisely — it just has to be keyed on the plural fact
 * the row already carries ({@link DatasetInfo.contributingRuns}) instead of the singular one.
 *
 * KEYING ON THE SINGULAR WAS A HOLE IN THE TTL, measured (TRIAGE-2026-08-25 §2), and it failed in
 * both directions at once:
 *
 *   - A partition two v2 **Runs** wrote resolved to NO `runId`, so `groupByRun` skipped it and it
 *     appeared in neither `collected` nor `kept` — untouchable by the TTL and invisible in the one
 *     report built to show the blast radius.
 *   - A partition the LEDGER knew both **Runs** of resolved to one ARBITRARY contributor (the newest
 *     dispatch wins in `withDatasetNames`), so the sweep collected under that Run alone and left the
 *     other's rows AND its ledger row behind for good, while reporting the Dataset collected — and,
 *     conversely, a tag on the arbitrary winner shielded rows the operator never tagged.
 *
 * The longest-lived Datasets are exactly the ones several **Runs** append to, so the singular key
 * exempted the largest output on the box from the policy that exists for it.
 *
 * `contributingRunsPartial` (a data file whose statistics straddle **Runs**) makes this set a LOWER
 * BOUND, and that is the safe direction: a contributor nobody can name is a contributor nobody
 * collects. Its rows survive; they are never deleted under another **Run**'s decision, because the
 * DELETE is per-`run_id`.
 */
function candidateRuns(info: DatasetInfo): string[] {
  if (info.kind !== 'output') return [];
  const runs = new Set<string>();
  // The ledger's/owner's answer first, so a Dataset written before `run_id` was stamped on rows —
  // which has no statistics to contribute — is still a candidate.
  if (info.runId) runs.add(info.runId);
  for (const r of info.contributingRuns ?? []) runs.add(r);
  return [...runs];
}

/**
 * Every **Run** the sweep must read a record for — the union of what the listing would ask about and
 * every contributor a shared partition names.
 *
 * It is a SUPERSET of `datasetRunIds`, and it has to be: the record is the tag authority, and asking
 * only about sole contributors is how a tagged **Run** inside a shared partition would read as
 * untagged and be collected. Both reads (the identities and the records) take this one set, so the
 * report can name a shared partition's contributors as well as decide about them.
 */
export function sweepRunIds(
  infos: readonly DatasetInfo[],
  dispatches: readonly DispatchRef[] = []
): string[] {
  const ids = new Set<string>(datasetRunIds(infos, dispatches));
  for (const info of infos) for (const r of candidateRuns(info)) ids.add(r);
  return [...ids];
}

/**
 * Assemble the sweep's candidates: one per (**Run**, resolvable output Dataset), joined across the
 * lake, the ledger and the Dataset record.
 *
 * IT REUSES THE LISTING'S NAME JOIN ({@link withDatasetNames}) so the sweep and the surface an
 * operator reads cannot disagree about what a Dataset is CALLED. It does NOT reuse the listing's
 * DEVIATION join: `withDatasetDeviations` keys on the row's singular `runId` and returns a shared
 * partition's row untouched, so the row-grain join cannot answer "is this Run's output tagged?" for
 * the very rows fix #5 exists for. Tags are read from the record, per **Run**, below.
 *
 * FAILS LOUD, DOES NOT SWEEP BLIND. The listing route swallows a ledger or record-store outage and
 * returns a partial list, because a partial list is harmless. A SWEEP cannot: if it could not read
 * the record store it would see every Dataset as untagged and collect kept output, which is fix #1's
 * whole point. So a failure to read the ledger or the record store PROPAGATES and the sweep aborts
 * with nothing deleted.
 *
 * IT HEARTBEATS THROUGH ALL FOUR READS, not after the first — see {@link GATHER_HEARTBEAT_MS}.
 *
 * WHAT NEVER BECOMES A CANDIDATE: a standalone list (an operator input, no Run); any output row that
 * names no Run at all — neither a ledger dispatch, nor an owner marker, nor a `run_id` statistic
 * (nothing to key a record read or a purge on). Both are simply absent — the sweep only ever acts on
 * a Dataset it can attribute to a Run.
 */
export async function gatherCandidates(
  deps: RetentionDeps,
  heartbeat?: SweepOptions['heartbeat'],
  heartbeatIntervalMs: number = GATHER_HEARTBEAT_MS
): Promise<SweepCandidate[]> {
  const every = heartbeatIntervalMs > 0 ? heartbeatIntervalMs : GATHER_HEARTBEAT_MS;
  const infos = await reading(heartbeat, every, () => listDatasets(deps.store, {}, deps.lake));

  // The ledger names the Run behind each (actor, version, dt) partition. A failure here is NOT
  // swallowed: without a Run id there is no record to read and no safe way to decide, so the sweep
  // aborts rather than treats everything as un-attributable.
  const dispatches = await reading(heartbeat, every, () => deps.materialization.listDispatches({}));

  // The one id set both remaining reads take — every Run any authority can name for any row.
  const runIds = sweepRunIds(infos, dispatches);

  // The identity the name renders. A failure here IS swallowed, unlike the two around it: a missing
  // caller identity changes the label in the report and nothing the sweep decides, and the fallback
  // (`withDatasetNames`) still names every row. Aborting a sweep over a cosmetic read would be the
  // opposite trade from the tags, where reading nothing means "everything is untagged, collect it".
  let identities: RunWorkflow[] = [];
  try {
    identities = await reading(heartbeat, every, async () => (await deps.workflows?.list(runIds)) ?? []);
  } catch {
    /* names fall back to the producing Actor's identity */
  }
  const named = withDatasetNames(infos, dispatches, identities);

  // The tags. THE authority the sweep keeps on — read from the record, never Temporal (fix #1). A
  // failure here aborts the sweep for the same reason.
  const deviations = await reading(heartbeat, every, () => deps.records.list(runIds));

  // INDEXED BY RUN, and NOT layered onto the rows with `withDatasetDeviations` the way the listing
  // does it. That join keys on the row's singular `runId`, so it returns a shared partition's row
  // untouched — it cannot attach a tag or a rename to it at all — which is the read half of the same
  // defect fix #5 fixes on the write half. The sweep asks the record about every Run it is about to
  // decide on, which is strictly more than the listing can show, and reads tags from nowhere else.
  return groupByRun(
    named,
    new Map(deviations.map((d) => [d.runId, d])),
    new Map(identities.map((i) => [i.runId, i])),
    // The liveness authority, out of the ledger read above — no extra call. One entry per Run, its
    // WORST dispatch state, which is what `listDispatches` already folds for the dashboard.
    new Map(dispatches.map((d) => [d.runId, d.state]))
  );
}

/**
 * Fold the joined output rows into one candidate per **Run** — every Run that wrote each row, not the
 * one the listing prints.
 *
 * A sharded Run writing several tables is ONE candidate; its tables collect together and its rows
 * sum, matching how the listing treats a Run's output as one Dataset. A partition several Runs wrote
 * produces one candidate EACH, every one of them reading its own record, and each marked
 * {@link SweepCandidate.shared} so the report says its row count is a bound.
 *
 * `temporary` IS PER-ROW: a temp is owned by the Run that opened it and the marker is that Run's.
 * `tagged` is per-Run — the record is Run-grain (ADR 0029 §4), so one contributor's tag keeps that
 * contributor's rows and nobody else's, which is the direction that makes a tag mean something on a
 * shared partition at all.
 *
 * ── `open` WAS PER-ROW TOO, AND THAT MADE THE WHOLE SWEEP A NO-OP ───────────────────────────────
 *
 * It used to read `if (i.state === 'open') c.open = true`, on the stated reasoning that "a partition
 * still being appended to keeps every contributor to it". The premise is false, and the consequence
 * was total.
 *
 * THE LIFECYCLE MARKER IS PER **DATASET NAME**, NOT PER PARTITION AND NOT PER RUN. It lives at
 * `datasets/<name>/_state.json` — ONE object per logical Dataset — and `publishBatch` REWRITES it to
 * `open` on every single append (`activities/datasets.ts`), while `sealed` is only ever written by a
 * caller's explicit close, which run output never performs. So the marker says `open` from the first
 * row an Actor ever produced until somebody closes it by hand, and one marker was being applied to
 * every historical Run-share of that Actor.
 *
 * MEASURED ON THE DEV BOX, 2026-09-24, before this changed:
 *
 *     GET /api/datasets/retention/preview  ->  scanned 84, collected 0
 *     dispositions:  kept-open 84   (100%)
 *     ages:          9.3h … 151.9h   against a 24h TTL + 6h grace
 *     markers:       12 `_state.json` objects, every one `open`, oldest written six days ago
 *
 * Every Dataset on the box, exempt forever, by construction. `collect` was unreachable — not rare,
 * UNREACHABLE — and nothing said so, because "kept-open" reads like a safety feature working.
 *
 * ── SO `open` IS NOW A FACT ABOUT THE **RUN**, FROM THE LEDGER ──────────────────────────────────
 *
 * The question fix #4 actually wants answered is "might THIS Run still append?", and the marker
 * cannot answer it: it describes a Dataset that other Runs also write to. The materialization ledger
 * can, and `gatherCandidates` already reads it one call earlier for the partition attribution — a
 * Run whose dispatches have not all reached `complete` has work outstanding, and work outstanding is
 * what "might still append" means.
 *
 * THIS IS SAFE PRECISELY BECAUSE OF FIX #5. Collection deletes `WHERE run_id = <this Run>`, so a
 * finished Run's rows can go while a live Run appends to the same table — they are different rows,
 * and the live Run's are not touched. Keeping A's rows because B is writing was never protecting
 * anything; it was only ever preventing everything.
 *
 * AND THE AGE GATE IS STILL UNDER IT. A candidate only reaches the ledger question at all once its
 * own newest write is TTL + GRACE behind (fix #2, fix #3). A Run that has written nothing for thirty
 * hours AND has no outstanding materialization is finished by both authorities, not one.
 */
function groupByRun(
  infos: readonly DatasetInfo[],
  deviations: ReadonlyMap<string, DatasetDeviation>,
  callers: ReadonlyMap<string, RunWorkflow>,
  liveness: ReadonlyMap<string, MaterializationState>
): SweepCandidate[] {
  const byRun = new Map<string, SweepCandidate>();
  for (const i of infos) {
    const runs = candidateRuns(i);
    for (const runId of runs) {
      const c =
        byRun.get(runId) ??
        ({
          runId,
          name: candidateName(i, runId, deviations.get(runId), callers),
          lastWriteAt: 0,
          rows: 0,
          tagged: false,
          open: false,
          temporary: false,
          tables: [],
          shared: false,
        } satisfies SweepCandidate);
      c.lastWriteAt = Math.max(c.lastWriteAt, i.updatedAt ?? 0);
      c.rows += i.rows;
      // The record, keyed by THIS Run — never the row's `tags`, which carry the sole contributor's
      // record and would shield (or expose) everyone else's rows along with it.
      if ((deviations.get(runId)?.tags.length ?? 0) > 0) c.tagged = true;
      // PER RUN, FROM THE LEDGER — see the header. A Run the ledger has never heard of is not
      // "maybe live", it is unattributed, and the age gate decides it; a Run with any dispatch not
      // yet `complete` has outstanding work and is kept.
      const state = liveness.get(runId);
      if (state !== undefined && state !== 'complete') c.open = true;
      if (i.temporary) c.temporary = true;
      if (runs.length > 1) c.shared = true;
      const table = safeName(i.name);
      if (!c.tables.includes(table)) c.tables.push(table);
      byRun.set(runId, c);
    }
  }
  return [...byRun.values()];
}

/**
 * What to CALL one **Run**'s share of a row: its rename, else the run-grain derived name, else the
 * bare table.
 *
 * The rename comes from the RECORD rather than from a row (the row-grain deviation join cannot carry
 * one for a shared partition at all), and the sole-contributor path otherwise takes the string
 * {@link withDatasetNames} already rendered, so the report and the Datasets page say the same words. A shared partition has no such string — the listing
 * refuses to name a row several Runs wrote — so the name is rendered here through the SAME
 * {@link datasetName} function from the same three parts, and only when this Run's caller identity is
 * actually recorded. Without it the row's own name stands: an Actor-grain fallback is what every
 * unattributed Dataset has always shown, and inventing a workflow identity would read exactly like a
 * real one.
 */
function candidateName(
  info: DatasetInfo,
  runId: string,
  deviation: DatasetDeviation | undefined,
  callers: ReadonlyMap<string, RunWorkflow>
): string {
  if (deviation?.renamedTo) return deviation.renamedTo;
  if (runId === info.runId) return info.datasetName ?? info.name;
  const caller = callers.get(runId);
  if (caller && info.dt) {
    return datasetName({
      workflow: caller.workflow,
      version: caller.version,
      runStartedAt: parseDtPartition(info.dt),
      runId,
    });
  }
  return info.name;
}

/**
 * Sweep untagged Datasets past their TTL. DEFAULTS TO A DRY RUN.
 *
 * Gather → classify → (only when `dryRun` is false) collect. The report says what was — or would be —
 * collected and which safeguard kept everything else, so the blast radius is inspectable before a
 * single delete.
 */
export async function sweepDatasets(deps: RetentionDeps, opts: SweepOptions = {}): Promise<SweepReport> {
  const ttlMs = opts.ttlMs ?? DATASET_RETENTION_TTL_MS;
  const graceMs = opts.graceMs ?? DATASET_RETENTION_GRACE_MS;
  const now = opts.now ?? Date.now();
  const dryRun = opts.dryRun ?? true;

  const candidates = await gatherCandidates(deps, opts.heartbeat, opts.heartbeatIntervalMs);
  const byRun = new Map(candidates.map((c) => [c.runId, c]));
  const decisions = classifySweep(candidates, { ttlMs, graceMs, now });
  const collected = decisions.filter((d) => d.disposition === 'collect');
  const kept = decisions.filter((d) => d.disposition !== 'collect');

  let purgedRuns = 0;
  let purgedRows = 0;
  if (!dryRun) {
    for (const d of collected) {
      opts.heartbeat?.({ phase: 'collect', runId: d.runId });
      const cand = byRun.get(d.runId);
      if (!cand) continue;
      await collectRun(deps, cand);
      purgedRuns += 1;
      purgedRows += cand.rows;
    }
  }

  return { scanned: candidates.length, collected, kept, dryRun, ttlMs, graceMs, purgedRuns, purgedRows };
}

/**
 * Collect one Run's Dataset: drop its output rows and every store's record of it.
 *
 * PHYSICAL, BY `run_id`, EXACTLY. Each of the Run's `output` tables is deleted from `WHERE run_id =
 * <run>`, so a partition another Run shares (two Runs of one Actor version in the same second) is
 * untouched — the same run-grain the ledger and the record are keyed on. The physical parquet is
 * reclaimed later by DuckLake's snapshot expiry + file cleanup (`data/maintenance.ts`), which is what
 * "durable output ages out by retention" has always meant here; this removes it from the catalog.
 *
 * THEN THE FOUR RETENTION ARMS: the materialization ledger, the run summaries, the Dataset record and
 * the Run's caller-workflow identity — so a collected Run leaves nothing behind that outlives its
 * data. `records.purgeRun` on an untagged Run removes nothing (it had no row), which is correct and
 * cheap; the identity row is the one arm that is always present for a Run started since it existed,
 * and leaving it would be the one table here that grows forever.
 */
async function collectRun(deps: RetentionDeps, cand: SweepCandidate): Promise<void> {
  if (lakeEnabled(deps.store, deps.lake)) {
    const conn = await lakeConnection(deps.store, resolveLakeConfig(deps.store, deps.lake));
    for (const table of cand.tables) {
      // The table name is a catalog identifier, checked against the catalog before it is
      // interpolated — an identifier cannot be parameterised, the discipline every read in
      // `datasets.ts` follows.
      if (!(await tableInCatalog(conn, OUTPUT_SCHEMA, table))) continue;
      await conn.run(
        `DELETE FROM ${LAKE}.${OUTPUT_SCHEMA}."${table}" WHERE run_id = '${cand.runId.replace(/'/g, "''")}'`
      );
    }
  }
  await deps.materialization.purgeRun(cand.runId);
  await deps.summaries?.purgeRun(cand.runId);
  await deps.records.purgeRun(cand.runId);
  await deps.workflows?.purgeRun(cand.runId);
}

/** Whether the catalog lists this output table — a local copy of `datasets.ts`'s guard so a name
 *  reaching a DELETE is one the catalog already holds. */
async function tableInCatalog(
  conn: Awaited<ReturnType<typeof lakeConnection>>,
  schema: string,
  table: string
): Promise<boolean> {
  const known = await conn.runAndReadAll(
    `SELECT count(*) FROM (SHOW ALL TABLES) WHERE database = '${LAKE}' ` +
      `AND schema = '${schema.replace(/'/g, "''")}' AND name = '${table.replace(/'/g, "''")}'`
  );
  return Number(known.getRows()[0]?.[0] ?? 0) > 0;
}
