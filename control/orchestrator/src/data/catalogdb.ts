/**
 * catalogdb.ts — making a POSTGRES DuckLake catalog attachable, which is `ensureCatalogDir`'s twin.
 *
 * THE GAP THIS CLOSES. `workspaceAddress` gives every workspace its own catalog database —
 * `dbname=kontra_ducklake_ws_<name>` — and `ensureCatalogDir` says of that case *"a server catalog:
 * it is the server's job to exist"*. Nothing was that server: `postgres-init.sh` creates the base
 * `kontra_ducklake` and no more, so a fresh install's FIRST `publishBatch` failed with
 *
 *     IO Error: Failed to attach DuckLake MetaData "__ducklake_metadata_lake" at
 *     "postgres:dbname=kontra_ducklake_ws_hello host=postgres user=kontra ***"
 *     FATAL: database "kontra_ducklake_ws_hello" does not exist
 *
 * — measured in CI on both the install job and the parity gate, after the actor had run, opened a
 * Session and emitted its rows. An install that cannot publish a Dataset is not installed.
 *
 * WHY HERE AND NOT IN `postgres-init.sh`. That script runs once, before any workspace exists, and
 * workspaces are made later by an operator, by the seed, or by a gate copying a directory into the
 * tree. The name is derived at attach time, so that is the only place that knows it.
 *
 * ONLY WHERE A LAKE IS BEING WRITTEN. This is called beside `ensureCatalogDir`, which the READ_ONLY
 * path deliberately does not call — `parquet.ts` records why: a read of a lake nobody has written
 * must keep failing so `friendlyError` can say "no datasets yet", and creating one as a side effect
 * of reading it "would replace a correct answer with an empty table". Creating an EMPTY DATABASE is
 * the same hazard one level down, so it stays on the write path.
 *
 * IT CREATES A DATABASE AND NEVER TOUCHES ITS CONTENTS. DuckLake owns every table inside; this runs
 * `CREATE DATABASE` and nothing else, so it cannot be the thing that corrupts a lake's metadata.
 */

import { Client } from 'pg';

/** What a `postgres:` DuckLake catalog names: the database to make, and where to make it. */
export interface CatalogTarget {
  dbname: string;
  /** The same connstring with `dbname` swapped for the maintenance database. */
  maintenance: string;
}

/**
 * The maintenance database every Postgres has and nobody stores anything in. `CREATE DATABASE`
 * cannot run inside the database it creates, so the connection has to land somewhere else first.
 */
const MAINTENANCE_DB = 'postgres';

/**
 * Parse a DuckLake catalog into the database it wants, or null when it is not a Postgres one.
 *
 * The `dbname=` boundary includes `:` as well as whitespace for the reason `parquet.ts` gives at its
 * own copy of this regex: the first key sits directly against the scheme (`postgres:dbname=…`), and
 * anchoring on `(^|\s)` alone misses exactly that case.
 */
export function catalogTarget(catalog: string): CatalogTarget | null {
  const s = catalog.trim();
  if (!s.startsWith('postgres:')) return null;
  const body = s.slice('postgres:'.length);
  const m = /(^|[\s])?dbname=(\S+)/.exec(body);
  const dbname = m?.[2];
  if (dbname === undefined || dbname === '') return null;
  if (dbname === MAINTENANCE_DB) return null; // nothing to create
  const maintenance = body.replace(/(^|[\s])dbname=\S+/, `$1dbname=${MAINTENANCE_DB}`);
  return { dbname, maintenance: maintenance === body ? `dbname=${MAINTENANCE_DB} ${body}` : maintenance };
}

/** A Postgres identifier, quoted so a name can never be read as SQL. */
function quoteIdent(name: string): string {
  return `"${name.replace(/"/g, '""')}"`;
}

/**
 * Create the catalog's database if it is absent. A no-op for a file catalog, and for a Postgres one
 * that already exists.
 *
 * FAILURE IS NOT MASKED AND NOT FATAL EITHER. If Postgres cannot be reached, or the role may not
 * create a database, this returns and lets `ATTACH` produce the error — which names the catalog, the
 * host and the user, and is the message an operator can act on. Throwing a second, earlier error
 * about the same condition would only move the report further from the cause.
 */
export async function ensureCatalogDatabase(catalog: string): Promise<void> {
  const target = catalogTarget(catalog);
  if (target === null) return;

  const client = new Client({ connectionString: target.maintenance, connectionTimeoutMillis: 5_000 });
  try {
    await client.connect();
    const existing = await client.query('SELECT 1 FROM pg_database WHERE datname = $1', [target.dbname]);
    if (existing.rowCount === 0) {
      // `CREATE DATABASE` takes no parameters and has no `IF NOT EXISTS`, which is why this is a
      // check-then-create and why the duplicate below is tolerated rather than prevented: two
      // materializer connections opening one workspace's lake at the same moment is ordinary.
      await client.query(`CREATE DATABASE ${quoteIdent(target.dbname)}`);
    }
  } catch {
    // SWALLOWED, AND THE HEADER SAYS WHY. `42P04` (duplicate_database) is the race two materializers
    // opening one workspace's lake can lose, and it is the outcome this wanted anyway; anything else
    // — unreachable host, a role that may not create a database — is a condition `ATTACH` is about
    // to report with the catalog, the host and the user in the message. A second, earlier error
    // about the same thing only moves the report away from the cause.
  } finally {
    await client.end().catch(() => undefined);
  }
}
