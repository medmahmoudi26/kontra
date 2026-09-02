/**
 * The two-dimensional status model (ADR 0017) — the rules that decide whether a run's
 * output is queryable, tested without a lake, a database or a Temporal cluster.
 *
 * These assertions are the guard on the defect the ADR exists to fix: three distinct
 * outcomes (found nothing / materialization failed / materialization stores pointers)
 * wearing one green label. Every case below pins ONE of those apart from the others.
 */

import { describe, expect, it } from 'vitest';

import type { RunStatus } from '../../contract/types';
import {
  MATERIALIZATION_SCHEMA_VERSION,
  MAX_ERROR_CHARS,
  boundedError,
  canTransition,
  isMaterializationTerminal,
  materializationKeyOf,
  publicLifecycle,
  summarize,
  type MaterializationRecord,
  type MaterializationState,
} from './materialization';

/** The identity separator every key string uses. */
const NUL = '\u0000';

describe('canTransition', () => {
  it('admits only pending or running as a first write', () => {
    expect(canTransition(undefined, 'pending')).toBe(true);
    expect(canTransition(undefined, 'running')).toBe(true);
    expect(canTransition(undefined, 'complete')).toBe(false);
    expect(canTransition(undefined, 'failed')).toBe(false);
  });

  it('allows running -> running so a cross-worker retry cannot deadlock the gate', () => {
    // ADR 0016: a Temporal retry may land on a different worker after the previous
    // attempt died mid-write. Refusing this edge would wedge the run permanently.
    expect(canTransition('running', 'running')).toBe(true);
  });

  it('treats complete as absorbing', () => {
    expect(canTransition('complete', 'complete')).toBe(true);
    for (const to of ['pending', 'running', 'failed'] as MaterializationState[]) {
      expect(canTransition('complete', to)).toBe(false);
    }
  });

  it('reconciles a lost status write: pending|running -> complete', () => {
    expect(canTransition('pending', 'complete')).toBe(true);
    expect(canTransition('running', 'complete')).toBe(true);
  });

  it('lets a failed key be retried, and repaired if its commit had actually landed', () => {
    expect(canTransition('failed', 'running')).toBe(true);
    expect(canTransition('failed', 'complete')).toBe(true);
  });

  it('never walks backwards to pending', () => {
    for (const from of ['pending', 'running', 'complete', 'failed'] as MaterializationState[]) {
      expect(canTransition(from, 'pending')).toBe(false);
    }
  });
});

describe('publicLifecycle', () => {
  const nonTerminal: RunStatus[] = ['pending', 'running'];

  it('reports executing while the graph is still running, whatever materialization says', () => {
    for (const e of nonTerminal) {
      expect(publicLifecycle(e, [])).toBe('executing');
      expect(publicLifecycle(e, ['complete'])).toBe('executing');
      expect(publicLifecycle(e, ['failed'])).toBe('executing');
    }
  });

  it('reports finalizing when execution is done but output is still being written', () => {
    expect(publicLifecycle('completed', ['complete', 'running'])).toBe('finalizing');
    expect(publicLifecycle('completed', ['pending'])).toBe('finalizing');
  });

  it('reports completed only when execution succeeded and every node committed', () => {
    expect(publicLifecycle('completed', ['complete', 'complete'])).toBe('completed');
  });

  it('reports completed for a successful run that produced no datasets at all', () => {
    // A storeless run, or one whose nodes emitted nothing: no records, nothing
    // outstanding, nothing failed. Absent output is not a failure here — but it is
    // distinguishable, because the record set is empty rather than `complete` with 0 rows.
    expect(publicLifecycle('completed', [])).toBe('completed');
  });

  it('reports output_failed when any node materialization is exhausted', () => {
    expect(publicLifecycle('completed', ['complete', 'failed'])).toBe('output_failed');
  });

  it('ranks a failed materialization above an outstanding one', () => {
    // "Some output is missing and will not arrive" is the fact an operator must act on;
    // it must not be hidden behind "still finalizing".
    expect(publicLifecycle('completed', ['running', 'failed'])).toBe('output_failed');
  });

  it('never reports completed for a run whose execution failed or was cancelled', () => {
    expect(publicLifecycle('failed', ['complete'])).toBe('output_failed');
    expect(publicLifecycle('cancelled', ['complete'])).toBe('output_failed');
    expect(publicLifecycle('failed', [])).toBe('output_failed');
  });

  it('still reports finalizing for a failed run with materialization outstanding', () => {
    // The outstanding work is real and will settle; only then does the run's output
    // verdict become final.
    expect(publicLifecycle('failed', ['running'])).toBe('finalizing');
  });
});

describe('summarize', () => {
  const rec = (state: MaterializationState, rows: number, bytes: number): MaterializationRecord => ({
    runId: 'r',
    actor: 'a',
    version: 'v',
    node: 'n',
    schemaVersion: MATERIALIZATION_SCHEMA_VERSION,
    state,
    attempt: 1,
    rows,
    bytes,
    snapshotId: null,
    tbl: null,
    error: null,
    runStartedAt: 0,
    createdAt: 0,
    updatedAt: 0,
  });

  it('counts by state and totals only committed rows and bytes', () => {
    const s = summarize([
      rec('complete', 10, 100),
      rec('complete', 5, 50),
      // A failed record's partial counters must not inflate the run's totals — that is
      // how a wiped batch reports as a healthy one.
      rec('failed', 999, 9999),
      rec('running', 7, 70),
      rec('pending', 0, 0),
    ]);
    expect(s).toEqual({
      total: 5,
      pending: 1,
      running: 1,
      complete: 2,
      failed: 1,
      rows: 15,
      bytes: 150,
    });
  });

  it('keeps a zero-row success visible as a success', () => {
    const s = summarize([rec('complete', 0, 0)]);
    expect(s.complete).toBe(1);
    expect(s.failed).toBe(0);
    expect(s.rows).toBe(0);
  });
});

describe('boundedError', () => {
  it('flattens whitespace so a multi-line DuckDB error stays one row', () => {
    expect(boundedError(new Error('line one\n  line two'))).toBe('line one line two');
  });

  it('caps a runaway error and says so', () => {
    const out = boundedError(new Error('x'.repeat(MAX_ERROR_CHARS * 3)));
    expect(out.length).toBeLessThan(MAX_ERROR_CHARS + 32);
    expect(out.endsWith('… (truncated)')).toBe(true);
  });

  it('accepts a non-Error throw', () => {
    expect(boundedError('plain string')).toBe('plain string');
  });
});

describe('identity helpers', () => {
  it('renders a stable key string', () => {
    expect(
      materializationKeyOf({ runId: 'r', actor: 'a', version: 'v', node: 'n', schemaVersion: 2 })
    ).toBe(['r', 'a', 'v', 'n', '2'].join(NUL));
  });

  it('classifies terminal materialization states', () => {
    expect(isMaterializationTerminal('complete')).toBe(true);
    expect(isMaterializationTerminal('failed')).toBe(true);
    expect(isMaterializationTerminal('pending')).toBe(false);
    expect(isMaterializationTerminal('running')).toBe(false);
  });
});
