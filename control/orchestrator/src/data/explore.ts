/**
 * The explore MANIFEST — everything `kontra explore <actor[@version]>` needs to open a
 * dispatch's output on the operator's own workstation, and nothing more.
 *
 * ADDRESSING. An operator names an ACTOR and, if they care, a time. Never a run id, never a
 * table name. The run id still exists — it scopes presigning and it is what the ledger keys
 * on — but it is tooling, and no human should have to carry one. That is the whole reason
 * output moved to `output/<actor>/version=<v>/dt=<dispatch>/`.
 *
 * THE THREAT MODEL (plan §3, architecture gate 4)
 *
 * A presigned URL is OBJECT-LEVEL authorization. Whoever holds it can read that object,
 * whole. It carries no notion of a row filter, so a `WHERE` clause in generated SQL is
 * defence in depth and NOT access control.
 *
 * Three properties make the scope real rather than advisory:
 *
 *   1. PHYSICAL EXCLUSIVITY. `dt` is the dispatch time to the second, taken from the
 *      server-minted `run_started_at`, so a partition directory holds one dispatch. This
 *      module presigns only files under the requested actor/version/dt directories.
 *      Compaction must never merge across them (see `maintenance.ts`).
 *   2. SHORT EXPIRY as the boundary. The URL, not the file, is the secret. Local cleanup of
 *      the manifest is best-effort — a crash must not leave a credential behind, so the
 *      credential is time-boxed rather than trusted to be deleted.
 *   3. NO DURABLE CREDENTIAL LEAVES THE SERVER. The workstation receives URLs, never the
 *      object-store keys or the catalog connstring.
 *
 * Cross-run analysis is a different mode with a different credential (`--catalog`), and
 * deliberately not the default.
 */

import type { ObjectStore } from '../codec/objectStore';
import type { MaterializationRecord } from './materialization';
import { runFiles, type DatasetColumn } from './datasets';
import { dtPartition, safeName, type LakeConfig } from './parquet';

/**
 * How long an exact-dispatch URL stays valid. Short on purpose (see property 2). Long enough
 * to open a workspace and run a few queries; short enough that a leaked manifest is stale
 * before it is useful. `kontra explore` re-fetches rather than caching.
 */
export const DEFAULT_URL_TTL_SECONDS = 900;

/**
 * One ACTOR'S output for one dispatch — the grain an operator addresses, and the grain that
 * becomes a view.
 *
 * A dispatch id (`n1`, `crawl-7f3a`) is a label one Method call was made under: several calls
 * to one Actor in one Run are ONE dataset. Those ids stay visible for diagnosis; they are never
 * how output is named.
 */
export interface ExploreDataset {
  actor: string;
  version: string;
  /** `YYYY-MM-DDTHH-MM-SS` — the dispatch, and the directory the files live under. */
  dt: string;
  /** The view the CLI creates: the actor name. Never a dispatch id, never a table hash. */
  view: string;
  /** Presigned GET URLs for THIS dispatch's parquet files only. */
  urls: string[];
  columns: DatasetColumn[];
  /** Rows committed across every dispatch folded into this dataset. */
  rows: number;
  /** The dispatch ids folded in — diagnosis only, never an address. */
  nodes: string[];
  /** Worst state across those dispatches: `failed` wins, then non-terminal, then `complete`. */
  state: string;
  /** Bounded failure summary when `state` is `failed` — shown INSTEAD of empty results. */
  error: string | null;
}

export interface ExploreManifest {
  runId: string;
  /** Server-minted dispatch time (ms). */
  runStartedAt: number;
  /** `YYYY-MM-DDTHH-MM-SS` — what `--dt` addressed. */
  dt: string;
  /** When these URLs stop working (epoch ms) — the CLI shows it and re-fetches. */
  expiresAt: number;
  /** ONE ENTRY PER ACTOR. This is what the CLI turns into views. */
  datasets: ExploreDataset[];
  /** Actors whose materialization is recorded but has no readable files yet. */
  pending: string[];
  /** Actors whose materialization is exhausted — shown as failure, never as empty. */
  failed: string[];
}

/** The view name for one actor's output — `crawl4ai`, sanitised to a SQL identifier. */
export function viewName(actor: string): string {
  return safeName(actor).replace(/[.-]/g, '_');
}

/** Structural shape of what `runFiles` returns, kept local to the join below. */
interface FilesFor {
  keys: string[];
  columns: DatasetColumn[];
}

/**
 * Build the manifest for one dispatch.
 *
 * Materialization records and lake files are joined so the two ways output can be absent
 * stay distinguishable: an actor with a `failed` record is reported as FAILED, and one with
 * a `complete` record but no files is reported as an EMPTY SUCCESS. Neither is shown as
 * "no rows", which is the ambiguity ADR 0017 exists to remove.
 *
 * Records are the ONLY input that decides what exists — a dispatch still running has records
 * in `running`, so its finished actors are already readable while the rest are still being
 * written. That is what makes output queryable AS A RUN PROGRESSES.
 */
export async function buildExploreManifest(
  store: ObjectStore,
  runId: string,
  records: readonly MaterializationRecord[],
  opts: { ttlSeconds?: number; lake?: Partial<LakeConfig> } = {}
): Promise<ExploreManifest> {
  const ttl = opts.ttlSeconds ?? DEFAULT_URL_TTL_SECONDS;
  const files = await runFiles(store, records, opts.lake ?? {});
  const byActor = new Map<string, FilesFor>();
  for (const f of files) {
    byActor.set(`${f.actor} ${f.version} ${f.dt}`, { keys: f.keys, columns: f.columns });
  }

  // Fold the ledger's per-node records into one entry per (actor, version, dispatch).
  const grouped = new Map<string, ExploreDataset>();
  for (const r of records) {
    const actor = safeName(r.actor);
    const dt = dtPartition(r.runStartedAt);
    const key = `${actor} ${r.version} ${dt}`;
    let d = grouped.get(key);
    if (!d) {
      d = {
        actor,
        version: r.version,
        dt,
        view: viewName(actor),
        urls: [],
        columns: [],
        rows: 0,
        nodes: [],
        state: 'complete',
        error: null,
      };
      grouped.set(key, d);
    }
    // Only committed rows count. A failed node's partial counter must not inflate a total —
    // that is how a wiped batch reports as a healthy one.
    if (r.state === 'complete') d.rows += r.rows;
    if (!d.nodes.includes(r.node)) d.nodes.push(r.node);
    // Worst state wins: a dataset whose one shard failed is not a healthy dataset.
    if (r.state === 'failed') {
      d.state = 'failed';
      d.error = d.error ?? r.error;
    } else if (d.state !== 'failed' && r.state !== 'complete') {
      d.state = r.state;
    }
  }

  // Attach the files. Presigned ONE AT A TIME, and only files already filtered to this
  // dispatch's partition directories — nothing here can widen that scope.
  const pending: string[] = [];
  const failed: string[] = [];
  for (const d of grouped.values()) {
    const f = byActor.get(`${d.actor} ${d.version} ${d.dt}`);
    if (f) {
      d.urls = await Promise.all(f.keys.map((k) => store.presignGet(k, ttl)));
      d.columns = f.columns;
    }
    d.nodes.sort();
    if (d.state === 'failed') failed.push(d.actor);
    else if (d.state !== 'complete') pending.push(d.actor);
  }

  const datasets = [...grouped.values()].sort((a, b) => a.view.localeCompare(b.view));
  const startedAt = records.length > 0 ? Math.min(...records.map((r) => r.runStartedAt)) : 0;
  return {
    runId,
    runStartedAt: startedAt,
    dt: startedAt > 0 ? dtPartition(startedAt) : '',
    expiresAt: Date.now() + ttl * 1000,
    datasets,
    pending: [...new Set(pending)].sort(),
    failed: [...new Set(failed)].sort(),
  };
}
