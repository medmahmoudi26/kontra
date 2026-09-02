/**
 * The query workbench's client.
 *
 * The engine is on the SERVER (`data/queryEngine.ts`), on a connection attached READ_ONLY with
 * the local filesystem disabled and its configuration locked. This module only carries SQL over
 * and gets rows back — there is no query engine in the browser, which is the whole point: the
 * page that used to ship 76 MB of DuckDB-WASM now ships a text editor.
 *
 * The routes are bearer-gated because they can read every dataset. The token is injected at
 * build time, so a deployment that does not set it gets a page that says "unauthorized" rather
 * than one that silently shows nothing.
 */

const BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api';

/** One dataset's queryable shape — the sidebar tree and the editor's autocomplete source. */
export interface SchemaEntry {
  kind: 'output' | 'standalone';
  name: string;
  version?: string;
  dt?: string;
  columns: Array<{ name: string; type: string }>;
}

export interface QueryResult {
  columns: Array<{ name: string; type: string }>;
  rows: unknown[][];
  /** Server-side execution time, shown so a slow query is visibly slow. */
  elapsedMs: number;
  /** More rows exist past this block. */
  truncated: boolean;
}

export const EXPORT_FORMATS = ['csv', 'parquet', 'json', 'jsonl'] as const;
export type ExportFormat = (typeof EXPORT_FORMATS)[number];

function token(): string {
  return (import.meta.env?.VITE_KONTRA_EXPLORE_TOKEN as string | undefined) ?? '';
}

function authHeaders(): Record<string, string> {
  const t = token();
  return t ? { Authorization: `Bearer ${t}` } : {};
}

/**
 * Surface the server's own message.
 *
 * A failed query is nearly always the operator's SQL, and the engine already explains it —
 * including the case where a query outgrew the workbench's memory, where the useful sentence is
 * "narrow it or run it locally". Replacing that with "request failed" would throw away the only
 * part worth reading.
 */
async function detail(res: Response, what: string): Promise<never> {
  let message = `${res.status} ${res.statusText}`;
  try {
    const body = (await res.json()) as { error?: string };
    if (body?.error) message = body.error;
  } catch {
    /* not JSON — keep the status line */
  }
  throw new Error(`${what}: ${message}`);
}

/** Run one block of SQL. `offset` pages; the grid asks for the next block as it scrolls. */
export async function runQuery(
  sql: string,
  opts: { limit?: number; offset?: number } = {}
): Promise<QueryResult> {
  const res = await fetch(`${BASE}/datasets/query`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', ...authHeaders() },
    body: JSON.stringify({ sql, ...opts }),
  });
  if (!res.ok) return detail(res, 'query');
  return (await res.json()) as QueryResult;
}

/** Every dataset's columns. */
export async function fetchSchema(): Promise<SchemaEntry[]> {
  const res = await fetch(`${BASE}/datasets/schema`, { headers: authHeaders() });
  if (!res.ok) return detail(res, 'schema');
  return ((await res.json()) as { datasets: SchemaEntry[] }).datasets;
}

/**
 * Download a query's FULL result — not the page on screen.
 *
 * Fetched as a blob rather than navigated to, because the route needs an Authorization header
 * and a plain `window.open` cannot carry one. The object URL is revoked immediately after the
 * click; leaving them attached is how a long session leaks every file it ever exported.
 */
export async function downloadExport(
  sql: string,
  format: ExportFormat,
  filename: string
): Promise<void> {
  const q = new URLSearchParams({ sql, format, filename });
  const res = await fetch(`${BASE}/datasets/export?${q}`, { headers: authHeaders() });
  if (!res.ok) return detail(res, 'export');
  const url = URL.createObjectURL(await res.blob());
  try {
    const a = document.createElement('a');
    a.href = url;
    a.download = `${filename}.${format}`;
    a.click();
  } finally {
    URL.revokeObjectURL(url);
  }
}
