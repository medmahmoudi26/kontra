import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { FastifyInstance } from 'fastify';
import { buildServer } from './server';
import { Repo } from './db/repo';

vi.mock('./temporalClient', () => ({
  runWorkflowId: (runId: string) => `orch-${runId}`,
  startRun: vi.fn(),
  getRunStatus: vi.fn(),
  getRunOutput: vi.fn(),
  listExecutions: vi.fn(),
  describeRunHeartbeats: vi.fn(),
}));

/**
 * Guard tests for /api/state/*.
 *
 * These cover the gate rather than the read: every case here must be rejected BEFORE a Redis
 * connection is attempted, which is also why they need no Redis. That ordering is the property
 * worth pinning — an endpoint that authenticates after it has already fetched the data is not
 * authenticated, and a bounds check that runs after the scan has not bounded anything.
 *
 * KONTRA_REDIS_HOST is deliberately left unset for the whole file so that any regression which
 * moves a check below the reader shows up as a 503 (or a hang) instead of quietly passing.
 */
const TOKEN = 'test-token-0123456789abcdef';

describe('GET /api/state/:tier — access control', () => {
  let app: FastifyInstance;

  beforeEach(() => {
    process.env.KONTRA_STATE_TOKEN = TOKEN;
    delete process.env.KONTRA_REDIS_HOST;
    app = buildServer({ repo: new Repo(':memory:'), webRoot: '' });
  });
  afterEach(async () => {
    delete process.env.KONTRA_STATE_TOKEN;
    await app.close();
  });

  const get = (url: string, token?: string) =>
    app.inject({
      method: 'GET',
      url,
      headers: token ? { authorization: `Bearer ${token}` } : {},
    });

  it('401s with no credentials', async () => {
    expect((await get('/api/state/global?actor=cachebuster')).statusCode).toBe(401);
  });

  it('401s on a wrong token', async () => {
    expect((await get('/api/state/global?actor=cachebuster', 'wrong')).statusCode).toBe(401);
  });

  it('401s on a correct-length but wrong token', async () => {
    const near = `${TOKEN.slice(0, -1)}X`;
    expect((await get('/api/state/global?actor=cachebuster', near)).statusCode).toBe(401);
  });

  it('does not leak whether the actor exists to an unauthenticated caller', async () => {
    const real = await get('/api/state/global?actor=cachebuster');
    const fake = await get('/api/state/global?actor=does-not-exist');
    expect(real.statusCode).toBe(401);
    expect(fake.statusCode).toBe(401);
    expect(real.body).toBe(fake.body);
  });

  it('503s rather than 401s when the token is not configured at all', async () => {
    delete process.env.KONTRA_STATE_TOKEN;
    const res = await get('/api/state/global?actor=a', TOKEN);
    // "Disabled" and "denied" must not look the same to an operator debugging provisioning.
    expect(res.statusCode).toBe(503);
  });

  describe('with a valid token', () => {
    it('400s on an unknown tier', async () => {
      const res = await get('/api/state/wat?actor=a&entity=e', TOKEN);
      expect(res.statusCode).toBe(400);
      expect(res.json().error).toMatch(/unknown tier/);
    });

    it('400s without an actor', async () => {
      expect((await get('/api/state/global', TOKEN)).statusCode).toBe(400);
    });

    it.each(['actor'])('400s without an entity for tier %s', async (tier) => {
      const res = await get(`/api/state/${tier}?actor=a`, TOKEN);
      expect(res.statusCode).toBe(400);
      expect(res.json().error).toMatch(/requires an entity/);
    });

    it('does not require an entity for global', async () => {
      // Reaches the reader and 503s on the unset host — i.e. it got PAST validation, which is
      // exactly what distinguishes this from the 400 cases above.
      const res = await get('/api/state/global?actor=a', TOKEN);
      expect(res.statusCode).toBe(503);
      expect(res.json().error).toMatch(/KONTRA_REDIS_HOST/);
    });
  });
});
