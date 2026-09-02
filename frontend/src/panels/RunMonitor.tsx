/**
 * The Monitor tab: the Machines under THIS run, and one of their panes.
 *
 * WHAT CHANGED IS THE SCOPE, NOT THE PANE. The tab used to hold one `WorkerPane` — the worker
 * serving this workflow — and the wall of everything else was a surface away, where an operator who
 * had just started a four-Machine sweep filtered sixty tiles by hand to find their own four. This
 * draws the run's own Machines, from the run's own account (`runMachines.ts`), and the pane below is
 * that same component with the Terminal handed to it. Nothing about the pane is rebuilt or
 * restyled: it is the same header, the same tile, the same snapshot rule, the same link out to the
 * Monitor where ADR 0020 says a pane may be promoted to a live attach.
 *
 * FOUR ABSENCES, FOUR SENTENCES, because this codebase does not let "nobody looked" and "nothing
 * there" render the same:
 *
 *   no run          nothing is selected. Not a fact about any run.
 *   not read yet    the run's account has not arrived, so which Actors it called is UNKNOWN — an
 *                   empty list here would report a four-Machine sweep as machine-less for as long
 *                   as the transcript took to load.
 *   nothing named   the account IS read and the run dispatched nowhere and brought nothing up. A
 *                   real and ordinary state for a run that only read a Dataset.
 *   none matched    the run named its Actors and its fleet, and no Terminal in the inventory is
 *                   serving any of them. THIS IS THE INTERESTING ONE — it is what a worker that
 *                   booted and died looks like from here — so it prints the names it looked under.
 *
 * AND THE WHOLE INVENTORY BEING EMPTY IS SAID SEPARATELY, because "the streamer can see no Terminals
 * at all" is a fact about the appliance and not about this run.
 *
 * IT TAKES THE PANE AS A PROP, like everything else the thread frames: xterm cannot load under the
 * test runner, so handing it in is what lets every state above be asserted in node as a string.
 *
 * `RunDataTab` IS AT THE BOTTOM OF THIS FILE, and it is here rather than in one of its own because
 * it is the other half of one question. "What did this run actually do" is answered from two ends —
 * the Machines under it and what came out of it — and neither is derivable from the other (ADR 0017
 * keeps execution and materialization apart for exactly that reason). Both are filters over a
 * listing the app already polls, both are scoped by the same open **Run**, and both draw an absence
 * rather than an empty box.
 */

import type { ReactNode } from 'react';
import { ExternalLink } from 'lucide-react';

import { datasetBadge, datasetState } from '../datasets/state';
import type { DatasetInfo, RunRow } from '../run/api';
import { DatasetTail } from './DatasetTail';
import { modeStakes, terminalHealthReading, tileRefFor } from './chrome/tileRef';
import type { Terminal } from './panelsClient';
import { attributionWords, type RunDatasets } from './runDatasets';
import { scopeNames, type RunMachine, type RunMachines } from './runMachines';

/** The word one Machine's rollup reads as. `null` is NOT `down` — see `panelsClient.healthReading`:
 *  "we never measured this pane" and "this pane's handler is down" are different next actions. */
function healthWord(reading: boolean | null): string {
  return reading === true ? 'serving' : reading === false ? 'broken' : 'unmeasured';
}

/** Why this Terminal is here, in one word for the row and the `data-reason` hook. */
const REASON_LABEL: Record<RunMachine<Terminal>['reason'], string> = {
  workflow: 'worker',
  actor: 'actor',
  fleet: 'fleet',
};

export interface RunMonitorProps {
  /** The run every row below is about. `null` is the thread with no conversation open. */
  run: RunRow | null;
  /** The scoping, already done — see `runMachines.ts`. */
  scoped: RunMachines<Terminal>;
  /** The Machine whose pane is on screen. `null` when there is none to show. */
  focused: RunMachine<Terminal> | null;
  /** Show this Machine's pane. The id round-trips through the address, so this is a navigation. */
  onFocus: (id: string) => void;
  /** How many Terminals the streamer reports IN TOTAL — the one number that tells "this run has no
   *  Machines" from "the Monitor can see nothing at all". */
  inventory: number;
  /** The focused Machine's pane, handed in (xterm). Absent draws the scope and no screen, which is
   *  what a surface with nowhere to put a pane should do rather than offer a dead rectangle. */
  pane?: ReactNode;
}

export function RunMonitor({
  run,
  scoped,
  focused,
  onFocus,
  inventory,
  pane,
}: RunMonitorProps): JSX.Element {
  const { machines, scope, read } = scoped;

  if (!run) {
    return (
      <Empty testid="run-monitor-no-run">
        No run open. Pick one on the left — this tab shows the Machines under a run, and there is no
        run for it to be about.
      </Empty>
    );
  }

  if (machines.length === 0) {
    return (
      <div
        className="min-h-0 flex-1 overflow-y-auto p-4"
        data-testid="run-monitor"
        data-run={run.runId}
        data-machines="0"
      >
        {!read ? (
          // NOBODY LOOKED. The account is what names the Actors, and it has not arrived — so the
          // honest sentence is about the reading, never about the run.
          <Empty testid="run-monitor-unread">
            Reading {run.runId}’s account to find out which Actors it called. Until then nothing here
            can honestly say whether it has any Machines.
          </Empty>
        ) : inventory === 0 ? (
          // A FACT ABOUT THE APPLIANCE, NOT ABOUT THIS RUN, and it has its own fix.
          <Empty testid="run-monitor-no-inventory">
            <strong className="font-semibold text-foreground">
              The Monitor can see no Terminals at all.
            </strong>{' '}
            Not just this run’s — the streamer reports an empty inventory, so there is nothing here
            for any run. A Worker served with <span className="font-mono">--tmux</span> appears
            within about 30 seconds of the streamer rediscovering sessions.
          </Empty>
        ) : scopeNames(scope).length === 0 ? (
          // NOTHING TO LOOK FOR. Ordinary: a run that only read a Dataset dispatches to no Actor and
          // brings up no fleet, and has no Machines of its own by construction.
          <Empty testid="run-monitor-nothing-named">
            <strong className="font-semibold text-foreground">
              This run names no Machine of its own.
            </strong>{' '}
            Its account records no dispatch to an Actor and no fleet operation, so there is no
            Terminal that belongs to it. That is a different answer from a run whose Machines are
            gone.
          </Empty>
        ) : (
          // THE INTERESTING ABSENCE: we know exactly what to look for and none of it is there. This
          // is what a worker that booted and died looks like from this tab, so the names are printed
          // rather than summarised.
          <div data-testid="run-monitor-none">
            <p className="m-0 text-[12.5px]">
              <strong className="font-semibold">
                None of this run’s Machines is on the Monitor.
              </strong>{' '}
              {inventory} Terminal{inventory === 1 ? ' is' : 's are'} in the inventory and none of
              them is serving anything this run named.
            </p>
            <p className="m-0 mt-1.5 text-[11.5px] text-muted-foreground">
              Looked under:{' '}
              {scopeNames(scope).map((name, i) => (
                <span key={name}>
                  {i > 0 ? ', ' : ''}
                  <code className="font-mono" data-testid={`run-monitor-wanted-${name}`}>
                    {name}
                  </code>
                </span>
              ))}
              .
            </p>
            <p className="m-0 mt-1.5 text-[11.5px] text-muted-foreground">
              A worker that booted and died leaves {run.runId} simply waiting, with no status on this
              page changing — and a session the streamer has not rediscovered yet looks identical for
              about 30 seconds.
            </p>
          </div>
        )}
      </div>
    );
  }

  return (
    <div
      className="flex min-h-0 flex-1 flex-col overflow-hidden"
      data-testid="run-monitor"
      data-run={run.runId}
      data-machines={machines.length}
    >
      {/* THE SCOPE, ON ONE LINE. It says how many Machines this run has and NOT how many exist,
          because the number an operator is checking against is the fleet they asked for. The whole
          inventory is one number beside it so the scoping is visible rather than implied — a tab
          that silently showed four of sixty would look like a Monitor that had lost fifty-six. */}
      <div
        className="flex shrink-0 items-stretch gap-1 overflow-x-auto border-b border-border px-2 py-1"
        data-testid="run-monitor-rail"
      >
        <span className="flex shrink-0 items-center pr-1 text-[10px] uppercase tracking-wide text-muted-foreground">
          {machines.length} of {inventory}
        </span>
        {machines.map((m) => (
          <MachineChip
            key={m.terminal.id}
            machine={m}
            focused={focused?.terminal.id === m.terminal.id}
            onFocus={onFocus}
          />
        ))}
      </div>

      {focused && (
        <p
          className="m-0 shrink-0 border-b border-border px-4 py-1 text-[10.5px] text-muted-foreground"
          data-testid="run-monitor-because"
        >
          <code className="font-mono">{focused.terminal.id}</code> — {focused.because}.{' '}
          {modeStakes(tileRefFor(focused.terminal).mode).sentence}
        </p>
      )}

      <div className="flex min-h-0 flex-1 flex-col overflow-hidden" data-testid="run-monitor-pane">
        {pane}
      </div>
    </div>
  );
}

function MachineChip({
  machine,
  focused,
  onFocus,
}: {
  machine: RunMachine<Terminal>;
  focused: boolean;
  onFocus: (id: string) => void;
}): JSX.Element {
  const ref = tileRefFor(machine.terminal);
  const reading = terminalHealthReading(machine.terminal);
  const word = healthWord(reading);
  return (
    <button
      type="button"
      data-testid={`run-monitor-machine-${machine.terminal.id}`}
      data-reason={machine.reason}
      data-health={word}
      data-focused={focused ? 'true' : undefined}
      aria-current={focused ? 'true' : undefined}
      title={`${machine.because}. ${modeStakes(ref.mode).sentence}`}
      onClick={() => onFocus(machine.terminal.id)}
      className={`flex shrink-0 items-center gap-1.5 rounded border px-2 py-0.5 text-left outline-none hover:bg-accent/60 focus-visible:ring-2 focus-visible:ring-ring/50 ${
        focused ? 'border-foreground bg-accent' : 'border-border'
      }`}
    >
      {/* THREE STATES, NEVER TWO. An unmeasured pane is drawn dashed rather than in a paler green:
          "we never looked" reading as "fine" across a row of Machines is the collapse ADR 0020
          forbids, and it is the one this tab would make first. */}
      <span
        className={`size-[6px] shrink-0 rounded-full ${
          reading === true
            ? 'bg-emerald-400'
            : reading === false
              ? 'bg-rose-500'
              : 'border border-dashed border-muted-foreground'
        }`}
      />
      <span className="font-mono text-[10.5px]">{ref.node}</span>
      <span className="font-mono text-[9.5px] text-muted-foreground">
        {ref.session}/{ref.window}
      </span>
      <span className="rounded bg-muted px-1 text-[9px] uppercase tracking-wide text-muted-foreground">
        {REASON_LABEL[machine.reason]}
      </span>
    </button>
  );
}

function Empty({ testid, children }: { testid: string; children: ReactNode }): JSX.Element {
  return (
    <p className="m-0 p-4 text-[12.5px] text-muted-foreground" data-testid={testid}>
      {children}
    </p>
  );
}

/**
 * The Datasets this run wrote, each one click from the run that made it.
 *
 * THE OUTPUT AND THE RUN THAT MADE IT WERE TWO SEARCHES. A run said `completed` and an operator went
 * to the Datasets surface to find out what that meant, with only a timestamp to match on. The
 * catalog carries the Run now (ADR 0029 §2), so the link is a filter over a listing the app already
 * holds — and it lands on the Dataset ALREADY SCOPED to this run, so "623 of 1,246 rows" is a
 * sentence about the run you came from rather than a number to find again.
 *
 * IT LIVES BESIDE THE MONITOR because both answer "what did this run actually do" from the two ends
 * — the Machines under it and what came out — and neither is derivable from the other.
 *
 * COMPACT BY DEFAULT, WITH THE ROWS ONE TWISTY AWAY (issue 25). The summary — name, attribution,
 * lifecycle, count, link — is the right default and is untouched. Under each row is a `<details>`
 * that tails the last ten rows inline (`DatasetTail`), because "what do the rows actually look like"
 * is the one question the summary cannot answer and it used to cost a navigation. The link is still
 * the way to the WHOLE Dataset: ten rows is a look, and asking a question of it is the console's job.
 * This function holds none of that expander's state, which is what keeps a row landing in one
 * Dataset's tail from re-rendering the list it is in.
 */
export function RunDataTab({
  run,
  datasets,
  onOpen,
}: {
  run: RunRow | null;
  /** The filter over the catalog the app already holds — see `runDatasets.ts`. */
  datasets: RunDatasets;
  /** Open it on the Datasets surface, scoped to this run. One call, because arriving unscoped is
   *  the second search this link exists to remove. */
  onOpen: (info: DatasetInfo) => void;
}): JSX.Element {
  const { wrote, partial, read } = datasets;
  if (!run) {
    return (
      <Empty testid="run-data-no-run">
        No run open. Pick one on the left — this tab is what one run wrote.
      </Empty>
    );
  }
  return (
    <div
      className="min-h-0 flex-1 overflow-y-auto p-4"
      data-testid="data-tab"
      data-run={run.runId}
      data-datasets={wrote.length}
    >
      {!read ? (
        <Empty testid="run-data-unread">
          Reading the Dataset catalog. Until it answers, nothing here can say whether{' '}
          <code className="font-mono">{run.runId}</code> wrote anything.
        </Empty>
      ) : wrote.length === 0 ? (
        <p className="m-0 text-[12.5px] text-muted-foreground" data-testid="run-data-none">
          <strong className="font-semibold text-foreground">
            No Dataset in the lake carries this run.
          </strong>{' '}
          A run that produced nothing and a run that failed are two different facts, and only the
          ledger can tell them apart (ADR 0017) — this says the narrower thing it can: nothing in the
          catalog is filed under <code className="font-mono">{run.runId}</code>.
          {partial && (
            <>
              {' '}
              <span data-testid="run-data-partial">
                And at least one Dataset cannot list its Runs exhaustively, so this is a floor rather
                than a measurement.
              </span>
            </>
          )}
        </p>
      ) : (
        <ul className="m-0 flex list-none flex-col gap-1.5 p-0">
          {wrote.map(({ info, how }) => {
            const words = attributionWords(how);
            const badge = datasetBadge(datasetState(info));
            return (
              <li key={`${info.kind}:${info.name}`}>
                <button
                  type="button"
                  data-testid={`run-dataset-${info.name}`}
                  data-how={how}
                  data-rows={info.rows}
                  title={`${words}. Opens scoped to ${run.runId}.`}
                  onClick={() => onOpen(info)}
                  className="flex w-full items-baseline gap-2 rounded border border-border px-2 py-1 text-left outline-none hover:bg-accent/60 focus-visible:ring-2 focus-visible:ring-ring/50"
                >
                  <span className="min-w-0 flex-1 truncate font-mono text-[11.5px]">{info.name}</span>
                  {/* THE SHARED BADGE, so an `open` Dataset reads the same here as it does on the
                      surface it links to. `no lifecycle` is its own state, never folded into
                      `sealed`: a Dataset a run is still appending to drawn as the finished article
                      is the one failure §11 exists to prevent. */}
                  <span
                    data-testid={`run-dataset-state-${info.name}`}
                    data-state={datasetState(info)}
                    title={badge.title}
                    className={`shrink-0 rounded px-1 text-[9px] uppercase tracking-wide ${badge.className}`}
                  >
                    {badge.label}
                  </span>
                  <span className="shrink-0 font-mono text-[10.5px] tabular-nums text-muted-foreground">
                    {info.rows.toLocaleString()} rows
                  </span>
                  <ExternalLink size={10} className="shrink-0 text-muted-foreground" />
                </button>
                <p className="m-0 mt-0.5 px-2 text-[10px] text-muted-foreground">{words}</p>
                {/* AND THE ROWS THEMSELVES, ONE TWISTY AWAY. Everything above is unchanged and stays
                    the default — the summary is what answers "did this run write anything, and where
                    is it" at a glance. The expander answers the one question a summary cannot, and it
                    holds ALL of its own state (`DatasetTail` says why), so the list above re-renders
                    neither when it opens nor when its rows arrive. */}
                <DatasetTail info={info} how={how} />
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
