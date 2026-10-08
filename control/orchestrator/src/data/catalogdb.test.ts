import { describe, it, expect } from 'vitest';
import { catalogTarget } from './catalogdb';

// WHAT THIS PINS. `workspaceAddress` derives `dbname=kontra_ducklake_ws_<name>` per workspace and
// nothing created it: a fresh install's first `publishBatch` failed with
// `FATAL: database "kontra_ducklake_ws_hello" does not exist`, after the actor had run and emitted
// its rows. The parsing below is what decides whether anything gets created at all.

describe('catalogTarget', () => {
  it('reads the database out of a workspace catalog, scheme-adjacent key and all', () => {
    const t = catalogTarget(
      'postgres:dbname=kontra_ducklake_ws_hello host=postgres user=kontra password=kontra'
    );
    expect(t?.dbname).toBe('kontra_ducklake_ws_hello');
    // The maintenance connstring keeps host and credentials and swaps ONLY the database, because
    // `CREATE DATABASE` cannot run inside the database it creates.
    expect(t?.maintenance).toContain('dbname=postgres');
    expect(t?.maintenance).toContain('host=postgres');
    expect(t?.maintenance).not.toContain('kontra_ducklake_ws_hello');
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

  it('keeps a name that would otherwise be read as SQL quotable', () => {
    // `assertWorkspaceName` restricts what reaches here, and the identifier is quoted anyway — so
    // what this checks is that parsing does not SPLIT such a name and silently create another.
    const t = catalogTarget('postgres:dbname=ws_a"b host=postgres');
    expect(t?.dbname).toBe('ws_a"b');
  });
});
