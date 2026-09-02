/**
 * The DERIVED name of a Dataset (ADR 0029 §2) — one string, rendered from facts the ledger
 * already holds, never stored and never indexed.
 *
 *     wf-<workflow>-<version>--<dtPartition(runStartedAt)>Z--<runFragment(runId)>
 *     wf-nscheck-0.1.0--2026-08-19T14-32-07Z--a3f9c1
 *
 * THIS IS THE ONE PLACE THE STRING IS SPELLED. The API computes it, and the Datasets page and
 * `kontra dataset ls` render what the API returns — none of them re-derives it — so the name
 * cannot drift between the three surfaces. That is the whole reason it is a function and not a
 * format string copied into each caller (ADR 0029 §2; the previous `_kontra_dataset` registry,
 * whose only job was translating a hash back into a name, was deliberately deleted — this must
 * not walk that back, so it stays a rendering of existing fields with no lookup table).
 *
 * THREE CONSTRAINTS THE ADR FIXES, each carried here so a later edit cannot lose one:
 *
 *   - THE DATETIME IS UTC, VIA {@link dtPartition}. Two controllers exist (sfo3, nyc1); a
 *     local-time name would denote two different instants depending on which box wrote it. The
 *     UI renders local; the NAME never does. `dtPartition` is the same second-precision,
 *     colons-dashed renderer the `dt=` partition already uses, so a second datetime format is
 *     not introduced here.
 *   - THE RUN-ID FRAGMENT IS IN THE NAME, not only in metadata. It disambiguates two runs that
 *     started in the same second (their `dtPartition` is identical). It is a DIGEST of the run
 *     id rather than its first characters — see {@link runFragment} for the measurement that
 *     forced that. Traceability back to the run rides the full `runId` stamped on the row.
 *   - THE WORKFLOW/VERSION ARE THE CALLER WORKFLOW'S IDENTITY, NEVER THE TASK-QUEUE STRING.
 *     `--queue` can be overridden at start, so the queue is transport; the caller passes the
 *     MANIFEST's name and version, which are identity — the caller workflow's, snapshotted at start
 *     (`data/runWorkflows.ts`), falling back to the producing Actor's when no such snapshot exists.
 *     See {@link DatasetNameParts.workflow}.
 */

import { createHash } from 'node:crypto';

import { dtPartition } from './parquet';

/** How many characters of the run-id fragment go in the name (ADR 0029 §2). */
export const RUN_ID_FRAGMENT = 6;

/**
 * The run-id fragment: a DIGEST of the full run id, not its first characters.
 *
 * ADR 0029 §2 wrote this as `runId[:6]`, and that spelling is defeated by how this repo mints a
 * run id. BOTH start paths produce `<type>-<unixseconds>` — `workflowControl.ts` behind
 * `POST /api/runs` and `cli/workflow.go` — and the ledger's runId IS that workflow id. So the
 * first six characters are the workflow NAME's prefix and carry no run entropy at all: every
 * nscheck run in history renders `--nschec`. Measured, not reasoned: two same-second nscheck runs
 * produced byte-identical names, and the unit test that claimed otherwise passed only because it
 * hand-fed UUID-shaped ids the system never mints.
 *
 * A digest restores what the ADR asked the fragment FOR — distinguishing two runs whose
 * `dtPartition` is identical — under every id scheme, including ids that differ only in a
 * trailing character. It is not reversible, and it does not need to be: the full runId is stamped
 * on the listing row beside the name (and is the key a tag or rename addresses), so a kept
 * Dataset stays traceable to its run after Temporal has deleted the execution. Hex also matches
 * the shape of the ADR's own example, `a3f9c1`.
 */
export function runFragment(runId: string): string {
  return createHash('sha256').update(runId).digest('hex').slice(0, RUN_ID_FRAGMENT);
}

/** The facts a Dataset name is rendered from. Every one is on the materialization record or the
 *  producer's manifest — none is looked up, and none is the task-queue string. */
export interface DatasetNameParts {
  /**
   * The CALLER WORKFLOW's manifest NAME, never the task-queue string it happens to be served on.
   *
   * ADR 0029 §2 means the **workflow**'s manifest name, and that is what callers pass: both start
   * paths snapshot the caller's `workflow.json` `name`/`version` into `data/runWorkflows.ts` at the
   * moment the Run starts (ADR 0025's pattern — what must outlive Temporal's 24h retention is
   * recorded, not looked up later), and `withDatasetNames` renders the recorded identity. That is
   * what makes ONE Run writing SEVERAL actor tables carry ONE name, which is the Consequence §2
   * states and the thing an Actor-grain identity cannot express.
   *
   * FALLBACK, EXPLICIT AND PERMANENT: when no identity was recorded for the Run — every Dataset that
   * predates that store, and any Run whose stamp did not land — the caller passes the PRODUCING
   * ACTOR's stamped name and version instead, so a Dataset always says something it is called rather
   * than nothing. Such a name is actor-grain: two actor tables from one Run then differ, which is the
   * old behaviour and is why the record exists.
   *
   * Both readings are a MANIFEST identity, which is the invariant that never bends: this field is a
   * field somebody stamped, NOT the queue a `--queue` could once override. This function has no queue
   * parameter at all, so a queue string cannot reach the name even by accident.
   */
  workflow: string;
  /** The caller workflow's manifest version — the Actor's version under the fallback above. Manifest,
   *  not queue, on the same terms as {@link DatasetNameParts.workflow}. */
  version: string;
  /** Server-minted run start (ms). Rendered UTC through {@link dtPartition}. */
  runStartedAt: number;
  /** The caller workflow's id (which is what a Run IS, ADR 0023 §12); {@link runFragment} of it
   *  goes in the name. */
  runId: string;
}

/**
 * Render the derived name. A pure total function over plain values — no I/O, no lake — so the
 * exact format, the `Z`, and the run-id fragment can be pinned without a store.
 */
export function datasetName(parts: DatasetNameParts): string {
  const workflow = parts.workflow.trim();
  const version = parts.version.trim();
  const fragment = runFragment(parts.runId);
  return `wf-${workflow}-${version}--${dtPartition(parts.runStartedAt)}Z--${fragment}`;
}
