import { describe, it, expect } from 'vitest';
import { catalogTarget } from './catalogdb';

// WHAT THIS PINS, AND THE SECOND ASSERTION IS THE ONE THAT COST A CI ROUND.
//
// `workspaceAddress` derives `dbname=kontra_ducklake_ws_<name>` per workspace and nothing created
// it: a fresh install's first `publishBatch` failed with
// `FATAL: database "kontra_ducklake_ws_hello" does not exist`, after the actor had run and emitted
// its rows. The first fix handed the catalog to `new Client({ connectionString })` — and libpq's
// keyword/value form is NOT what `pg` reads. Measured with pg-connection-string@2.14.1:
//
//   parse('dbname=postgres host=postgres user=kontra password=kontra')
//   → { user: '', password: '', host: 'base', port: '', database: 'dbname=postgres host=…' }
//
// So it dialled a host called `base`, threw, and the error was swallowed. These tests assert the
// FIELDS, because that is the thing that was wrong and nothing else would have caught it.

describe('catalogTarget', () => {
  const catalog =
    'postgres:dbname=kontra_ducklake_ws_hello host=postgres user=kontra password=kontra';

  it('reads the database out of a workspace catalog, scheme-adjacent key and all', () => {
    expect(catalogTarget(catalog)?.dbname).toBe('kontra_ducklake_ws_hello');
  });

  it('parses the libpq fields rather than handing the string to pg', () => {
    const t = catalogTarget(catalog);
    expect(t?.connect.host).toBe('postgres');
    expect(t?.connect.user).toBe('kontra');
    expect(t?.connect.password).toBe('kontra');
    // The maintenance database, because CREATE DATABASE cannot run inside the one it creates.
    expect(t?.connect.database).toBe('postgres');
  });

  it('takes a port when the catalog names one, and leaves it to pg otherwise', () => {
    expect(catalogTarget('postgres:dbname=x host=h port=6543')?.connect.port).toBe(6543);
    expect(catalogTarget('postgres:dbname=x host=h')?.connect.port).toBeUndefined();
    // A non-numeric port is not a port; pg's default is a better answer than NaN.
    expect(catalogTarget('postgres:dbname=x host=h port=abc')?.connect.port).toBeUndefined();
  });

  it('accepts `database=` as well, which libpq treats as the same keyword', () => {
    expect(catalogTarget('postgres:database=ws_a host=h')?.dbname).toBe('ws_a');
  });

  it('is null for a file catalog, which `ensureCatalogDir` already handles', () => {
    expect(catalogTarget('/var/lib/kontra/data/datasets.ducklake')).toBeNull();
    expect(catalogTarget('')).toBeNull();
  });

  it('is null for the maintenance database itself, which there is nothing to create', () => {
    expect(catalogTarget('postgres:dbname=postgres host=postgres user=kontra')).toBeNull();
  });

  it('is null when a postgres catalog names no database', () => {
    // libpq would fall back to the user's name; guessing which database an install meant is not
    // this function's business, and ATTACH will say what it could not reach.
    expect(catalogTarget('postgres:host=postgres user=kontra')).toBeNull();
  });
});
