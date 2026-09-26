import { describe, expect, it } from 'vitest';
import { buildServer } from './server';
import { Repo } from './db/repo';
import type { PollerInfo, QueueDescriber } from './pollers';

/**
 * `GET /api/queues/:queue/pollers` — the third state a workflow can be in.
 *
 * A workflow is RUNNING, or SERVED-AND-IDLE, or NEITHER, and only the first was knowable from this
 * API. "Served" is not a property of a file or of a run: it is whether a worker is polling, which
 * only Temporal can answer. Registration says a workflow exists; a poller says it can run.
 *
 * ZERO AND UNKNOWN ARE DIFFERENT ANSWERS, and most of this file is about keeping them apart. A
 * describe that FAILED is not a queue with no pollers — rendering both as "idle" puts a grey dot on
 * a workflow that is serving perfectly, which is the same defect as a green label over lost work.
 */

const poller = (identity: string, lastAccess = 1_700_000_000_000): PollerInfo => ({
  identity,
  lastAccess,
});

function server(describer: QueueDescriber) {
  return buildServer({
    repo: new Repo(':memory:'),
    webRoot: '',
    queueDescriber: describer,
  });
}

describe('who is polling this queue', () => {
  it('counts DISTINCT identities across workflow and activity pollers', () => {
    // One worker polls BOTH types, so summing the two lists would report every worker twice — and
    // "2 of 4 Machines are serving" is a sentence an operator acts on.
    const describer: QueueDescriber = {
      async pollers(): Promise<PollerInfo[]> {
        return [poller('1@host-a@'), poller('1@host-a@'), poller('2@host-b@')];
      },
    };
    const app = server(describer);
    return app
      .inject({ method: 'GET', url: '/api/queues/nscheck-0.1.0/pollers' })
      .then((res) => {
        expect(res.statusCode).toBe(200);
        expect(res.json()).toMatchObject({
          queue: 'nscheck-0.1.0',
          pollers: 2,
          identities: ['1@host-a@', '2@host-b@'],
        });
        expect(res.json().error).toBeUndefined();
      })
      .finally(() => app.close());
  });

  it('reports a queue nobody serves as zero, with no error', () => {
    // THE IDLE CASE, and it has to be distinguishable from the one below. A workflow that is
    // registered and unserved is exactly what "not serving" means on the Workflows page.
    const app = server({ async pollers() { return []; } });
    return app
      .inject({ method: 'GET', url: '/api/queues/recon/pollers' })
      .then((res) => {
        expect(res.json()).toMatchObject({ queue: 'recon', pollers: 0, identities: [] });
        expect(res.json().error).toBeUndefined();
      })
      .finally(() => app.close());
  });

  it('says UNKNOWN when Temporal cannot be asked, rather than zero', () => {
    // `pollers: 0` is then meaningless and the `error` is what says so. A page that read this as
    // "idle" would report every workflow as unserved for as long as the cluster was unreachable.
    const app = server({
      async pollers(): Promise<PollerInfo[]> {
        throw new Error('Connection refused: localhost:7233');
      },
    });
    return app
      .inject({ method: 'GET', url: '/api/queues/recon/pollers' })
      .then((res) => {
        // 200, not 502: the answer "we do not know" is a real answer and the route always has one.
        expect(res.statusCode).toBe(200);
        expect(res.json().pollers).toBe(0);
        expect(res.json().error).toContain('Connection refused');
      })
      .finally(() => app.close());
  });

  it('drops the count entirely when only HALF the describe answered', () => {
    // `describeQueue` breaks on the first type that errors rather than continuing, because a count
    // from half the evidence reported as a whole one is the failure this signal exists to prevent.
    let call = 0;
    const app = server({
      async pollers(): Promise<PollerInfo[]> {
        call += 1;
        if (call === 1) return [poller('1@host-a@')];
        throw new Error('deadline exceeded');
      },
    });
    return app
      .inject({ method: 'GET', url: '/api/queues/recon/pollers' })
      .then((res) => {
        expect(res.json().pollers).toBe(0);
        expect(res.json().identities).toEqual([]);
        expect(res.json().error).toContain('deadline exceeded');
      })
      .finally(() => app.close());
  });

  it('refuses a queue name that is not one, before dialling anything', () => {
    let asked = false;
    const app = server({
      async pollers(): Promise<PollerInfo[]> {
        asked = true;
        return [];
      },
    });
    return app
      .inject({ method: 'GET', url: '/api/queues/not%20a%20queue/pollers' })
      .then((res) => {
        expect(res.statusCode).toBe(400);
        expect(asked).toBe(false);
      })
      .finally(() => app.close());
  });

  it('reports the freshest poll, so a stale poller reads as stale', () => {
    // A worker that registered and stopped polling still has an identity. The timestamp is what
    // separates "serving" from "was serving".
    const app = server({
      async pollers(): Promise<PollerInfo[]> {
        return [poller('1@a@', 1000), poller('2@b@', 5000)];
      },
    });
    return app
      .inject({ method: 'GET', url: '/api/queues/recon/pollers' })
      .then((res) => expect(res.json().lastPoll).toBe(5000))
      .finally(() => app.close());
  });

  it('dates EACH poller, not just the freshest of them', () => {
    // THE QUEUE-LEVEL TIMESTAMP CANNOT ANSWER "IS THIS WORKER SERVING". A queue with one live
    // worker and one killed three minutes ago reports a fresh `lastPoll` and two identities, and a
    // reader choosing a dispatch target from that list chooses the corpse as readily as the worker.
    // `workers` is what the Actors page reads to keep them apart.
    const app = server({
      async pollers(): Promise<PollerInfo[]> {
        return [poller('1@a@', 1000), poller('2@b@', 5000)];
      },
    });
    return app
      .inject({ method: 'GET', url: '/api/queues/recon/pollers' })
      .then((res) => {
        expect(res.json().workers).toEqual([
          { identity: '1@a@', lastPoll: 1000 },
          { identity: '2@b@', lastPoll: 5000 },
        ]);
        // The two shapes are one fold, so they cannot report different sets of workers.
        expect(res.json().workers.map((w: { identity: string }) => w.identity)).toEqual(
          res.json().identities
        );
      })
      .finally(() => app.close());
  });

  it('keeps ONE date per identity — the freshest — across both queue types', () => {
    // A worker polls WORKFLOW and ACTIVITY on the same queue, and the two are described separately.
    // Taking the last one seen rather than the freshest would age a live worker by whichever list
    // happened to answer second, and ageing is the whole signal.
    let call = 0;
    const app = server({
      async pollers(): Promise<PollerInfo[]> {
        call += 1;
        return call === 1 ? [poller('1@a@', 9000)] : [poller('1@a@', 3000)];
      },
    });
    return app
      .inject({ method: 'GET', url: '/api/queues/recon/pollers' })
      .then((res) => {
        expect(res.json().pollers).toBe(1);
        expect(res.json().workers).toEqual([{ identity: '1@a@', lastPoll: 9000 }]);
      })
      .finally(() => app.close());
  });

  it('drops the per-poller dates too when the describe failed', () => {
    // Same rule as `identities`: a half-fold is not a smaller fold, it is an unknown one, and a
    // `workers` list surviving an error would be the one place the count came back to life.
    const app = server({
      async pollers(): Promise<PollerInfo[]> {
        throw new Error('deadline exceeded');
      },
    });
    return app
      .inject({ method: 'GET', url: '/api/queues/recon/pollers' })
      .then((res) => {
        expect(res.json().workers).toEqual([]);
        expect(res.json().error).toContain('deadline exceeded');
      })
      .finally(() => app.close());
  });
});
