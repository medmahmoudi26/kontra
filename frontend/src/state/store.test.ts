/**
 * The surface state, and the two hand-offs that are a surface change PLUS an id.
 *
 * `view` is zustand state rather than a route, so a link between surfaces is a store action and
 * nothing else. Both of the ones tested here have already been shipped broken in the obvious way:
 * setting the id without the surface, which reads as a control that did nothing at all.
 */

import { beforeEach, describe, expect, it } from 'vitest';

import { useAppStore } from './store';

describe('the surfaces', () => {
  beforeEach(() => {
    useAppStore.setState({
      view: 'workflows',
      workflowName: null,
      runId: null,
      runPane: null,
      focusTerminal: null,
      datasetFocus: null,
    });
  });

  it('starts on Workflows — the definitions, which is where work starts', () => {
    // It used to start on the Catalog. That surface listed the Actors and Workflows this
    // installation holds, which is what the Actors and Workflows surfaces each already do for
    // their own kind — so it was a third place to read the same two lists, and it is gone.
    expect(useAppStore.getState().view).toBe('workflows');
  });

  it('opens a Run by id alone: no file, no type, no list row', () => {
    // THE HAND-OFF THE RUN BUTTON MAKES. `startRun` answers with the caller's workflow id, which
    // IS the run id (ADR 0023 §12) — and that id is the whole of what the detail needs.
    //
    // IT LANDS ON WORKFLOWS, because the global Runs surface is retired: a run is reached through
    // the workflow that produced it.
    useAppStore.getState().openRun('nightly-sweep-2026-08-14');

    expect(useAppStore.getState().view).toBe('workflows');
    expect(useAppStore.getState().runId).toBe('nightly-sweep-2026-08-14');
  });

  it('opens a workflow thread, and closes whatever conversation was in the last one', () => {
    // A run id left over from the previous workflow would address `/workflows/<new>/<old run>` —
    // a URL that reads as a run of a workflow that never produced it.
    useAppStore.getState().openRun('r-from-dnssweep');
    useAppStore.getState().openWorkflow('nscheck');

    expect(useAppStore.getState().view).toBe('workflows');
    expect(useAppStore.getState().workflowName).toBe('nscheck');
    expect(useAppStore.getState().runId).toBeNull();
  });

  it('drops the Machine the last run’s Monitor was showing, on every way of changing run', () => {
    // A Terminal id from the run before last names a Machine that is not this run's, and drawing it
    // under this run's heading is the same lie `workflowThread.ts` refuses one layer up. All three
    // of the ways a run or a thread changes have to clear it, not just the obvious one.
    for (const change of [
      () => useAppStore.getState().openRun('r2'),
      () => useAppStore.getState().setRunId('r2'),
      () => useAppStore.getState().openWorkflow('nscheck'),
    ]) {
      useAppStore.setState({ runId: 'r1' });
      useAppStore.getState().focusRunPane('local:main-droplet/kontra-recon/0.1');
      expect(useAppStore.getState().runPane).toBe('local:main-droplet/kontra-recon/0.1');
      change();
      expect(useAppStore.getState().runPane).toBeNull();
    }
  });

  it('leaves the surface alone when only the selection changes', () => {
    // `setRunId` is a run list picking a row; it must not be able to navigate, or the rail would
    // move the page under whoever clicked it.
    useAppStore.setState({ view: 'datasets' });
    useAppStore.getState().setRunId('r1');
    expect(useAppStore.getState().view).toBe('datasets');
  });

  it('closes a run without leaving the surface', () => {
    useAppStore.getState().openRun('r1');
    useAppStore.getState().setRunId(null);
    expect(useAppStore.getState().runId).toBeNull();
    expect(useAppStore.getState().view).toBe('workflows');
  });

  it('watching a Terminal is the same shape: the Monitor plus one id', () => {
    useAppStore.getState().watchTerminal('kontra-recon:0.1');
    expect(useAppStore.getState().view).toBe('monitor');
    expect(useAppStore.getState().focusTerminal).toBe('kontra-recon:0.1');
  });

  it('opens a Dataset from a Run, carrying the Run it came from', () => {
    // THE OTHER HALF OF THE ROUND TRIP. From a Dataset an operator reaches the runs that wrote
    // it; from a Run they reach the Dataset's total — arriving scoped to the run they were
    // reading, so "623 of 1,246 rows" is about that run and not a number to go and find again.
    useAppStore.getState().openDataset({ name: 'lame', kind: 'output', run: 'nightly-2026-08-15' });

    expect(useAppStore.getState().view).toBe('datasets');
    expect(useAppStore.getState().datasetFocus).toEqual({
      name: 'lame',
      kind: 'output',
      run: 'nightly-2026-08-15',
    });
  });

  it('clears the Dataset focus once consumed — it is a navigation, not a selection', () => {
    useAppStore.getState().openDataset({ name: 'lame' });
    useAppStore.getState().clearDatasetFocus();
    expect(useAppStore.getState().datasetFocus).toBeNull();
    expect(useAppStore.getState().view).toBe('datasets');
  });
});
