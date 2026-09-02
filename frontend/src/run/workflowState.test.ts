import { describe, expect, it } from 'vitest';
import {
  POLL_FRESH_MS,
  isServing,
  stateWords,
  strandedRun,
  workflowState,
  type PollerReport,
} from './workflowState';
import type { RunRow } from './api';

/**
 * The three states the Workflows page draws, and the two it must not confuse.
 *
 * They come from different authorities answering different questions — Temporal's task queue for
 * "is anyone serving", the run list for "is anything running" — and reading either alone gets a
 * workflow wrong in a way that looks right.
 */

const NOW = 1_800_000_000_000;

const run = (status: RunRow['status'], type = 'NsCheck'): RunRow =>
  ({ runId: `${type}-1`, type, status, tenant: '', startedAt: NOW - 60_000, closedAt: 0, dispatches: 1 }) as RunRow;

const polling = (over: Partial<PollerReport> = {}): PollerReport => ({
  queue: 'recon',
  pollers: 1,
  identities: ['1@host'],
  workers: [{ identity: '1@host', lastPoll: NOW - 10_000 }],
  lastPoll: NOW - 10_000,
  ...over,
});

describe('is anyone actually serving', () => {
  it('counts a worker that polled recently', () => {
    expect(isServing(polling(), NOW)).toBe(true);
  });

  it('does NOT count a worker Temporal is only remembering', () => {
    // THE MEASURED TRAP. `DescribeTaskQueue` keeps a poller for about five minutes after it was
    // last seen, so a worker killed thirty seconds ago is still in the list — the identity alone
    // reports a dead worker as serving for five minutes. A long poll is 60s, so a live worker is
    // never much staler than that.
    expect(isServing(polling({ lastPoll: NOW - POLL_FRESH_MS - 1 }), NOW)).toBe(false);
    expect(isServing(polling({ lastPoll: NOW - POLL_FRESH_MS + 1 }), NOW)).toBe(true);
  });

  it('does not count a poller with no timestamp at all', () => {
    expect(isServing(polling({ lastPoll: 0 }), NOW)).toBe(false);
  });

  it('does not count an answer that is missing', () => {
    // `pollers: 0` WITH an error means the count is meaningless, not zero.
    expect(isServing(polling({ pollers: 0, error: 'Connection refused' }), NOW)).toBe(false);
    expect(isServing(polling({ error: 'Connection refused' }), NOW)).toBe(false);
    expect(isServing(null, NOW)).toBe(false);
  });
});

describe('the state to draw', () => {
  it('is running when a Run is open, even if nothing is serving', () => {
    // A run is the more specific fact and the more urgent one.
    expect(workflowState([run('running')], polling(), NOW)).toBe('running');
    expect(workflowState([run('running')], null, NOW)).toBe('running');
  });

  it('is serving when a worker polls and nothing is running', () => {
    expect(workflowState([run('completed')], polling(), NOW)).toBe('serving');
    expect(workflowState([], polling(), NOW)).toBe('serving');
  });

  it('is idle when the answer is a real negative', () => {
    expect(workflowState([run('completed')], polling({ pollers: 0, identities: [] }), NOW)).toBe('idle');
  });

  it('is UNKNOWN when Temporal could not be asked — never idle', () => {
    // The distinction that stops an unreachable cluster from drawing every workflow as un-served,
    // which is a page-wide lie told confidently.
    expect(workflowState([], null, NOW)).toBe('unknown');
    expect(workflowState([], polling({ pollers: 0, error: 'timeout' }), NOW)).toBe('unknown');
  });

  it('reports a worker that has gone stale as idle, not serving', () => {
    expect(workflowState([], polling({ lastPoll: NOW - 10 * 60_000 }), NOW)).toBe('idle');
  });
});

describe('the state worth interrupting somebody for', () => {
  it('flags a Run that is open with NOTHING serving it', () => {
    // It looks exactly like a slow run and is not one: the caller's worker is gone — a pause nobody
    // resumed, a crashed process, a rebooted machine — so the run is making no progress and will
    // make none. Temporal reports it as `running` the whole time.
    expect(strandedRun([run('running')], polling({ pollers: 0, identities: [] }), NOW)).toBe(true);
    expect(strandedRun([run('running')], polling({ lastPoll: NOW - 10 * 60_000 }), NOW)).toBe(true);
  });

  it('does not flag a run that IS being served', () => {
    expect(strandedRun([run('running')], polling(), NOW)).toBe(false);
  });

  it('does not flag anything when nothing is running', () => {
    expect(strandedRun([run('completed')], polling({ pollers: 0 }), NOW)).toBe(false);
  });

  it('does not raise the alarm on a missing answer', () => {
    // An unreachable Temporal is not evidence that a worker died, and an alarm that fires on an
    // outage is one an operator learns to ignore.
    expect(strandedRun([run('running')], null, NOW)).toBe(false);
    expect(strandedRun([run('running')], polling({ error: 'timeout' }), NOW)).toBe(false);
  });
});

describe('the words', () => {
  it('animates exactly the states where something is happening', () => {
    expect(stateWords('running').live).toBe(true);
    expect(stateWords('serving').live).toBe(true);
    expect(stateWords('idle').live).toBe(false);
    // An animation on `unknown` would be a page insisting something is happening while admitting
    // it cannot tell.
    expect(stateWords('unknown').live).toBe(false);
  });

  it('says what idle costs, since it is the state that makes a Run hang', () => {
    expect(stateWords('idle').title).toMatch(/queue nobody polls/);
  });

  it('says that unknown is a missing answer rather than a negative one', () => {
    expect(stateWords('unknown').title).toMatch(/NOT the same as idle/);
  });
});
