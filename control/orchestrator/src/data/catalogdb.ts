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
 * THE CONNSTRING IS libpq's KEYWORD/VALUE FORM AND `pg` DOES NOT READ IT. This is the one thing to
 * know before touching this file. `KONTRA_DUCKLAKE_CATALOG` is
 * `postgres:dbname=… host=… user=… password=…`, which is what DuckDB's ducklake extension wants —
 * and handing it to `new Client({ connectionString })` does not fail loudly, it MIS-PARSES.
 * Measured with `pg-connection-string@2.14.1`:
 *
 *     parse('dbname=postgres host=postgres user=kontra password=kontra')
 *     → { user: '', password: '', host: 'base', port: '', database: 'dbname=postgres host=…' }
 *
 * `host: 'base'`. So the first version of this file dialled a host that does not exist, threw, had
 * its error swallowed by the `catch` below, and created nothing — and CI reported the identical
 * missing-database error one commit later. The fields are parsed here instead.
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

import { runLog } from '../activities/runLog';

/** What a `postgres:` DuckLake catalog names: the database to make, and where to make it. */
export interface CatalogTarget {
  dbname: string;
  /** Where to connect to create it — the same server, the maintenance database. */
  connect: { host?: string; port?: number; user?: string; password?: string; database: string };
}

/**
 * The maintenance database every Postgres has and nobody stores anything in. `CREATE DATABASE`
 * cannot run inside the database it creates, so the connection has to land somewhere else first.
 */
const MAINTENANCE_DB = 'postgres';

/** libpq keyword/value pairs — `k=v k=v`, which is the form DuckLake's catalog is written in. */
function keywords(body: string): Map<string, string> {
  const out = new Map<string, string>();
  for (const pair of body.trim().split(/\s+/)) {
    const i = pair.indexOf('=');
    if (i > 0) out.set(pair.slice(0, i).toLowerCase(), pair.slice(i + 1));
  }
  return out;
}

/**
 * Parse a DuckLake catalog into the database it wants and the server to make it on, or null when it
 * is not a Postgres catalog.
 */
export function catalogTarget(catalog: string): CatalogTarget | null {
  const s = catalog.trim();
  if (!s.startsWith('postgres:')) return null;
  const kv = keywords(s.slice('postgres:'.length));
  const dbname = kv.get('dbname') ?? kv.get('database');
  if (dbname === undefined || dbname === '') return null;
  if (dbname === MAINTENANCE_DB) return null; // nothing to create
  const port = Number.parseInt(kv.get('port') ?? '', 10);
  return {
    dbname,
    connect: {
      host: kv.get('host'),
      port: Number.isFinite(port) ? port : undefined,
      user: kv.get('user'),
      password: kv.get('password'),
      database: MAINTENANCE_DB,
    },
  };
}

/** A Postgres identifier, quoted so a name can never be read as SQL. */
function quoteIdent(name: string): string {
  return `"${name.replace(/"/g, '""')}"`;
}

/**
 * Create the catalog's database if it is absent. A no-op for a file catalog, and for a Postgres one
 * that already exists.
 *
 * NOT FATAL, AND NOT SILENT EITHER — AND THE SECOND HALF OF THAT IS A CORRECTION. `ATTACH` is about
 * to report an unreachable server or a role that may not create a database, with the catalog, the
 * host and the user in the message, so throwing an earlier error about the same condition would
 * only move the report away from its cause. But the first version of this said nothing at all, and
 * what it hid was its own bug: a mis-parsed connstring, a dial to a host called `base`, and a whole
 * CI round reporting the missing database it had just failed to create. So the failure is logged.
 */
export async function ensureCatalogDatabase(catalog: string): Promise<void> {
  const target = catalogTarget(catalog);
  if (target === null) return;

  const client = new Client({ ...target.connect, connectionTimeoutMillis: 5_000 });
  try {
    await client.connect();
    const existing = await client.query('SELECT 1 FROM pg_database WHERE datname = $1', [
      target.dbname,
    ]);
    if (existing.rowCount === 0) {
      // `CREATE DATABASE` takes no parameters and has no `IF NOT EXISTS`, which is why this is a
      // check-then-create and why a duplicate is tolerated rather than prevented: two materializer
      // connections opening one workspace's lake at the same moment is ordinary.
      await client.query(`CREATE DATABASE ${quoteIdent(target.dbname)}`);
      runLog('lake', `created the catalog database ${target.dbname}`, {
        catalog_db: target.dbname,
        host: target.connect.host ?? '',
      });
    }
  } catch (err) {
    const code = (err as { code?: string }).code;
    if (code === '42P04') return; // duplicate_database: the race above, and the outcome it wanted
    // NEVER THE PASSWORD. The message can carry a connstring, so what is logged is the three fields
    // an operator needs and nothing else.
    runLog(
      'lake',
      `could not create the catalog database ${target.dbname} — ATTACH will report what it cannot reach`,
      {
        catalog_db: target.dbname,
        host: target.connect.host ?? '',
        user: target.connect.user ?? '',
        code: code ?? '',
      },
      'warn'
    );
  } finally {
    await client.end().catch(() => undefined);
  }
}
