/**
 * The two things the logs proxy exists to guarantee (ADR 0050, issue #17).
 *
 * Neither is about plumbing. VictoriaLogs has NO AUTHENTICATION — its multi-tenancy is a request
 * header, so the header IS the authorisation decision — and the console sends arbitrary LogsQL. So
 * the proxy has exactly two jobs, and a test that only checked "does it forward" would pass while
 * both were broken.
 */

import { readFileSync } from 'node:fs';

import { describe, describe as test, expect, it } from 'vitest';

import { DEFAULT_LOGS_URL, logsBase, scopedQuery } from './logs';

test('the query a caller sends cannot widen past their tenant', () => {
  /**
   * THE ATTACK THIS IS SHAPED AGAINST. If the scope were concatenated — `caller + ' AND tenant:x'` —
   * then a caller ending their query with `OR *` would bind looser than the AND and match
   * everything. Wrapping the caller's whole expression in parentheses first makes that impossible
   * without any parsing, escaping or allowlist.
   */
  it('parenthesises the caller expression, so a trailing OR cannot escape the AND', () => {
    const q = scopedQuery('level:error OR *', 'acme');
    expect(q).toBe('(level:error OR *) AND _stream:{tenant="acme"}');
    // The caller's text is INSIDE the parens, and the scope is outside them.
    expect(q.indexOf('OR *')).toBeLessThan(q.indexOf('_stream:'));
    expect(q.startsWith('(')).toBe(true);
  });

  it('JSON-quotes the tenant, so a tenant name cannot close the string', () => {
    // Not reachable today (the tenant is the namespace, not user input) and pinned anyway: the
    // moment a tenant name comes from anywhere else, this is the line that has to already be true.
    expect(scopedQuery('*', 'a"b')).toBe('(*) AND _stream:{tenant="a\\"b"}');
  });

  it('treats an empty query as everything IN SCOPE, never as no scope', () => {
    // `*` is LogsQL for everything, and the scope still ANDs on. The failure being excluded is a
    // blank query short-circuiting to an unscoped read.
    for (const empty of [undefined, '', '   ']) {
      expect(scopedQuery(empty, 'acme')).toBe('(*) AND _stream:{tenant="acme"}');
    }
  });

  it('never emits a query without the tenant clause', () => {
    for (const caller of ['*', 'level:error', '_time:5m', ')(', 'AND', '"']) {
      expect(scopedQuery(caller, 'acme')).toContain('_stream:{tenant="acme"}');
    }
  });
});

test('the backend address', () => {
  it('is the compose name, because the service is deliberately unpublished', () => {
    // If this ever becomes a host port, the service has been published — which is the thing
    // `docker-compose.yml` refuses to do for a store with no authentication.
    expect(DEFAULT_LOGS_URL).toBe('http://victorialogs:9428');
    expect(DEFAULT_LOGS_URL).not.toMatch(/127\.0\.0\.1|localhost|0\.0\.0\.0/);
  });

  it('is overridable, so a deployment with its own store re-points one variable', () => {
    const prev = process.env.KONTRA_LOGS_URL;
    process.env.KONTRA_LOGS_URL = 'http://elsewhere:9428';
    try {
      expect(logsBase()).toBe('http://elsewhere:9428');
    } finally {
      if (prev === undefined) delete process.env.KONTRA_LOGS_URL;
      else process.env.KONTRA_LOGS_URL = prev;
    }
  });
});

/*
THE HEAD MUST GO OUT BEFORE THE UPSTREAM IS ASKED.

VictoriaLogs' `/select/logsql/tail` withholds its own headers until it has a line, so a route that
awaits that fetch before `writeHead` sends ZERO BYTES on a quiet fleet. MEASURED with curl against
an idle fleet: 20 seconds, not one byte of header. A browser's `fetch()` does not resolve until
headers arrive, so the console sat in "connecting" — above several hundred successfully backfilled
lines. A working backend rendering as a broken connection is the exact confusion `unreachable()`
exists to prevent, produced by the one route that streams.

This asserts the ORDER by reading the source, because the behaviour needs a hung upstream to
reproduce and a test that needs one is a test nobody keeps.
*/
describe('the tail route commits to the stream before contacting the backend', () => {
  const src = readFileSync(new URL('./logs.ts', import.meta.url), 'utf8');
  // COMMENTS STRIPPED FIRST. The fix is explained in a comment that quotes the code it replaced
  // (`await fetch(...)`), so a naive `indexOf` finds the PROSE before the code and the assertion
  // fails on a file that is correct. A test that reads source has to read only the source.
  const tail = src
    .slice(src.indexOf("app.get('/api/logs/tail'"))
    .split('\n')
    .filter((l) => !l.trim().startsWith('//') && !l.trim().startsWith('*') && !l.trim().startsWith('/*'))
    .join('\n');

  it('writes the SSE head before awaiting the upstream fetch', () => {
    const head = tail.indexOf('writeHead(200');
    const fetchAt = tail.indexOf('await fetch(');
    expect(head).toBeGreaterThan(-1);
    expect(fetchAt).toBeGreaterThan(-1);
    expect(head).toBeLessThan(fetchAt);
  });

  it('flushes a first byte, so a proxy cannot hold the head in a buffer', () => {
    expect(tail).toContain("reply.raw.write(': open\\n\\n')");
  });

  /*
   * ONCE THE HEAD IS WRITTEN THE STATUS IS SPENT. A second `writeHead` throws
   * ERR_HTTP_HEADERS_SENT, which lands in the empty catch beside it and loses the only
   * explanation the client was going to get — so a late failure must be reported IN-BAND.
   */
  it('reports a late backend failure as a frame, never as a second status', () => {
    const afterHead = tail.slice(tail.indexOf('writeHead(200'));
    expect(afterHead).not.toMatch(/writeHead\(\s*(?:502|503)/);
    expect(afterHead).toContain('event: error');
  });
});

/*
SATURATION MUST NOT REPORT ITSELF AS "THE BACKEND IS UNREACHABLE".

`AbortSignal.timeout(QUERY_TIMEOUT_MS)` (10s) and VictoriaLogs' `-search.maxQueueDuration` (10s)
are the same number measuring different things, and both start at the same instant — so when the
select pool is full it is a coin toss which fires. When ours wins, the AbortError reaches the same
catch as ECONNREFUSED.

MEASURED: a browser got a 503 with this sentence while curl answered the identical query in 0.5s
at the same moment, `vl_concurrent_select_current 3` of capacity 4. Every clause was wrong — the
backend was reachable, healthy, and half a second from answering — and the sentence sends an
operator to `docker compose ps` to look at a container that is fine.
*/
describe('a timeout of our own is told apart from a dead backend', () => {
  const src = readFileSync(new URL('./logs.ts', import.meta.url), 'utf8');
  const fn = src.slice(src.indexOf('function unreachable'));

  it('branches on the abort before falling through to the unreachable sentence', () => {
    const branch = fn.indexOf('ourTimeout');
    const sentence = fn.indexOf('is not answering at');
    expect(branch).toBeGreaterThan(-1);
    expect(branch).toBeLessThan(sentence);
  });

  it('recognises both the name and the message, since runtimes disagree', () => {
    expect(fn).toContain("'TimeoutError'");
    expect(fn).toContain("'AbortError'");
    expect(fn).toMatch(/aborted due to timeout/i);
  });

  /*
   * 504 AND NOT 503. They are different claims: 503 says the dependency is unavailable, 504 says
   * an upstream did not answer in time. A caller that retries on 503 and not on 504 — or the
   * reverse — is making a decision this distinction exists to inform.
   */
  it('answers 504 for our deadline and keeps 503 for a genuinely dead backend', () => {
    expect(fn).toContain('reply.code(504)');
    expect(fn).toContain('reply.code(503)');
  });

  /*
   * THE MESSAGE HAS TO NAME THE NEXT ACTION. "It timed out" sends the reader nowhere; the pool
   * counters are what actually distinguishes saturation from a hang, and a live tail holding a
   * slot for its whole life is the non-obvious part.
   */
  it('points at the select-pool counters rather than at the container', () => {
    const timeoutBranch = fn.slice(fn.indexOf('reply.code(504)'), fn.indexOf('reply.code(503)'));
    expect(timeoutBranch).toContain('vl_concurrent_select_current');
    expect(timeoutBranch).toContain('vl_concurrent_select_capacity');
    expect(timeoutBranch).not.toContain('docker compose ps');
  });
});
