/**
 * Who may reach the infra surface — the first test this surface has ever had.
 *
 * The file it tests opens by claiming it has been "TOKEN-GATED FROM THE FIRST COMMIT, WITHOUT
 * EXCEPTION". That was false: `checkBearer` admitted any live console session BEFORE it consulted
 * `INFRA_ROUTE_TOKEN_VARS`, so one browser credential reached `POST /api/infra/stacks/:fqn/:op` —
 * the route that converges a Pulumi stack and spends money — on an install with no
 * `KONTRA_STATE_TOKEN` configured at all.
 *
 * Four comments and an ADR asserted the invariant the code did not hold. These are the assertions
 * that make it true.
 */

import Fastify, { type FastifyInstance } from 'fastify';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { CONSOLE_SCOPE, INFRA_SCOPE, sessions } from './auth/session';
import { registerInfraRoutes, INFRA_ROUTE_TOKEN_VARS } from './infraRoutes';

const STATE_VAR = INFRA_ROUTE_TOKEN_VARS[0];
const TOKEN = 'a-service-token-nobody-guesses';

/** The one route on this surface that mutates: it starts a stack op. */
const MUTATING = { method: 'POST' as const, url: '/api/infra/stacks/kontra-fleet%2Fdemo/up' };

/** The read side, which the console's Infra page and the run page's Fleet strip depend on. */
const READS = [
  '/api/infra/ops/kontra-fleet%2Fdemo',
  '/api/infra/stacks',
  '/api/infra/stacks/kontra-fleet%2Fdemo/state',
  '/api/infra/stacks/kontra-fleet%2Fdemo/history',
  '/api/infra/stacks/kontra-fleet%2Fdemo/leases',
];

let app: FastifyInstance;
let minted: string[] = [];

function session(scopes?: readonly string[]): string {
  const s = scopes ? sessions.mint('operator', scopes) : sessions.mint('operator');
  minted.push(s.token);
  return `Bearer ${s.token}`;
}

beforeEach(async () => {
  app = Fastify();
  registerInfraRoutes(app);
  await app.ready();
});

afterEach(async () => {
  await app.close();
  // The book is a module singleton, so a session left behind would admit the next test's requests.
  for (const t of minted) sessions.revoke(t);
  minted = [];
  delete process.env[STATE_VAR];
});

describe('a console session cannot spend money', () => {
  it('is refused on the mutating route', async () => {
    process.env[STATE_VAR] = TOKEN;
    const res = await app.inject({ ...MUTATING, headers: { authorization: session() } });
    expect(res.statusCode).toBe(403);
    expect(res.json().error).toMatch(/infra/);
  });

  /**
   * 403 AND NOT 401, and the distinction is not pedantry: the console clears its token on a 401
   * (`packages/core/src/run/session.ts`), so answering a scope refusal with 401 would sign the
   * operator out of the whole console and read to them as "the orchestrator restarted".
   */
  it('is told it lacks a scope, not that it is unauthenticated', async () => {
    process.env[STATE_VAR] = TOKEN;
    const res = await app.inject({ ...MUTATING, headers: { authorization: session() } });
    expect(res.statusCode).not.toBe(401);
    expect(res.statusCode).toBe(403);
  });

  /**
   * THE CASE THAT WAS ACTUALLY EXPLOITABLE. With no service token configured at all, the route
   * used to admit a session outright — there was nothing for it to be compared against.
   */
  it('is refused even when no service token is configured, which is when it used to get in', async () => {
    delete process.env[STATE_VAR];
    const res = await app.inject({ ...MUTATING, headers: { authorization: session() } });
    expect(res.statusCode).toBe(403);
  });

  it('a session carrying the infra scope is admitted, so the refusal is about the scope', async () => {
    process.env[STATE_VAR] = TOKEN;
    const res = await app.inject({
      ...MUTATING,
      headers: { authorization: session([CONSOLE_SCOPE, INFRA_SCOPE]) },
    });
    expect([401, 403]).not.toContain(res.statusCode);
  });

  /** No mint path grants it, which is what keeps `infra` a service-token capability. */
  it('signing in never grants the infra scope', () => {
    const s = sessions.mint('operator');
    minted.push(s.token);
    expect(s.scopes).toEqual([CONSOLE_SCOPE]);
    expect(s.scopes).not.toContain(INFRA_SCOPE);
  });
});

describe('the service token still works, and nothing else does', () => {
  it('admits the configured state token on the mutating route', async () => {
    process.env[STATE_VAR] = TOKEN;
    const res = await app.inject({ ...MUTATING, headers: { authorization: `Bearer ${TOKEN}` } });
    expect([401, 403]).not.toContain(res.statusCode);
  });

  it('503s with no credential configured and none sent, naming the variable', async () => {
    delete process.env[STATE_VAR];
    const res = await app.inject(MUTATING);
    expect(res.statusCode).toBe(503);
    expect(res.json().error).toContain(STATE_VAR);
  });

  it('401s a wrong token', async () => {
    process.env[STATE_VAR] = TOKEN;
    const res = await app.inject({ ...MUTATING, headers: { authorization: 'Bearer nope' } });
    expect(res.statusCode).toBe(401);
  });
});

/**
 * The read side is deliberately NOT scoped. Reading which Machines exist is not the capability that
 * was leaking, and gating it would 403 the console's Infra page and the run page's Fleet strip —
 * both documented browser clients of exactly these paths.
 */
describe('the read side stays reachable by the console', () => {
  it('covers every read route', () => {
    expect(READS).toHaveLength(5); // a loop over an empty list would pass every assertion below
  });

  for (const url of READS) {
    it(`admits a plain console session on ${url}`, async () => {
      process.env[STATE_VAR] = TOKEN;
      const res = await app.inject({ method: 'GET', url, headers: { authorization: session() } });
      expect([401, 403]).not.toContain(res.statusCode);
    });
  }
});
