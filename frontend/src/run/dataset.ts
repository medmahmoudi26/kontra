/**
 * The Datasets page's handoff to the CLI.
 *
 * There is no query engine in here any more. The page used to embed DuckDB-WASM — 40 MB
 * `duckdb-mvp.wasm` + 36 MB `duckdb-eh.wasm` plus two ~800 KB workers, served uncompressed and
 * re-fetched on every visit — so the browser could range-read presigned parquet itself. That is
 * an enormous amount of machinery for a capped preview, and it required minting object-store
 * URLs for a page that only ever draws a grid. The server renders the preview now
 * (`GET /api/datasets/:name/preview`), and anything deeper belongs in a real DuckDB.
 */

import type { DatasetInfo } from './api';

/** SQL string literal — a dataset name reaches the command as data, never as syntax. */
function lit(v: string): string {
  return `'${v.replace(/'/g, "''")}'`;
}

/**
 * The command that opens this dataset on the operator's own workstation.
 *
 * Addressed exactly as the CLI addresses it: by NAME, scoped with `--version` / `--dt` when the
 * dataset has them. No run UUID and no physical table name — those are the two things an
 * operator should never have to type, and `--version`/`--dt` are partition columns, so the
 * scoped query prunes to one dispatch instead of scanning the actor's whole table.
 *
 * The same query string is what `kontra dispatch --input <name> --query "…"` accepts, so a
 * preview here can become the input to the next actor without rewriting anything.
 */
export function queryCommand(d: Pick<DatasetInfo, 'name' | 'version' | 'dt'>): string {
  const scope = [
    d.version ? `--version ${d.version}` : '',
    d.dt ? `--dt ${d.dt}` : '',
  ]
    .filter(Boolean)
    .join(' ');
  const sql = `SELECT * FROM ${d.name} LIMIT 100`;
  return `kontra dataset query ${d.name} ${scope ? scope + ' ' : ''}--sql ${lit(sql)}`;
}
