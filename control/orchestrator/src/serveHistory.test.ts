/**
 * `listServes` — the SHOWING half of serve-dev, driven against a fake cluster.
 *
 * ── WHAT WAS MISSING ───────────────────────────────────────────────────────────────────────────
 *
 * Pressing Serve starts a `serveDevWorkflow` execution, and `visibility.ts` keeps that type off the
 * Runs page on purpose — it is kontra's own infrastructure, not a caller's Run, and a wall of these
 * would bury what somebody actually ran. But hiding was ALL there was. Nothing in this control
 * plane answered "when was this folder served, and did it work", so a serve that died on an import
 * error left a button that looked pressed, an actor nothing was polling, and the sentence
 * explaining it held only in an execution no surface listed.
 *
 * ── WHY IT IS ITS OWN FILE, AND NOT PART OF `sourceRoutes.test.ts` ─────────────────────────────
 *
 * The same reason `runDiscovery.test.ts` is its own file: the route tests `vi.mock` the whole
 * Temporal module, so everything interesting about the read — the query it narrows with, the
 * ordering, the cap, and the second RPC that fetches a failure's sentence — is mocked away there.
 * These drive the REAL `listServes` against a fake `@temporalio/client`, which is the only place
 * those four can be pinned.
 *
 * The fake visibility filter understands exactly the clause forms this codebase builds and refuses
 * anything else, so a query that grows a clause fails loudly here instead of quietly matching
 * everything — and, like the real SQLite backend, it does not understand `ORDER BY`.
 */

import { beforeEach, describe, expect, it, vi } from 'vitest';

/** One execution the fake cluster holds, under some workflow id. */
interface FakeExecution {
  workflowId: string;
  type: string;
  runId: string;
  status: string;
  startTime: Date;
  closeTime?: Date;
  /** The CLOSE event this execution's history ends on, if a test gives it one. */
  close?: Record<string, unknown>;
}

const cluster: FakeExecution[] = [];
/** Every query string sent, so a test can assert on the query and not only on its result. */
const queries: string[] = [];
/** Every `{workflowId, runId}` a close-event read asked for — this is the fan-out under test. */
const historyReads: Array<{ workflowId: string; runId?: string }> = [];

function evaluate(query: string): FakeExecution[] {
  return cluster.filter((e) =>
    query.split(' AND ').every((clause) => {
      const c = clause.trim();
      let m = /^WorkflowType = '(.*)'$/.exec(c);
      if (m) return e.type === m[1];
      m = /^WorkflowId = '(.*)'$/.exec(c);
      // The same un-escaping the server does: `''` inside a literal is one quote.
      if (m) return e.workflowId === (m[1] ?? '').replace(/''/g, "'");
      throw new Error(`fake visibility cannot parse clause: ${c}`);
    })
  );
}

vi.mock('@temporalio/client', () => {
  class Client {
    // The namespace the real client was built for, which every raw RPC is addressed to.
    options: { namespace: string };
    constructor(opts?: { namespace?: string }) {
      this.options = { namespace: opts?.namespace ?? 'default' };
    }
    workflow = {
      // eslint-disable-next-line require-yield
      async *list({ query }: { query: string }) {
        queries.push(query);
        // Newest first, as Temporal returns by default — which is what makes the cap below take
        // the newest rows rather than an arbitrary page.
        const hits = [...evaluate(query)].sort(
          (a, b) => b.startTime.getTime() - a.startTime.getTime()
        );
        for (const e of hits) {
          yield {
            workflowId: e.workflowId,
            runId: e.runId,
            type: e.type,
            status: { name: e.status },
            startTime: e.startTime,
            closeTime: e.closeTime,
            typedSearchAttributes: { get: () => undefined },
          };
        }
      },
      getHandle() {
        throw new Error('listServes must not describe an execution to list it');
      },
    };
    workflowService = {
      async getWorkflowExecutionHistory({
        execution,
        historyEventFilterType,
      }: {
        execution: { workflowId: string; runId?: string };
        historyEventFilterType?: number;
      }) {
        // 2 = CLOSE_EVENT. Asserted rather than ignored: a read that forgot the filter would page
        // a whole history to find its last event, which is the cost this path exists to avoid.
        if (historyEventFilterType !== 2) {
          throw new Error(`the close-event read must ask for CLOSE_EVENT, got ${historyEventFilterType}`);
        }
        historyReads.push({ workflowId: execution.workflowId, runId: execution.runId });
        const hit = cluster.find((e) => e.runId === execution.runId);
        return { history: { events: hit?.close ? [hit.close] : [] } };
      },
    };
    connection = {};
  }
  return {
    Client,
    Connection: { connect: async () => ({ operatorService: { addSearchAttributes: async () => {} } }) },
    defaultPayloadConverter: {},
  };
});

vi.mock('./otel', () => ({ startTracing: () => {}, tracingEnabled: false }));
vi.mock('./codec/dataConverter', () => ({ dataConverter: {} }));

/** The folder every test below serves, and the workflow id its serves run under. */
const FOLDER = '/w/actors/desync';
const SOURCE_ID = `at:${FOLDER}`;
const WORKFLOW_ID = `serve-dev/${SOURCE_ID}`;

const at = (s: number) => new Date(s * 1000);

function serve(
  runId: string,
  status: string,
  startedAt: number,
  opts: { closedAt?: number; close?: Record<string, unknown>; workflowId?: string } = {}
): FakeExecution {
  return {
    workflowId: opts.workflowId ?? WORKFLOW_ID,
    type: 'serveDevWorkflow',
    runId,
    status,
    startTime: at(startedAt),
    ...(opts.closedAt === undefined ? {} : { closeTime: at(opts.closedAt) }),
    ...(opts.close ? { close: opts.close } : {}),
  };
}

beforeEach(() => {
  cluster.length = 0;
  queries.length = 0;
  historyReads.length = 0;
});

describe('listServes scopes to ONE folder', () => {
  it('asks by the workflow id the serve verb starts under', async () => {
    const { listServes } = await import('./temporalClient');
    await listServes(SOURCE_ID);
    expect(queries).toEqual([
      `WorkflowType = 'serveDevWorkflow' AND WorkflowId = '${WORKFLOW_ID}'`,
    ]);
  });

  it("does not show another folder's serves", async () => {
    // THE FAILURE THIS CATCHES IS AN INVISIBLE ONE. A query that lost its `WorkflowId` clause still
    // returns rows, still sorts, still renders — as one actor's history containing every serve on
    // the cluster. Nothing on screen would look wrong.
    const { listServes } = await import('./temporalClient');
    cluster.push(
      serve('mine', 'COMPLETED', 200, { closedAt: 210 }),
      serve('theirs', 'COMPLETED', 300, { closedAt: 310, workflowId: 'serve-dev/at:/w/actors/other' })
    );

    const { serves } = await listServes(SOURCE_ID);

    expect(serves.map((s) => s.execId)).toEqual(['mine']);
  });

  it('is empty, not absent, for a folder nobody has ever served', async () => {
    const { listServes } = await import('./temporalClient');
    expect(await listServes(SOURCE_ID)).toEqual({ serves: [], capped: false });
  });
});

describe('listServes orders and bounds what it returns', () => {
  it('is newest first — the last serve is the one explaining the state on screen', async () => {
    const { listServes } = await import('./temporalClient');
    cluster.push(
      serve('old', 'COMPLETED', 100, { closedAt: 110 }),
      serve('new', 'COMPLETED', 900, { closedAt: 910 }),
      serve('mid', 'COMPLETED', 500, { closedAt: 510 })
    );

    const { serves } = await listServes(SOURCE_ID);

    expect(serves.map((s) => s.execId)).toEqual(['new', 'mid', 'old']);
    // Sorted HERE and not with a visibility `ORDER BY`: SQLite visibility rejects that outright,
    // which is why the fake above refuses to parse one.
    expect(queries[0]).not.toMatch(/ORDER BY/i);
  });

  it('says the cap bit rather than serving a slice that reads as the whole history', async () => {
    // A silent slice is the failure: "this actor was served twice" is a different claim from "here
    // are the last two of many", and only one of them is true.
    const { listServes } = await import('./temporalClient');
    for (let i = 0; i < 5; i++) cluster.push(serve(`e${i}`, 'COMPLETED', 100 + i, { closedAt: 200 }));

    const { serves, capped } = await listServes(SOURCE_ID, 2);

    expect(serves).toHaveLength(2);
    expect(capped).toBe(true);
    // The cap takes the NEWEST two, because Temporal pages newest-first by default.
    expect(serves.map((s) => s.execId)).toEqual(['e4', 'e3']);
  });

  it('does not claim a cap it did not hit', async () => {
    const { listServes } = await import('./temporalClient');
    cluster.push(serve('only', 'COMPLETED', 100, { closedAt: 110 }));
    expect((await listServes(SOURCE_ID, 5)).capped).toBe(false);
  });

  it('clamps a caller asking for more than the scan limit', async () => {
    // The close-event read below is one RPC per failed row, so this bound is a fan-out bound and
    // not merely a page size — a `?limit=100000` from a browser must not become 100,000 round trips.
    const { listServes, SERVE_SCAN_LIMIT } = await import('./temporalClient');
    for (let i = 0; i < SERVE_SCAN_LIMIT + 5; i++) {
      cluster.push(serve(`e${i}`, 'COMPLETED', 100 + i, { closedAt: 500 }));
    }

    const { serves, capped } = await listServes(SOURCE_ID, 100_000);

    expect(serves).toHaveLength(SERVE_SCAN_LIMIT);
    expect(capped).toBe(true);
  });
});

/**
 * WHY A SERVE FAILED IS THE WHOLE REASON TO KEEP THE HISTORY.
 *
 * `status` cannot carry it: `mapStatus` collapses FAILED, TERMINATED and TIMED_OUT into the single
 * word `failed`, and behind those three are "the CLI exited non-zero and here is its stderr",
 * "somebody killed it", and "the two-minute budget ran out while the image was pulling". Three
 * different afternoons.
 */
describe('listServes explains a failure', () => {
  it('reads the innermost sentence, not the SDK wrapper around it', async () => {
    const { listServes } = await import('./temporalClient');
    cluster.push(
      serve('boom', 'FAILED', 100, {
        closedAt: 130,
        close: {
          workflowExecutionFailedEventAttributes: {
            failure: {
              message: 'Activity task failed',
              cause: {
                message: "serve-dev failed (exit 1): ModuleNotFoundError: No module named 'httpx'",
              },
            },
          },
        },
      })
    );

    const { serves } = await listServes(SOURCE_ID);

    expect(serves[0]?.status).toBe('failed');
    expect(serves[0]?.failure).toBe(
      "Activity task failed: serve-dev failed (exit 1): ModuleNotFoundError: No module named 'httpx'"
    );
  });

  it('tells a timeout apart from a crash, which the status cannot', async () => {
    const { listServes } = await import('./temporalClient');
    cluster.push(
      serve('slow', 'TIMED_OUT', 100, {
        closedAt: 220,
        close: { workflowExecutionTimedOutEventAttributes: {} },
      })
    );

    const { serves } = await listServes(SOURCE_ID);

    expect(serves[0]?.status).toBe('failed');
    expect(serves[0]?.failure).toBe('timed out');
  });

  it("uses the operator's own words for a termination", async () => {
    const { listServes } = await import('./temporalClient');
    cluster.push(
      serve('killed', 'TERMINATED', 100, {
        closedAt: 140,
        close: {
          workflowExecutionTerminatedEventAttributes: {
            reason: 'wedged on the image pull',
            // `identity` is present on the real event and must not win over the reason — which is
            // exactly what `@kontra/core`'s general-purpose `describe()` would have done with it.
            identity: 'mo@laptop',
          },
        },
      })
    );

    const { serves } = await listServes(SOURCE_ID);

    expect(serves[0]?.failure).toBe('wedged on the image pull');
  });

  it('invents nothing when the close event is gone', async () => {
    // Retention drops a closed execution's HISTORY long before its visibility row, so "it failed and
    // the sentence is gone" is an ordinary answer on this path. Absent is honest.
    const { listServes } = await import('./temporalClient');
    cluster.push(serve('forgotten', 'FAILED', 100, { closedAt: 130 }));

    const { serves } = await listServes(SOURCE_ID);

    expect(serves[0]?.status).toBe('failed');
    expect(serves[0]?.failure).toBeUndefined();
  });
});

describe('listServes pays for the reason only where there is one to read', () => {
  it('reads no history at all for a folder that has always served cleanly', async () => {
    const { listServes } = await import('./temporalClient');
    for (let i = 0; i < 6; i++) cluster.push(serve(`ok${i}`, 'COMPLETED', 100 + i, { closedAt: 200 }));

    await listServes(SOURCE_ID);

    // One RPC total — the listing. The close-event fan-out is what makes this read cost more than
    // a page, so a clean history must not trigger it.
    expect(historyReads).toEqual([]);
  });

  it('does not ask a RUNNING serve for a close event it cannot have', async () => {
    const { listServes } = await import('./temporalClient');
    cluster.push(serve('live', 'RUNNING', 100));

    const { serves } = await listServes(SOURCE_ID);

    expect(serves[0]?.status).toBe('running');
    // 0 and not absent: an open serve has no close time, and the field says so rather than omitting.
    expect(serves[0]?.closedAt).toBe(0);
    expect(historyReads).toEqual([]);
  });

  it('pins the read to the EXECUTION, because one folder reuses one workflow id', async () => {
    // Asking by workflow id alone answers with whichever execution ran LAST — so an older failure
    // would be reported with the newest one's reason. `serve-dev/at:<folder>` is deliberately reused
    // across every press of the button, which is what makes this the ordinary case here.
    const { listServes } = await import('./temporalClient');
    cluster.push(
      serve('first', 'FAILED', 100, {
        closedAt: 110,
        close: { workflowExecutionFailedEventAttributes: { failure: { message: 'no such source' } } },
      }),
      serve('second', 'FAILED', 200, {
        closedAt: 210,
        close: { workflowExecutionFailedEventAttributes: { failure: { message: 'port already bound' } } },
      })
    );

    const { serves } = await listServes(SOURCE_ID);

    expect(serves.map((s) => s.failure)).toEqual(['port already bound', 'no such source']);
    expect(historyReads).toEqual([
      { workflowId: WORKFLOW_ID, runId: 'second' },
      { workflowId: WORKFLOW_ID, runId: 'first' },
    ]);
  });
});
