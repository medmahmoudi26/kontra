/**
 * The join #13 got wrong, pinned.
 *
 * The Workflows list joined each closed row to its runs by `guessTypeFromFilename`, so a row whose
 * filename does not title-case into its `@workflow.defn` class matched no run and painted a RUNNING
 * workflow as NEVER RUN. These tests fix the honest answer to the decorated type, and demonstrate —
 * side by side — that the filename guess is what produced the lie.
 */

import { describe, expect, it } from 'vitest';
import type { RunRow } from '../run/api';
import { rowStatus } from './workflowRow';
import { guessTypeFromFilename, typeFromSource } from './workflowSource';

function run(type: string, status: RunRow['status']): RunRow {
  return {
    runId: `${type.toLowerCase()}-1`,
    type,
    status,
    tenant: 'default',
    startedAt: 1_787_084_868_000,
    closedAt: 0,
    dispatches: 3,
  };
}

describe('rowStatus joins on the DECORATED type, not the filename', () => {
  // The live bug: `dhmonitor` is a running `DockerLeakMonitor`. Its filename guesses to `Dhmonitor`,
  // which is nobody's type.
  const source = '@workflow.defn\nclass DockerLeakMonitor:\n    pass\n';
  const runs = [run('DockerLeakMonitor', 'running')];

  it('reports the real status of a workflow whose filename guess does NOT match its type', () => {
    // What the row must resolve its type to, and it is not what the filename says.
    const real = typeFromSource(source, 'dhmonitor.py');
    expect(real).toBe('DockerLeakMonitor');
    expect(guessTypeFromFilename('dhmonitor.py')).toBe('Dhmonitor');
    expect(real).not.toBe(guessTypeFromFilename('dhmonitor.py'));

    const status = rowStatus(real, runs);
    expect(status.label).toBe('running');
    expect(status.state).toBe('running');
  });

  it('is the FILENAME GUESS that produced the never-run lie', () => {
    // Feeding the join the guess — exactly what the old list did — matches no run and lies.
    const byGuess = rowStatus(guessTypeFromFilename('dhmonitor.py'), runs);
    expect(byGuess.label).toBe('never run');
    // And the decorated type does not.
    expect(rowStatus('DockerLeakMonitor', runs).label).not.toBe('never run');
  });
});

describe('rowStatus is honest about what it cannot name', () => {
  it('says UNKNOWN, never never-run, before the type is resolved', () => {
    // The source has not been read (or could not be): the join key is unknown. `never run` here is
    // the #13 lie in a different disguise.
    const status = rowStatus(undefined, [run('DockerLeakMonitor', 'running')]);
    expect(status.label).toBe('unknown');
    expect(status.state).toBe('unknown');
    expect(status.runs).toEqual([]);
  });

  it('says never run only when the type is known and truly has no runs', () => {
    const status = rowStatus('Canary', [run('DockerLeakMonitor', 'running')]);
    expect(status.label).toBe('never run');
    expect(status.state).toBe('idle');
  });

  it('carries the newest run as last, and marks running when any run is open', () => {
    const rows = [run('Sweep', 'completed'), run('Sweep', 'running')];
    const status = rowStatus('Sweep', rows);
    expect(status.state).toBe('running');
    expect(status.last).toBe(rows[0]);
    expect(status.runs).toHaveLength(2);
  });
});
