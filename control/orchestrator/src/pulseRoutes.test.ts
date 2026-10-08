/**
 * `GET /api/pulse` — the one route the chrome polls from every surface.
 *
 * WHAT THIS PINS THAT `pulse.test.ts` CANNOT. That file is the arithmetic; this is the wiring, and
 * two properties of the wiring are the whole reason the route exists:
 *
 *   IT DOES NOT LIST RUNS. `/api/runs` is the list and costs what a list costs — a page of up to
 *     200 executions, a ledger read per row, a describe per open row, a second visibility scan over
 *     every dispatch on the cluster. The rail used to poll THAT for a two-digit number. If this
 *     route ever grows a call into it the chrome is back to paying run-history prices on the
 *     Datasets page, so `listRuns` is mocked to throw here rather than to return: a regression that
 *     reintroduces it fails loudly instead of merely getting slower.
 *
 *   AN UNREACHABLE TEMPORAL IS A 502, NEVER A ZERO. A badge reading "idle" because the cluster
 *     could not be asked is the failure this install keeps writing defences against.
 */

import { describe, expect, it, vi } from 'vitest';

import { buildServer } from './server';
import { Repo } from './db/repo';
import { ASK_MEMO_PREFIX } from './hitl';
import { OPEN_SCAN_CAP, type PulseDeps } from './pulse';

vi.mock('./temporalClient', async (original) => ({
  ...(await original<typeof import('./temporalClient')>()),
  // THE GUARD, not a stub. Nothing in this route may reach the run list.
  listRuns: vi.fn(async () => {
    throw new Error('the pulse must not list runs');
  }),
}));

const NOW = 1_800_000_000_000;

function server(pulse: Partial<PulseDeps> = {}) {
  return buildServer({
    repo: new Repo(':memory:'),
    webRoot: '',
    pulse: {
      count: async () => 0,
      open: async () => ({ runs: [], capped: false }),
      now: () => NOW,
      ...pulse,
    },
  });
}

describe('GET /api/pulse', () => {
  it('answers an idle cluster with zeros and no error', async () => {
    const app = server();
    try {
      const res = await app.inject({ method: 'GET', url: '/api/pulse' });
      expect(res.statusCode).toBe(200);
      expect(res.json()).toEqual({
        running: 0,
        parked: 0,
        named: [],
        scanned: 0,
        capped: false,
        at: NOW,
      });
    } finally {
      await app.close();
    }
  });

  it('reports a parked run by name, off the memo the LISTING already carried', async () => {
    // No describe anywhere in this path: the visibility record carries the memo, which is what
    // makes "which of the open runs are parked" cost zero extra RPCs.
    const app = server({
      count: async () => 2,
      open: async () => ({
        runs: [
          { runId: 'sweep-a', type: 'NightlySweep', memo: {} },
          {
            runId: 'sweep-b',
            type: 'NightlySweep',
            memo: {
              [`${ASK_MEMO_PREFIX}approve`]: {
                id: 'approve',
                prompt: 'Bring the fleet up?',
                askedAt: NOW - 120_000,
                state: 'pending',
              },
            },
          },
        ],
        capped: false,
      }),
    });
    try {
      const res = await app.inject({ method: 'GET', url: '/api/pulse' });
      expect(res.statusCode).toBe(200);
      expect(res.json()).toMatchObject({
        running: 2,
        parked: 1,
        scanned: 2,
        named: [{ runId: 'sweep-b', workflow: 'NightlySweep', pending: 1, since: NOW - 120_000 }],
      });
    } finally {
      await app.close();
    }
  });

  it('asks for a BOUNDED scan — the cap is the route’s, not the caller’s', async () => {
    // There is no `?limit=` here on purpose. The one thing the chrome must not be able to do is ask
    // this for the whole cluster, which is how a counter becomes a listing again.
    const caps: number[] = [];
    const app = server({
      open: async (cap) => {
        caps.push(cap);
        return { runs: [], capped: false };
      },
    });
    try {
      await app.inject({ method: 'GET', url: '/api/pulse?limit=5000' });
      expect(caps).toEqual([OPEN_SCAN_CAP]);
    } finally {
      await app.close();
    }
  });

  it('502s when Temporal cannot be reached, and never answers "idle"', async () => {
    const app = server({
      count: async () => {
        throw new Error('Failed to connect before the deadline');
      },
    });
    try {
      const res = await app.inject({ method: 'GET', url: '/api/pulse' });
      expect(res.statusCode).toBe(502);
      expect(res.json().error).toMatch(/could not read the pulse/);
      // The shape a zero would have arrived in must be absent, so no browser can mistake the
      // failure for a resting cluster by reading a missing field as 0.
      expect(res.json().running).toBeUndefined();
    } finally {
      await app.close();
    }
  });

  it('never touches the run list', async () => {
    const { listRuns } = await import('./temporalClient');
    const app = server({ count: async () => 3 });
    try {
      const res = await app.inject({ method: 'GET', url: '/api/pulse' });
      expect(res.statusCode).toBe(200);
      expect(listRuns).not.toHaveBeenCalled();
    } finally {
      await app.close();
    }
  });
});
