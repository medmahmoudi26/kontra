/**
 * The reduced event log, kept after Temporal drops the execution (ADR 0025).
 *
 * WHY THIS EXISTS. A **Dataset** outlives the **Run** that wrote it by design — durable, queryable,
 * still there months later. The Run's STORY is not: retention drops the execution and the rows
 * survive while the account of how they came to exist does not. MEASURED on this controller
 * (2026-08-16, Temporal Server 1.31.0): namespace `default` carries
 * `WorkflowExecutionRetentionTtl = 24h`, and `temporal workflow list` holds nothing older than the
 * last few hours — while the lake still answers for runs from weeks ago.
 *
 * THE ARCHIVE IS THE REDUCER'S OUTPUT, NOT A SECOND FORMAT. What is stored is exactly what
 * {@link mapHistory} produced for the live path — payload-free (ADR 0007), the same `EVENT_CAP`,
 * the same `HEAD_KEEP`, the same explicit `elided`. There is deliberately no second reducer and no
 * second cap to keep in step, which is why a log with a hole says so after retention exactly as it
 * said so before. MEASURED by sweeping this controller: the three runs it still held archived to
 * 38,123 / 53,635 / 53,011 bytes, the last being the 305-event, 294-second `nscheck-1786831339` run
 * — ~174 bytes an event — so a capped log cannot exceed roughly 175 KB, whatever the run did. That
 * bound is what makes "keep it" an honest answer rather than a deferred storage problem.
 *
 * IT IS WRITTEN FROM OUTSIDE THE RUN, AFTER IT CLOSES. The obvious worry — "a run that dies never
 * reaches a clean close, and that is the run whose story matters most" — is what made incremental,
 * caller-side writes look necessary. It dissolves on one measurement: a workflow started on a queue
 * nobody polls and then TERMINATED reports a `closeTime` and a `WorkflowExecutionTerminated` close
 * event (probe `kontra-archive-probe-1`, 2026-08-16). Failed, terminated, timed-out and cancelled
 * executions are all CLOSED executions, and retention is measured from the close — so a sweep that
 * watches for closed runs sees every one of them. A step inside the caller's workflow would not:
 * it is the step a dying run does not reach.
 */

import { ObjectStore } from './codec/objectStore';
import { partSafe } from './codec/shard';
import { dtPartition } from './data/parquet';
import type { RunHistory } from './history';
import { readAsks, type RunAsk } from './hitl';
import { LIST_LIMIT, fetchRunHistory, listRuns, type RunRow } from './temporalClient';

/** The envelope's version. Bumped only if the STORED shape changes; `RunHistory` growing a field
 *  does not, because every reader of it already treats new fields as optional. */
export const ARCHIVE_VERSION = 1;

/** The top-level prefix. Beside `cas/`, `units/`, `datasets/` and `output/` — and, per ADR 0025,
 *  outside every retention sweep in this repo: `sweepUnits` lists `units/` and nothing else. */
export const ARCHIVE_ROOT = 'history';

/** How often the sweep runs, and the switch that turns it off. 15 minutes against a 24-hour
 *  retention is ~96 chances to catch any given run; the cost of a pass that finds nothing new is
 *  one visibility query plus a HEAD per closed run. */
const DEFAULT_INTERVAL_MS = 15 * 60_000;

/**
 * Every object for one workflow id. `run=` LEADS, which is the measured layout (`units/run=…`, a
 * 50× LIST win) and not a stylistic echo of it: the only read this key serves is scoped to one run.
 */
export function archivePrefix(workflowId: string): string {
  return `${ARCHIVE_ROOT}/run=${partSafe(workflowId || 'run')}/`;
}

/**
 * One archived log: `history/run=<id>/dt=<YYYY-MM-DDTHH-MM-SS>/log.json`.
 *
 * `dt=` is the run's START instant, in the same second-precision spelling `units/` and `output/`
 * use. It is there so a REUSED workflow id gets its own object instead of overwriting the story of
 * the run before it — `kontra-fleet/dns` is the id of every fleet bring-up AND teardown, so an id
 * is not by itself a unique thing to archive. ISO strings sort lexicographically in time order,
 * which is what lets the read take the newest without parsing a date.
 */
export function archiveKey(workflowId: string, startedAt: number): string {
  return `${archivePrefix(workflowId)}dt=${dtPartition(startedAt)}/log.json`;
}

/** What one stored object holds. The run coordinates ride ALONGSIDE the log rather than being
 *  parsed back out of the key, so a reader never has to trust a path. */
export interface ArchivedLog {
  v: number;
  /** The caller's workflow id — the run id, and the only identifier a Run has. */
  runId: string;
  /** Epoch ms the run started, and the `dt=` this object is filed under. */
  startedAt: number;
  /** Epoch ms the execution closed. Never 0 here: an open run is never archived. */
  closedAt: number;
  /** Epoch ms this object was written. Surfaced to the console, which says how old the account is. */
  archivedAt: number;
  /** The reducer's output, verbatim. */
  history: RunHistory;
  /**
   * Every question this run asked a human, and what it was told.
   *
   * BESIDE THE LOG, NOT INSIDE IT, and that is not a second reducer. An ask lives in the run's MEMO
   * (`hitl.ts`), which is a different Temporal surface from its history — the reduced log records
   * that an ask happened and that a signal answered it, and cannot record the sentence, because
   * doing so would mean decoding a payload (ADR 0007). So the memo is captured here, at the one
   * moment it is both final and still readable: the run has closed, so no further ask can appear,
   * and retention has not yet dropped it.
   *
   * Absent on a log written before this field existed, and empty on the overwhelming majority of
   * runs, which never ask anybody anything.
   */
  asks?: RunAsk[];
}

/** The runs a sweep pass saw, in the six ways it can see one. Every count is separate on purpose:
 *  `gone` is the one that means the sweep is too slow, and rolling it into `failed` would hide
 *  exactly the number that says so. */
export interface ArchiveSweep {
  /** Runs Temporal could still describe. */
  scanned: number;
  /** Of those, the closed ones — the archivable set. */
  closed: number;
  /** Objects written this pass. */
  archived: number;
  /** Already archived, skipped WITHOUT reading a history. */
  present: number;
  /** Closed, unarchived, and Temporal no longer holds the history. Too late for this one. */
  gone: number;
  /**
   * WHICH runs went, not just how many — issue F5.
   *
   * `gone` alone was a number with nothing behind it: events were lost permanently and the only
   * trace was an integer, so there was no way to reconcile after the fact or even to say what had
   * been in them. Under per-event billing that is revenue nobody can account for; before that, it
   * is a run whose whole story is missing and whose id nobody can name.
   *
   * BOUNDED, because this rides in a report that is logged. A pass that lost two hundred runs has
   * a much larger problem than the list, and the count beside it stays exact.
   */
  goneIds: string[];
  /** Errored on one run, which must never stop the pass. */
  failed: number;
  /**
   * The listing came back FULL, so there may be more runs than this pass considered.
   *
   * `listRuns` caps at `LIST_LIMIT` (200). A full page and a complete one are indistinguishable
   * without this — and silently truncating a sweep that feeds a durable archive is the same class
   * of bug as reporting a capped event count as a total (F3). Said, not inferred.
   */
  capped: boolean;
}

/** How many `gone` run ids one report carries before it stops listing them. A report is a log line;
 *  a pass that lost more than this has a bigger problem than the list, and `gone` is still exact. */
export const GONE_IDS_CAP = 50;

/** The reads a sweep makes, injectable so a test needs neither Temporal nor a clock. */
export interface ArchiveDeps {
  list?: () => Promise<RunRow[]>;
  read?: (runId: string, execId?: string) => Promise<RunHistory | undefined>;
  now?: () => number;
  /**
   * Called with something an operator should know that is NOT a failure.
   *
   * SEPARATE FROM `onError` BECAUSE THE AUDIENCE DIFFERS. An unconfigured store, a deliberate
   * `off`, a run that aged out and a page that came back full are all ordinary states of a
   * healthy system — routing them through the error channel would train whoever reads it to
   * ignore the channel, which is how the real failures stop being read too.
   */
  onNote?: (note: string) => void;
  /** Called once per failed run and once per failed pass. A sweep that throws into a `setInterval`
   *  would take the API process down with it, so nothing here is allowed to escape. */
  onError?: (err: unknown, runId?: string) => void;
}

/** The reduced log of one workflow, from Temporal or from the archive. */
export type HistorySource = (runId: string, execId?: string) => Promise<RunHistory | undefined>;

/**
 * The archived logs in one object store.
 *
 * Every method answers `undefined` rather than throwing on an object that is missing or unreadable:
 * the archive is a fallback, and a fallback that 502s the page it exists to rescue is worse than
 * one that says there is nothing there.
 */
export class HistoryArchive {
  constructor(private readonly store: ObjectStore) {}

  /** Is there anywhere to write? A store with no endpoint and no injected backing is a passthrough,
   *  and a sweep against one would read every history for nothing. */
  get enabled(): boolean {
    return this.store.enabled;
  }

  /** Write one run's log. Returns the key, so a caller can log WHERE the story went. */
  async write(
    run: { runId: string; startedAt: number; closedAt: number; memo?: Record<string, unknown> },
    history: RunHistory,
    now = Date.now()
  ): Promise<string> {
    const key = archiveKey(run.runId, run.startedAt);
    // Read at the run's CLOSE instant, not at `now`: the ask's own `waitedMs` must be how long the
    // run waited, and measuring it against the moment the sweep happened to come round would grow
    // the recorded wait of every unanswered ask by however late the archiver was.
    const asks = readAsks(run.memo, run.closedAt || now);
    const body: ArchivedLog = {
      v: ARCHIVE_VERSION,
      runId: run.runId,
      startedAt: run.startedAt,
      closedAt: run.closedAt,
      archivedAt: now,
      history,
      ...(asks.length > 0 ? { asks } : {}),
    };
    await this.store.put(key, Buffer.from(JSON.stringify(body), 'utf8'));
    return key;
  }

  /** Has this exact run — this id, started at this instant — been archived? The check a sweep makes
   *  before reading a history, which is what keeps a steady-state pass cheap. */
  async has(runId: string, startedAt: number): Promise<boolean> {
    return this.store.exists(archiveKey(runId, startedAt));
  }

  /**
   * The newest archived log for a workflow id, marked as archived.
   *
   * AN EXECUTION-PINNED REQUEST IS NEVER SERVED FROM HERE. `?exec=` exists because a workflow id is
   * reusable and asking by id alone answers with the latest execution; this object records no
   * execution id, so it cannot honour that pin. Answering anyway would render a DIFFERENT
   * execution's history under a row that asked for a specific one — the plausible wrong answer this
   * whole surface refuses (see the namespace guard in `history.ts`).
   */
  /**
   * The questions one archived run asked, after Temporal has dropped the execution that held them.
   *
   * `undefined` means there is no archived account of this run at all — which is a different answer
   * from an archived run that asked nobody anything, and the route serves a 404 for one and an
   * empty list for the other.
   */
  async asks(workflowId: string): Promise<RunAsk[] | undefined> {
    const log = await this.readEnvelope(workflowId);
    return log ? (log.asks ?? []) : undefined;
  }

  async read(workflowId: string, execId?: string): Promise<RunHistory | undefined> {
    if (execId) return undefined;
    const log = await this.readEnvelope(workflowId);
    if (!log) return undefined;
    // Marked here rather than at the boundary, so every path that serves an archive says so.
    return { ...log.history, archived: true, archivedAt: log.archivedAt };
  }

  /** The stored object itself, newest `dt=` first. Exposed for tests and for anything that wants
   *  the run coordinates beside the log. */
  async readEnvelope(workflowId: string): Promise<ArchivedLog | undefined> {
    const objects = await this.store.list(archivePrefix(workflowId));
    if (objects.length === 0) return undefined;
    // Lexicographic IS chronological: `dt=` is a fixed-width ISO instant. Sorting the keys avoids
    // parsing a date out of a path, which is the step that would need a guess when it failed.
    const newest = objects.map((o) => o.key).sort()[objects.length - 1]!;
    const body = await this.store.get(newest);
    if (!body) return undefined;
    try {
      const parsed = JSON.parse(Buffer.from(body).toString('utf8')) as Partial<ArchivedLog>;
      // A shape we do not recognise reads as no archive rather than as an error. The one thing it
      // must not do is reach the console as half a log.
      if (!parsed || !Array.isArray(parsed.history?.events)) return undefined;
      return {
        v: Number(parsed.v ?? 0),
        runId: String(parsed.runId ?? workflowId),
        startedAt: Number(parsed.startedAt ?? 0),
        closedAt: Number(parsed.closedAt ?? 0),
        archivedAt: Number(parsed.archivedAt ?? 0),
        history: parsed.history as RunHistory,
        ...(Array.isArray(parsed.asks) ? { asks: parsed.asks as RunAsk[] } : {}),
      };
    } catch {
      return undefined;
    }
  }
}

/**
 * Temporal's history, else the archive — the whole of the fallback.
 *
 * THE FALLBACK KEYS OFF NOT-FOUND AND OFF NOTHING ELSE. `fetchRunHistory` answers `undefined` for
 * gRPC code 5 and THROWS for everything else, so a cluster that is unwell propagates as a 502 and
 * never quietly serves a stale log as if it were live. That distinction is the reason this is three
 * lines rather than a try/catch.
 */
export async function readHistoryOrArchive(
  runId: string,
  execId: string | undefined,
  archive: HistoryArchive,
  live: HistorySource = fetchRunHistory
): Promise<RunHistory | undefined> {
  const found = await live(runId, execId);
  if (found) return found;
  return archive.read(runId, execId);
}

/**
 * One pass: archive every closed run that is not archived yet.
 *
 * SEQUENTIAL, DELIBERATELY. The first pass after this ships is the expensive one — every closed run
 * in the retention window at once — and there is no deadline on it: the next pass is fifteen
 * minutes away and the runs are not going anywhere for a day. Fanning out would only mean the
 * archive competes with the console for the same cluster.
 *
 * A CONTINUED-AS-NEW RUN IS NOT CLOSED, whatever its `closedAt` says. Each leg of the chain closes
 * as it hands over, so `closedAt > 0` alone would archive a partial story and then skip the run
 * forever. The status is what settles it — `mapStatus` reports `running` for the whole chain.
 */
export async function sweepClosedRuns(
  archive: HistoryArchive,
  deps: ArchiveDeps = {}
): Promise<ArchiveSweep> {
  const list = deps.list ?? (() => listRuns());
  const read = deps.read ?? fetchRunHistory;
  const now = deps.now ?? Date.now;
  const out: ArchiveSweep = {
    scanned: 0,
    closed: 0,
    archived: 0,
    present: 0,
    gone: 0,
    goneIds: [],
    failed: 0,
    capped: false,
  };

  const runs = await list();
  out.scanned = runs.length;
  // A FULL PAGE IS NOT A COMPLETE ONE. `listRuns` caps at `LIST_LIMIT`; at exactly that number this
  // pass may have considered a prefix of what exists, and nothing downstream could tell.
  out.capped = runs.length >= LIST_LIMIT;
  for (const run of runs) {
    if (!isArchivable(run)) continue;
    out.closed += 1;
    try {
      if (await archive.has(run.runId, run.startedAt)) {
        out.present += 1;
        continue;
      }
      const history = await read(run.runId);
      if (!history) {
        // Temporal could describe it a moment ago and cannot serve its history now: retention took
        // it between the two calls, or between this pass and the last. Counted, never inferred —
        // and NAMED, because a number is not something anybody can act on or reconcile against.
        out.gone += 1;
        if (out.goneIds.length < GONE_IDS_CAP) out.goneIds.push(run.runId);
        continue;
      }
      await archive.write(run, history, now());
      out.archived += 1;
    } catch (err) {
      out.failed += 1;
      deps.onError?.(err, run.runId);
    }
  }
  return out;
}

/** Is this run finished, in the sense that its history will never grow again? */
export function isArchivable(run: Pick<RunRow, 'status' | 'closedAt'>): boolean {
  return run.closedAt > 0 && run.status !== 'running' && run.status !== 'pending';
}

/**
 * Start the sweep on an interval. Returns the stop function.
 *
 * IT RUNS IN `orchestrator-api` (ADR 0025): that process already holds both authorities this needs —
 * the memoized Temporal client, which is also what registers the Search Attributes the discovery
 * query depends on, and the object store — and it is the process that serves the archive back, so
 * the writer and the reader live or die together. The materializer is deliberately the opposite:
 * activities only, one slot, a hard memory limit, and relocatable to another host, where this
 * loop's silence would be indistinguishable from having nothing to archive.
 *
 * The timer is `unref`'d so it can never hold a process open — a CLI or a test that builds a server
 * must still exit.
 */
export function startHistoryArchiver(
  store: ObjectStore,
  deps: ArchiveDeps = {}
): () => void {
  const archive = new HistoryArchive(store);
  /* TWO WAYS TO DO NOTHING, AND THEY WERE THE SAME SILENCE — issue F5.
     A store nobody configured and an archive somebody switched off are different facts with
     different fixes, and neither used to reach a log: the sweep simply never ran, and the first
     anybody knew was a run whose history had aged out with no record of it. `deps.onNote` rather
     than `onError` because neither is a fault — an installation with no object store is a
     legitimate one, and `off` is a choice somebody made on purpose. */
  if (process.env.KONTRA_HISTORY_ARCHIVE === 'off') {
    deps.onNote?.(
      'history archive: OFF (KONTRA_HISTORY_ARCHIVE=off). Closed runs will age out of Temporal ' +
        'with no durable record — this is deliberate, and it is the only warning of it.'
    );
    return () => undefined;
  }
  if (!archive.enabled) {
    deps.onNote?.(
      'history archive: DISABLED — no object store is configured (KONTRA_S3_ENDPOINT). Closed ' +
        'runs will age out of Temporal with no durable record.'
    );
    return () => undefined;
  }
  const every = Number(process.env.KONTRA_HISTORY_ARCHIVE_MS ?? DEFAULT_INTERVAL_MS);
  const interval = Number.isFinite(every) && every > 0 ? every : DEFAULT_INTERVAL_MS;

  let running = false;
  const pass = async (): Promise<void> => {
    // One pass at a time. A first pass over a full retention window can outlast the interval, and
    // two of them would read every history twice to write the same bytes to the same key.
    if (running) return;
    running = true;
    try {
      const report = await sweepClosedRuns(archive, deps);
      /* NAMED, NOT COUNTED. Events lost to retention are unrecoverable, so the one moment they can
         be reconciled is now, and an integer is not something anybody can reconcile against. The
         capped page rides the same line because a sweep that considered a PREFIX of what exists
         and said nothing is the quieter half of the same failure. */
      if (report.gone > 0) {
        deps.onNote?.(
          `history archive: ${report.gone} run(s) aged out of Temporal before they were archived — ` +
            `${report.goneIds.join(', ')}${report.gone > report.goneIds.length ? ', …' : ''}`
        );
      }
      if (report.capped) {
        deps.onNote?.(
          `history archive: the run listing came back FULL (${report.scanned}) — there may be more ` +
            'than this pass considered.'
        );
      }
    } catch (err) {
      // Temporal down, S3 down, credentials wrong — all ordinary, all transient, and none of them
      // may kill the API process. The next pass tries again.
      deps.onError?.(err);
    } finally {
      running = false;
    }
  };

  void pass();
  const timer = setInterval(() => void pass(), interval);
  timer.unref?.();
  return () => clearInterval(timer);
}
