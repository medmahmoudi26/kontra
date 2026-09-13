/**
 * IS THIS STATEMENT A READ? — the gate in front of a Postgres-wire front door.
 *
 * The plan is to serve kontra Datasets over the Postgres wire protocol, so `psql`, DBeaver, Metabase
 * and Grafana work against them with no integration on either side. A Dataset is produced by a Run;
 * a SQL client is a READER, and a wire protocol that can write would be a second, unaudited way to
 * change the lake.
 *
 * THIS IS THE WHOLE SECURITY BOUNDARY OF THAT FEATURE, which is why it exists before the transport
 * does and why it is tested far harder than its size suggests. Getting it wrong does not produce a
 * wrong answer — it produces a deleted table.
 *
 * ── WHY A CLASSIFIER AND NOT A PARSER ───────────────────────────────────────────────────────────
 *
 * A full SQL parser would be more precise and is the right answer eventually. This is deliberately
 * conservative instead: anything it cannot confidently call a READ is refused. A false refusal is an
 * operator retyping a query; a false acceptance is data loss. The bias is not symmetric, so the code
 * is not either.
 *
 * ── WHAT ACTUALLY GETS PAST A NAIVE CHECK ───────────────────────────────────────────────────────
 *
 * `sql.trim().toLowerCase().startsWith('select')` admits every one of these:
 *
 *   SELECT 1; DROP TABLE findings          a second statement after the first
 *   WITH x AS (DELETE FROM f RETURNING *) SELECT * FROM x   a CTE that writes
 *   /* comment *\/ DELETE FROM findings    a leading comment
 *   SELECT * FROM t; -- harmless           trailing, but the split still matters
 *   COPY findings TO '/tmp/x'              writes a FILE, not a row
 *   ATTACH 'other.db' AS o                 opens a database this gate never saw
 *   INSTALL httpfs; LOAD httpfs            loads an extension that can reach the network
 *
 * Every one is covered below, and every one has a test.
 */

/** What a statement is allowed to be. */
export type SqlKind = 'read' | 'introspection' | 'write' | 'unknown';

export interface Verdict {
  allowed: boolean;
  kind: SqlKind;
  /** The statement that decided it — the one to show an operator, not the whole batch. */
  statement: string;
  reason: string;
}

/**
 * Strip comments so a statement cannot hide behind one.
 *
 * Handles `--` to end of line and `/* … *\/` blocks, and leaves string literals alone — a `--`
 * INSIDE a quoted string is data, and removing it would corrupt the query this is about to allow.
 */
export function stripComments(sql: string): string {
  let out = '';
  let i = 0;
  let quote: "'" | '"' | null = null;

  while (i < sql.length) {
    const c = sql[i]!;
    const next = sql[i + 1];

    if (quote) {
      out += c;
      // Doubled quote is an escaped quote inside the literal, not the end of it.
      if (c === quote && next === quote) {
        out += next;
        i += 2;
        continue;
      }
      if (c === quote) quote = null;
      i += 1;
      continue;
    }

    if (c === "'" || c === '"') {
      quote = c;
      out += c;
      i += 1;
      continue;
    }
    if (c === '-' && next === '-') {
      while (i < sql.length && sql[i] !== '\n') i += 1;
      continue;
    }
    if (c === '/' && next === '*') {
      i += 2;
      while (i < sql.length && !(sql[i] === '*' && sql[i + 1] === '/')) i += 1;
      i += 2;
      // A block comment can join two tokens; a space keeps `SELEC/**/T` from becoming `SELECT`.
      out += ' ';
      continue;
    }
    out += c;
    i += 1;
  }
  return out;
}

/**
 * Split on `;` — but not inside a string literal.
 *
 * The whole point: a client may legitimately send several statements, and every one of them has to
 * be judged. Splitting naively on `;` would break a query containing `';'` as data and, worse,
 * could hide a statement from the gate.
 */
export function splitStatements(sql: string): string[] {
  const out: string[] = [];
  let cur = '';
  let quote: "'" | '"' | null = null;

  for (let i = 0; i < sql.length; i++) {
    const c = sql[i]!;
    if (quote) {
      cur += c;
      if (c === quote && sql[i + 1] === quote) {
        cur += sql[i + 1];
        i += 1;
        continue;
      }
      if (c === quote) quote = null;
      continue;
    }
    if (c === "'" || c === '"') {
      quote = c;
      cur += c;
      continue;
    }
    if (c === ';') {
      if (cur.trim()) out.push(cur.trim());
      cur = '';
      continue;
    }
    cur += c;
  }
  if (cur.trim()) out.push(cur.trim());
  return out;
}

/** Verbs that change something — data, schema, files, the session's reach. */
const WRITE_VERBS = [
  'insert', 'update', 'delete', 'merge', 'truncate', 'drop', 'create', 'alter', 'replace',
  'grant', 'revoke', 'vacuum', 'reindex', 'cluster', 'refresh', 'call', 'do', 'import',
  // DuckDB-specific, and every one of these is a real escape:
  //   copy    writes a FILE, which is not a row but is still a write
  //   attach  opens another database this gate has never seen
  //   install/load  brings in an extension that can reach the network or the filesystem
  //   export  writes a directory
  'copy', 'attach', 'detach', 'install', 'load', 'export', 'checkpoint', 'begin', 'commit', 'rollback',
];

/** Verbs that read. `pragma` and `show` are introspection, which psql needs to connect at all. */
const READ_VERBS = ['select', 'values', 'table', 'explain'];
const INTROSPECTION_VERBS = ['show', 'describe', 'pragma', 'summarize'];

/** The first word, lowercased. */
function verbOf(statement: string): string {
  return (/^\s*([A-Za-z_]+)/.exec(statement)?.[1] ?? '').toLowerCase();
}

/**
 * A `WITH` can hide a write: `WITH x AS (DELETE … RETURNING *) SELECT * FROM x` is a DELETE that
 * begins with the letters S-E-L… nowhere in sight, and reads as harmless to any first-word check.
 *
 * So a CTE is only a read when NO write verb appears anywhere in it. Conservative on purpose: a
 * column literally named `update_count` will be refused, and that is the right way to be wrong.
 */
function cteIsRead(statement: string): boolean {
  const body = statement.toLowerCase();
  return !WRITE_VERBS.some((v) => new RegExp(`\\b${v}\\b`).test(body));
}

export function classifyStatement(statement: string): SqlKind {
  const verb = verbOf(statement);
  if (!verb) return 'unknown';
  if (WRITE_VERBS.includes(verb)) return 'write';
  if (INTROSPECTION_VERBS.includes(verb)) return 'introspection';
  if (verb === 'with') return cteIsRead(statement) ? 'read' : 'write';
  if (READ_VERBS.includes(verb)) {
    // `EXPLAIN DELETE …` does not run the delete, but allowing it hands out a way to probe for
    // tables and is not worth the argument. Judge what follows.
    if (verb === 'explain') {
      const rest = statement.replace(/^\s*explain\s+(analyze\s+)?/i, '');
      return classifyStatement(rest) === 'read' ? 'read' : 'write';
    }
    return 'read';
  }
  // SET is the one verb a client sends before anything else — `SET extra_float_digits`, timezone,
  // application_name. Session-local, affects nothing durable, and refusing it means psql cannot
  // connect at all.
  if (verb === 'set' || verb === 'reset') return 'introspection';
  return 'unknown';
}

/**
 * Judge a whole request. EVERY statement must be a read; the first that is not decides.
 *
 * Returning the offending statement rather than the batch is deliberate: an operator pasting forty
 * lines needs to know which one was refused.
 */
export function assertReadOnly(sql: string): Verdict {
  const statements = splitStatements(stripComments(sql));
  if (statements.length === 0) {
    return { allowed: false, kind: 'unknown', statement: '', reason: 'no statement to run' };
  }
  for (const statement of statements) {
    const kind = classifyStatement(statement);
    if (kind === 'read' || kind === 'introspection') continue;
    return {
      allowed: false,
      kind,
      statement,
      reason:
        kind === 'write'
          ? 'this connection is read-only — a Dataset is written by a Run, not by a SQL client'
          : 'refused: this statement could not be confidently identified as a read',
    };
  }
  return { allowed: true, kind: 'read', statement: statements[0]!, reason: 'ok' };
}
