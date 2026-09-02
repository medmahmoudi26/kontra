/**
 * The run read surface. A **Run** is one execution of a caller's workflow, identified by that
 * workflow's id (ADR 0023 §12).
 *
 * WHAT THIS USED TO BE. The orchestrator started runs: it minted a run id, derived an
 * `orch-`-prefixed workflow id from it, persisted a durable record BEFORE starting so losing
 * the tab could not lose the run, then started the graph interpreter and reconciled that
 * record against Temporal afterwards. All of it followed from the server owning execution.
 * With the caller owning it, the caller's workflow id IS the run id — there is one identifier,
 * Temporal already holds it, and a record written here could only ever be a copy that drifts.
 *
 * What survives is the part that was never about the interpreter: a run has TWO status
 * dimensions and they have different authorities (ADR 0017). Temporal answers "did it finish";
 * the materialization store answers "is the output queryable". They are combined only into a
 * reporting projection, never merged in storage — one label over three outcomes is the defect
 * that model exists to prevent.
 */

import {
  publicLifecycle,
  summarize,
  type MaterializationRecord,
  type MaterializationSummary,
  type PublicLifecycle,
} from './data/materialization';
import type { MaterializationStore } from './data/materializationStore';
import type { RunStatus } from '../contract/types';
import { pendingAsks, readAsks, type RunAsk } from './hitl';
import { readRunActivity, type RunActivity } from './runActivity';
import { describeRun, listRuns, type RunDescription, type RunRow } from './temporalClient';

/**
 * How many runs' ledger records are read at once. Mirrors `DESCRIBE_CONCURRENCY` in
 * `temporalClient.ts` for the same reason: a page of 200 runs must not open 200 database
 * round trips at once on a pool sized for a handful.
 */
const LEDGER_CONCURRENCY = 8;

/**
 * How many OPEN runs one page describes to learn what each is doing.
 *
 * THE LIST STOPPED DESCRIBING RUNS ON PURPOSE — status comes off the listing, and a describe per
 * row is what discovery used to cost. This buys back exactly one thing the listing cannot answer:
 * `stalled`. Bounded to open runs alone, and capped, because a list of 200 closed runs must not
 * pay for a question none of them can be asked. Past the cap the reading is `null` — an honest
 * "not determined here", never an optimistic `running`.
 */
const ACTIVITY_DESCRIBE_CAP = 40;

/**
 * One row of the run LIST, across BOTH dimensions (ADR 0017).
 *
 * The execution dimension keeps its existing name and meaning — `status`, exactly as Temporal
 * reports it and exactly as the CLI already reads it. The materialization dimension is this
 * run's ledger records rolled up, and the projection over the two sits BESIDE them rather than
 * replacing either: a row that showed one merged verdict is the defect ADR 0017 exists for.
 */
export interface RunListRow extends RunRow {
  /**
   * MATERIALIZATION dimension — authority the ledger.
   *
   * `null` means the ledger could not be read, which is NOT the statement a summary whose
   * `total` is 0 makes. "Nobody declared a record for this run" and "we do not know" are
   * different answers, and a store outage that reported the first about every run would be a
   * confident green label over an unknown.
   */
  materialization: MaterializationSummary | null;
  /** The projection over both dimensions. `null` whenever one of them is unknown — a projection
   *  over a single dimension is not a projection, it is that dimension wearing another word. */
  lifecycle: PublicLifecycle | null;
  /**
   * ACTIVITY dimension — what an OPEN run is doing (`runActivity.ts`).
   *
   * `null` on a closed run, and on an open one this page did not describe (see
   * {@link ACTIVITY_DESCRIBE_CAP}). Beside the other two rather than folded into `status`: a
   * run waiting on a human and a run making progress are both `running` to Temporal, and
   * one column per dimension is the rule ADR 0017 already sets for the other two.
   */
  activity: RunActivity | null;
  /** How many of this run's asks are still waiting on a human. Counted for CLOSED runs too: an
   *  unanswered ask on a run that has ended is a fact, not an absence. */
  pendingAsks: number;
}

/**
 * A run's status across BOTH dimensions (ADR 0017). The two raw dimensions are always
 * returned beside the projection, never replaced by it — that pairing is what stops a
 * single label from covering three different outcomes.
 */
export interface RunView {
  /** The caller's workflow id. There is no second identifier. */
  runId: string;
  /** The caller's workflow type, when Temporal still has the execution. */
  type: string;
  tenant: string;
  startedAt: number;
  /** 0 while the run is open. */
  closedAt: number;
  /** EXECUTION dimension — authority Temporal. "Did the caller's workflow finish?" */
  execution: RunStatus;
  /** MATERIALIZATION dimension — authority the application status store. */
  materialization: MaterializationSummary;
  /** The per-Dataset materialization records — the detail behind the summary. */
  materializationRecords: MaterializationRecord[];
  /** The PROJECTION over both. Never a `RunStatus` member. */
  lifecycle: PublicLifecycle;
  /** True once `lifecycle` can no longer change without new work being scheduled. */
  settled: boolean;
  /**
   * ACTIVITY dimension — running, parked or stalled (`runActivity.ts`). `null` on a closed run and
   * on one Temporal could not be asked about.
   */
  activity: RunActivity | null;
  /**
   * Every question this run published — pending, answered, expired and abandoned alike, oldest
   * first.
   *
   * READ FROM THE RUN'S OWN MEMO, so a parked run whose worker is down still answers. Empty on a
   * run that never asked anything, which is almost all of them.
   */
  asks: RunAsk[];
  /** Milliseconds the run has been waiting on the ask it has been parked on longest. 0 when it is
   *  waiting on nobody. The answer to "am I the bottleneck?". */
  parkedForMs: number;
}

export class RunLifecycle {
  constructor(
    /**
     * The materialization authority. Optional so the API still boots — and still serves
     * the execution dimension — with no status store reachable, the same way it boots
     * with no Temporal.
     */
    private readonly materialization?: MaterializationStore,
    /**
     * The EXECUTION authority, injectable purely so tests can be hermetic about it.
     *
     * `read` deliberately rethrows a Temporal error when the other authority has nothing to
     * say (see below), so any test that asserts a status for an unknown run is asserting
     * against a live cluster whether it means to or not. That is how three of these passed on
     * a developer's machine and 502'd in CI for months: the dev box had a Temporal dev server
     * up, the runner did not, and the failure read as a 10-second timeout rather than as a
     * missing dependency. Same class as the ambient-`.env` trap this file's sibling
     * (exploreRoutes.test.ts) already documents.
     */
    private readonly describe?: (runId: string) => Promise<RunDescription | undefined>,
    /** The clock. Injected so "parked for four minutes" and "unstarted for a minute" are numbers
     *  a test can state rather than ones it has to wait for. */
    private readonly now: () => number = () => Date.now()
  ) {}

  /**
   * Every run kontra can see, newest first, in both dimensions. See {@link listRuns} for what
   * "can see" means.
   *
   * The list carries the materialization dimension for the same reason `read` does: a run list
   * that reported only Temporal's verdict would say `completed` about a run whose output never
   * became queryable, which is the single green label over three outcomes that ADR 0017 exists
   * to stop. It is one dimension per column, never one word.
   */
  async list(
    filter: { tenant?: string; status?: string; limit?: number } = {}
  ): Promise<RunListRow[]> {
    const rows = await listRuns(filter);
    const ledger = await this.ledgerFor(rows.map((r) => r.runId));
    const live = await this.activityFor(rows);
    const now = this.now();
    return rows.map((row) => {
      const records = ledger?.get(row.runId);
      const described = live.get(row.runId);
      // The DESCRIBED memo wins where there is one: an open run's memo reaches visibility
      // asynchronously, and a listing that said "no pending asks" about a run that parked a second
      // ago is exactly the stale green label this dimension exists to prevent.
      const asks = readAsks(described?.memo ?? row.memo, now);
      return {
        ...row,
        materialization: records ? summarize(records) : null,
        lifecycle: records ? publicLifecycle(row.status, records.map((r) => r.state)) : null,
        pendingAsks: pendingAsks(asks).length,
        activity: described
          ? readRunActivity({
              execution: row.status,
              pendingAsks: pendingAsks(asks).length,
              inFlight: described.inFlight ?? [],
              now,
            })
          : null,
      };
    });
  }

  /**
   * Describe the OPEN runs of one page, so each can say what it is doing.
   *
   * OPEN ONLY, AND CAPPED. `listRuns` deliberately stopped describing every row — that fan-out is
   * what discovery used to cost — and this buys back the one question a listing cannot answer:
   * whether a running run is running, parked or stalled. A closed run is asked nothing, because
   * `null` is already the right answer for it.
   *
   * ONE FAILURE BLANKS ONE ROW, not the dimension. Unlike the ledger — which is one connection, so
   * a refusal for one run is a refusal for all — each describe is its own RPC against a run that
   * may have closed between the list and this call. A run that cannot be described gets `null`,
   * which reads as "not determined", and the rest of the page is unaffected.
   */
  private async activityFor(rows: RunRow[]): Promise<Map<string, RunDescription>> {
    const out = new Map<string, RunDescription>();
    const open = rows.filter((r) => r.status === 'running').slice(0, ACTIVITY_DESCRIBE_CAP);
    if (open.length === 0) return out;
    const describe = this.describe ?? describeRun;
    for (let i = 0; i < open.length; i += LEDGER_CONCURRENCY) {
      const batch = open.slice(i, i + LEDGER_CONCURRENCY);
      const seen = await Promise.all(
        batch.map(async (r) => {
          try {
            return await describe(r.runId);
          } catch {
            return undefined;
          }
        })
      );
      batch.forEach((r, j) => {
        const desc = seen[j];
        if (desc) out.set(r.runId, desc);
      });
    }
    return out;
  }

  /**
   * Every listed run's materialization records, keyed by run — or `null` when the ledger cannot
   * answer at all.
   *
   * N INDEXED POINT READS, NOT ONE GROUPED SCAN. `listForRun` reads the ledger's own
   * `idx_matnode_run` and the page is capped (`LIST_LIMIT`, 200), so this cost is bounded by
   * what the caller asked for; a `GROUP BY run_id` over the whole ledger would be one round trip
   * and would grow with every run ever materialized. It is also strictly smaller than what the
   * list already pays: discovering those runs describes each of them against Temporal first.
   *
   * ONE FAILURE BLANKS THE WHOLE DIMENSION, deliberately. The store is one connection: a read it
   * refuses for one run it is refusing for all of them, and falling back per row would print "no
   * output recorded" over runs that materialized perfectly.
   */
  private async ledgerFor(runIds: string[]): Promise<Map<string, MaterializationRecord[]> | null> {
    const store = this.materialization;
    if (!store) return null;
    const byRun = new Map<string, MaterializationRecord[]>();
    try {
      for (let i = 0; i < runIds.length; i += LEDGER_CONCURRENCY) {
        const batch = runIds.slice(i, i + LEDGER_CONCURRENCY);
        const records = await Promise.all(batch.map((id) => store.listForRun(id)));
        batch.forEach((id, j) => byRun.set(id, records[j] ?? []));
      }
    } catch {
      return null;
    }
    return byRun;
  }

  /**
   * Every question one run published, oldest first — or `undefined` when Temporal has no such
   * execution.
   *
   * ONE DESCRIBE, NO WORKER, NO HISTORY SCAN. The asks live in the run's memo, which rides on the
   * describe response — so a run parked with its worker down answers this exactly as well as a
   * healthy one, which is the entire reason an ask is not a query handler.
   *
   * `undefined` IS NOT AN EMPTY LIST. A run that asked nobody anything answers `[]`; a run
   * Temporal has dropped for retention answers `undefined`, and the caller then has an archive to
   * try. Collapsing the two would serve "this run asked nothing" about a run whose questions are
   * sitting in the archive.
   */
  async asks(runId: string): Promise<RunAsk[] | undefined> {
    const described = await (this.describe ?? describeRun)(runId);
    return described ? readAsks(described.memo, this.now()) : undefined;
  }

  /**
   * One run, across both dimensions. `undefined` only when NEITHER authority has heard of it.
   *
   * A run whose workflow Temporal has already dropped for retention but whose output is still
   * in the lake is still a run: answering 404 there would hide readable output, which is the
   * same class of mistake as reporting an empty result for a failed decode.
   */
  async read(runId: string): Promise<RunView | undefined> {
    let records: MaterializationRecord[] = [];
    try {
      records = (await this.materialization?.listForRun(runId)) ?? [];
    } catch {
      // Status store unreachable: report the execution dimension rather than nothing.
    }

    let described: RunDescription | undefined;
    try {
      // Resolved at CALL time, not as a default parameter: a default would read the
      // `describeRun` import when the class is constructed, which forces every existing
      // `vi.mock('./temporalClient')` to grow an export it never needed.
      described = await (this.describe ?? describeRun)(runId);
    } catch (err) {
      // Temporal is UNWELL, which is not the same answer as "no such run". Fall through only
      // when the other authority can still describe the run; otherwise the caller must hear
      // about the outage, because a 404 here would report a live run as one that never
      // existed — a typo and a cluster outage rendering identically is what this distinction
      // exists to prevent.
      if (records.length === 0) throw err;
    }
    if (!described && records.length === 0) return undefined;

    const execution: RunStatus = described?.status ?? 'pending';
    const lifecycle = publicLifecycle(
      execution,
      records.map((r) => r.state)
    );
    const now = this.now();
    const asks = readAsks(described?.memo, now);
    const waiting = pendingAsks(asks);
    return {
      runId,
      type: described?.type ?? '',
      tenant: described?.tenant ?? '',
      startedAt: described?.startedAt ?? 0,
      closedAt: described?.closedAt ?? 0,
      execution,
      materialization: summarize(records),
      materializationRecords: records,
      lifecycle,
      settled: lifecycle === 'completed' || lifecycle === 'output_failed',
      // `null` WITHOUT A DESCRIBE, not `running`. A run answered from the ledger alone is one
      // Temporal has dropped or could not be asked about, and there is nothing honest to say about
      // what it is doing right now.
      activity: described
        ? readRunActivity({
            execution,
            pendingAsks: waiting.length,
            inFlight: described.inFlight ?? [],
            now,
          })
        : null,
      asks,
      parkedForMs: waiting.reduce((longest, a) => Math.max(longest, a.waitedMs), 0),
    };
  }
}
