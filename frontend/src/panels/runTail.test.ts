/**
 * The run tail's POLL — the loop, not the rows.
 *
 * WHY THIS FILE EXISTS AT ALL. `RunTail` has one paragraph of header devoted to a rule it enforces
 * in four lines of `useEffect`: a settled run's history is final, so the panel stops asking. Until
 * the suite had a DOM, no effect ran, so that early return had never once executed under test — the
 * one thing about this component that costs money when it is wrong (a request per second per open
 * tab, forever, about an answer that cannot change) was the one thing nothing could reach.
 *
 * FAKE TIMERS, REAL PROMISES. The loop is `await read()` then `setTimeout(tick, POLL_MS)`, so a
 * test that only advanced the clock would advance it past a `tick` that had not resolved yet.
 * `advanceTimersByTimeAsync` runs the microtask queue between timer firings, which is what makes
 * "one poll per two seconds" observable as a call count rather than as a sleep.
 *
 * THE READS ARE MOCKED AT `../run/api`, not at `fetch`: what is under test is the panel's decision
 * to ask again or not, and putting a URL and a JSON body between the assertion and that decision
 * would test the client instead.
 */

import { createElement } from 'react';
import { act, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { RunDetail, RunEvent, RunHistory } from '../run/api';

const fetchRun = vi.fn<(runId: string) => Promise<RunDetail>>();
const fetchRunHistory = vi.fn<(runId: string) => Promise<RunHistory | null>>();
const fetchRunHeartbeats = vi.fn<(runId: string) => Promise<Record<string, { done?: number; total?: number }>>>();

vi.mock('../run/api', async (actual) => ({
  ...(await actual<typeof import('../run/api')>()),
  fetchRun: (id: string) => fetchRun(id),
  fetchRunHistory: (id: string) => fetchRunHistory(id),
  fetchRunHeartbeats: (id: string) => fetchRunHeartbeats(id),
}));

const { RunTail } = await import('./RunTail');

/** The panel's own `POLL_MS`. Not imported, because a test that reads the constant it is pinning
 *  cannot notice it changing — this is the second opinion. */
const POLL_MS = 2000;

function detail(over: Partial<RunDetail> = {}): RunDetail {
  return {
    runId: 'sweep-9',
    type: 'DnsSweep',
    tenant: 'default',
    startedAt: 1_700_000_000_000,
    closedAt: 0,
    execution: 'running',
    lifecycle: 'executing',
    settled: false,
    ...over,
  };
}

function event(over: Partial<RunEvent> = {}): RunEvent {
  return {
    id: 1,
    type: 'ActivityTaskScheduled',
    cat: 'activity',
    t: 1.5,
    at: 1_700_000_001_500,
    detail: '',
    attempt: 1,
    dur: 0,
    ...over,
  };
}

function history(events: RunEvent[]): RunHistory {
  return { events, scanned: events.length, elided: 0, truncated: false };
}

/** Mount and let the first poll resolve. `act` so React flushes the state the reads set. */
async function mount(runId = 'sweep-9'): Promise<void> {
  render(createElement(RunTail, { runId }));
  await act(async () => {
    await Promise.resolve();
  });
}

beforeEach(() => {
  vi.useFakeTimers();
  localStorage.clear();
  fetchRun.mockReset().mockResolvedValue(detail());
  fetchRunHistory.mockReset().mockResolvedValue(history([event()]));
  fetchRunHeartbeats.mockReset().mockResolvedValue({});
});

afterEach(() => {
  vi.useRealTimers();
});

describe('the poll', () => {
  it('reads all three authorities once on mount', async () => {
    await mount();
    expect(fetchRun).toHaveBeenCalledTimes(1);
    expect(fetchRunHistory).toHaveBeenCalledTimes(1);
    expect(fetchRunHeartbeats).toHaveBeenCalledTimes(1);
    expect(fetchRun).toHaveBeenCalledWith('sweep-9');
  });

  it('asks again every POLL_MS while the run is open', async () => {
    await mount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_MS * 3);
    });
    expect(fetchRun).toHaveBeenCalledTimes(4);
  });

  it('STOPS on a settled run — the early return nothing could reach before', async () => {
    fetchRun.mockResolvedValue(detail({ settled: true, execution: 'completed', closedAt: 1 }));
    await mount();
    expect(fetchRun).toHaveBeenCalledTimes(1);
    // Ten polls' worth of clock. A settled run's history cannot change, so one read is the whole
    // budget — this is the assertion the `d?.settled` branch exists for.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_MS * 10);
    });
    expect(fetchRun).toHaveBeenCalledTimes(1);
    expect(screen.getByTestId('run-tail-execution').textContent).toBe('completed');
  });

  it('stops as soon as a run that was open settles, not on the next mount', async () => {
    await mount();
    fetchRun.mockResolvedValue(detail({ settled: true, execution: 'completed' }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_MS);
    });
    expect(fetchRun).toHaveBeenCalledTimes(2);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_MS * 5);
    });
    expect(fetchRun).toHaveBeenCalledTimes(2);
  });

  it('keeps polling a run that is merely finished-looking but not settled', async () => {
    // `completed` is Temporal's dimension; `settled` is the ledger's (ADR 0017). Only the second
    // one is final, and reading the first would stop the panel over output still being written.
    fetchRun.mockResolvedValue(detail({ execution: 'completed', settled: false }));
    await mount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_MS);
    });
    expect(fetchRun).toHaveBeenCalledTimes(2);
  });

  it('stops when the panel goes away', async () => {
    const view = render(createElement(RunTail, { runId: 'sweep-9' }));
    await act(async () => {
      await Promise.resolve();
    });
    view.unmount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_MS * 4);
    });
    expect(fetchRun).toHaveBeenCalledTimes(1);
  });

  it('re-reads on demand without waiting for the tick', async () => {
    await mount();
    await act(async () => {
      screen.getByTestId('run-tail-refresh').click();
      await Promise.resolve();
    });
    expect(fetchRun).toHaveBeenCalledTimes(2);
  });

  it('does not blank the log a failed poll interrupted', async () => {
    await mount();
    expect(screen.getByTestId('run-tail-event-1')).toBeTruthy();
    fetchRun.mockRejectedValue(new Error('the cluster is unreachable'));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_MS);
    });
    expect(screen.getByTestId('run-tail-error').textContent).toContain('unreachable');
    // The rows somebody was reading are still there, which is the whole point of the catch.
    expect(screen.getByTestId('run-tail-event-1')).toBeTruthy();
    // …and a poll that failed did not settle anything, so the loop keeps going.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(POLL_MS);
    });
    expect(fetchRun).toHaveBeenCalledTimes(3);
  });

  it('survives a heartbeat read that fails, because that one is caught in the panel', async () => {
    fetchRunHeartbeats.mockRejectedValue(new Error('no heartbeats endpoint'));
    await mount();
    expect(screen.queryByTestId('run-tail-error')).toBeNull();
    expect(screen.getByTestId('run-tail-event-1')).toBeTruthy();
  });
});

describe('a different run', () => {
  it('leaves nothing of the last one behind', async () => {
    fetchRun.mockResolvedValue(detail({ materializationRecords: [{ actor: 'probe', rows: 12 }] }));
    const view = render(createElement(RunTail, { runId: 'sweep-9' }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.getByTestId('run-tail-wrote').textContent).toContain('probe');

    // The new run's reads never resolve, so what is on screen is whatever the switch left — which
    // must be nothing at all. A stale event log under a new run id is a wrong answer that looks
    // like a fast one.
    fetchRun.mockReturnValue(new Promise(() => {}));
    fetchRunHistory.mockReturnValue(new Promise(() => {}));
    view.rerender(createElement(RunTail, { runId: 'sweep-10' }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.queryByTestId('run-tail-wrote')).toBeNull();
    expect(screen.queryByTestId('run-tail-event-1')).toBeNull();
    expect(screen.getByTestId('run-tail-id').textContent).toBe('sweep-10');
    expect(screen.getByTestId('run-tail')).toHaveProperty('dataset.run', 'sweep-10');
  });

  it('polls the run it was given, not the one it started on', async () => {
    const view = render(createElement(RunTail, { runId: 'sweep-9' }));
    await act(async () => {
      await Promise.resolve();
    });
    view.rerender(createElement(RunTail, { runId: 'sweep-10' }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(fetchRun).toHaveBeenLastCalledWith('sweep-10');
  });
});

describe('the controls', () => {
  it('folds the log and remembers it, keeping the header', async () => {
    await mount();
    expect(screen.getByTestId('run-tail-events')).toBeTruthy();
    await act(async () => {
      screen.getByTestId('run-tail-fold').click();
    });
    expect(screen.queryByTestId('run-tail-events')).toBeNull();
    // The header does NOT fold with it: "did I start it?" has to stay answerable.
    expect(screen.getByTestId('run-tail-id')).toBeTruthy();
    expect(localStorage.getItem('kontra.runTail.folded')).toBe('1');
  });

  it('comes back folded, because that is what the flag is for', async () => {
    localStorage.setItem('kontra.runTail.folded', '1');
    await mount();
    expect(screen.queryByTestId('run-tail-events')).toBeNull();
  });

  it('shows an error even while folded', async () => {
    fetchRun.mockRejectedValue(new Error('gone'));
    localStorage.setItem('kontra.runTail.folded', '1');
    await mount();
    expect(screen.getByTestId('run-tail-error')).toBeTruthy();
  });

  it('names and counts the paperwork rather than dropping it silently', async () => {
    fetchRunHistory.mockResolvedValue(
      history([
        event({ id: 1, cat: 'activity', type: 'ActivityTaskScheduled' }),
        event({ id: 2, cat: 'task', type: 'WorkflowTaskScheduled' }),
        event({ id: 3, cat: 'task', type: 'WorkflowTaskStarted' }),
      ])
    );
    await mount();
    expect(screen.queryByTestId('run-tail-event-2')).toBeNull();
    const toggle = screen.getByTestId('run-tail-paperwork');
    expect(toggle.textContent).toBe('+2 bookkeeping');
    await act(async () => {
      toggle.click();
    });
    expect(screen.getByTestId('run-tail-event-2')).toBeTruthy();
    expect(screen.getByTestId('run-tail-paperwork').textContent).toBe('hide 2 bookkeeping');
  });

  it('offers no paperwork toggle when there is none held', async () => {
    await mount();
    expect(screen.queryByTestId('run-tail-paperwork')).toBeNull();
  });
});

describe('the two dimensions', () => {
  it('draws rows and failures as separate badges, never as one word', async () => {
    fetchRun.mockResolvedValue(
      detail({
        execution: 'completed',
        settled: true,
        materialization: { total: 3, pending: 0, running: 0, complete: 2, failed: 1, rows: 1234, bytes: 0 },
      })
    );
    await mount();
    expect(screen.getByTestId('run-tail-execution').textContent).toBe('completed');
    expect(screen.getByTestId('run-tail-rows').textContent).toBe('1,234 rows');
    expect(screen.getByTestId('run-tail-output-failed').textContent).toBe('1 output failed');
  });

  it('says nothing about output a run did not write', async () => {
    fetchRun.mockResolvedValue(
      detail({ materialization: { total: 0, pending: 0, running: 0, complete: 0, failed: 0, rows: 0, bytes: 0 } })
    );
    await mount();
    expect(screen.queryByTestId('run-tail-rows')).toBeNull();
    expect(screen.queryByTestId('run-tail-output-failed')).toBeNull();
  });

  it('draws what each Batch has beaten while it is in flight', async () => {
    fetchRunHeartbeats.mockResolvedValue({ 'probe@0.1.0': { done: 7, total: 40 } });
    await mount();
    expect(screen.getByTestId('run-tail-beats').textContent).toContain('7/40');
  });
});
