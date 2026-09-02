/**
 * The Monitor tab and the Data tab, drawn — every state, as markup.
 *
 * NODE, NO JSDOM, as everywhere else here: `renderToStaticMarkup` and string assertions about text
 * and `data-testid`. The pane is a PROP (xterm cannot load under the test runner), which is what
 * lets the four absences and the scoped rail all be asserted without a browser.
 *
 * WHAT IS ASSERTED IS THE SENTENCES. Not that a component received a prop — that the words an
 * operator reads are the right ones for the state they are in. "Nobody has read this run's account"
 * and "none of this run's Machines is on the Monitor" render the same empty rectangle and mean
 * completely different things, and only one of them is something to go and fix.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';

import { categorize } from '@core/history';

import type { DatasetInfo, RunEvent, RunHistory, RunRow } from '../run/api';
import { readRunTurns, type RunTurns } from '../run/turns';
import type { Terminal } from './panelsClient';
import { RunDataTab, RunMonitor } from './RunMonitor';
import { runDatasets } from './runDatasets';
import { focusedMachine, runMachines } from './runMachines';

const T0 = 1_787_084_868_000;

const RUN: RunRow = {
  runId: 'sweep-1787084868',
  type: 'DnsSweep',
  status: 'running',
  tenant: 'default',
  startedAt: T0,
  closedAt: 0,
  dispatches: 3,
};

function ev(id: number, type: string, ms: number, extra: Partial<RunEvent> = {}): RunEvent {
  return {
    id,
    type,
    cat: categorize(type),
    t: ms / 1000,
    at: T0 + ms,
    detail: type,
    attempt: 1,
    dur: 0,
    ...extra,
  };
}

function history(events: RunEvent[]): RunHistory {
  return { events, scanned: events.length, elided: 0, truncated: false };
}

const STARTED = ev(1, 'WorkflowExecutionStarted', 0, {
  detail: 'workflowType=DnsSweep · taskQueue=wf-dnssweep · identity=1@65520c20a6a4',
});

function dispatched(endpoint = 'kontra-nscheck-0-1-0'): RunTurns {
  return readRunTurns(
    RUN.runId,
    history([
      STARTED,
      ev(2, 'NexusOperationScheduled', 1000, { detail: `endpoint=${endpoint}` }),
      ev(3, 'NexusOperationStarted', 1100, { detail: `endpoint=${endpoint}`, dur: 0.1 }),
    ])
  );
}

/** A run that dispatched nowhere: the account is READ and there is nothing to look for. */
const QUIET = readRunTurns(RUN.runId, history([STARTED]));

function terminal(over: Partial<Terminal> & { id: string }): Terminal {
  return {
    machine: '',
    host: '',
    publicIp: '',
    tag: '',
    fleet: '',
    actor: '',
    version: '',
    window: '',
    health: { reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' },
    ...over,
  };
}

const WORKER = terminal({
  id: 'local:main-droplet/dnssweep/worker',
  machine: 'main-droplet',
  host: 'main-droplet',
  window: 'worker',
});

/** A tmux window really can be `0.1` — a pane index — so a Terminal id carries a colon AND a dot.
 *  This is the shape the SPA fallback was caught by, and it has to survive being addressed. */
const DOTTED = terminal({
  id: 'local:main-droplet/kontra-recon/0.1',
  machine: 'main-droplet',
  host: 'main-droplet',
  window: '0.1',
  actor: 'nscheck',
  version: '0.1.0',
});

const FLEET = [1, 2, 10].map((n) =>
  terminal({
    id: `fleet:kf-dns-${String(n).padStart(2, '0')}/nscheck-0_1_0/actor`,
    machine: `kf-dns-${String(n).padStart(2, '0')}`,
    host: `10.124.0.${n}`,
    tag: 'dns',
    fleet: 'nscheck-0.1.0',
    actor: 'nscheck',
    version: '0.1.0',
    window: 'actor',
    // Nothing has been measured on this one: the streamer answered `unknown`, which must not draw
    // as a green Machine.
    health: { reachable: 'unknown', session: 'unknown', poller: 'unknown', loads: 'unknown' },
  })
);

/** The tab, with the scoping done exactly as the page does it. */
function drawMonitor({
  run = RUN as RunRow | null,
  turns = null as RunTurns | null,
  session = 'dnssweep',
  terminals = [] as Terminal[],
  wanted = null as string | null,
}): string {
  const scoped = runMachines(turns, session, terminals);
  const focused = focusedMachine(scoped.machines, wanted);
  return renderToStaticMarkup(
    createElement(RunMonitor, {
      run,
      scoped,
      focused,
      onFocus: () => {},
      inventory: terminals.length,
      pane: focused ? createElement('div', { 'data-testid': 'the-pane' }, focused.terminal.id) : null,
    })
  );
}

/* ───────────────────────────── no machines ───────────────────────────── */

describe('a run with no Machines', () => {
  it('says so rather than showing the wall', () => {
    // THE WHOLE POINT OF THE SLICE, negatively stated: an empty scope must never fall back to every
    // Terminal on the appliance, which is what an operator was filtering by hand before.
    const html = drawMonitor({ turns: QUIET, session: '', terminals: [WORKER, ...FLEET] });
    expect(html).toContain('data-machines="0"');
    expect(html).not.toContain('kf-dns-01');
    expect(html).toContain('names no Machine of its own');
  });

  it('separates "nobody looked" from "nothing there"', () => {
    // The account has not arrived: which Actors this run called is UNKNOWN, and the sentence is
    // about the reading rather than about the run.
    const unread = drawMonitor({ turns: null, session: '', terminals: [WORKER] });
    expect(unread).toContain('data-testid="run-monitor-unread"');
    expect(unread).toContain('Reading sweep-1787084868’s account');
    expect(unread).not.toContain('run-monitor-nothing-named');

    // The account IS read and the run dispatched nowhere. Ordinary, and its own sentence.
    const quiet = drawMonitor({ turns: QUIET, session: '', terminals: [WORKER] });
    expect(quiet).toContain('data-testid="run-monitor-nothing-named"');
    expect(quiet).not.toContain('run-monitor-unread');
  });

  it('prints the names it looked under when the run named things and none of them answered', () => {
    // This is what a worker that booted and died looks like from here. A bare "no machines" would
    // hide the one fact that tells an operator where to go next.
    const html = drawMonitor({
      turns: dispatched('kontra-ghost-9-9-9'),
      session: '',
      terminals: [WORKER],
    });
    expect(html).toContain('data-testid="run-monitor-none"');
    expect(html).toContain('None of this run’s Machines is on the Monitor');
    expect(html).toContain('ghost@9.9.9');
    expect(html).toContain('1 Terminal is in the inventory');
  });

  it('says an empty inventory is about the appliance, not about this run', () => {
    const html = drawMonitor({ turns: dispatched(), session: 'dnssweep', terminals: [] });
    expect(html).toContain('data-testid="run-monitor-no-inventory"');
    expect(html).toContain('no Terminals at all');
  });

  it('refuses to be about a run when none is open', () => {
    const html = drawMonitor({ run: null, turns: null, terminals: [WORKER] });
    expect(html).toContain('data-testid="run-monitor-no-run"');
  });
});

/* ───────────────────────────── one, and many ───────────────────────────── */

describe('a run with one Machine', () => {
  const html = drawMonitor({ turns: dispatched(), session: 'dnssweep', terminals: [WORKER] });

  it('draws it, says why it is here, and shows its pane', () => {
    expect(html).toContain('data-machines="1"');
    expect(html).toContain('data-testid="run-monitor-machine-local:main-droplet/dnssweep/worker"');
    expect(html).toContain('data-reason="workflow"');
    expect(html).toContain('serves this workflow’s own worker');
    expect(html).toContain('data-testid="the-pane"');
  });

  it('states what crashing this tmux server would cost, because local panes hold the Worker', () => {
    // ADR 0020 finding (2), and CONTRACT.md's slice 6 amendment: the asymmetry must be stated in
    // the UI. A local pane holds the real process; a fleet pane holds a journal.
    expect(html).toContain('kills a running Worker');
  });
});

describe('a run with many Machines', () => {
  const terminals = [WORKER, ...FLEET];
  const html = drawMonitor({ turns: dispatched(), session: 'dnssweep', terminals });

  it('shows this run’s four and says how many the appliance has', () => {
    expect(html).toContain('data-machines="4"');
    expect(html).toContain('4 of 4');
  });

  it('scopes: a Machine of another fleet never reaches the rail', () => {
    const withStranger = drawMonitor({
      turns: dispatched(),
      session: 'dnssweep',
      terminals: [
        ...terminals,
        terminal({
          id: 'fleet:kf-crawl-01/webcrawl-0_2_0/actor',
          machine: 'kf-crawl-01',
          fleet: 'webcrawl-0.2.0',
          actor: 'webcrawl',
          version: '0.2.0',
          window: 'actor',
        }),
      ],
    });
    expect(withStranger).toContain('data-machines="4"');
    expect(withStranger).toContain('4 of 5');
    expect(withStranger).not.toContain('kf-crawl-01');
  });

  it('draws all three health readings apart — serving, broken, and never measured', () => {
    // Three states, never two. `unknown` rendering the way `ok` does is the collapse ADR 0020
    // forbids, and a row of Machines is where it would be made first.
    expect(html).toContain('data-health="unmeasured"');
    expect(html).toContain('data-health="serving"');
    expect(html).not.toContain('data-health="broken"');

    const dead = drawMonitor({
      turns: dispatched(),
      session: 'dnssweep',
      terminals: [
        terminal({
          id: 'fleet:kf-dns-04/nscheck-0_1_0/actor',
          machine: 'kf-dns-04',
          fleet: 'nscheck-0.1.0',
          actor: 'nscheck',
          version: '0.1.0',
          window: 'actor',
          health: { reachable: 'ok', session: 'present', poller: 'none', loads: 'ok' },
        }),
      ],
    });
    expect(dead).toContain('data-health="broken"');
  });

  it('focuses the first — the caller’s own worker — when nothing is asked for', () => {
    expect(html).toContain('data-focused="true"');
    expect(html).toContain('>local:main-droplet/dnssweep/worker</div>');
  });
});

/* ───────────────────────────── the deep link ───────────────────────────── */

describe('a deep link to one Machine', () => {
  it('opens the Machine whose id carries a colon and a dot', () => {
    const html = drawMonitor({
      turns: dispatched(),
      session: 'dnssweep',
      terminals: [WORKER, DOTTED],
      wanted: 'local:main-droplet/kontra-recon/0.1',
    });
    // The PANE is the assertion, not the chip: a link that highlighted a row while streaming the
    // first Machine's screen is the failure this addressing exists to avoid.
    expect(html).toContain('>local:main-droplet/kontra-recon/0.1</div>');
    expect(html).toContain('data-testid="run-monitor-because"');
    expect(html).toContain('this run dispatched to nscheck@0.1.0');
  });

  it('falls back to the first Machine when the link names one that is not this run’s', () => {
    // A `?pane=` survives switching runs in the same tab, so the id in the bar can name a Machine
    // that belonged to the run before last. Drawing it under this run's heading is the same lie
    // `transcriptFor` refuses one level up — the caller's own worker stands instead.
    const html = drawMonitor({
      turns: dispatched(),
      session: 'dnssweep',
      terminals: [WORKER, DOTTED],
      wanted: 'fleet:kf-crawl-01/webcrawl-0_2_0/actor',
    });
    expect(html).toContain('>local:main-droplet/dnssweep/worker</div>');
    expect(html).not.toContain('>local:main-droplet/kontra-recon/0.1</div>');
  });
});

/* ───────────────────────────── the Data tab ───────────────────────────── */

function ds(over: Partial<DatasetInfo> & { name: string }): DatasetInfo {
  return { kind: 'output', rows: 0, bytes: 0, ...over };
}

function drawData(catalog: DatasetInfo[], listedAt: number, run: RunRow | null = RUN): string {
  return renderToStaticMarkup(
    createElement(RunDataTab, {
      run,
      datasets: runDatasets(catalog, run?.runId ?? null, listedAt),
      onOpen: () => {},
    })
  );
}

describe('what the run wrote', () => {
  it('is one click, and it says which part of the total is this run’s', () => {
    const html = drawData(
      [
        ds({ name: 'lame', runId: RUN.runId, rows: 623, state: 'open', updatedAt: T0 + 2 }),
        ds({ name: 'apexes', contributingRuns: ['r0', RUN.runId], rows: 1246, state: 'sealed' }),
      ],
      T0
    );
    expect(html).toContain('data-testid="run-dataset-lame"');
    expect(html).toContain('data-how="sole"');
    expect(html).toContain('data-how="among"');
    expect(html).toContain('623 rows');
    // The warning that stops a shared total being read as this run's output.
    expect(html).toContain('not this run’s');
  });

  it('keeps an open Dataset apart from a sealed one', () => {
    // §11: an open Dataset drawn as sealed reads as a final result while its producer may still be
    // writing, or may have died.
    const html = drawData([ds({ name: 'lame', runId: RUN.runId, state: 'open' })], T0);
    expect(html).toContain('data-state="open"');
    const none = drawData([ds({ name: 'lame', runId: RUN.runId })], T0);
    expect(none).toContain('data-state="none"');
  });

  it('says nobody has looked before the catalog answers', () => {
    expect(drawData([], 0)).toContain('data-testid="run-data-unread"');
  });

  it('says nothing carries this run, and marks a floor as a floor', () => {
    expect(drawData([], T0)).toContain('data-testid="run-data-none"');
    const partial = drawData(
      [ds({ name: 'shared', contributingRuns: ['r0'], contributingRunsPartial: true })],
      T0
    );
    expect(partial).toContain('data-testid="run-data-partial"');
  });

  it('refuses to be about a run when none is open', () => {
    expect(drawData([], T0, null)).toContain('data-testid="run-data-no-run"');
  });
});
