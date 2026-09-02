/**
 * The Workflows surface, WIRED — 31 pieces of state and 14 effects that nothing could reach.
 *
 * WHAT THIS FILE IS FOR. Every decision this page makes had a pure module beside it and a test on
 * that module: `workflowRow.ts` for the join, `run/workflowState.ts` for the three states,
 * `workflowInput.ts` for the form, `sourceFolders.ts` for the merge. What none of them could say is
 * whether the page CALLS them with the right arguments at the right moment — and the bugs this
 * page's own header records are all of that kind: a queue carried across a workflow switch, a
 * source fetch painting a row the operator had already left, a descriptor poll wiping what somebody
 * was typing. Those live in the wiring, and the wiring needed a DOM.
 *
 * THE SEAMS ARE THE FETCHES, and nothing else is stubbed that the page decides anything with.
 * `../run/api` is one module and every network call on this surface goes through it, so mocking it
 * leaves the page's own effects, refs and tokens entirely real. Two leaves are replaced because
 * they are engines rather than decisions — CodeMirror's viewer and the worker's xterm pane — and
 * the page hands both in as props precisely so that swapping them changes nothing it owns.
 */

import { createElement } from 'react';
import { act, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type {
  PauseResult,
  RunRow,
  ServeResult,
  Source,
  StartedRun,
  StopResult,
  WorkflowDescriptor,
  WorkflowFile,
} from '../run/api';
import type { PollerReport } from '../run/workflowState';

/* ── the seam ──────────────────────────────────────────────────────────────────────────────── */

const fetchWorkflows = vi.fn();
const fetchWorkflowSource = vi.fn();
const fetchExposure = vi.fn();
const fetchPollers = vi.fn();
const fetchRuns = vi.fn();
const fetchSources = vi.fn();
const fetchWorkflowSketch = vi.fn();
const serveWorkflow = vi.fn();
const pauseWorkflow = vi.fn();
const startRun = vi.fn();
const stopRun = vi.fn();
const fetchRun = vi.fn();
const fetchRunHistory = vi.fn();
const fetchRunHeartbeats = vi.fn();
const fetchCatalog = vi.fn();
const fetchDatasets = vi.fn();
const fetchPulse = vi.fn();
const fetchRunTurns = vi.fn();
const fetchSchema = vi.fn();

vi.mock('../run/api', async (actual) => ({
  ...(await actual<typeof import('../run/api')>()),
  fetchWorkflows: (...a: unknown[]) => fetchWorkflows(...a),
  fetchWorkflowSource: (...a: unknown[]) => fetchWorkflowSource(...a),
  fetchExposure: (...a: unknown[]) => fetchExposure(...a),
  fetchPollers: (...a: unknown[]) => fetchPollers(...a),
  fetchRuns: (...a: unknown[]) => fetchRuns(...a),
  fetchSources: (...a: unknown[]) => fetchSources(...a),
  fetchWorkflowSketch: (...a: unknown[]) => fetchWorkflowSketch(...a),
  serveWorkflow: (...a: unknown[]) => serveWorkflow(...a),
  pauseWorkflow: (...a: unknown[]) => pauseWorkflow(...a),
  startRun: (...a: unknown[]) => startRun(...a),
  stopRun: (...a: unknown[]) => stopRun(...a),
  fetchRun: (...a: unknown[]) => fetchRun(...a),
  fetchRunHistory: (...a: unknown[]) => fetchRunHistory(...a),
  fetchRunHeartbeats: (...a: unknown[]) => fetchRunHeartbeats(...a),
  fetchCatalog: (...a: unknown[]) => fetchCatalog(...a),
  fetchDatasets: (...a: unknown[]) => fetchDatasets(...a),
  fetchPulse: (...a: unknown[]) => fetchPulse(...a),
}));

vi.mock('../run/turns', async (actual) => ({
  ...(await actual<typeof import('../run/turns')>()),
  fetchRunTurns: (...a: unknown[]) => fetchRunTurns(...a),
}));

vi.mock('../run/query', async (actual) => ({
  ...(await actual<typeof import('../run/query')>()),
  fetchSchema: (...a: unknown[]) => fetchSchema(...a),
}));

/** The read-only viewer. It is handed in as a prop by the page for exactly this reason; the stub
 *  keeps the line count and the theme observable without a real editor in the document. */
vi.mock('@uiw/react-codemirror', () => ({
  default: ({ value }: { value: string }) =>
    createElement('pre', { 'data-testid': 'source-viewer' }, value),
}));

/** xterm reaches for a canvas jsdom does not implement. The pane is a screen, not a decision. */
vi.mock('./WorkerPane', () => ({
  WorkerPane: ({ terminal }: { terminal: { id: string } }) =>
    createElement('div', { 'data-testid': `worker-pane-${terminal.id}` }),
}));

const { useAppStore } = await import('../state/store');
const WorkflowsPage = (await import('./WorkflowsPage')).default;

/* ── fixtures ──────────────────────────────────────────────────────────────────────────────── */

/** A workflow file's text, with the ONE line the page reads out of it: the decorated class name.
 *  Keyed by folder so a switch really does change the type — `dnssweep` declares `DnsSweep` and
 *  `canary` declares `Canary`, which is the difference the queue-clearing test turns on. */
const TYPES: Record<string, string> = { dnssweep: 'DnsSweep', canary: 'Canary' };
const sourceFor = (name: string): string =>
  ['@workflow.defn', `class ${TYPES[name] ?? 'Unknown'}:`, '    pass'].join('\n');
const SOURCE = sourceFor('dnssweep');

function folder(name: string, over: Partial<Source> = {}): Source {
  return {
    id: `src-${name}`,
    kind: 'workflow',
    name,
    path: `/root/checkout/${name}`,
    version: '',
    description: '',
    registeredAt: 1,
    ...over,
  };
}

function file(name: string, over: Partial<WorkflowFile> = {}): WorkflowFile {
  return { name, bytes: 200, modifiedAt: 1, ...over };
}

function descriptor(over: Partial<WorkflowDescriptor> = {}): WorkflowDescriptor {
  return { name: 'DnsSweep', queue: 'wf-dnssweep-0123456789ab', savedAt: 1, ...over };
}

function pollers(over: Partial<PollerReport> = {}): PollerReport {
  return { queue: 'wf-dnssweep-0123456789ab', pollers: 0, identities: [], workers: [], lastPoll: 0, ...over };
}

function run(over: Partial<RunRow> = {}): RunRow {
  return {
    runId: 'sweep-9',
    type: 'DnsSweep',
    status: 'running',
    tenant: 'default',
    startedAt: Date.now() - 60_000,
    closedAt: 0,
    dispatches: 1,
    ...over,
  };
}

/** Mount, and drain the mount effects' promises. Two flushes because the listing's answer is what
 *  starts the source read, and the source's answer is what sets the type. */
async function mount(): Promise<void> {
  render(createElement(WorkflowsPage));
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

async function settle(): Promise<void> {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

const initial = useAppStore.getState();

beforeEach(() => {
  vi.useFakeTimers();
  localStorage.clear();
  useAppStore.setState(initial, true);
  vi.clearAllMocks();

  fetchWorkflows.mockResolvedValue({
    dir: '.kontra/workflows/',
    workflows: [file('dnssweep'), file('canary')],
    registered: [descriptor()],
  });
  fetchWorkflowSource.mockImplementation((name: string) => Promise.resolve(sourceFor(name)));
  fetchExposure.mockResolvedValue({ open: false, detail: '' });
  fetchPollers.mockResolvedValue(pollers());
  fetchRuns.mockResolvedValue([]);
  fetchSources.mockResolvedValue({
    defaultRoot: '.kontra/workflows/',
    sources: [folder('dnssweep'), folder('canary')],
  });
  fetchWorkflowSketch.mockResolvedValue(null);
  fetchCatalog.mockResolvedValue([]);
  fetchDatasets.mockResolvedValue([]);
  fetchPulse.mockResolvedValue(null);
  fetchSchema.mockResolvedValue([]);
  fetchRunTurns.mockResolvedValue({ state: 'gone' });
  fetchRun.mockResolvedValue(null);
  fetchRunHistory.mockResolvedValue(null);
  fetchRunHeartbeats.mockResolvedValue({});
  serveWorkflow.mockResolvedValue({
    file: 'dnssweep',
    queue: 'wf-dnssweep-0123456789ab',
    session: 'dnssweep',
    attach: 'tmux attach -t dnssweep',
  } satisfies ServeResult);
  pauseWorkflow.mockResolvedValue({
    file: 'dnssweep',
    session: 'dnssweep',
    paused: true,
    detail: 'paused the worker',
  } satisfies PauseResult);
  startRun.mockResolvedValue({
    runId: 'sweep-42',
    type: 'DnsSweep',
    queue: 'wf-dnssweep-0123456789ab',
  } satisfies StartedRun);
  stopRun.mockResolvedValue({
    runId: 'sweep-9',
    outcome: 'cancelling',
    waitedMs: 0,
    detail: 'cancel requested',
  } satisfies StopResult);
});

afterEach(() => {
  vi.useRealTimers();
});

/* ── the list ──────────────────────────────────────────────────────────────────────────────── */

describe('what the page reads on arrival', () => {
  it('lists one row per registered folder and publishes the count to the chrome', async () => {
    await mount();
    expect(screen.getByTestId('workflow-file-dnssweep')).toBeTruthy();
    expect(screen.getByTestId('workflow-file-canary')).toBeTruthy();
    // The rail's inventory comes from this page, not from a second poll of its own.
    expect(useAppStore.getState().workflowCount).toBe(2);
  });

  it('opens nothing until the address names something', async () => {
    await mount();
    expect(screen.getByText('Pick a workflow to view, serve and run it.')).toBeTruthy();
  });

  it('says where it looked when nothing is registered', async () => {
    fetchSources.mockResolvedValue({ defaultRoot: '.kontra/workflows/', sources: [] });
    fetchWorkflows.mockResolvedValue({ dir: '.kontra/workflows/', workflows: [], registered: [] });
    await mount();
    expect(screen.getByText(/No folders registered/)).toBeTruthy();
  });

  it('reads each listed row’s source EXACTLY once, so a closed row can join to its runs', async () => {
    await mount();
    const asked = fetchWorkflowSource.mock.calls.map(([name]) => name as string);
    expect(new Set(asked)).toEqual(new Set(['dnssweep', 'canary']));
    expect(asked.length).toBe(2);
    // A re-render must not stack a second read on a name already requested.
    await settle();
    expect(fetchWorkflowSource.mock.calls.length).toBe(2);
  });

  it('raises the open-control-surface warning only when the server says so', async () => {
    await mount();
    expect(screen.queryByText(/This control surface is open/)).toBeNull();
    fetchExposure.mockResolvedValue({ open: true, detail: 'bound on 0.0.0.0' });
    await mount();
    expect(screen.getByText(/This control surface is open/)).toBeTruthy();
  });

  it('shows a listing failure rather than an empty page', async () => {
    fetchWorkflows.mockRejectedValue(new Error('orchestrator unreachable'));
    await mount();
    expect(screen.getByTestId('workflow-error').textContent).toContain('unreachable');
  });
});

/* ── the address, applied ──────────────────────────────────────────────────────────────────── */

describe('opening a workflow', () => {
  it('follows the store rather than a local selection', async () => {
    await mount();
    await act(async () => {
      useAppStore.getState().openWorkflow('dnssweep');
    });
    await settle();
    expect(screen.getByRole('heading', { name: 'dnssweep' })).toBeTruthy();
    // The viewer lives on the Code tab, one of six readings of the open thread.
    await act(async () => {
      screen.getByTestId('tab-code').click();
    });
    expect(screen.getByTestId('source-viewer').textContent).toBe(SOURCE);
  });

  it('takes the type from the source’s own decorator, never from the filename', async () => {
    await mount();
    await act(async () => {
      useAppStore.getState().openWorkflow('dnssweep');
    });
    await settle();
    // `dnssweep` would guess `Dnssweep`; the class is `DnsSweep`, and starting a type nothing
    // registered hangs instead of failing.
    expect(screen.getByTestId('run-options').textContent).toContain('DnsSweep');
  });

  it('DROPS a source that arrives for a row the operator has already left', async () => {
    // #13's racing clicks, which the selection token exists for. The first read is left hanging and
    // resolved AFTER the second has landed; the page must keep the newer row's source.
    let releaseFirst: (src: string) => void = () => {};
    fetchWorkflowSource.mockImplementation((name: string) =>
      name === 'dnssweep'
        ? new Promise<string>((res) => (releaseFirst = res))
        : Promise.resolve(sourceFor(name))
    );
    await mount();
    await act(async () => {
      useAppStore.getState().openWorkflow('dnssweep');
    });
    await settle();
    await act(async () => {
      useAppStore.getState().openWorkflow('canary');
    });
    await settle();
    await act(async () => {
      releaseFirst(sourceFor('dnssweep'));
    });
    await settle();
    expect(screen.getByRole('heading', { name: 'canary' })).toBeTruthy();
    expect(screen.getByTestId('run-options').textContent).toContain('Canary');
    expect(screen.getByTestId('run-options').textContent).not.toContain('DnsSweep');
  });

  it('clears the previous workflow’s queue, poller reading and serve result', async () => {
    fetchPollers.mockResolvedValue(pollers({ pollers: 2, lastPoll: Date.now() }));
    await mount();
    await act(async () => {
      useAppStore.getState().openWorkflow('dnssweep');
    });
    await settle();
    expect(screen.getByTestId('run-options').textContent).toContain('wf-dnssweep-0123456789ab');
    expect(screen.getByTestId('workflow-state').textContent).toBe('serving');

    // `canary` has no registered descriptor, so it has no queue — and carrying the last one over is
    // how a start went to a queue nobody served (measured, and recorded in the page's header).
    await act(async () => {
      useAppStore.getState().openWorkflow('canary');
    });
    await settle();
    expect(screen.getByTestId('run-options').textContent).not.toContain('wf-dnssweep');
    expect(screen.getByTestId('run-options').textContent).toContain('queue —');
  });

  it('applies a name the checkout does not have, rather than falling back to row one', async () => {
    fetchWorkflowSource.mockRejectedValue(new Error('no such workflow'));
    await mount();
    await act(async () => {
      useAppStore.getState().openWorkflow('ghost');
    });
    await settle();
    expect(screen.getByRole('heading', { name: 'ghost' })).toBeTruthy();
  });
});

/* ── the serving signal ────────────────────────────────────────────────────────────────────── */

describe('the poller poll', () => {
  it('asks about the DERIVED queue and re-asks on its own cadence', async () => {
    await mount();
    await act(async () => {
      useAppStore.getState().openWorkflow('dnssweep');
    });
    await settle();
    expect(fetchPollers).toHaveBeenCalledWith('wf-dnssweep-0123456789ab');
    const first = fetchPollers.mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4000);
    });
    expect(fetchPollers.mock.calls.length).toBe(first + 1);
  });

  it('does not ask at all while no workflow with a queue is open', async () => {
    await mount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(12_000);
    });
    expect(fetchPollers).not.toHaveBeenCalled();
  });

  it('re-reads the descriptors when a worker IS polling, so a --watch save reaches the form', async () => {
    fetchPollers.mockResolvedValue(pollers({ pollers: 1, lastPoll: Date.now() }));
    await mount();
    await act(async () => {
      useAppStore.getState().openWorkflow('dnssweep');
    });
    await settle();
    const listed = fetchWorkflows.mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4000);
    });
    expect(fetchWorkflows.mock.calls.length).toBeGreaterThan(listed);
  });

  it('does not re-read them for a queue nobody serves', async () => {
    await mount();
    await act(async () => {
      useAppStore.getState().openWorkflow('dnssweep');
    });
    await settle();
    const listed = fetchWorkflows.mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(8000);
    });
    expect(fetchWorkflows.mock.calls.length).toBe(listed);
  });

  it('stops asking when the page goes away', async () => {
    const view = render(createElement(WorkflowsPage));
    await settle();
    await act(async () => {
      useAppStore.getState().openWorkflow('dnssweep');
    });
    await settle();
    const asked = fetchPollers.mock.calls.length;
    view.unmount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(20_000);
    });
    expect(fetchPollers.mock.calls.length).toBe(asked);
  });
});

/* ── the three states, and the fourth ──────────────────────────────────────────────────────── */

describe('what the header claims', () => {
  it('draws `unknown` rather than `idle` when the cluster could not be asked', async () => {
    fetchPollers.mockResolvedValue(pollers({ error: 'DescribeTaskQueue failed' }));
    await mount();
    await act(async () => {
      useAppStore.getState().openWorkflow('dnssweep');
    });
    await settle();
    expect(screen.getByTestId('workflow-state').textContent).toBe('unknown');
  });

  it('raises the stranded alarm for a run nothing is serving', async () => {
    fetchRuns.mockResolvedValue([run()]);
    await mount();
    await act(async () => {
      useAppStore.getState().openWorkflow('dnssweep');
    });
    await settle();
    expect(screen.getByTestId('stranded-run')).toBeTruthy();
    expect(screen.getByTestId('workflow-state').textContent).toBe('running');
  });

  it('does NOT raise it when the poller reading is merely unavailable', async () => {
    fetchRuns.mockResolvedValue([run()]);
    fetchPollers.mockResolvedValue(pollers({ error: 'cluster unreachable' }));
    await mount();
    await act(async () => {
      useAppStore.getState().openWorkflow('dnssweep');
    });
    await settle();
    expect(screen.queryByTestId('stranded-run')).toBeNull();
  });
});

/* ── the controls ──────────────────────────────────────────────────────────────────────────── */

describe('serve, pause and run', () => {
  async function open(): Promise<void> {
    await mount();
    await act(async () => {
      useAppStore.getState().openWorkflow('dnssweep');
    });
    await settle();
  }

  it('serves the open workflow and offers Pause only afterwards', async () => {
    await open();
    expect(screen.queryByTestId('pause-button')).toBeNull();
    await act(async () => {
      screen.getByTestId('serve-button').click();
    });
    await settle();
    expect(serveWorkflow).toHaveBeenCalledWith('dnssweep');
  });

  it('offers Pause while a worker is polling, and flips it to Resume', async () => {
    fetchPollers.mockResolvedValue(pollers({ pollers: 1, lastPoll: Date.now() }));
    await open();
    const pause = screen.getByTestId('pause-button');
    expect(pause.textContent).toContain('Pause');
    await act(async () => {
      pause.click();
    });
    await settle();
    expect(pauseWorkflow).toHaveBeenCalledWith('dnssweep', false);
    expect(screen.getByTestId('pause-button').textContent).toContain('Resume');
    expect(screen.getByTestId('workflow-notice').textContent).toBe('paused the worker');
  });

  it('starts the run and stays on this page, moving only the address', async () => {
    await open();
    await act(async () => {
      screen.getByTestId('run-button').click();
    });
    await settle();
    expect(startRun).toHaveBeenCalledWith('dnssweep', 'DnsSweep', undefined);
    // The run is WATCHED here. `openRun` is what carries the surface AND the id, and the surface is
    // already this one.
    expect(useAppStore.getState().runId).toBe('sweep-42');
    expect(useAppStore.getState().view).toBe('workflows');
  });

  it('refuses to start a workflow with no queue, because nothing is serving it', async () => {
    fetchWorkflows.mockResolvedValue({
      dir: '.kontra/workflows/',
      workflows: [file('dnssweep')],
      registered: [],
    });
    await open();
    expect(screen.getByTestId('run-button')).toHaveProperty('disabled', true);
  });

  it('reports what a failed action said, on the page rather than in a console', async () => {
    serveWorkflow.mockRejectedValue(new Error('tmux is not installed'));
    await open();
    await act(async () => {
      screen.getByTestId('serve-button').click();
    });
    await settle();
    expect(screen.getByTestId('workflow-error').textContent).toContain('tmux is not installed');
  });
});

/* ── the form, and the poll that must not disturb it ───────────────────────────────────────── */

describe('the run input', () => {
  it('is rebuilt from the DECLARED input when a descriptor arrives', async () => {
    fetchWorkflows.mockResolvedValue({
      dir: '.kontra/workflows/',
      workflows: [file('dnssweep')],
      registered: [
        descriptor({
          input: { type: 'object', properties: { machines: { type: 'integer' } }, required: ['machines'] },
        }),
      ],
    });
    await mount();
    await act(async () => {
      useAppStore.getState().openWorkflow('dnssweep');
    });
    await settle();
    await act(async () => {
      screen.getByTestId('run-options').click();
    });
    expect(screen.getByLabelText(/machines/i)).toBeTruthy();
  });

  it('survives a descriptor poll that mints a fresh object with the SAME schema', async () => {
    // The form is keyed on the serialized schema, not on the descriptor's identity — a poll every
    // four seconds re-mints `contract`, and depending on the object would wipe what is being typed.
    fetchPollers.mockResolvedValue(pollers({ pollers: 1, lastPoll: Date.now() }));
    fetchWorkflows.mockImplementation(() =>
      Promise.resolve({
        dir: '.kontra/workflows/',
        workflows: [file('dnssweep')],
        registered: [
          descriptor({ input: { type: 'object', properties: { region: { type: 'string' } } } }),
        ],
      })
    );
    await mount();
    await act(async () => {
      useAppStore.getState().openWorkflow('dnssweep');
    });
    await settle();
    await act(async () => {
      screen.getByTestId('run-options').click();
    });
    const box = screen.getByLabelText(/region/i) as HTMLInputElement;
    await act(async () => {
      box.focus();
      Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(box, 'sfo3');
      box.dispatchEvent(new Event('input', { bubbles: true }));
    });
    expect((screen.getByLabelText(/region/i) as HTMLInputElement).value).toBe('sfo3');
    await act(async () => {
      await vi.advanceTimersByTimeAsync(8000);
    });
    expect((screen.getByLabelText(/region/i) as HTMLInputElement).value).toBe('sfo3');
  });
});
