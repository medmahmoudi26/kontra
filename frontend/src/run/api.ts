/**
 * Thin client for the orchestrator backend. It knows only the HTTP contract, not Temporal.
 *
 * IT DOES START RUNS. What went with the graph interpreter (ADR 0023 §12) was starting an
 * arbitrary TOPOLOGY, not the verb: `startRun` posts a registered folder and a workflow type to
 * `POST /api/runs`, and `runProbe` posts one Actor, one version, one Method and one Batch (ADR
 * 0033). Each starts ONE execution of somebody's workflow, and the count is the line — a request
 * that could name a second Method would be a topology, and a server executing one is the
 * interpreter again.
 *
 * WHAT IT STILL DOES NOT DO IS NAME A RUN. A Run is one execution of a caller's workflow and its
 * id IS that workflow's id, so nothing here mints one or holds a second identifier beside it.
 *
 * `VITE_API_BASE` overrides the base (default `/api`, proxied to the backend in dev).
 */

import { workflowScratchId } from '@core/scratch';
import type { RunStatus } from '@contract/types';
import type { DatasetState, MaterializationState } from '@contract/datasets';
import type { CatalogActor, JsonSchema } from '../types';
import type { PollerReport } from './workflowState';

export type { CatalogActor };

const BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api';

/** The MATERIALIZATION dimension rolled up: how many of a run's Datasets reached each state, and
 *  how much landed. `complete` with `rows: 0` is a successful empty result, not a failure. */
export interface MaterializationSummary {
  total: number;
  pending: number;
  running: number;
  complete: number;
  failed: number;
  rows: number;
  bytes: number;
}

/** The projection over both dimensions (ADR 0017). Never a `RunStatus` member: a run can be
 *  `completed` and still have no queryable output. */
export type PublicLifecycle = 'executing' | 'finalizing' | 'completed' | 'output_failed';

/**
 * One row of the Runs list — a caller's workflow, in BOTH status dimensions (ADR 0017).
 *
 * `status` is the EXECUTION dimension, exactly as Temporal reports it; `materialization` is the
 * ledger's. They are never merged into one verdict, here or on screen — that merge is what let a
 * run report `completed` while its output was absent, unqueryable or never written.
 *
 * `dispatches` is how the run was DISCOVERED: the orchestrator lists workflows that made Actor
 * dispatches, because a Run is one execution of a caller's own workflow (ADR 0023 §12) and
 * nothing here mints or registers it.
 */
export interface RunRow {
  runId: string;
  /** The caller's workflow type — `DnsSweep`, not an actor name. */
  type: string;
  /** EXECUTION dimension — authority Temporal. */
  status: RunStatus;
  tenant: string;
  startedAt: number;
  /** 0 while the run is open. */
  closedAt: number;
  dispatches: number;
  /** MATERIALIZATION dimension. `null` = the ledger could not be read, which is a different
   *  answer from a summary whose `total` is 0 — see `runState.ts`, which is the only thing that
   *  may interpret it. Optional because a server older than this field simply omits it. */
  materialization?: MaterializationSummary | null;
  /** The projection over both. `null` when either dimension is unknown. */
  lifecycle?: PublicLifecycle | null;
}

/**
 * One Dataset a run materialized, as the ledger recorded it — keyed by
 * `(runId, actor, version, node, schemaVersion)`, which is ADR 0017's idempotency key verbatim.
 *
 * THE DATASET'S NAME IS `actor`. An output Dataset is addressed by the actor that produced it
 * (see {@link DatasetInfo}), and a caller-published Dataset writes its own name into the same
 * field. There is no `dataset` column; the index signature stays because this record is read from
 * a server that may know more about it than this client does.
 */
export interface MaterializationRecord {
  actor?: string;
  version?: string;
  node?: string;
  // THE LEDGER'S STATE, NOT THE DATASET'S. Two different unions have worn this field name since
  // both sides declared it as a bare `string`; narrowing this one to the lifecycle is a compile
  // error now, which is how they were told apart.
  state?: MaterializationState;
  rows?: number;
  bytes?: number;
  error?: string | null;
  [k: string]: unknown;
}

/**
 * A run's status in BOTH dimensions, plus the projection over them (ADR 0017).
 *
 * NOT an extension of {@link RunRow}, and that is a correction: it used to extend it and so
 * promised `status` and `dispatches`, neither of which `/api/runs/:id` sends — the execution
 * dimension arrives under its own name here, and `dispatches` is a property of how the LIST
 * discovers runs, not of a run. A detail panel reading `detail.status` got `undefined` and drew
 * nothing, which is the kind of blank this surface exists to stop telling.
 */
export interface RunDetail {
  runId: string;
  /** The caller's workflow type. Empty when Temporal has dropped the execution for retention. */
  type: string;
  tenant: string;
  startedAt: number;
  /** 0 while the run is open. */
  closedAt: number;
  /** Authority Temporal: did the caller's workflow finish? The same dimension a list row calls
   *  `status`, under the name `/api/runs/:id` gives it. */
  execution: RunStatus;
  /** Authority the materialization ledger: is its output queryable? */
  materialization?: MaterializationSummary | null;
  /** The per-Dataset detail behind the summary — what a run actually wrote. */
  materializationRecords?: MaterializationRecord[];
  /** The projection. NEVER a `RunStatus` member — a run can be `completed` with failed output. */
  lifecycle: PublicLifecycle;
  settled: boolean;
}

/**
 * The Temporal-native per-Batch progress: what each dispatch's `RunBatch` activity last beat.
 *
 * Distinct from the host healthcheck beat on `/progress`, and this is the one worth showing beside
 * a running workflow: it is emitted by the activity itself, so it keeps arriving for an actor whose
 * host is not posting healthchecks at all. Empty is not an error — it means no Batch is in flight.
 */
export interface RunHeartbeat {
  done?: number;
  total?: number;
  [k: string]: unknown;
}

export async function fetchRunHeartbeats(runId: string): Promise<Record<string, RunHeartbeat>> {
  const res = await fetch(`${BASE}/runs/${encodeURIComponent(runId)}/heartbeats`);
  if (!res.ok) return {};
  return ((await res.json()) as { nodes?: Record<string, RunHeartbeat> }).nodes ?? {};
}

/** How long a run took, or has been going. `closedAt` is 0 while it is open, which is why `now` is
 * passed in rather than read here — a duration that ticks must be driven by a render. */
export function runDuration(run: { startedAt: number; closedAt: number }, now: number): number {
  if (!run.startedAt) return 0;
  return (run.closedAt || now) - run.startedAt;
}

/** `1m 04s`. Milliseconds are noise at every duration a workflow actually takes, and hours are
 * common enough on a sweep that a bare seconds count stops being readable. */
export function fmtDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '—';
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, '0')}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${String(m % 60).padStart(2, '0')}m`;
}

/** Every run the orchestrator can see, newest first. Throws — an unreachable Temporal is a
 *  different answer from "no runs yet", and the caller must be able to say which it got. */
export async function fetchRuns(): Promise<RunRow[]> {
  const res = await fetch(`${BASE}/runs`);
  if (!res.ok) return asError(res, 'list runs');
  return (await res.json()) as RunRow[];
}

/* ───────────────────────────── the pulse ───────────────────────────── */

/** One run waiting on a human, as the pulse names it. Field-for-field `panels/ask.ts`'s
 *  `ParkedRun`, because they are the same fact learned two ways — one from the run whose transcript
 *  is open, one from the cluster-wide scan — and the chrome merges them into one list. */
export interface PulsePark {
  runId: string;
  /** The caller's workflow type: which thread this conversation belongs to. */
  workflow: string;
  pending: number;
  /** Epoch ms the oldest waiting ask was asked. `0` when no entry carried a readable instant. */
  since: number;
}

/**
 * What is happening across everything, right now — the whole answer, in six fields.
 *
 * `running` COUNTS THE PARKED ONES, because Temporal does: a workflow blocked on a human is
 * Running. `scanned` and `capped` are what stop a partial look from being read as an all-clear —
 * see `panels/chrome/pulse.ts`, which is the only place in the browser allowed to interpret them.
 */
export interface Pulse {
  running: number;
  /** How many of the SCANNED running runs are waiting on a human. A FLOOR when `capped`. */
  parked: number;
  /** The parked runs, longest-waiting first, capped by the server. The way in, not a listing. */
  named: PulsePark[];
  scanned: number;
  capped: boolean;
  /** When the server took this reading, by ITS clock. */
  at: number;
}

/**
 * Is anything happening at all, anywhere.
 *
 * THE ONE READ THE CHROME MAKES FROM EVERY SURFACE, and it is deliberately not `fetchRuns`. The
 * rail used to filter the run list for this number, which meant a session sitting on the Datasets
 * page paid for a page of up to 200 executions, a ledger read per row and a describe per open row
 * every few seconds — a cost that grows with how many runs this controller has EVER held, for a
 * question about right now. `/api/pulse` is two bounded reads (`backend/src/pulse.ts`).
 *
 * THROWS, for the reason `fetchRuns` throws and with more at stake: a badge reading "idle" because
 * the appliance could not be asked is precisely the lie this surface exists to stop telling. The
 * caller renders "not known", never a zero.
 */
export async function fetchPulse(): Promise<Pulse> {
  const res = await fetch(`${BASE}/pulse`);
  if (!res.ok) return asError(res, 'read the pulse');
  const body: unknown = await res.json();
  // Same shape check as `fetchFleetOperations`, and the same reason: a 200 carrying a proxy's error
  // page reaches `.map()` inside the render and blanks the whole SPA. The rail is the one component
  // that is mounted on every surface, so a throw from inside it is the worst one available.
  if (!body || typeof body !== 'object' || typeof (body as Pulse).running !== 'number') {
    throw new Error(`pulse: expected a reading, got ${body === null ? 'null' : typeof body}`);
  }
  const pulse = body as Pulse;
  return { ...pulse, named: Array.isArray(pulse.named) ? pulse.named : [] };
}

/**
 * One Fleet operation — a bring-up, preview or teardown of one stack.
 *
 * NOT a Run, and kept in its own type for that reason: it dispatches no Actors and writes no
 * Dataset, so every column the run list carries would be blank or meaningless on it.
 */
export interface FleetOperationRow {
  /** `kontra-fleet/<stack>` — the same id for every operation on that stack. */
  workflowId: string;
  /** Temporal's execution id, the only thing that tells two operations on one stack apart. */
  execId: string;
  stack: string;
  status: RunRow['status'];
  startedAt: number;
  /** 0 while the operation is open. */
  closedAt: number;
}

/**
 * Fleet history, newest first. Throws for the same reason `fetchRuns` does: "the cluster is
 * unreachable" and "you have never brought a fleet up" must not render the same.
 *
 * THE SHAPE IS CHECKED, not assumed, and that is not defensive programming for its own sake. A
 * `200` carrying a non-array — a proxy's error page, a stub, a route that answered for the wrong
 * path — reaches `.map()` inside the render and throws THERE, which unmounts the whole Runs page
 * and leaves a blank surface with the real cause buried in a console trace. That exact shape blanked
 * this SPA once before (a saved graph without positions, throwing inside React Flow). A thrown error
 * here is caught by the store and rendered as a sentence in the panel it belongs to.
 */
export async function fetchFleetOperations(): Promise<FleetOperationRow[]> {
  const res = await fetch(`${BASE}/fleet/operations`);
  if (!res.ok) return asError(res, 'list fleet operations');
  const body: unknown = await res.json();
  if (!Array.isArray(body)) {
    throw new Error(
      `fleet operations: expected a list, got ${body === null ? 'null' : typeof body}`
    );
  }
  return body as FleetOperationRow[];
}

/** One run, across both dimensions. */
export async function fetchRun(runId: string): Promise<RunDetail> {
  const res = await fetch(`${BASE}/runs/${encodeURIComponent(runId)}`);
  if (!res.ok) return asError(res, 'read run');
  return (await res.json()) as RunDetail;
}

/**
 * The workflow one event names — what a row drills into. Mirrors `EventLink` in
 * `backend/src/history.ts`, which is where the three recorded shapes it is read from live.
 */
export interface EventLink {
  /** The workflow id. Goes straight back to the history route — a child is just another id. */
  workflowId: string;
  /** Temporal's own run id for that execution. NOT a **Run**: a Run is identified by the workflow
   *  id. Sent because `kontra-fleet/dns` is the id of every fleet bring-up AND teardown, so without
   *  it a drill into one lands on the other. */
  execId?: string;
  /** The workflow type, when the event names one. */
  type?: string;
  via: 'child' | 'nexus';
  namespace?: string;
}

/** One line of a run's Temporal event history. See `backend/src/history.ts` for why the type
 *  name is derived from the attribute key and why no payload is ever decoded. */
export interface RunEvent {
  id: number;
  type: string;
  cat: 'workflow' | 'task' | 'activity' | 'failure' | 'timer' | 'marker' | 'child' | 'signal';
  /** Seconds since the run's first event. */
  t: number;
  /** Epoch ms. */
  at: number;
  detail: string;
  attempt: number;
  /** Seconds this event closes, when it closes something. 0 otherwise. */
  dur: number;
  /** Present only on the rows that are about another workflow — which is exactly the set of rows
   *  that are navigable. */
  link?: EventLink;
  /**
   * Temporal's user-metadata Summary, when the event carries one.
   *
   * THE FIELD THE MEANING WAS DELIBERATELY PUT IN, and the reason it is not a payload: `detail`
   * above is built from the event's own machinery (`timerId=1`, an identity, a queue name), while
   * this is the line somebody WROTE for this row — `speak`'s sentence rides a timer's Summary, a
   * dispatch's Method rides its scheduling event's. The server reads it out of `userMetadata`
   * beside the payload rather than out of one (`backend/src/history.ts`), so a row that shows
   * it still costs no blob GET.
   */
  summary?: string;
}

export interface RunHistory {
  events: RunEvent[];
  scanned: number;
  /** Events dropped from the middle to stay under the cap — printed, never swallowed. */
  elided: number;
  truncated: boolean;
  /**
   * This log came from the archive, not from Temporal (ADR 0025).
   *
   * ABSENT MEANS LIVE, which is the correct default rather than a convenient one: a server older
   * than the archive really is serving a live history. What it changes here is the affordances —
   * a follow cannot resume on a history that will never grow again.
   */
  archived?: boolean;
  /** Epoch ms the archive was written. Absent on a live read, where the answer is "now". */
  archivedAt?: number;
}

/**
 * One workflow's reduced history, with the REASON when there is none.
 *
 * `gone` is Temporal answering "no such execution" — retention dropped it, or it has not written
 * its first event yet. Anything else is the orchestrator or the cluster being unwell. The run's own
 * log can collapse the two (see {@link fetchRunHistory}); a drill-through cannot, because "this
 * child aged out" and "we could not ask" are different things to tell someone who just clicked.
 */
export type HistoryRead =
  | { ok: true; history: RunHistory }
  | { ok: false; gone: boolean; detail: string };

export async function readHistory(workflowId: string, execId?: string): Promise<HistoryRead> {
  // Same route at every level: a child workflow and a dispatch's backing workflow are workflow ids,
  // and the history route takes one. `exec` pins the execution behind a reused id.
  const q = execId ? `?exec=${encodeURIComponent(execId)}` : '';
  try {
    const res = await fetch(`${BASE}/runs/${encodeURIComponent(workflowId)}/history${q}`);
    if (res.ok) return { ok: true, history: (await res.json()) as RunHistory };
    return {
      ok: false,
      gone: res.status === 404,
      detail: res.status === 404 ? 'Temporal has no history under that id' : `${res.status} ${res.statusText}`,
    };
  } catch (err) {
    return { ok: false, gone: false, detail: err instanceof Error ? err.message : String(err) };
  }
}

/**
 * The run's event log.
 *
 * Never throws and never 404s outward: a run that Temporal has dropped for retention, and a run
 * whose first event has not been written yet, both mean "nothing to show" — and a log panel that
 * turned either into a red error would be shouting about the ordinary case. Real outages surface
 * on the run detail beside it, which does distinguish them.
 */
export async function fetchRunHistory(runId: string): Promise<RunHistory | null> {
  const read = await readHistory(runId);
  return read.ok ? read.history : null;
}

async function asError(res: Response, what: string): Promise<never> {
  let detail = '';
  try {
    detail = await res.text();
  } catch {
    /* ignore */
  }
  throw new Error(`${what} failed: ${res.status} ${res.statusText}${detail ? ` — ${reason(detail)}` : ''}`);
}

/**
 * The server's own sentence, out of the envelope it travels in.
 *
 * Every refusing route here answers `{"error":"<the reason>"}`, and this used to be pasted into the
 * message raw — so an operator who mistyped a path in the register form read
 * `register the folder failed: 400 Bad Request — {"error":"/srv/prob has no actor.json — that is
 * what makes a folder an Actor"}`, with the one sentence that tells them what to fix wrapped in
 * JSON. A body that is not that shape is passed through untouched: an HTML error page from a proxy
 * is still the most informative thing there is about what answered.
 */
function reason(body: string): string {
  try {
    const parsed = JSON.parse(body) as { error?: unknown };
    if (parsed && typeof parsed.error === 'string' && parsed.error.trim()) return parsed.error;
  } catch {
    /* not JSON — say what arrived */
  }
  return body;
}

// `/runs/:id/status` and `/runs/:id/output` are NOT here, and their absence is the point: both
// routes went with the interpreter (ADR 0023 §12) and this client kept calling them. A run's
// status is `fetchRun` (both dimensions), and its output is a Dataset — `fetchDatasets` below.

// `isTerminal(status)` WAS HERE and is gone: a pure predicate over `RunStatus` — `completed`,
// `failed` or `cancelled` — exported, with no caller anywhere in the repo. Not a module, not a
// test, not a string addressed from another language.
//
// NOTHING REGRESSED, because nothing asks the question that way. The transcript poller stops on
// `!read.turns.live` (`run/follow.ts`), which is the account saying it has nothing further to say,
// and the run detail carries its own `settled` (line 122) computed on the SERVER across both
// dimensions. A third answer derived in the browser from a word list is exactly the drift ADR 0035
// finding 2 measured on the retention TTL, and this one had no reader to drift for.
//
// Unlike `fetchFleetPhase` below, there is no route behind this and no fact it was the only way to
// recover — so it leaves, rather than staying with a note.

// --- the fleet operation a run is inside ----------------------------------------------------

/**
 * What one Fleet child reports about ITSELF. Mirrors `FleetPhase` in
 * `backend/src/fleetPhase.ts`, which is where the two fields it may carry are chosen.
 *
 * Every field but the id is optional, because "it said nothing" is an answer the run detail has to
 * render: a child dropped for retention, or one whose queue no Worker is polling, degrades to what
 * the run's own history recorded and never to a fabricated phase.
 */
export interface FleetPhase {
  workflowId: string;
  /** Temporal's run id for the execution asked about. NOT a **Run** — see {@link EventLink}. */
  execId?: string;
  /** The phase the child reported: `starting`, `running`, `done` or `compensating` today. */
  phase?: string;
  /** The operation it named — `up`, `preview`, `destroy`. The one fact the run's history cannot
   *  supply, because it travels in a payload the event log never decodes (ADR 0007). */
  op?: string;
  /** Why there is no phase, when there is none. */
  unavailable?: string;
}

/**
 * Ask one fleet child what it is doing.
 *
 * NEVER THROWS, like `fetchRunHistory` and for a sharper reason: the fleet turn this decorates is
 * drawn from the run's own history and is true whether or not the child answers. A rejection here
 * would take a measured 156-second window off the screen to report a missing adjective.
 *
 * `execId` PINS THE EXECUTION and a caller is expected to have one: a workflow id can be reused, so
 * a bare id is answered with whichever execution ran last — which for `kontra-fleet/dns` is a
 * plausible wrong answer rather than an obvious one, because a bring-up and its teardown share the
 * id and differ only in the execution.
 *
 * NOTHING IN THE CONSOLE CALLS THIS TODAY. The surface that did was the retired Runs page; the live
 * transcript gets its fleet turns from `backend/src/transcript.ts`, which reads the parent's history
 * and therefore knows the child's Temporal type but NOT which operation it was — `up`, `preview` or
 * `destroy` travels in the child's input, a payload the log never decodes (ADR 0007). This is the
 * one call that recovers that word, and `backend/src/routes/fleet.ts` still serves it.
 */
export async function fetchFleetPhase(workflowId: string, execId?: string): Promise<FleetPhase> {
  const base: FleetPhase = { workflowId, ...(execId ? { execId } : {}) };
  const q = execId ? `?exec=${encodeURIComponent(execId)}` : '';
  try {
    const res = await fetch(`${BASE}/runs/${encodeURIComponent(workflowId)}/phase${q}`);
    if (!res.ok) return { ...base, unavailable: `${res.status} ${res.statusText}` };
    return (await res.json()) as FleetPhase;
  } catch (err) {
    return { ...base, unavailable: err instanceof Error ? err.message : String(err) };
  }
}

// --- serve + start -------------------------------------------------------------------------
//
// The two writes on this surface, and the only two. They do NOT put execution back in the
// server (ADR 0023 §12): `serve` spawns the operator's own `kontra workflow serve … --tmux` and
// returns, `start` is the same `client.workflow.start` the CLI makes. See
// `backend/src/workflowControl.ts`.

// --- registered folders ---------------------------------------------------------------------
//
// Where an operator's own code lives. Registration is a PATH, not an upload: you write an actor
// where you write code, register the folder once, and the page keeps showing where it is. See
// `backend/src/sources.ts` — including why registering grants the same authority as serve.

export type SourceKind = 'actor' | 'workflow';

/** One registered folder, as the pages list it. Mirrors `Source` on the server. */
export interface Source {
  id: string;
  kind: SourceKind;
  name: string;
  /** Absolute and symlink-resolved — what the page shows as "where this lives". */
  path: string;
  /** An actor's version from actor.json. Empty for a workflow, which has none. */
  version: string;
  /** First paragraph of description.md, or '' when there is no such file. */
  description: string;
  /** 0 for a folder found in the default root rather than registered by hand. */
  registeredAt: number;
  /**
   * The folder is not on the orchestrator's disk any more. Only a REGISTERED folder can be absent —
   * a discovered one stops being listed instead — and the row is still shown, because a registration
   * whose directory went with a `git checkout` is the operator's to keep or forget. Absent on an
   * older server, which means "no server ever said this was absent", not "it is present".
   */
  absent?: boolean;
}

/** `defaultRoot` is echoed so the form can SUGGEST it — a path field with no default is a guess. */
export async function fetchSources(
  kind: SourceKind
): Promise<{ defaultRoot: string; sources: Source[] }> {
  const res = await fetch(`${BASE}/sources/${kind}`);
  if (!res.ok) return asError(res, `list ${kind}s`);
  return (await res.json()) as { defaultRoot: string; sources: Source[] };
}

export async function registerSource(kind: SourceKind, path: string): Promise<Source> {
  const res = await fetch(`${BASE}/sources/${kind}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ path }),
  });
  if (!res.ok) return asError(res, 'register the folder');
  return (await res.json()) as Source;
}

export async function forgetSource(kind: SourceKind, id: string): Promise<void> {
  const res = await fetch(`${BASE}/sources/${kind}/${encodeURIComponent(id)}`, { method: 'DELETE' });
  if (!res.ok) return asError(res, 'forget the folder');
}

/** One file in a registered folder. Mirrors `SourceFile` on the server. */
export interface SourceFile {
  name: string;
  bytes: number;
  modifiedAt: number;
}

/**
 * What is in a registered folder — the editor's file list.
 *
 * `path` is echoed because the workbench's first job is to say WHICH copy of an actor is open: two
 * checkouts of `probe` have the same files and the same names, and the path is the only thing that
 * tells them apart.
 */
export async function fetchSourceFiles(
  kind: SourceKind,
  id: string
): Promise<{ path: string; files: SourceFile[] }> {
  const res = await fetch(`${BASE}/sources/${kind}/${encodeURIComponent(id)}/files`);
  if (!res.ok) return asError(res, 'list the folder');
  return (await res.json()) as { path: string; files: SourceFile[] };
}

export async function fetchSourceFile(
  kind: SourceKind,
  id: string,
  name?: string
): Promise<{ name: string; source: string }> {
  const q = name ? `?name=${encodeURIComponent(name)}` : '';
  const res = await fetch(`${BASE}/sources/${kind}/${encodeURIComponent(id)}/file${q}`);
  if (!res.ok) return asError(res, 'read the file');
  return (await res.json()) as { name: string; source: string };
}

/*
 * THERE IS NO `saveSourceFile`, AND ITS ROUTE IS GONE WITH IT (ADR 0033 §6).
 *
 * It had one caller: the Actors page writing a generated caller into a registered Workflow folder.
 * That errand is what ADR 0033 removed — the page calls the Method now, and the generated caller
 * survives beside the Run button as a read-only, copyable artefact. ADR 0030 had already made both
 * editors read-only viewers, so nothing in this app writes source to disk any more, and an unused
 * route is removed outright rather than left as a surface with no caller.
 */

/** What serving a registered Actor answers with. Mirrors `ActorServeResult` on the server. */
export interface ActorServeResult {
  actor: string;
  version: string;
  /** Where the code was served from — the registered folder, echoed back. */
  path: string;
  /** The tmux session the worker landed in; the Monitor finds its pane by this name. */
  session: string;
  attach: string;
}

/**
 * Serve a registered Actor's worker on THIS machine, in tmux.
 *
 * THE CALL TAKES NOTHING BUT THE FOLDER, and that is the decision rather than an omission.
 * `kontra serve --actor` has three modes and the server passes `local`; a `mode` parameter here
 * would make "try this actor" and "put this actor on nine Machines that keep billing" the same
 * gesture, one argument apart, from a page whose whole value is that starting is cheap and
 * reversible. See `backend/src/actorControl.ts`.
 */
export async function serveActorSource(
  id: string,
  { restart = false }: { restart?: boolean } = {}
): Promise<ActorServeResult> {
  const res = await fetch(`${BASE}/sources/actor/${encodeURIComponent(id)}/serve`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ restart }),
  });
  // 409 IS NOT A FAILURE, IT IS A QUESTION. A worker is already serving this Actor; the caller can
  // answer it with `restart`. Thrown as its own type so the console can draw a button instead of
  // reporting an error for a state that is, on its own, the thing the operator wanted.
  if (res.status === 409) {
    let body = '';
    try {
      body = await res.text();
    } catch {
      /* ignore */
    }
    throw new AlreadyServingError(reason(body) || 'a worker is already serving this Actor');
  }
  if (!res.ok) return asError(res, 'serve the actor');
  return (await res.json()) as ActorServeResult;
}

/** A worker is already there. Its own class so `catch` can offer restart rather than apologise. */
export class AlreadyServingError extends Error {}

/** The caller workflow the Actors page shows beside its Run button. Mirrors the `caller` route. */
export interface GeneratedCaller {
  /** What the file would be called if the operator kept it — `workflow.py`, the marker that makes
   *  a folder a Workflow. The server names it rather than the page guessing it. */
  filename: string;
  source: string;
}

/**
 * The caller workflow that makes this call — the READ-ONLY artefact, not the dispatch.
 *
 * IT RETURNS SOURCE AND STARTS NOTHING. `runProbe` below is the one that starts something, and the
 * two are separate on purpose: reading the code you are about to run should not require running it.
 *
 * WHAT IT SHOWS IS WHAT THE PROBE RUNS (ADR 0033 §3), which is why this is worth keeping now that
 * the page can call the Method. The probe workflow reaches the SAME SDK caller half —
 * `catalog.actor(...)`, a callable handle, a destructured `(results, dropped)`, an optional Dataset
 * writer — so the block beside the button is honest rather than illustrative. What went with ADR
 * 0033 §6 is the errand: there is no save target, no folder shelf and no write.
 */
export async function generateCaller(
  id: string,
  method: string,
  units: unknown[]
): Promise<GeneratedCaller> {
  const res = await fetch(`${BASE}/sources/actor/${encodeURIComponent(id)}/caller`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ method, units }),
  });
  if (!res.ok) return asError(res, 'generate the caller');
  return (await res.json()) as GeneratedCaller;
}

/**
 * What starting one probe answers with. Mirrors `ProbeStarted` on the server.
 *
 * `units` is the count SENT, and it is here so an empty result has a denominator before the run
 * returns: "0 of 12 rows" and "0 of 0" are different findings, and only one of them is worth
 * opening a worker's pane over.
 */
export interface ProbeStarted {
  /** The Run. One execution of a caller's workflow, like any other — there is no "probe run" kind
   *  (ADR 0033's first consequence), and the Runs surface shows it beside everything else. */
  runId: string;
  actor: string;
  version: string;
  method: string;
  units: number;
  /** The untagged Dataset the results land in — queryable from the Datasets surface, and swept by
   *  ADR 0029 §3's ordinary TTL unless somebody tags it. */
  dataset: string;
  queue: string;
  /** The Actor's registered Nexus endpoint — where the dispatch is aimed. */
  endpoint: string;
  workflow?: { name: string; version: string };
}

/**
 * CALL THE METHOD — start the one-shot workflow that dispatches it over this Batch (ADR 0033).
 *
 * ONE ACTOR, ONE VERSION, ONE METHOD, ONE BATCH, and the request has no field for a second of any
 * of them. The Actor and the version are the registered folder's, so this takes the folder id and
 * not a name a page could get wrong.
 *
 * IT REFUSES RATHER THAN HANGING, which is most of what the 400s here are: an Actor with no Nexus
 * endpoint, a queue whose only pollers are stale, a probe worker nobody started. Each names its
 * own fix, and the caller shows the server's sentence verbatim.
 */
export async function runProbe(
  id: string,
  method: string,
  units: unknown[]
): Promise<ProbeStarted> {
  const res = await fetch(`${BASE}/sources/actor/${encodeURIComponent(id)}/probe`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ method, units }),
  });
  if (!res.ok) return asError(res, 'call the Method');
  return (await res.json()) as ProbeStarted;
}

/**
 * What the probe answered — the workflow's own return value. Mirrors `ProbeReading` on the server.
 *
 * `isolated` AND `done` ARE HERE BESIDE `results`, which is ADR 0028 §4 carried to the surface: a
 * Method that dropped every Unit and one that legitimately found nothing both return zero rows, and
 * a probe UI drawing only `results` reproduces the failure that let a 15,814-target run report
 * `completed` in seven minutes having scanned almost nothing.
 */
export interface ProbeReading {
  runId: string;
  /** Temporal's own word — `RUNNING`, `COMPLETED`, `FAILED`, … Not folded into a boolean: "still
   *  going" and "failed" send an operator to two different places. */
  status: string;
  /** Present once the workflow returned. */
  result?: {
    units: number;
    results: number;
    isolated: number;
    done: boolean;
    machine: string;
    dataset: string;
  };
  /** The failure's own sentence, when there is one. */
  failure?: string;
}

/**
 * Read one probe's answer back.
 *
 * IT IS NOT A "PROBE RUN" KIND (ADR 0033's first consequence). The Run is an ordinary Run — listed
 * and read by every ordinary run surface — and this reads the RETURN VALUE of the one workflow type
 * kontra owns, which no general surface has a shape for. The route refuses a run that is not a
 * probe, because a workflow returns whatever its author put in it.
 */
export async function readProbe(runId: string): Promise<ProbeReading> {
  const res = await fetch(`${BASE}/probes/${encodeURIComponent(runId)}`);
  if (!res.ok) return asError(res, 'read the probe');
  return (await res.json()) as ProbeReading;
}

/**
 * One workflow in `.kontra/workflows/` — an operator's own caller workflows.
 *
 * `name` is the FOLDER (`nscheck`), or the filename of a flat `<name>.py` beside it. It is what
 * every other call here takes: the editor reads and writes by it, and `serve` resolves it.
 */
export interface WorkflowFile {
  name: string;
  bytes: number;
  modifiedAt: number;
  /** First paragraph of the folder's `description.md`. '' — or absent, from an older server —
   *  means undescribed, which the row shows as nothing rather than as a placeholder. */
  description?: string;
}

/**
 * A workflow as its WORKER described it, pushed to the catalog on serve.
 *
 * NOT A FILE. `name` is the `@workflow.defn` type a caller starts (`NsCheck`) — one file can
 * declare several, and the worker that registered this may be serving code that is not in
 * `.kontra/workflows/` at all. The page joins the two by reading the type out of the source it is
 * showing (`typeFromSource`), which is the only side that knows which types a file holds.
 *
 * EVERY FIELD BUT THE NAME IS OPTIONAL, and absent means the author declared nothing — which is
 * not the same as declaring an empty one. A workflow annotated `dict` registers an `input` with no
 * properties, and that says "any object", where a missing `input` says "nobody wrote a type down".
 */
export interface WorkflowDescriptor {
  name: string;
  description?: string;
  /**
   * The task queue the worker that registered this was polling — where a start has to be sent.
   *
   * ABSENT MEANS NOBODY HAS SAID, which is a different thing from the empty queue and must stay
   * different: the Run form prefills from this, and prefilling with '' would replace a usable
   * value with a blank. Absent on a descriptor from an older SDK, and on one from a worker that
   * never named a queue.
   */
  queue?: string;
  /**
   * THE FILE NO LONGER IMPORTS — a broken workflow is a state, not a silence.
   *
   * A watch-mode serve (`kontra workflow serve --watch`) re-derives this descriptor on every save,
   * and when the save breaks the import it posts the error here rather than leaving the last good
   * form standing. Absent means the file imports (the ordinary case) and the contract panel draws
   * the schema; present means it does not, and the panel draws this instead — the loop doubling as a
   * liveness check on the operator's own code. A broken descriptor carries no `input`/`output`, so
   * the form drops with it and a clean recovery restores it.
   */
  error?: string;
  input?: JsonSchema;
  output?: JsonSchema;
  savedAt: number;
}

/** List `.kontra/workflows/`. `dir` is echoed so the page can SAY where it is looking — a "no
 *  workflows" that does not name the directory is unactionable. `registered` is the descriptor
 *  side, absent from an older server — hence the `?? []`, which reads as "nothing registered"
 *  rather than crashing the list a workflow file is drawn from. */
export async function fetchWorkflows(): Promise<{
  dir: string;
  workflows: WorkflowFile[];
  registered: WorkflowDescriptor[];
}> {
  const res = await fetch(`${BASE}/workflows`);
  if (!res.ok) return asError(res, 'list workflows');
  const got = (await res.json()) as {
    dir: string;
    workflows: WorkflowFile[];
    registered?: WorkflowDescriptor[];
  };
  return { ...got, registered: got.registered ?? [] };
}

export async function fetchWorkflowSource(name: string): Promise<string> {
  const res = await fetch(`${BASE}/workflows/file/${encodeURIComponent(name)}`);
  if (!res.ok) return asError(res, 'read the workflow');
  return ((await res.json()) as { source: string }).source;
}

/**
 * There is no `saveWorkflowSource`, and its route is gone with it (ADR 0030). The Workflows page is
 * a READ-ONLY viewer — `GET /api/workflows/file/:name` reads the bytes on disk, and the way to
 * CHANGE them is the operator's own editor plus a re-serve, not a write from the browser. A dashboard
 * that could write let the file and the registered digest disagree; ADR 0020 made the same call for
 * Terminals. The one exception this note used to carry — a generated caller writing a NEW
 * `workflow.py` — is gone too (ADR 0033 §6), so there is now no write path from this app to any
 * file on the operator's disk at all.
 */

export interface ServeResult {
  file: string;
  queue: string;
  /** The tmux session the worker landed in — the Dashboard discovers it by this name. */
  session: string;
  attach: string;
}

/** Serve a workflow file as a detached tmux worker on the orchestrator's host. */
/**
 * Serve a workflow. THE QUEUE IS NOT A PARAMETER: the server derives `wf-<name>-<digest12>` from the
 * folder's manifest name and its content digest (`workflowControl.ts:workflowQueue`), which is the
 * only string the worker will actually poll. This used to take whatever was in the page's queue
 * field — a field that defaulted to the literal `'recon'`, so the button's out-of-the-box behaviour
 * was to serve every workflow onto one stale queue named after something else (GitHub #15).
 */
export async function serveWorkflow(file: string): Promise<ServeResult> {
  const res = await fetch(`${BASE}/workflows/serve`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ file }),
  });
  if (!res.ok) return asError(res, 'serve the workflow');
  return (await res.json()) as ServeResult;
}

/**
 * Who is polling a task queue — the "serving" signal, and the only authority on it.
 *
 * NEVER THROWS on an unreachable Temporal: the route answers `pollers: 0` with an `error`, which is
 * "we do not know" and is a different fact from "nothing is serving". `run/workflowState.ts` is
 * the only place allowed to tell them apart, and every caller goes through it.
 */
export async function fetchPollers(queue: string): Promise<PollerReport> {
  const res = await fetch(`${BASE}/queues/${encodeURIComponent(queue)}/pollers`);
  if (!res.ok) {
    return {
      queue,
      pollers: 0,
      identities: [],
      workers: [],
      lastPoll: 0,
      error: `${res.status} ${res.statusText}`,
    };
  }
  return (await res.json()) as PollerReport;
}

export interface StopResult {
  runId: string;
  outcome: 'cancelled' | 'cancelling' | 'terminated' | 'already-closed';
  waitedMs: number;
  detail: string;
}

/**
 * Stop a Run. `escalate` is the difference between cancel and terminate.
 *
 * CANCEL is cooperative — scope exits run, so a fleet the run holds is DESTROYED. TERMINATE is
 * unilateral and skips them, so that fleet keeps billing. Which is why `escalate` still cancels
 * first server-side and only escalates if that does not land; see `workflowControl.ts:stopRun`.
 */
export async function stopRun(
  runId: string,
  opts: { escalate?: boolean; force?: boolean } = {}
): Promise<StopResult> {
  const res = await fetch(`${BASE}/runs/${encodeURIComponent(runId)}/stop`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ escalate: opts.escalate === true, force: opts.force === true }),
  });
  if (!res.ok) return asError(res, 'stop the run');
  return (await res.json()) as StopResult;
}

export interface PauseResult {
  file: string;
  session: string;
  paused: boolean;
  detail: string;
}

/** Pause or resume the WORKER a workflow is served in — not a Temporal operation; see the server's
 *  `pauseWorkflow` for what it does and does not hold. No queue: resume re-derives it from the
 *  folder (GitHub #15), so there is nothing to pass. */
export async function pauseWorkflow(file: string, resume: boolean): Promise<PauseResult> {
  const res = await fetch(
    `${BASE}/workflows/file/${encodeURIComponent(file)}/${resume ? 'resume' : 'pause'}`,
    {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({}),
    }
  );
  if (!res.ok) return asError(res, resume ? 'resume the worker' : 'pause the worker');
  return (await res.json()) as PauseResult;
}

// --- scratches ----------------------------------------------------------------------------------
//
// A **Scratch** is a drawing of an orchestration that an agent reads back to write code from. The
// document's shape is defined ONCE, on the server (`backend/src/scratch.ts`), because the
// whole feature rests on the editor not being the only thing that knows what a drawing meant —
// these types are that shape, restated for the browser and pinned by `scratchDocument.test.ts`.

export interface ScratchPoint {
  x: number;
  y: number;
}

export type ScratchNode =
  | { id: string; kind: 'actor'; at: ScratchPoint; actor: string; version: string; method: string }
  | { id: string; kind: 'workflow'; at: ScratchPoint; file: string }
  | { id: string; kind: 'dataset'; at: ScratchPoint; name: string; direction: 'in' | 'out' };

export interface ScratchEdge {
  id: string;
  from: string;
  to: string;
  /** The FIELD at each end — `fetch.body → title.html`. Absent means the whole node, which is what
   *  every edge drawn before typed ports means and what a node with nothing declared can only
   *  offer. */
  fromPort?: string;
  toPort?: string;
  label?: string;
}

export interface ScratchNote {
  id: string;
  at: ScratchPoint;
  text: string;
  on?: string;
}

export interface ScratchDocument {
  nodes: ScratchNode[];
  edges: ScratchEdge[];
  notes: ScratchNote[];
}

export interface ScratchSummary {
  id: string;
  name: string;
  updatedAt: number;
}

export interface ScratchRecord extends ScratchSummary {
  document: ScratchDocument;
}

// TWO READS WENT WITH THE PAGE THAT WAS THEIR ONLY CALLER, and the third is deliberately still here.
//
// `fetchScratches()` (`GET /api/scratch`, every sketch) and `fetchScratchSpec(id)`
// (`GET /api/scratch/:id/spec`, the agent-readable rendering) were imported by exactly one module —
// `ScratchPage.tsx`, retired in the slice before this one. Checked against that commit's parent
// rather than assumed: the page's import list named both, and nothing else in the tree did then or
// does now.
//
// NEITHER ROUTE LOST ITS ONLY CLIENT, which is the test that let them go where `fetchFleetPhase`
// stayed. `/spec` is what the MCP server reads — `cli/mcp.go:166`, the `get_scratch` tool — and
// that is the surface it was always for: an agent turning a drawing into a caller workflow. A
// browser copy of a read no browser performs is a second implementation of an agent's route.
//
// The LIST is a different absence and worth naming: nothing enumerates sketches any more, because
// a sketch is now keyed to the workflow it is a sketch OF (`workflow:<name>`), so the Workflows
// surface enumerates them by enumerating workflows. `fetchScratch`/`deleteScratch` below are kept —
// see the note on `fetchScratch`.

/**
 * One sketch by id. NO DIRECT CALLER, and kept anyway — this is a judgement, so it is written down.
 *
 * `fetchWorkflowSketch` below performs this exact GET against a derived id, so the ROUTE is on the
 * live path and only this un-narrowed spelling of it is unused. It is the shape a caller needs the
 * day a sketch is addressed by anything other than its workflow, and it is four lines. Removing it
 * would be a decision about the sketch feature's future, not a finding about dead code, and this
 * slice's rule is that those are different things (`deleteScratch` below is kept on the same terms:
 * `DELETE /api/scratch/:id` is live and nothing in the console offers deletion yet).
 */
export async function fetchScratch(id: string): Promise<ScratchRecord> {
  const res = await fetch(`${BASE}/scratch/${encodeURIComponent(id)}`);
  if (!res.ok) return asError(res, 'open the sketch');
  return (await res.json()) as ScratchRecord;
}

export async function saveScratch(input: {
  id?: string;
  name: string;
  document: ScratchDocument;
}): Promise<ScratchRecord> {
  const res = await fetch(`${BASE}/scratch`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(input),
  });
  if (!res.ok) return asError(res, 'save the sketch');
  return (await res.json()) as ScratchRecord;
}

export async function deleteScratch(id: string): Promise<void> {
  const res = await fetch(`${BASE}/scratch/${encodeURIComponent(id)}`, { method: 'DELETE' });
  if (!res.ok) return asError(res, 'delete the sketch');
}

/**
 * One workflow's own sketch, or `null` for a workflow nobody has drawn one for.
 *
 * `null` RATHER THAN A THROW, and that is the only reason this exists beside {@link fetchScratch}.
 * Every other read here treats a 404 as a failure because every other read is asked for something
 * the caller has just been told exists; this one is asked on OPENING a workflow, and most workflows
 * have no sketch. Routed through `asError` a missing sketch would arrive as
 * `open the sketch failed: 404 Not Found — not found`, and the design tab would open on an error
 * about an absence — which is the shape of the lie this whole surface keeps refusing (`neverRun`
 * is not `unresolved`, an empty Dataset is not a failed run). Nothing drawn is a state, not a fault.
 *
 * A 404 IS THE ONLY ABSENCE. Anything else — the appliance is down, the token is wrong, a proxy
 * answered — is a failure to READ, which is a different sentence and still throws, because a tab
 * that drew "no sketch yet" over an unreachable server would invite somebody to draw a second one.
 */
export async function fetchWorkflowSketch(workflow: string): Promise<ScratchRecord | null> {
  const res = await fetch(`${BASE}/scratch/${encodeURIComponent(workflowScratchId(workflow))}`);
  if (res.status === 404) return null;
  if (!res.ok) return asError(res, 'read this workflow’s sketch');
  return (await res.json()) as ScratchRecord;
}

/**
 * Write one workflow's sketch, creating it if this is the first thing drawn.
 *
 * IT IS THE SAME UPSERT EVERY OTHER SCRATCH USES. `POST /api/scratch` writes by id
 * (`ON CONFLICT(id) DO UPDATE`), so a sketch keyed to its workflow needs no create/update
 * distinction here and no route of its own — which also means the server's `parseScratchDocument`
 * narrows this document exactly as it narrows any other, and `renderScratch` reads it back the same
 * way. One rendering, on the server (ADR 0026).
 */
export async function saveWorkflowSketch(
  workflow: string,
  document: ScratchDocument
): Promise<ScratchRecord> {
  return saveScratch({ id: workflowScratchId(workflow), name: workflow, document });
}

export interface StartedRun {
  /** The caller's workflow id, which IS the run id. There is no second identifier. */
  runId: string;
  type: string;
  queue: string;
}

/**
 * Start one Run of a served workflow. `input` is the workflow's ONE argument.
 *
 * THE QUEUE IS NOT A PARAMETER (GitHub #15). The server derives it from `file` — the registered
 * folder — so a Run cannot be aimed at a queue string, and the server REFUSES if no worker is
 * serving that folder's current code. `type` is the `@workflow.defn` class the page is showing (a
 * folder may declare several); the queue still comes from the folder, never the type.
 */
export async function startRun(
  file: string,
  type: string,
  input?: unknown
): Promise<StartedRun> {
  const res = await fetch(`${BASE}/runs`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(input === undefined ? { file, type } : { file, type, input }),
  });
  if (!res.ok) return asError(res, 'start the run');
  return (await res.json()) as StartedRun;
}

/** What the two routes above admit. Read so the page can SHOW the posture — an open control
 *  surface the operator chose is fine; one nobody mentions is not. */
export async function fetchExposure(): Promise<{ open: boolean; detail: string } | null> {
  try {
    const res = await fetch(`${BASE}/workflows/exposure`);
    if (!res.ok) return null;
    return (await res.json()) as { open: boolean; detail: string };
  } catch {
    return null;
  }
}

/**
 * Fetch the server catalog — the actors deployed/registered with the orchestrator (a
 * worker self-registers on boot; `make register` adds its I/O schemas). This IS the
 * catalog the workflow editor shows: no folder upload, the actor list is auto-discovered.
 * Returns `null` if the backend is unreachable (pure browser/localStorage flow), so a
 * transient failure is distinguishable from a genuinely empty catalog.
 */
export async function fetchCatalog(): Promise<CatalogActor[] | null> {
  try {
    const res = await fetch(`${BASE}/actors`);
    if (!res.ok) return null;
    return (await res.json()) as CatalogActor[];
  } catch {
    return null;
  }
}

/**
 * One dataset. EVERYTHING IS A DATASET: an actor's output for one dispatch (`kind: 'output'`,
 * addressed by name + version + dt) or an operator-loaded list (`kind: 'standalone'`, addressed
 * by name alone). The two differ only in whether those coordinates exist.
 */
export interface DatasetInfo {
  kind: 'output' | 'standalone';
  /** The actor's name, or the list's. Never a run id, a table hash, or a graph node id. */
  name: string;
  version?: string;
  /** Dispatch time, `YYYY-MM-DDTHH-MM-SS` — the directory the output lives under. */
  dt?: string;
  rows: number;
  bytes: number;
  /** When the newest file was committed (ms), for newest-first ordering. */
  updatedAt?: number;
  /**
   * The §11 lifecycle — `open` / `sealed` / `abandoned` — when a caller's writer recorded one.
   * Absent means no writer ever did; see `datasets/state.ts`, which is the only thing that may
   * interpret it.
   */
  state?: DatasetState;
  /**
   * TRUE for a temporary Dataset — framework-named, owned by its Run, short-lived (temp-datasets
   * slice 01). The listing surfaces it so a `tmp_…` name reads as the temporary it is, distinct
   * from a durable Dataset, and can be deleted from the page (temp-datasets slice 04). Authority is
   * the server's owner marker, never the name prefix.
   */
  temporary?: boolean;
  /**
   * The owning Run's id, PRESENT only on a temporary Dataset — the Run that opened it, and the fact
   * an operator needs in order to decide whether an `open` temp still has a run behind it.
   */
  owner?: string;
  /**
   * The DERIVED run-grain name (ADR 0029 §2), `wf-<workflow>-<version>--<dt>Z--<digest6(runId)>` —
   * the CALLER WORKFLOW's identity, so one Run's several actor tables all read the same string. The
   * SERVER renders it, through the one `data/datasetName.ts` function, so this page shows exactly the
   * string `kontra dataset ls` shows — the page never re-derives it. ABSENT when nothing can say
   * which Run wrote the row (a standalone list; a partition several Runs share).
   *
   * ITS DATETIME IS UTC AND STAYS UTC IN THIS STRING. ADR 0029 §2 fixes that — two controllers
   * exist and a local-time name would denote two different instants depending on which box wrote
   * it — and adds "The UI renders local", which `datasets/localName.ts` does at RENDER time,
   * leaving the identity here untouched.
   */
  datasetName?: string;
  /**
   * The ONE **Run** behind this row — the KEY a tag or rename addresses (ADR 0029 §4, the record is
   * keyed by runId). PRESENT wherever {@link datasetName} is; a page holding a row can mutate the
   * record with it directly. Not a secret — it is the caller workflow's id (ADR 0023 §12).
   *
   * ABSENT when the row has more than one Run behind it, which is a structural guarantee and not a
   * promise: see {@link contributingRuns}.
   */
  runId?: string;
  /**
   * Every **Run** whose rows are in this partition, ascending — the honest PLURAL beside the
   * singular {@link runId}.
   *
   * A temp has exactly one owning Run; a durable Dataset ACCUMULATES, so one `runId` over it would
   * become a lie the second time anything promoted into it. The server reads this from the lake's
   * own per-file `run_id` statistics (no scan), and fills `runId` only where this names exactly
   * one. ABSENT for a table with no `run_id` column — an operator-loaded list — which is not the
   * same as "no Run wrote it".
   */
  contributingRuns?: string[];
  /**
   * TRUE when a data file spans several **Runs**, so {@link contributingRuns} is a lower bound.
   * The server refuses {@link runId} whenever it is set, because a bound is not a set.
   */
  contributingRunsPartial?: boolean;
  /**
   * The tag SET this Dataset carries (ADR 0029 §1), from the server's Dataset record. ABSENT when
   * untagged — only DEVIATION is stored, so no tags means no field rather than an empty array. Tags
   * are added and removed, never assigned as a scalar (`addDatasetTag` / `removeDatasetTag`).
   */
  tags?: string[];
  /**
   * The operator's RENAME (ADR 0029 §4), when the record holds one. ABSENT means the derived
   * {@link datasetName} stands: a surface shows `renamedTo ?? datasetName`, which is how the default
   * is used whenever no rename exists.
   */
  renamedTo?: string;
}

/** A Run's stored deviation, as the tag/rename routes return it (mirrors `DatasetDeviation` on the
 *  server). `tags: []` with no `renamedTo` is the emptied record — the store keeps no such row, but
 *  the route reports the post-state so a caller sees the cleared set rather than a 404. */
export interface DatasetDeviation {
  runId: string;
  tags: string[];
  renamedTo?: string;
}

/**
 * Add a tag to a Run's Dataset (ADR 0029 §1). Idempotent and a SET, so re-adding is a no-op and a
 * concurrent add of another tag survives — the server enforces both. Returns the post-state deviation.
 */
export async function addDatasetTag(runId: string, tag: string): Promise<DatasetDeviation> {
  const res = await fetch(`${BASE}/datasets/runs/${encodeURIComponent(runId)}/tags`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ tag }),
  });
  if (!res.ok) return asError(res, 'tag the Dataset');
  return (await res.json()) as DatasetDeviation;
}

/** Remove a tag from a Run's Dataset. Removing an absent tag is a no-op. */
export async function removeDatasetTag(runId: string, tag: string): Promise<DatasetDeviation> {
  const res = await fetch(
    `${BASE}/datasets/runs/${encodeURIComponent(runId)}/tags/${encodeURIComponent(tag)}`,
    { method: 'DELETE' }
  );
  if (!res.ok) return asError(res, 'untag the Dataset');
  return (await res.json()) as DatasetDeviation;
}

/** Rename a Run's Dataset — store a name that overrides the derived default (ADR 0029 §4). */
export async function renameDataset(runId: string, name: string): Promise<DatasetDeviation> {
  const res = await fetch(`${BASE}/datasets/runs/${encodeURIComponent(runId)}/name`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ name }),
  });
  if (!res.ok) return asError(res, 'rename the Dataset');
  return (await res.json()) as DatasetDeviation;
}

/** Drop the rename, so the derived default (ADR 0029 §2) stands again. */
export async function resetDatasetName(runId: string): Promise<DatasetDeviation> {
  const res = await fetch(`${BASE}/datasets/runs/${encodeURIComponent(runId)}/name`, {
    method: 'DELETE',
  });
  if (!res.ok) return asError(res, 'reset the Dataset name');
  return (await res.json()) as DatasetDeviation;
}

/** What deleting a temporary Dataset recovered — mirrors `DeletedDataset` on the server. */
export interface DeletedDataset {
  name: string;
  /** Rows the catalog held for this name at the moment of deletion. */
  rows: number;
  /** Bytes those rows' data files occupied — what the drop freed. */
  bytes: number;
  /** The owning Run the deleted temp was attributed to. */
  owner: string;
}

/**
 * Delete a temporary Dataset by name — temp-datasets slice 04's button, over slice 03's route.
 *
 * The route REFUSES a durable Dataset with a 409: only a Run's temporary Dataset is deleted by name,
 * so a delete aimed at the wrong name cannot destroy a durable record. On refusal the server's own
 * sentence is surfaced verbatim (via `asError`), so the operator reads WHY rather than a bare status.
 */
export async function deleteDataset(name: string): Promise<DeletedDataset> {
  const res = await fetch(`${BASE}/datasets/${encodeURIComponent(name)}`, { method: 'DELETE' });
  if (!res.ok) return asError(res, 'delete the temporary Dataset');
  return (await res.json()) as DeletedDataset;
}

/** A bounded preview: the grid's columns and rows, already JSON-safe. */
export interface DatasetPreview {
  columns: Array<{ name: string; type: string }>;
  rows: unknown[][];
}

/** List every dataset — output and standalone together, newest-first. */
export async function fetchDatasets(): Promise<DatasetInfo[]> {
  const res = await fetch(`${BASE}/datasets`);
  if (!res.ok) return asError(res, 'list datasets');
  return (await res.json()) as DatasetInfo[];
}

/**
 * A bounded preview of one dataset, rendered by the SERVER.
 *
 * This used to presign every parquet file and range-read them here with DuckDB-WASM — 76 MB of
 * WebAssembly, uncached, to fill a grid. The server holds an attached read connection already,
 * so the browser asks for rows and gets rows. No object-store URL reaches the page, which also
 * means no token has to be baked into the bundle for it.
 */
export async function fetchDatasetPreview(sel: DatasetInfo, limit = 500): Promise<DatasetPreview> {
  const q = new URLSearchParams({ kind: sel.kind, limit: String(limit) });
  if (sel.version) q.set('version', sel.version);
  if (sel.dt) q.set('dt', sel.dt);
  const res = await fetch(`${BASE}/datasets/${encodeURIComponent(sel.name)}/preview?${q}`);
  if (!res.ok) return asError(res, 'preview dataset');
  return (await res.json()) as DatasetPreview;
}

// --- secrets (issue 19) -------------------------------------------------------------------------
//
// NOTHING HERE CAN RETURN A VALUE, and that is not this client being careful — the routes it calls
// have no value to give (`backend/src/secrets/routes.ts`). The one route that does resolve a
// secret is the ACTOR's, authenticated as the actor by a signed identity, and a browser is not an
// actor: there is deliberately no function for it in this file.
//
// NO TOKEN IS SENT, and that is a limitation worth stating rather than a bug to paper over here.
// The management routes are open unless `KONTRA_SECRETS_TOKEN` is set (`secrets/routes.ts` says
// why), and the SPA has no way to hold a token that is not baked into the bundle at build time —
// the exact durable leak `/api/panels/ticket` exists to have removed. So on an appliance that gates
// this surface, secrets are managed from the API or the CLI, and this console reports 401.

/** One version of a secret. Metadata only — there is no value field, here or on the wire. */
export interface SecretVersion {
  version: number;
  createdAt: number;
  /** Set once revoked. The file backend has destroyed the ciphertext by the time this appears. */
  revokedAt?: number;
}

/** A secret as every read path sees it. */
export interface Secret {
  name: string;
  /** `actor:<name>` when an actor fetches this one itself; absent for an operator secret. */
  owner?: string;
  createdAt: number;
  updatedAt: number;
  versions: SecretVersion[];
  /** The highest version that is not revoked. Absent when every version has been revoked. */
  current?: number;
}

export interface SecretList {
  /** `file`, `kms`, … — which backend this appliance is configured with. */
  backend: string;
  /** Where the values rest, when the backend can say. */
  location?: string;
  secrets: Secret[];
}

export interface SecretWritten {
  secret: Secret;
  version: number;
  /** False the first time, true for every rotation after it. */
  rotated: boolean;
  /** The store trimmed surrounding whitespace — worth saying, since nobody will ever see it. */
  trimmed: boolean;
}

export async function fetchSecrets(): Promise<SecretList> {
  const res = await fetch(`${BASE}/secrets`);
  if (!res.ok) return asError(res, 'list secrets');
  return (await res.json()) as SecretList;
}

/** Create a secret, or rotate it by writing a new version. The same call for both, on purpose. */
export async function writeSecret(
  name: string,
  value: string,
  owner?: string
): Promise<SecretWritten> {
  const res = await fetch(`${BASE}/secrets/${encodeURIComponent(name)}`, {
    method: 'PUT',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(owner ? { value, owner } : { value }),
  });
  if (!res.ok) return asError(res, 'write secret');
  return (await res.json()) as SecretWritten;
}

/** Make one version unusable. */
export async function revokeSecretVersion(name: string, version: number): Promise<Secret> {
  const res = await fetch(
    `${BASE}/secrets/${encodeURIComponent(name)}/versions/${version}/revoke`,
    { method: 'POST' }
  );
  if (!res.ok) return asError(res, 'revoke secret version');
  return ((await res.json()) as { secret: Secret }).secret;
}

/** Forget a secret and every version of it. */
export async function destroySecret(name: string): Promise<void> {
  const res = await fetch(`${BASE}/secrets/${encodeURIComponent(name)}`, { method: 'DELETE' });
  if (!res.ok) await asError(res, 'destroy secret');
}

// --- slots and bindings (issue 20) ------------------------------------------------------------
//
// An actor declares a SLOT; the operator BINDS it to one of their secrets. These types mirror
// `backend/src/secrets/slots.ts`, which is the authority — none of them has a `value` field,
// for the reason every secret type here lacks one, and the resolution route is not called from the
// browser at all: it answers only an ACTOR authenticated as itself.

/** Whether a declared slot can be resolved, and if not, why not. */
export type SlotState = 'unbound' | 'bound' | 'revoked' | 'missing';

export interface SlotStatus {
  slot: string;
  /** The actor author's own sentence about what the credential is for. */
  description?: string;
  state: SlotState;
  /** The operator's secret this slot is bound to. Shown to the OPERATOR; never to the actor. */
  secret?: string;
  secretVersion?: number;
  boundAt?: number;
  detail: string;
}

/** One actor version's slots, and the diff against the version before it. */
export interface ActorSlots {
  actor: string;
  version: string;
  slots: SlotStatus[];
  /** Slots this version asks for that the previous one did not. */
  added: string[];
  /** The version `added` was computed against. Absent means there was none to compare. */
  comparedWith?: string;
}

export interface SlotBinding {
  actor: string;
  slot: string;
  secret: string;
  boundAt: number;
}

/** A secret as the binding picker needs it: a name, who owns it, and whether it resolves. */
export interface BindableSecret {
  name: string;
  owner?: string;
  usable: boolean;
}

export interface SlotSurface {
  location: string;
  actors: ActorSlots[];
  bindings: SlotBinding[];
  secrets: BindableSecret[];
}

/** One resolution, as the operator reads it back. No value, no digest — see `secrets/audit.ts`. */
export interface Resolution {
  at: number;
  actor: string;
  version: string;
  slot: string;
  run: string;
  secret: string;
  secretVersion: number;
  outcome: 'resolved' | 'undeclared' | 'unbound' | 'revoked' | 'missing' | 'forbidden';
}

export async function fetchSlots(): Promise<SlotSurface> {
  const res = await fetch(`${BASE}/slots`);
  if (!res.ok) return asError(res, 'list slots');
  return (await res.json()) as SlotSurface;
}

/** One actor's slots — what the Actors page draws on a card. */
export async function fetchActorSlots(actor: string, version?: string): Promise<ActorSlots> {
  const query = version ? `?version=${encodeURIComponent(version)}` : '';
  const res = await fetch(`${BASE}/slots/actor/${encodeURIComponent(actor)}${query}`);
  if (!res.ok) return asError(res, 'read actor slots');
  return (await res.json()) as ActorSlots;
}

/** Bind a slot to a secret, or rebind it. One verb for both, as the store has one for write. */
export async function bindSlot(actor: string, slot: string, secret: string): Promise<ActorSlots> {
  const res = await fetch(
    `${BASE}/slots/actor/${encodeURIComponent(actor)}/${encodeURIComponent(slot)}`,
    { method: 'PUT', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ secret }) }
  );
  if (!res.ok) return asError(res, 'bind slot');
  return ((await res.json()) as { actor: ActorSlots }).actor;
}

/** Withdraw a grant. */
export async function unbindSlot(actor: string, slot: string): Promise<void> {
  const res = await fetch(
    `${BASE}/slots/actor/${encodeURIComponent(actor)}/${encodeURIComponent(slot)}`,
    { method: 'DELETE' }
  );
  if (!res.ok) await asError(res, 'unbind slot');
}

/** The resolution ledger — "which actor read my key, and when". */
export async function fetchResolutions(filter: { actor?: string; slot?: string; run?: string } = {}): Promise<Resolution[]> {
  const query = new URLSearchParams(
    Object.entries(filter).filter(([, v]) => Boolean(v)) as Array<[string, string]>
  ).toString();
  const res = await fetch(`${BASE}/slots/audit${query ? `?${query}` : ''}`);
  if (!res.ok) return asError(res, 'read the resolution ledger');
  return ((await res.json()) as { resolutions: Resolution[] }).resolutions;
}
