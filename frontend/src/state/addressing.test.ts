/**
 * The store and the address bar, wired together: cold load, Back, Forward, the redirect out of a
 * retired surface, and the one thing that must never navigate.
 *
 * THE HISTORY STACK IS REAL HERE, not simulated with spies — `memoryAddressBar` implements the
 * browser's semantics (push truncates the forward stack, replace does not move the index), so
 * `entries.length` is the honest count of places Back can reach. Everything below is asserted
 * against that number rather than against "did we call pushState".
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { memoryAddressBar, type MemoryAddressBar } from './addressBar';
import { startAddressing } from './addressing';
import { useAppStore } from './store';
import { SURFACES } from './surfaces';

let bar: MemoryAddressBar;
let stop: () => void;

/** Cold load: this is what `main.tsx` does before the first render. */
function coldLoad(url: string): void {
  bar = memoryAddressBar(url);
  stop = startAddressing(bar);
}

beforeEach(() => {
  useAppStore.setState({
    view: 'workflows',
    workflowName: null,
    runId: null,
    focusTerminal: null,
    datasetFocus: null,
    datasets: [],
    datasetSeries: {},
    fleetSeries: [],
    datasetsAt: 0,
  });
  coldLoad('/workflows');
});

afterEach(() => {
  stop();
  vi.unstubAllGlobals();
});

describe('a cold load', () => {
  it('lands on the workflow in the URL, not on the default surface', () => {
    // THE REPORTED BUG, from the other end: "when I reload the page, I am taken back to the default
    // workflow page every time". The store's `view: 'workflows'` initial value is still there — it
    // is just never what a session that opened a deep link sees.
    coldLoad('/workflows/dnssweep');

    expect(useAppStore.getState().view).toBe('workflows');
    expect(useAppStore.getState().workflowName).toBe('dnssweep');
    expect(bar.url()).toBe('/workflows/dnssweep');
  });

  it('lands on one run of one workflow', () => {
    coldLoad('/workflows/dnssweep/nscheck-123');

    expect(useAppStore.getState().workflowName).toBe('dnssweep');
    expect(useAppStore.getState().runId).toBe('nscheck-123');
    expect(bar.url()).toBe('/workflows/dnssweep/nscheck-123');
  });

  it('survives a dot in both ids, which is what a cold load kept 404ing on', () => {
    // The server answers this address because its first segment is a surface — the extension
    // heuristic alone read `sweep-v1.2` as a file and 404ed it. This is the frontend half.
    coldLoad('/workflows/nscheck-0.1.0/sweep-v1.2');

    expect(useAppStore.getState().workflowName).toBe('nscheck-0.1.0');
    expect(useAppStore.getState().runId).toBe('sweep-v1.2');
    expect(bar.url()).toBe('/workflows/nscheck-0.1.0/sweep-v1.2');
    expect(bar.entries).toHaveLength(1);
  });

  it('lands on the Dataset in the URL, scoped to the Run the link carried', () => {
    coldLoad('/datasets/lame?kind=output&run=nightly-2026-08-15');

    expect(useAppStore.getState().view).toBe('datasets');
    expect(useAppStore.getState().datasetFocus).toEqual({
      name: 'lame',
      kind: 'output',
      run: 'nightly-2026-08-15',
    });
  });

  it('lands on a dotted Dataset name', () => {
    coldLoad('/datasets/acme.com');
    expect(useAppStore.getState().datasetFocus).toEqual({ name: 'acme.com' });
    expect(bar.url()).toBe('/datasets/acme.com');
  });

  it('lands on the Terminal in the URL', () => {
    coldLoad('/monitor/kontra-recon%3A0.1');

    expect(useAppStore.getState().view).toBe('monitor');
    expect(useAppStore.getState().focusTerminal).toBe('kontra-recon:0.1');
  });

  it('lands on Settings, which nothing else in the app can reach by id', () => {
    coldLoad('/settings');
    expect(useAppStore.getState().view).toBe('settings');
    expect(bar.url()).toBe('/settings');
  });

  it('names the default surface in the bar instead of leaving a bare /', () => {
    coldLoad('/');

    expect(useAppStore.getState().view).toBe('workflows');
    expect(bar.url()).toBe('/workflows');
    // REPLACED, NOT PUSHED. Pushing the correction would leave `/` behind it, so Back would return
    // to the address we just decided was wrong and correct it again — a Back that cannot leave.
    expect(bar.entries).toEqual(['/workflows']);
  });

  it('falls back from an address this app cannot mean, without a blank page', () => {
    // The written-down answer to "what does a bad URL do": a MALFORMED path falls back to the
    // default surface and the bar is corrected. A well-formed id for something that does not exist
    // does NOT — see the next test.
    coldLoad('/nope/whatever/at/all');

    expect(useAppStore.getState().view).toBe('workflows');
    expect(bar.url()).toBe('/workflows');
    expect(bar.entries).toHaveLength(1);
  });

  it('keeps a well-formed id for an entity that no longer exists, and lets the surface answer', () => {
    // AN ID IN AN ADDRESS IS NOT A PROMISE THAT IT EXISTS. A run Temporal has dropped for retention
    // is still a run id, and the honest landing is the surface holding it. Falling back to the list
    // would hide the one fact the operator pasted, and a spinner would hide it forever.
    coldLoad('/workflows/gone/deleted-run-2024');

    expect(useAppStore.getState().workflowName).toBe('gone');
    expect(useAppStore.getState().runId).toBe('deleted-run-2024');
    expect(bar.url()).toBe('/workflows/gone/deleted-run-2024');
  });

  it('drops a query it does not understand, in place', () => {
    coldLoad('/datasets/lame?kind=banana');

    expect(useAppStore.getState().datasetFocus).toEqual({ name: 'lame' });
    expect(bar.url()).toBe('/datasets/lame');
    expect(bar.entries).toHaveLength(1);
  });
});

describe('a retired surface', () => {
  it('redirects the run somebody pasted, and keeps the id', () => {
    // `/runs/nscheck-123` is in somebody's notes. It still names a run, so it must not 404 and must
    // not blank — it lands on the surface that holds runs now, carrying the id.
    coldLoad('/runs/nscheck-123');

    expect(useAppStore.getState().view).toBe('workflows');
    expect(useAppStore.getState().runId).toBe('nscheck-123');
    expect(bar.url()).toBe('/workflows?run=nscheck-123');
  });

  it('redirects a run id with a dot in it', () => {
    coldLoad('/runs/sweep-v1.2');
    expect(useAppStore.getState().runId).toBe('sweep-v1.2');
    expect(bar.url()).toBe('/workflows?run=sweep-v1.2');
  });

  it('redirects the global list, and Scratch, to the workflows list', () => {
    coldLoad('/runs');
    expect(useAppStore.getState().view).toBe('workflows');
    expect(bar.url()).toBe('/workflows');

    coldLoad('/scratch');
    expect(useAppStore.getState().view).toBe('workflows');
    expect(bar.url()).toBe('/workflows');
  });

  it('REPLACES rather than pushes, so Back cannot land on the dead surface again', () => {
    // Pushing the redirect would leave `/runs/nscheck-123` in the history, and Back would bounce
    // off it forever — the same loop `/` used to have.
    coldLoad('/runs/nscheck-123');
    expect(bar.entries).toEqual(['/workflows?run=nscheck-123']);
  });
});

describe('the store writing the bar', () => {
  it('gives every surface an address', () => {
    const others = SURFACES.map((s) => s.id).filter((id) => id !== 'workflows');
    for (const view of others) {
      useAppStore.getState().setView(view);
      expect(bar.url()).toBe(`/${view}`);
    }
    expect(bar.entries).toEqual(['/workflows', '/actors', '/datasets', '/monitor', '/settings']);
  });

  it('addresses a workflow thread', () => {
    useAppStore.getState().openWorkflow('dnssweep');
    expect(bar.url()).toBe('/workflows/dnssweep');
  });

  it('addresses a Run under the thread it was opened from', () => {
    useAppStore.getState().openWorkflow('dnssweep');
    useAppStore.getState().openRun('nightly-sweep-2026-08-14');
    expect(bar.url()).toBe('/workflows/dnssweep/nightly-sweep-2026-08-14');
  });

  it('addresses a Run whose thread is not known, without inventing one', () => {
    useAppStore.getState().openRun('nightly-sweep-2026-08-14');
    expect(bar.url()).toBe('/workflows?run=nightly-sweep-2026-08-14');
  });

  it('addresses a Dataset opened from a Run, carrying the Run', () => {
    useAppStore.getState().openDataset({ name: 'lame', kind: 'output', run: 'r1' });
    expect(bar.url()).toBe('/datasets/lame?kind=output&run=r1');
  });

  it('addresses a Terminal', () => {
    useAppStore.getState().watchTerminal('kontra-recon:0.1');
    expect(bar.url()).toBe('/monitor/kontra-recon%3A0.1');
  });

  it('adds nothing for a click that goes where we already are', () => {
    useAppStore.getState().setView('actors');
    useAppStore.getState().setView('actors');
    useAppStore.getState().setView('actors');
    expect(bar.entries).toEqual(['/workflows', '/actors']);
  });

  it('says nothing about a selection the current surface does not address', () => {
    // `setRunId` while the Datasets page is open changes which Run the Workflows surface has
    // selected without navigating — the store's own rule. A bar that pushed here would move the
    // page under whoever did it, on Back if not immediately.
    useAppStore.getState().setView('datasets');
    useAppStore.getState().setRunId('r1');

    expect(useAppStore.getState().view).toBe('datasets');
    expect(bar.url()).toBe('/datasets');
    expect(bar.entries).toEqual(['/workflows', '/datasets']);
  });

  it('keeps the Dataset in the bar after the surface has consumed the focus', () => {
    // THIS IS WHAT MAKES A PASTED DATASET LINK SURVIVE F5. The Datasets surface clears the focus
    // as soon as it has resolved it against the catalog (`DatasetPage`), because a focus is a
    // navigation and not a selection. If the bar mirrored that clear it would fall back to
    // `/datasets` a tick after arriving, and reloading the link would land on the listing.
    useAppStore.getState().openDataset({ name: 'lame', run: 'r1' });
    useAppStore.getState().clearDatasetFocus();

    expect(useAppStore.getState().datasetFocus).toBeNull();
    expect(bar.url()).toBe('/datasets/lame?run=r1');
    expect(bar.entries).toEqual(['/workflows', '/datasets/lame?run=r1']);
  });

  it('keeps the Terminal in the bar after the Monitor has revealed it', () => {
    useAppStore.getState().watchTerminal('kontra-recon:0.1');
    useAppStore.getState().clearFocusTerminal();
    expect(bar.url()).toBe('/monitor/kontra-recon%3A0.1');
  });
});

describe('Back and Forward', () => {
  it('returns to the surface it came from, and forward again', () => {
    useAppStore.getState().openWorkflow('dnssweep');
    useAppStore.getState().setView('datasets');
    expect(bar.entries).toEqual(['/workflows', '/workflows/dnssweep', '/datasets']);

    bar.back();
    expect(useAppStore.getState().view).toBe('workflows');
    expect(useAppStore.getState().workflowName).toBe('dnssweep');

    bar.back();
    expect(useAppStore.getState().workflowName).toBeNull();

    bar.forward();
    expect(useAppStore.getState().workflowName).toBe('dnssweep');
    bar.forward();
    expect(useAppStore.getState().view).toBe('datasets');
  });

  it('closes the run on the way back out of it, and the thread on the way out of that', () => {
    // `/workflows/<w>` is a place — the thread with no conversation open — and `/workflows` is
    // another, so Back walks out of a run one level at a time.
    useAppStore.getState().openWorkflow('dnssweep');
    useAppStore.getState().setRunId('r1');
    expect(bar.url()).toBe('/workflows/dnssweep/r1');

    bar.back();
    expect(useAppStore.getState().workflowName).toBe('dnssweep');
    expect(useAppStore.getState().runId).toBeNull();

    bar.back();
    expect(useAppStore.getState().workflowName).toBeNull();
  });

  it('re-opens the Dataset a link named, after the surface consumed the focus', () => {
    useAppStore.getState().openDataset({ name: 'lame' });
    useAppStore.getState().clearDatasetFocus();
    useAppStore.getState().setView('actors');

    bar.back();
    expect(useAppStore.getState().view).toBe('datasets');
    expect(useAppStore.getState().datasetFocus).toEqual({ name: 'lame' });
  });

  it('adds no entry of its own — Back is not a navigation to record', () => {
    // Back drives the store through `setState`, which is the one silent path into it. Going through
    // an action would push an entry for the place the browser had just moved us to, and Back would
    // become a loop that cannot leave.
    useAppStore.getState().openWorkflow('dnssweep');
    useAppStore.getState().setView('datasets');
    const before = bar.entries.length;

    bar.back();
    bar.back();

    expect(bar.entries).toHaveLength(before);
    expect(bar.index).toBe(0);
  });

  it('keeps the Workflows surface selection when the address is about another surface', () => {
    useAppStore.getState().openWorkflow('dnssweep');
    useAppStore.getState().openDataset({ name: 'lame' });

    bar.back();
    bar.forward();

    expect(useAppStore.getState().view).toBe('datasets');
    // A Datasets address says nothing about which workflow the Workflows surface had open, so it
    // must not blank it — the operator never left that page, they are just not looking at it.
    expect(useAppStore.getState().workflowName).toBe('dnssweep');
  });
});

describe('a poll', () => {
  /** One `/api/datasets` answer, the shape `loadDatasets` samples rates from. */
  function stubCatalog(rows: number): void {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => ({
        ok: true,
        json: async () => [{ kind: 'output', name: 'lame', version: 'v1', dt: 'd1', rows, bytes: rows * 10 }],
      }))
    );
  }

  it('NEVER pushes a history entry, however many times it fires', async () => {
    // THE TRAP IN THIS WHOLE DESIGN. Four pollers write this store every 2–30 s — the Dataset
    // catalog at 2 s while anything is running, the run list at 5 s, the Terminal inventory at
    // 30 s, the Actor catalog at 15 s — plus the Monitor's `setPanes`/`setWallCounts` on every
    // socket message. One history entry per tick and Back is useless inside a minute: the
    // operator's last real navigation would be a hundred presses behind.
    //
    // It holds by construction rather than by filtering — a poller cannot reach the address bar at
    // all, because only the navigation actions write it (`store.ts`'s `addressed`).
    coldLoad('/workflows/dnssweep/nscheck-123');
    const before = bar.entries.length;

    stubCatalog(10);
    await useAppStore.getState().loadDatasets();
    stubCatalog(4_000);
    await useAppStore.getState().loadDatasets();
    stubCatalog(9_000);
    await useAppStore.getState().loadDatasets();
    await useAppStore.getState().loadRuns();
    await useAppStore.getState().loadCatalog();
    useAppStore.getState().setWallCounts({ live: 4 });
    useAppStore.getState().setPanes([]);

    // The polls really did land — otherwise this test would pass on a store that did nothing.
    expect(useAppStore.getState().datasets).toHaveLength(1);
    expect(useAppStore.getState().datasetsAt).toBeGreaterThan(0);

    expect(bar.entries).toHaveLength(before);
    expect(bar.url()).toBe('/workflows/dnssweep/nscheck-123');
  });
});
