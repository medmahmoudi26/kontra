/**
 * Shard derivation — the TypeScript arm of a THREE-language contract.
 *
 * A unit blob's key is hive-partitioned (ADR 0015):
 *
 *     units/run={run}/dt={date}/actor={actor}/shard={shard}/unit={nnnnn}/{sha}.json
 *
 * The Go and Python actor hosts WRITE those keys; this orchestrator READS them to size a Run's
 * output and to scope an explore manifest to it. All three must derive `shard` identically from
 * a dispatch id, or a reader lists nothing and reports a run that produced plenty as empty — a
 * silent wrong answer, not an error.
 *
 * The three have drifted before (isolation counters shipped Go-only), so all three are pinned to
 * one golden fixture: conformance/blobkey.json, asserted by each language's own suite.
 *
 * NOTHING IN TYPESCRIPT READS A UNIT BLOB TODAY. The reader was the interpreter's streaming
 * cursor, which is gone (ADR 0023 §12); the lake surfaces read Parquet, not `units/`. This arm
 * stays because it is one third of a cross-language contract that the Go and Python hosts still
 * WRITE, and because the fixture is only meaningful while all three assert it — not because it
 * has a caller here.
 */

/** Strip characters that would forge a path or glob segment. Mirrors Go's partSafe. Exported so
 *  the history archive files its own `run=` partition by the same rule (ADR 0025) — a second
 *  spelling of "what is safe in a partition value" is how two readers end up disagreeing about
 *  which keys exist. */
export function partSafe(v: string): string {
  return v.replace(/[/= *?]/g, '_');
}

/**
 * Render a dispatch id as its shard partition value: `n7` -> `0007`, `crawl` -> `crawl`.
 *
 * The id is whatever the caller stamped on the dispatch (`EntryInput.node_id`) — a label for
 * one Method call, not a position in anything. Numeric-looking ids are zero-padded, which is
 * what stops `n1` from prefix-matching `n10`. That collision is not hypothetical: a per-id
 * reader globbed `n1*` and reported 2,424 blobs where the truth was 63, which put two wrong
 * figures into a published report.
 *
 * A dotted suffix (`n1.2`) is split off before padding rather than defeating it, so `n1.2` ->
 * `0001.2` and cannot collide with `n10.2`.
 */
export function shardOf(dispatchId: string): string {
  const name = dispatchId || 'node';
  const dot = name.indexOf('.');
  const base = dot === -1 ? name : name.slice(0, dot);
  const suffix = dot === -1 ? '' : name.slice(dot + 1);
  const digits = base.startsWith('n') ? base.slice(1) : base;
  // Guard the string form too: Number('') is 0 and Number(' 7') is 7, either of which would
  // silently mis-shard an id that merely looks numeric.
  if (!/^\d+$/.test(digits)) return partSafe(name);
  const padded = String(Number(digits)).padStart(4, '0');
  return suffix ? `${padded}.${partSafe(suffix)}` : padded;
}

/**
 * The S3 prefix holding one run's unit blobs in the hive layout.
 *
 * The empty-string default must match the writers': they emit `run=run` rather than `run=`,
 * because a key with an empty partition value is one the reader can never match — a blob that
 * exists and is invisible. Defaulting differently here would look harmless and lose exactly
 * those blobs.
 */
export function runPrefix(runId: string): string {
  return `units/run=${partSafe(runId || 'run')}/`;
}

/** The pre-hive prefix. Months of output still live here, so readers must accept both. */
export function legacyRunPrefix(runId: string): string {
  return `units/${runId}/`;
}
