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

export interface TemplateContext {
  run: RunContext;
  workflow: WorkflowContext;
  input: unknown;
  result: unknown;
  report: ReportContext;
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
}

/**
 * Build the context for one render.
 *
 * DETERMINISTIC APART FROM `report.rendered_at`, which is why that one is an input rather than a
 * `Date.now()` inside: two renders of the same Run must produce the same bytes for the idempotency key
 * to mean anything, and a timestamp baked in here would make every render differ.
 */
export function buildContext(input: BuildContextInput): TemplateContext {
  const duration =
    input.closedAt > input.startedAt ? Math.round(((input.closedAt - input.startedAt) / 1000) * 10) / 10 : 0;
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
    },
    workflow: {
      name: input.identity?.workflow ?? input.type ?? '',
      version: input.identity?.version ?? '',
      workspace: input.pinned?.workspace ?? '',
    },
    // `null` rather than absent, for both: a template may legitimately ask `{% if input %}`, and
    // `strictVariables` makes an absent name an error rather than a falsy value.
    input: input.input ?? null,
    result: status === 'completed' ? (input.output ?? null) : null,
    report: {
      rendered_at: isoOf(input.now ?? 0),
      template_hash: input.pinned?.templateHash ?? '',
      version: input.version,
    },
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
  const stable = {
    run: context.run,
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
