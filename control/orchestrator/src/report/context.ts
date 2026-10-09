/**
 * THE TEMPLATE CONTEXT — §2.4's contract, built once, published as the whole truth.
 *
 * ── WHAT A TEMPLATE CAN SEE, AND WHY THE LIST IS SHORT ─────────────────────────────────────────
 *
 * `run`, `workflow`, `input`, `result`, `report`. No environment, no secrets, no orchestrator
 * internals, no Temporal client, no store. A report template is written by whoever wrote the workflow
 * and runs on the process holding the object-store credentials, so the context is an ALLOWLIST built
 * here rather than a filtered view of something larger: there is nothing to filter, because nothing
 * else is ever put in.
 *
 * ── `result` IS NULL WHEN THE RUN DID NOT COMPLETE, AND THAT IS LOAD-BEARING ────────────────────
 *
 * A failed, cancelled, terminated or timed-out Run has no return value — Temporal's close event
 * carries a failure rather than a result. So `result` is `null`, which with `lenientIf` makes
 * `{% if result %}…{% else %}…{% endif %}` the documented way to write a template that handles both
 * (acceptance test 11). `undefined` would be an `UndefinedVariableError` under `strictVariables`,
 * which would make the else-branch unreachable.
 *
 * ── `duration_s` IS SECONDS, AND IT IS COMPUTED HERE ───────────────────────────────────────────
 *
 * From `closedAt - startedAt`, both epoch milliseconds from the describe. Rounded to one decimal:
 * a report says "461.3s", not "461283ms", and not the float that division produces.
 */

import { createHash } from 'node:crypto';

import type { PinnedTemplate } from './store';

/** §2.4's `run`. Every field is from the describe, which costs no payload read. */
export interface RunContext {
  id: string;
  workflow_id: string;
  status: string;
  started_at: string;
  ended_at: string;
  duration_s: number;
  error: { type: string; message: string } | null;
  /** `null` rather than absent, so `{% if run.progress %}` is falsy instead of a strictVariables error. */
  progress: ProgressContext | null;
}

/** §2.4's `workflow`. */
export interface WorkflowContext {
  name: string;
  version: string;
  workspace: string;
}

/** §2.4's `report`. */
export interface ReportContext {
  rendered_at: string;
  template_hash: string;
  version: number;
}

/**
 * `run.progress` (ADR 0062) — what a live report counts while the run is open.
 *
 * `units_done` INCLUDES `isolated`: ADR 0060 is explicit that a unit abandoned after repeated
 * failure is finished, and counting it as outstanding makes a healthy run read as stuck for ever.
 */
export interface ProgressContext {
  units_done: number;
  units_total: number;
  isolated: number;
  phase: string;
  updated_at: string;
}

/** The head and tail bounds a template may read. Not configurable: they bound the render, not a view. */
export const DATASET_HEAD_MAX = 20;
export const DATASET_TAIL_MAX = 50;

/**
 * `datasets.<name>` (ADR 0062) — the run's own Datasets, summarised while it is still writing them.
 *
 * BOUNDED AT THE CONTEXT, not at the template: `head`/`tail` are clamped here, so no template can
 * ask for more and no author can be surprised by a report that got slower as a Dataset grew.
 */
export interface DatasetSummary {
  rows: number;
  batches: number;
  last_commit_at: string;
  head: readonly unknown[];
  tail: readonly unknown[];
  /**
   * The column names across `head`, in first-seen order — what lets a template draw a TABLE of a
   * Dataset it knows nothing about.
   *
   * WITHOUT THIS A LIVE TABLE IS NOT WRITABLE. `head` is a list of arbitrary row objects, and Liquid
   * has no way to take the union of their keys, so a template could only print rows it already knew
   * the shape of — which the default template, by definition, does not. Derived HERE, at the same
   * chokepoint that clamps the rows, so every producer gets it and none can disagree about it.
   */
  columns: readonly string[];
}

export interface TemplateContext {
  run: RunContext;
  workflow: WorkflowContext;
  input: unknown;
  result: unknown;
  report: ReportContext;
  /** THE SIXTH ROOT. `cli/reportlint.go`'s `contextRoots` must list it or every template using it
   *  fails at render under `strictVariables` — which is to say after the run. */
  datasets: Record<string, DatasetSummary>;
  [key: string]: unknown;
}

/** The terminal statuses §2.4 names, lowercased and underscored as a template will spell them. */
const STATUS_WORDS: Record<string, string> = {
  completed: 'completed',
  failed: 'failed',
  canceled: 'cancelled',
  cancelled: 'cancelled',
  terminated: 'terminated',
  'timed out': 'timed_out',
  timedout: 'timed_out',
  timed_out: 'timed_out',
  continued_as_new: 'continued_as_new',
  'continued as new': 'continued_as_new',
};

/**
 * Normalise Temporal's spelling to the contract's.
 *
 * TEMPORAL SAYS `canceled` AND THE CONTRACT SAYS `cancelled`. One l or two is exactly the kind of
 * difference a template author would discover by their `{% if run.status == "cancelled" %}` silently
 * never matching, so the mapping is explicit and total, and an unknown status passes through
 * unchanged rather than becoming `unknown` — a status this code has not met is still a fact.
 */
export function statusWord(raw: string): string {
  return STATUS_WORDS[raw.trim().toLowerCase()] ?? raw.trim().toLowerCase();
}

/** An ISO instant, or the empty string for an epoch of 0 — which is what "still open" looks like. */
function isoOf(epochMs: number): string {
  return epochMs > 0 ? new Date(epochMs).toISOString() : '';
}

export interface BuildContextInput {
  runId: string;
  status: string;
  startedAt: number;
  closedAt: number;
  /** The workflow TYPE — the `@workflow.defn` class — used only as a fallback name. */
  type?: string;
  /** The manifest identity snapshotted at start, when there is one (`data/runWorkflows.ts`). */
  identity?: { workflow: string; version: string } | undefined;
  pinned?: Pick<PinnedTemplate, 'templateHash' | 'workspace'> | undefined;
  input?: unknown;
  output?: unknown;
  /** What the close event said, when it was not a completion. */
  error?: { type: string; message: string } | undefined;
  version: number;
  now?: number;
  /** ADR 0062. Progress facts for an open run; absent renders `run.progress` as null. */
  progress?: ProgressContext | undefined;
  /** ADR 0062. Summaries of the run's own Datasets, clamped here rather than trusted. */
  datasets?: Record<string, DatasetSummary> | undefined;
  /**
   * ADR 0062. The workflow's `report` QUERY while the run is OPEN — what `result` becomes mid-run.
   *
   * Only consulted for an open run, so a FAILED, cancelled, terminated or timed-out run still has
   * `result === null`. That is the load-bearing part of §2.4 and it is unchanged; what does change is
   * that `{% if result %}` is true mid-run for a workflow that defines the handler, which makes
   * `run.status` the completion test rather than `result`.
   */
  partial?: unknown;
}

/** Clamp one Dataset summary to the context's own bounds. */
/** Columns beyond this are dropped: a table wider than a screen is not a table a person reads, and
 *  a Dataset with 60 columns would otherwise make every default report unreadable. */
const DATASET_COLUMN_MAX = 8;

function clampSummary(s: DatasetSummary): DatasetSummary {
  const head = s.head.slice(0, DATASET_HEAD_MAX);
  // FIRST-SEEN ORDER, NOT SORTED. A row's own key order is the author's, and alphabetising it puts
  // `batches` before `title` for no reason a reader would recognise.
  const columns: string[] = [];
  for (const row of head) {
    if (typeof row !== 'object' || row === null || Array.isArray(row)) continue;
    for (const k of Object.keys(row as Record<string, unknown>)) {
      if (!columns.includes(k) && columns.length < DATASET_COLUMN_MAX) columns.push(k);
    }
  }
  return { ...s, head, tail: s.tail.slice(0, DATASET_TAIL_MAX), columns };
}

/**
 * Build the context for one render.
 *
 * DETERMINISTIC APART FROM `report.rendered_at`, which is why that one is an input rather than a
 * `Date.now()` inside: two renders of the same Run must produce the same bytes for the idempotency key
 * to mean anything, and a timestamp baked in here would make every render differ.
 */
export function buildContext(input: BuildContextInput): TemplateContext {
  // ELAPSED WHILE OPEN, TOTAL ONCE CLOSED. `closedAt` is 0 for a running run, so the old form —
  // `closedAt > startedAt ? … : 0` — reported 0 SECONDS FOR THE WHOLE LIFE of every run, and a live
  // report that ticks is the one place that is most obviously wrong. Measured on the live install:
  // `duration_s` read 0 from the first frame to the last.
  //
  // `input.now` is the render clock the context already takes (and the only non-deterministic input
  // it has, which is why it is a parameter rather than a `Date.now()` in here). A frozen render
  // still computes from `closedAt`, so the stored document is unchanged and reproducible.
  const until = input.closedAt > input.startedAt ? input.closedAt : (input.now ?? 0);
  const duration =
    until > input.startedAt ? Math.round(((until - input.startedAt) / 1000) * 10) / 10 : 0;
  const status = statusWord(input.status);
  return {
    run: {
      id: input.runId,
      // THE SAME STRING AS `id`, and that is not a mistake. A Run IS its caller's workflow id in this
      // system (ADR 0023 §12); §2.4 lists both names, so both are served rather than one of them
      // being quietly absent for a template that happens to use the other.
      workflow_id: input.runId,
      status,
      started_at: isoOf(input.startedAt),
      ended_at: isoOf(input.closedAt),
      duration_s: duration,
      error: input.error ?? null,
      progress: input.progress ?? null,
    },
    workflow: {
      name: input.identity?.workflow ?? input.type ?? '',
      version: input.identity?.version ?? '',
      workspace: input.pinned?.workspace ?? '',
    },
    // `null` rather than absent, for both: a template may legitimately ask `{% if input %}`, and
    // `strictVariables` makes an absent name an error rather than a falsy value.
    input: input.input ?? null,
    // ADR 0062. THREE CASES, AND ONLY THE MIDDLE ONE IS NEW: the return value once the run completed,
    // the workflow's `report` query while it is still OPEN, and `null` for every terminal
    // non-completion — which is §2.4's load-bearing case and is unchanged.
    result:
      status === 'completed'
        ? (input.output ?? null)
        : input.closedAt > 0
          ? null
          : (input.partial ?? null),
    report: {
      rendered_at: isoOf(input.now ?? 0),
      template_hash: input.pinned?.templateHash ?? '',
      version: input.version,
    },
    // Always an object, never absent: `{{ datasets.nope.rows }}` should be an author's empty value
    // rather than a strictVariables error about a root they spelled correctly.
    datasets: Object.fromEntries(
      Object.entries(input.datasets ?? {}).map(([name, s]) => [name, clampSummary(s)])
    ),
  };
}

/**
 * The idempotency key for one render — §4.5's "the same run with the same template hash and the same
 * inputs twice gives one version".
 *
 * WHAT GOES IN IT, AND WHAT DELIBERATELY DOES NOT. The template hash and a digest over the context's
 * DATA — run metadata, input, result. `report.rendered_at` is excluded because it changes every render
 * by definition, and `report.version` because it is assigned BY the insert this key guards. Including
 * either would make every render unique and the idempotency guarantee vacuous, which is the failure
 * mode a key like this usually has.
 */
export function renderKey(templateHash: string, context: TemplateContext): string {
  // ADR 0062 ADDED TWO VALUES AND NEITHER MAY ENTER THIS KEY. `run.progress` and `datasets` change on
  // every batch commit, so including either mints a version per tick and the dropdown becomes the
  // tick history. PROJECTING A FIELD OUT IS BACKWARD-COMPATIBLE — a run that never carried `progress`
  // hashes exactly as it did before this change, so no stored version is orphaned and the convergence
  // pass does not re-render the whole retention window on deploy.
  //
  // CALL THIS ONLY WITH A FINAL CONTEXT. For an open run `result` is the workflow's `report` query,
  // which also changes per tick — live renders are never persisted, so no key is ever minted for one.
  const { progress: _progress, ...run } = context.run;
  const stable = {
    run,
    workflow: context.workflow,
    input: context.input,
    result: context.result,
  };
  // HASHED, AND THE FIRST VERSION OF THIS WAS NOT. Returning the canonical JSON itself would have put
  // the whole decoded result — which may be megabytes — into `report_version.render_key`, a column
  // carrying a UNIQUE INDEX. Both backends would have accepted it and the index would have grown with
  // the data it was meant to summarise.
  //
  // Canonical because the key order is fixed by the literal above rather than by object-insertion
  // luck, which is what makes the same inputs hash the same way on a second render.
  return `sha256:${createHash('sha256').update(`${templateHash}\u0000${JSON.stringify(stable)}`).digest('hex')}`;
}
