/**
 * The read-only gate, attacked rather than demonstrated.
 *
 * Getting this wrong does not produce a wrong answer — it produces a deleted table. So the suite is
 * mostly the bypasses a naive `startsWith('select')` admits, each written as the thing somebody
 * would actually send.
 */

import { describe, expect, it } from 'vitest';

import { assertReadOnly, assertSingleRead, classifyStatement, splitStatements, stripComments } from './readonlySql';

const allowed = (sql: string) => assertReadOnly(sql).allowed;

describe('ordinary reads are allowed', () => {
  it.each([
    'SELECT 1',
    'select * from findings where host = $1',
    'SELECT * FROM findings ORDER BY id LIMIT 10',
    'VALUES (1), (2)',
    'TABLE findings',
    'WITH recent AS (SELECT * FROM findings) SELECT count(*) FROM recent',
    'EXPLAIN SELECT * FROM findings',
    '  \n  SELECT 1  \n ',
    'SELECT * FROM findings; -- a trailing comment',
  ])('%s', (sql) => {
    expect(allowed(sql), sql).toBe(true);
  });
});

describe('a client must be able to connect', () => {
  it.each(['SET extra_float_digits = 3', "SET application_name = 'psql'", 'SHOW search_path', 'RESET ALL'])(
    '%s',
    (sql) => {
      // psql sends these before anything else. Refusing them means it cannot connect at all, and
      // they are session-local — nothing durable changes.
      expect(allowed(sql), sql).toBe(true);
    }
  );
});

describe('the bypasses a naive check admits', () => {
  it('a second statement after a harmless first', () => {
    const v = assertReadOnly('SELECT 1; DROP TABLE findings');
    expect(v.allowed).toBe(false);
    expect(v.kind).toBe('write');
    // The OFFENDING statement, not the batch: an operator pasting forty lines needs to know which.
    expect(v.statement).toMatch(/DROP TABLE findings/i);
  });

  it('a CTE that writes', () => {
    // Begins with WITH, ends with SELECT, and deletes everything in between.
    expect(allowed('WITH x AS (DELETE FROM findings RETURNING *) SELECT * FROM x')).toBe(false);
    expect(allowed('WITH x AS (INSERT INTO f VALUES (1) RETURNING *) SELECT * FROM x')).toBe(false);
    expect(allowed('WITH x AS (UPDATE f SET a = 1 RETURNING *) SELECT * FROM x')).toBe(false);
  });

  it('a statement hidden behind a leading comment', () => {
    expect(allowed('/* harmless */ DELETE FROM findings')).toBe(false);
    expect(allowed('-- nothing to see\nDROP TABLE findings')).toBe(false);
  });

  it('a comment that joins two tokens', () => {
    // `SELEC/**/T` must not become `SELECT` when the comment is removed.
    expect(stripComments('SELEC/**/T 1')).toBe('SELEC T 1');
  });

  it('DuckDB verbs that are writes without looking like it', () => {
    // COPY writes a FILE. ATTACH opens a database this gate never saw. INSTALL/LOAD bring in an
    // extension that can reach the network and the filesystem.
    expect(allowed("COPY findings TO '/tmp/leak.csv'")).toBe(false);
    expect(allowed("ATTACH 'other.db' AS o")).toBe(false);
    expect(allowed('INSTALL httpfs')).toBe(false);
    expect(allowed('LOAD httpfs')).toBe(false);
    expect(allowed("EXPORT DATABASE '/tmp/dump'")).toBe(false);
  });

  it('EXPLAIN over a write', () => {
    expect(allowed('EXPLAIN DELETE FROM findings')).toBe(false);
    expect(allowed('EXPLAIN ANALYZE DROP TABLE findings')).toBe(false);
  });

  it('a transaction, which a read-only connection has no use for', () => {
    expect(allowed('BEGIN')).toBe(false);
    expect(allowed('COMMIT')).toBe(false);
  });
});

describe('string literals are data, not syntax', () => {
  it('a semicolon inside a literal does not split the statement', () => {
    const parts = splitStatements("SELECT ';' AS s");
    expect(parts).toHaveLength(1);
    expect(allowed("SELECT ';' AS s")).toBe(true);
  });

  it('a comment marker inside a literal is not a comment', () => {
    // Removing it would corrupt the very query this is about to allow.
    expect(stripComments("SELECT '-- not a comment' AS s")).toBe("SELECT '-- not a comment' AS s");
    expect(allowed("SELECT '-- not a comment' AS s")).toBe(true);
  });

  it('a doubled quote is an escape, not the end of the literal', () => {
    expect(splitStatements("SELECT 'it''s; fine'")).toHaveLength(1);
  });

  it('the word DELETE inside a literal is still refused', () => {
    // CONSERVATIVE ON PURPOSE. This is a false refusal, and the bias is not symmetric: a refused
    // read costs an operator a retype, an accepted write costs a table.
    expect(allowed("SELECT 'DELETE' AS word")).toBe(true); // a plain SELECT is judged by its verb
    expect(allowed("WITH x AS (SELECT 'delete' AS w) SELECT * FROM x")).toBe(false);
  });
});

describe('nothing is allowed by default', () => {
  it('refuses an empty request', () => {
    const v = assertReadOnly('   ');
    expect(v.allowed).toBe(false);
    expect(v.reason).toContain('no statement');
  });

  it('refuses a verb it does not know', () => {
    const v = assertReadOnly('FROBNICATE findings');
    expect(v.allowed).toBe(false);
    expect(v.kind).toBe('unknown');
    expect(v.reason).toContain('could not be confidently identified');
  });

  it('classifies every known write verb as a write', () => {
    const verbs = ['insert', 'update', 'delete', 'drop', 'create', 'alter', 'truncate', 'grant'];
    for (const v of verbs) {
      expect(classifyStatement(`${v} something`), v).toBe('write');
    }
    // Non-vacuous: the same loop over reads must come back 'read'.
    for (const v of ['select', 'values', 'table']) {
      expect(classifyStatement(`${v} x`), v).toBe('read');
    }
  });

  it('the refusal explains itself in the operator', () => {
    const v = assertReadOnly('DELETE FROM findings');
    expect(v.reason).toContain('written by a Run');
  });
});

/**
 * THE WORKBENCH GATE. Each payload below was named in the hardening spec, and the first four were
 * measured landing on the live connection before this gate existed — one of them wrote an object to
 * the object store through `POST /api/datasets/query`.
 */
describe('assertSingleRead — exactly one read', () => {
  const refused = (sql: string) => assertSingleRead(sql).allowed === false;

  it('refuses a second statement, which is how a wrapped query becomes a write', () => {
    expect(refused('SELECT 1; DROP TABLE findings')).toBe(true);
  });

  /**
   * THE BREAKOUT THE WRAPPER MADE POSSIBLE. Every sink composes `SELECT * FROM (<text>) LIMIT n`,
   * so a text that closes the parenthesis gets to write whatever follows.
   */
  it("refuses the ') ; COPY (' breakout", () => {
    expect(refused("SELECT 1) ; COPY (SELECT 1) TO 's3://elsewhere/leak.csv' --")).toBe(true);
  });

  it('refuses COPY … TO on its own', () => {
    expect(refused("COPY (SELECT 1) TO 's3://elsewhere/leak.csv'")).toBe(true);
  });

  it('refuses ATTACH', () => {
    expect(refused("ATTACH 'other.db' AS o")).toBe(true);
  });

  /** `PRAGMA x=y` is a SET alias, and SET is a write primitive dressed as configuration. */
  it('refuses a PRAGMA that assigns', () => {
    expect(refused("PRAGMA profiling_output='/tmp/leak'")).toBe(true);
    expect(refused("PRAGMA enable_profiling='json'")).toBe(true);
  });

  it('refuses a SET that is not part of the client handshake', () => {
    expect(refused("SET profiling_output='/tmp/leak'")).toBe(true);
    expect(refused("SET s3_access_key_id='theirs'")).toBe(true);
    expect(refused('SET disabled_filesystems=\'\'')).toBe(true);
  });

  it('still admits the handshake a SQL client cannot connect without', () => {
    for (const sql of ['SET extra_float_digits = 3', "SET application_name='psql'", 'RESET ALL']) {
      expect(assertSingleRead(sql).allowed, sql).toBe(true);
    }
  });

  it('admits one ordinary read, and introspection, which is what a workbench is for', () => {
    for (const sql of ['SELECT * FROM findings LIMIT 10', 'SHOW TABLES', 'DESCRIBE findings']) {
      expect(assertSingleRead(sql).allowed, sql).toBe(true);
    }
  });

  it('names the offending statement rather than the whole batch', () => {
    const v = assertSingleRead('SELECT 1; DROP TABLE findings');
    expect(v.statement).toContain('DROP');
  });
});
