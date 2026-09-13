/**
 * `scanned` IS WHAT WAS READ; `historyLength` IS WHAT THERE IS — issue F3.
 *
 * The reader stops at `HISTORY_MAX_PAGES × HISTORY_PAGE` = 20,000 events and sets `truncated`, and
 * `scanned` then reported the CAP as if it were the count. Any run between 20,001 and Temporal's
 * 51,200 ceiling was recorded as exactly 20,000 — an undercount of up to 61%, invisible because the
 * number looks plausible and is only wrong for the largest runs.
 *
 * ── WHAT THIS FILE TESTS, AND WHAT IT DELIBERATELY DOES NOT ─────────────────────────────────────
 *
 * `describeLength` IS THE FIX, and it is tested here with a fake client — the field selection, the
 * Long conversion, absent-versus-zero, and the execution it asks about.
 *
 * IT DOES NOT DRIVE `fetchRunHistory`, and that is a correction rather than a gap. The first version
 * of this file mocked `@temporalio/client` wholesale so it could reach the memoized module-level
 * client; the factory spread the real package, which pulled protobufjs into a worker that could not
 * resolve it — six cases green when the file ran alone and six red the moment it shared one. A mock
 * that heavy tests the mock.
 *
 * The wiring it gave up is one line (`mapHistory(events, truncated, NAMESPACE, await
 * describeLength(...))`), it is typechecked, and it was VERIFIED LIVE against the running cluster:
 * `beaconcall-1787924865` answered `historyLength: 9, historySizeBytes: 1790` and
 * `scope-many-bc16f8c0` answered `56 / 9764` — both exactly what `temporal workflow describe` says.
 * `history.test.ts` covers the other end, that `mapHistory` carries what it is handed.
 */

import { describe, expect, it } from 'vitest';

import { describeLength } from './temporalClient';

/** Just enough client for the one call, plus a record of what it was asked. */
function client(info: unknown, opts: { throws?: boolean } = {}) {
  const calls: Array<{ execution?: { workflowId?: string; runId?: string } }> = [];
  return {
    calls,
    client: {
      workflowService: {
        describeWorkflowExecution: async (arg: { execution?: { workflowId?: string; runId?: string } }) => {
          calls.push(arg);
          if (opts.throws) throw new Error('describe is down');
          return { workflowExecutionInfo: info };
        },
      },
    } as unknown as Parameters<typeof describeLength>[0],
  };
}

describe('the count comes from the server', () => {
  it('reads both numbers off the description', async () => {
    const { client: c } = client({ historyLength: 31_402, historySizeBytes: 9_001_234 });
    expect(await describeLength(c, 'r1')).toEqual({
      historyLength: 31_402,
      historySizeBytes: 9_001_234,
    });
  });

  it('converts a Long, which is what the raw wire actually sends', async () => {
    // `Number(long)` is NaN — the first implementation used it, and it passed LIVE because this
    // cluster happens to send a plain number. A FAITHFUL LONG has a `toString`; the first version
    // of this fixture was a bare `{low, high}`, which protobufjs does not produce, so the fixture
    // was wrong in the same direction as the code and only the assertion caught either.
    const long = { low: 31_402, high: 0, unsigned: false, toString: () => '31402' };
    const { client: c } = client({ historyLength: long });
    const got = await describeLength(c, 'r1');
    expect(typeof got.historyLength).toBe('number');
    expect(got.historyLength).toBe(31_402);
  });

  it('keeps a real zero, because a history really can have none', async () => {
    // `kontra-dataset-retention-workflow-…` sat for 18 days without running a task. Zero is a
    // count; treating it as "nothing was said" would report a real number as unknown.
    const { client: c } = client({ historyLength: 0, historySizeBytes: 0 });
    const got = await describeLength(c, 'r1');
    expect(got.historyLength).toBe(0);
    expect('historyLength' in got).toBe(true);
  });
});

describe('what it does when the server cannot answer', () => {
  it('returns nothing rather than throwing — the events are already in hand', async () => {
    const { client: c } = client(null, { throws: true });
    expect(await describeLength(c, 'r1')).toEqual({});
  });

  it('omits a field the server did not send, rather than defaulting it', async () => {
    // ABSENT IS NOT ZERO. Zero is a claim that the run had no events; absent is "the server did not
    // say", and only one of those is true after a failed describe.
    const { client: c } = client({ historyLength: 12 });
    const got = await describeLength(c, 'r1');
    expect(got.historyLength).toBe(12);
    expect('historySizeBytes' in got).toBe(false);
  });

  it('ignores a value it cannot read as a number', async () => {
    const { client: c } = client({ historyLength: {} });
    expect('historyLength' in (await describeLength(c, 'r1'))).toBe(false);
  });
});

describe('it asks about the execution the events came from', () => {
  it('passes execId through, because a workflow id can be reused', async () => {
    // `kontra-fleet/dns` is the id of both the bring-up and the teardown of every fleet, and
    // `fetchRunHistory` documents that asking by id alone "answers with whichever ran last". A
    // length taken from a different execution than the events is worse than no length at all.
    const { client: c, calls } = client({ historyLength: 5 });
    await describeLength(c, 'kontra-fleet/dns', 'exec-abc');
    expect(calls).toHaveLength(1);
    expect(calls[0]!.execution).toEqual({ workflowId: 'kontra-fleet/dns', runId: 'exec-abc' });
  });

  it('omits runId entirely when there is none, rather than sending an empty one', async () => {
    const { client: c, calls } = client({ historyLength: 5 });
    await describeLength(c, 'r1');
    expect(calls[0]!.execution).toEqual({ workflowId: 'r1' });
    expect('runId' in calls[0]!.execution!).toBe(false);
  });
});
