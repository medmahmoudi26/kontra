/**
 * `scanned` IS THE CAP; `historyLength` IS THE COUNT — the wiring, not just the reducer.
 *
 * `history.test.ts` proves `mapHistory` carries a length it is handed. THIS proves `fetchRunHistory`
 * gets that length from `DescribeWorkflowExecution` and not from the pages it walked — which is the
 * whole of the fix, and the half a reducer test cannot reach.
 *
 * ── WHY IT IS STUBBED AND NOT LIVE ──────────────────────────────────────────────────────────────
 *
 * The failure needs a history longer than `HISTORY_MAX_PAGES × HISTORY_PAGE` = 20,000 events, and
 * the longest history on this cluster is 56: the runs that would have exercised it aged out with
 * the 24-hour retention. Manufacturing one means running a workflow for hours to assert a number.
 *
 * So the two sides are driven apart deliberately — the pager answers a handful of events while
 * `describe` reports tens of thousands, which is exactly the shape a capped read has — and the test
 * asserts they DISAGREE and that the right one wins. That is stronger than a live 20,000-event run
 * would be, because it is repeatable and because it fails if somebody ever derives the length from
 * `raw.length` again.
 *
 * A LIVE CHECK STILL HAPPENED, and it is the other half: against `beaconcall-1787924865` on the
 * running cluster, kontra answered `historyLength: 9, historySizeBytes: 1790` and
 * `temporal workflow describe` answered the same two numbers. That proves the FIELD is read
 * correctly off the wire; this proves it is read from the right CALL.
 */

import { beforeEach, describe, expect, it, vi } from 'vitest';

/** What the stubbed pager returns — set per test, before the module under test is imported. */
const pager = { events: [] as unknown[], pages: 1 };
/** What the stubbed `describe` returns. `null` makes the RPC throw, which is its own case. */
let described: { historyLength?: unknown; historySizeBytes?: unknown } | null = {};

vi.mock('./temporalClient', async (original) => {
  const real = (await original()) as Record<string, unknown>;
  return real;
});

vi.mock('@temporalio/client', async (original) => {
  const real = (await original()) as Record<string, unknown>;
  return {
    ...real,
    Connection: { connect: vi.fn(async () => ({})) },
    Client: class {
      namespace = 'default';
      workflowService = {
        getWorkflowExecutionHistory: vi.fn(async () => ({
          history: { events: pager.events },
          // One page and done: the cap is not what this test drives, the DISAGREEMENT is.
          nextPageToken: undefined,
        })),
        describeWorkflowExecution: vi.fn(async () => {
          if (described === null) throw new Error('describe is down');
          return { workflowExecutionInfo: described };
        }),
      };
      workflow = { getHandle: vi.fn() };
    },
  };
});

/** One legal event, so the reducer has something to reduce. */
function event(id: number) {
  return {
    eventId: id,
    eventTime: { seconds: 1_700_000_000 + id, nanos: 0 },
    activityTaskScheduledEventAttributes: { activityType: { name: 'RunBatch' } },
  };
}

async function fetch(runId = 'r1', execId?: string) {
  vi.resetModules();
  const mod = await import('./temporalClient');
  return mod.fetchRunHistory(runId, execId);
}

beforeEach(() => {
  pager.events = [event(1), event(2), event(3)];
  described = { historyLength: 31_402, historySizeBytes: 9_001_234 };
});

describe('the count comes from describe, not from the pages', () => {
  it('reports the server’s length even when the reader saw three events', async () => {
    const got = await fetch();
    expect(got, 'no history came back').toBeDefined();
    expect(got!.scanned).toBe(3);
    expect(got!.historyLength).toBe(31_402);
    // THE ASSERTION THE ISSUE ASKS FOR. "The count is right" passes when both numbers are wrong
    // together, which is the state this was in for every run above 20,000 events.
    expect(got!.historyLength).not.toBe(got!.scanned);
  });

  it('carries the size along, because a byte meter would otherwise ask twice', async () => {
    expect((await fetch())!.historySizeBytes).toBe(9_001_234);
  });

  it('converts a Long, which is what the wire actually sends', async () => {
    // `historyLength` arrives as a protobufjs Long, and `Number(long)` is NaN — which is what the
    // production code did until this case failed.
    //
    // A FAITHFUL LONG MEANS ONE WITH A `toString`. The first version of this fixture was a bare
    // `{low, high}`, which protobufjs does not produce — so the fixture was wrong in the SAME
    // direction as the code, both answered NaN, and only the assertion below caught either.
    described = {
      historyLength: { low: 31_402, high: 0, unsigned: false, toString: () => '31402' },
    };
    const got = await fetch();
    expect(typeof got!.historyLength).toBe('number');
    expect(got!.historyLength).toBe(31_402);
  });
});

describe('a describe that cannot answer costs the count and not the log', () => {
  it('returns the events with no length, rather than failing', async () => {
    described = null;
    const got = await fetch();
    expect(got, 'a failed describe took the whole history down').toBeDefined();
    expect(got!.events.length).toBeGreaterThan(0);
    expect(got!.scanned).toBe(3);
    // ABSENT, NOT ZERO. Zero is a claim that the run had no events — the one thing it cannot mean.
    expect('historyLength' in got!).toBe(false);
  });

  it('omits a field the server did not send, rather than defaulting it', async () => {
    described = { historyLength: 12 };
    const got = await fetch();
    expect(got!.historyLength).toBe(12);
    expect('historySizeBytes' in got!).toBe(false);
  });
});

describe('it asks about the execution the events came from', () => {
  it('passes execId through, because a workflow id can be reused', async () => {
    // `kontra-fleet/dns` is the id of both the bring-up and the teardown of every fleet. A length
    // read from a different execution than the events is worse than no length at all.
    vi.resetModules();
    const mod = await import('./temporalClient');
    const client = (await (mod as unknown as { getClient(): Promise<unknown> }).getClient()) as {
      workflowService: { describeWorkflowExecution: { mock: { calls: unknown[][] } } };
    };
    await mod.fetchRunHistory('kontra-fleet/dns', 'exec-abc');
    const calls = client.workflowService.describeWorkflowExecution.mock.calls;
    expect(calls.length, 'describe was never called').toBeGreaterThan(0);
    const arg = calls[calls.length - 1]![0] as { execution?: { runId?: string } };
    expect(arg.execution?.runId).toBe('exec-abc');
  });
});
