/**
 * The Dataset lifecycle and how long an untagged one lives — declared ONCE, for both halves.
 *
 * ── WHY THIS FILE EXISTS ──────────────────────────────────────────────────────────────────────
 *
 * `DatasetState`'s own header used to say it was "spelled the same in four places with no shared
 * code", and it was right: the server's union, the browser's badge, and the two SDKs. Two of those
 * four are TypeScript and had no reason to be apart — and they had already come apart. The server
 * declared the closed union; the browser declared `state?: string`. A new lifecycle word compiled
 * clean in the browser and reached the code that interprets it unchecked.
 *
 * The retention pair is the same failure with a sharper measurement. MEASURED 2026-08-27: change
 * `DATASET_RETENTION_TTL_MS` to 48h and exactly ONE test fails — the one that asserts the literal
 * `24 * HOUR`. Make the edit anyone would make next, updating that literal, and the browser's
 * entire 2,115-test suite passes with a 48-hour server and a 24-hour browser. The browser's own
 * copy admitted the risk in prose: "Two implementations of one policy is a drift with no loud
 * failure mode." There is one implementation now.
 *
 * ── WHAT MAY LIVE HERE ────────────────────────────────────────────────────────────────────────
 *
 * Types and constants, and nothing that imports anything. That is not a style rule — it is the
 * reason this file can be imported by a browser bundle at all. The server's `retention.ts` cannot
 * be: it pulls DuckDB and the object store in behind it, and a type dragging a database into the
 * browser's dependency graph is how the fork got justified in the first place.
 *
 * The other two spellings — the Python and Go SDKs — are a language boundary and cannot import
 * this. They are gated by a corpus instead (ADR 0035, rule two).
 */

/**
 * What a Dataset says about itself while and after it is written.
 *
 * `open` means a producer may still append; it is also what a reader falls back to when the marker
 * is absent, which is true of every pre-§11 output and equally after a producer died mid-append.
 * `sealed` is a caller saying it is complete; `abandoned` is a caller giving up on it. The
 * distinction exists so a crashed Run leaves a VISIBLY unfinished Dataset rather than a short one
 * that reads as done.
 *
 * It lives as a small object beside the data rather than in the DuckLake catalog, because the
 * catalog's row count is a live SUM and cannot express "still appending".
 */
export type DatasetState = 'open' | 'sealed' | 'abandoned';

/**
 * How long an UNTAGGED Dataset lives after its last write — a constant owned by THIS repo
 * (ADR 0029 §3). NOT read from the Temporal namespace's `WorkflowExecutionRetentionTtl`: that it
 * is also 24h today is the dev server's coincidence, and coupling the two would make Dataset
 * lifetime change with an ops setting nobody connected to storage. Change this number here, on
 * purpose, or it does not change.
 */
export const DATASET_RETENTION_TTL_MS = 24 * 60 * 60 * 1000;

/**
 * The GRACE window added behind the TTL before anything is collected (ADR 0029 §3). Tagging is
 * frequently retroactive — the ADR's own example is an operator tagging at hour 23 — so the sweep
 * fires against `now - (TTL + GRACE)`, giving a retroactive tag a margin to land BEFORE its
 * Dataset is a collection candidate. Six hours: comfortably past the "hour 23" case without
 * letting genuinely abandoned output sit for another whole day.
 */
export const DATASET_RETENTION_GRACE_MS = 6 * 60 * 60 * 1000;

/**
 * What the LEDGER says about one node's output — a different question from the lifecycle above,
 * and the two are worth keeping apart because they share a field name.
 *
 * A node materialization is `complete` or `failed`; there is no `partial`. A row count of zero is
 * a SUCCESSFUL EMPTY RESULT, not a failure, and that distinction is the whole point of recording a
 * count — collapsing it back into a status would recreate the three-failures-one-label defect
 * ADR 0017 exists to fix.
 *
 * NOT THE SAME THING AS `DatasetState`, however much `record.state` and `dataset.state` look alike
 * at a call site. The lifecycle is what a WRITER declares about whether it is finished; this is
 * what the LEDGER observed about whether the output landed. A Dataset can be `sealed` and its
 * materialization `failed` at once, which is exactly the case ADR 0017 keeps as two dimensions.
 * Narrowing the browser's `state?: string` to the wrong one of these two is a compile error now,
 * which is how the confusion was found.
 */
export type MaterializationState = 'pending' | 'running' | 'complete' | 'failed';

/**
 * How a Run's OUTPUT reads to somebody who did not run it. Distinct from the Run's execution
 * status: a run whose workflow completed while its output failed is `output_failed`, and reporting
 * it as `completed` is the collapse ADR 0017 forbids.
 */
export type PublicLifecycle = 'executing' | 'finalizing' | 'completed' | 'output_failed';
