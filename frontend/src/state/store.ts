/** App state: the Actor catalog, the Runs list, the current surface, and the theme.
 *
 * The graph editor's state went with the interpreter (ADR 0023 §12) — nodes, edges, handles,
 * node status painting, document normalisation, the undo stack. None of it had a producer left:
 * a Run is one execution of a CALLER's workflow now, and the caller owns its own loop, so there
 * is no graph for the UI to hold and nothing to paint statuses onto.
 *
 * What survives is what still has a source of truth on the server: the catalog every worker
 * self-registers into, the Runs Temporal knows about, and the Datasets a Run publishes.
 *
 * THIS STORE IS THE NAVIGATION API AND THE URL IS ITS PROJECTION (`address.ts`). `setView`,
 * `openRun`, `setRunId`, `openWorkflow`, `openDataset` and `watchTerminal` keep the names and
 * signatures every caller in `panels/` already uses — they additionally write the address bar.
 * Nothing in `panels/` knows a URL exists, and nothing there had to change for one to appear.
 */

import { create } from 'zustand';

import { addressBar } from './addressBar';
import { addressOf, formatAddress, type AddressedState } from './address';
import { DEFAULT_VIEW, type View } from './surfaces';
import {
  fetchCatalog,
  fetchDatasets,
  fetchFleetOperations,
  fetchPulse,
  fetchRuns,
  type CatalogActor,
  type DatasetInfo,
  type FleetOperationRow,
  type Pulse,
  type RunRow,
} from '../run/api';
import { push, rate } from '../components/spark';
import type { ParkedRun } from '../panels/ask';
import { fetchTerminals, type Terminal as PaneTerminal } from '../panels/panelsClient';

/**
 * The surfaces themselves live in `surfaces.ts`, which is the one list the nav rail, the router and
 * the server's SPA fallback all read. Re-exported here because every caller in `panels/` already
 * imports `View` from the store and none of them should have to learn a second module to keep
 * working.
 *
 *    workflows   YOUR caller workflows — the DEFINITIONS, each one's runs, and everything one run
 *                did. A run is reached THROUGH the workflow that produced it: a conversation
 *                belongs to a thread, and the global Runs list made a run id and the code that
 *                produced it two different searches.
 *    actors      what is deployed and registered — one card per Actor, its Methods, and each
 *                Method's typed input and output. The thing a caller has to type.
 *    datasets    what runs produced, and the lists you loaded — with the SQL console on the inside
 *                of a dataset rather than beside it.
 *    monitor     a wall of read-only Terminals over tmux (ADR 0020) — its own surface rather than a
 *                panel because it talks to a DIFFERENT origin (the streamer's port), and because
 *                its tiles must not sit in a transformed or zoomable container: xterm's measured
 *                cols/rows is what a session is sized to.
 *    settings    secrets and configuration. New, and the first home credentials have ever had here.
 *
 *  THERE IS NO `query` SURFACE, and folding it in was a correction rather than a tidy-up. It was a
 *  nav item whose schema tree listed the same datasets the Datasets page listed, so an operator
 *  picked a dataset twice — once to look at it and again, by name, to query it. Opening a dataset
 *  now IS opening its console: the schema is that dataset's, and `SELECT * FROM <it>` is already in
 *  the editor. `run/query.ts` is unchanged; only where it is reached from moved.
 *
 *  THERE IS NO `runs` SURFACE AND NO `scratch` SURFACE. Both were retired rather than deleted —
 *  their addresses still parse and still redirect (`address.ts`), because `/runs/nscheck-123` is in
 *  somebody's notes and it still names a run. Scratch comes back as a workflow's own design tab,
 *  where a drawing finally has a subject.
 */
export type { View } from './surfaces';

/** The two palettes the shadcn theme flips between. */
export type Theme = 'light' | 'dark';

/** Which Dataset to open, and — when the operator arrived from one — which **Run** to scope it to. */
export interface DatasetFocus {
  name: string;
  /** Defaults to actor output; a standalone list says so, because they are addressed separately. */
  kind?: 'output' | 'standalone';
  /** The Run whose contribution to this Dataset the operator was reading. */
  run?: string;
}

/** What the nav rail's footer counts. Published by the Monitor while it is mounted, and polled at
 * the app level otherwise — see {@link AppState.loadPanes}. */
export interface WallCounts {
  /** Terminals in the inventory. Every one of them is a tile on the wall. */
  panes: number;
  /** Terminals holding a real PTY attach. Zero whenever the Monitor is not mounted, which is the
   * truth: closing the socket kills every grouped session on the nodes. */
  live: number;
}

interface AppState {
  theme: Theme;
  setTheme: (t: Theme) => void;

  view: View;
  setView: (v: View) => void;

  /** Auto-discovered from the server: whatever is deployed and registered. No upload. */
  catalog: CatalogActor[];
  loadCatalog: () => Promise<void>;

  /** Runs, newest first, as Temporal knows them. */
  runs: RunRow[];
  runsError: string | null;
  loadRuns: () => Promise<void>;

  /**
   * IS ANYTHING HAPPENING AT ALL, ANYWHERE — the one fact the chrome carries on every surface.
   *
   * IT IS NOT DERIVED FROM {@link AppState.runs}, AND THAT SEPARATION IS THE WHOLE POINT. The rail
   * used to answer "is anything running" by filtering the run list, which meant every session — on
   * the Datasets page, on the Monitor, on nothing at all — polled a page of up to 200 executions
   * with a ledger read per row and a describe per open row, every few seconds, for a two-digit
   * number. That cost grows with how many runs this controller has EVER held, and the question is
   * about right now. `/api/pulse` is two bounded reads that do not move with history.
   *
   * `null` IS "NEVER READ", not "nothing running". The rail draws nothing at all until the first
   * answer arrives — see `panels/chrome/pulse.ts`.
   */
  pulse: Pulse | null;
  /**
   * Why the last pulse failed, or `null`.
   *
   * KEPT, LIKE `runsError` AND FOR A SHARPER REASON. A rail that renders "idle" because Temporal
   * could not be reached is the worst sentence this app can produce: it is a confident all-clear
   * over an appliance nobody can see into. The error is what turns that into "not known".
   */
  pulseError: string | null;
  loadPulse: () => Promise<void>;

  /**
   * The runs KNOWN to be waiting on a human — the chrome's copy of a fact a page learned.
   *
   * IT IS IN THE STORE AND NOT ON THE WORKFLOWS PAGE BECAUSE SURFACES SWAP. `App.tsx` renders one
   * surface at a time, so a page that unmounts takes its state with it — and the requirement is
   * exactly that an operator on the Datasets or Monitor surface is told they are the bottleneck.
   * The fact has to outlive the page that learned it, so it lives where the rail can read it.
   *
   * ONLY EVER WHAT WAS READ. A park lives on the run's own memo, so knowing which of two hundred
   * runs is parked would be two hundred describes; what is known is the run whose transcript was
   * open, whose asks were fetched anyway. A run absent from here is a run nothing has been read
   * about — never one this has decided is un-parked. Same rule as `runWord`'s, one layer up.
   *
   * PRUNED AGAINST THE RUN LIST BY ITS READER (`stillParked`), because the entry outlives the page
   * and a closed run is never parked: an ask left `pending` on a run that failed is a question
   * nobody will ever answer, and a live-looking mark for it is worse than saying nothing.
   */
  parked: ParkedRun[];
  setParked: (runId: string, parked: ParkedRun | null) => void;

  /** Fleet operations, newest first — bring-ups, previews and teardowns. Beside the runs rather
   *  than among them: a Fleet operation is not a Run (ADR 0017 — two authorities, two fields). */
  fleetOps: FleetOperationRow[];
  fleetOpsError: string | null;
  loadFleetOps: () => Promise<void>;

  /**
   * The Run whose detail is open, if any. Its id IS the caller's workflow id (ADR 0023 §12), and
   * it is the ONLY thing the detail needs: which definition produced it, whether the list has
   * discovered it yet, and whether Temporal still holds its history are all separate questions.
   */
  runId: string | null;
  setRunId: (id: string | null) => void;
  /**
   * Open a Run from anywhere. THIS IS WHAT MAKES THE RUN BUTTON STILL END IN A RUN: `startRun`
   * returns the caller's workflow id, and the working loop is edit → Run → watch, so the surface
   * change and the id always happen together — a `setRunId` with no surface change is a button that
   * appears to do nothing. Same shape as {@link AppState.watchTerminal}.
   *
   * IT LANDS ON WORKFLOWS NOW, not on a Runs surface, and it does NOT touch
   * {@link AppState.workflowName}. Called from the Workflows page the thread is already open and
   * the address becomes `/workflows/<workflow>/<run>`; called from anywhere else nothing in the
   * browser knows which workflow produced the run, and `/workflows?run=<id>` says exactly that
   * rather than guessing a thread.
   */
  openRun: (id: string) => void;

  /**
   * The **Terminal** the open run's Monitor tab is showing.
   *
   * ADDRESSED, LIKE THE RUN ABOVE IT, and for the same reason: the Machines under a run are what an
   * operator pastes to a colleague when a worker has died, and a selection the URL cannot say is a
   * tab that comes back on the first Machine after every reload. It rides as `?pane=` rather than as
   * a fourth path segment — `address.ts` says why a Terminal id must stay out of the path.
   *
   * IT IS NOT {@link AppState.focusTerminal}. That one is a REVEAL on the Monitor surface, consumed
   * once and cleared; this is a SELECTION inside a run, and it persists. Two names because they are
   * two things — folding them would make "watch this on the wall" silently repoint the tab an
   * operator was reading.
   *
   * CLEARED WHENEVER THE RUN OR THE THREAD CHANGES, because a Terminal id from the run before last
   * names a Machine that is not this run's, and drawing it under this run's heading is the same lie
   * `workflowThread.ts`'s `transcriptFor` refuses one layer up.
   */
  runPane: string | null;
  focusRunPane: (id: string | null) => void;

  /**
   * Which workflow's thread is open, if any — the folder name the Workflows surface has selected.
   *
   * IT IS ADDRESSED, WHICH IS WHY IT IS HERE AND NOT IN THE PAGE'S OWN `useState`. `/workflows/
   * dnssweep` is the link an operator pastes, and a selection the URL cannot say is a page that
   * comes back on the first row after every reload. `null` is the list with nothing open, which is
   * a real place and not a missing value.
   */
  workflowName: string | null;
  /** Open one workflow's thread. Same shape as {@link AppState.openRun} — the surface and the id
   *  always move together. */
  openWorkflow: (name: string) => void;

  /** How many files are in `.kontra/workflows/`, published by the Workflows surface when it reads
   *  the directory. `null` until then — the rail prints nothing rather than a 0 it has not earned,
   *  because "no workflows" is a real and actionable state and must not be shown to a session that
   *  simply has not looked yet. */
  workflowCount: number | null;
  setWorkflowCount: (n: number) => void;

  /** How many saved Scratches there are, on the same terms as `workflowCount`. */
  scratchCount: number | null;
  setScratchCount: (n: number) => void;

  /**
   * How many registered ACTOR folders there are — what the Actors page lists.
   *
   * NOT `catalog.length`, which is what the rail printed: every Actor any worker ever registered
   * about itself, on any machine. On this installation that is 23 against a page drawing 1, and a
   * rail whose number disagrees with the surface it labels teaches the reader to distrust both.
   * Same rule the `datasets` count already follows — the inventory of a surface is the thing that
   * surface lists. `null` until the page has read them.
   */
  actorFolderCount: number | null;
  setActorFolderCount: (n: number) => void;

  /**
   * Every Dataset, and how fast rows are landing in them.
   *
   * ONE POLLER, not one per surface. Three surfaces want this listing — a run detail watches
   * its own Datasets fill, the Datasets page lists everything, the rail draws throughput — and three
   * independent intervals over the same catalog scan is three times the cost for a number that
   * would then disagree with itself between panels. It also makes the derived SERIES possible at
   * all: a rate needs two samples taken by the same clock.
   */
  datasets: DatasetInfo[];
  /** Kept, not swallowed: "the lake is unreachable" and "nothing has been written" are different
   *  answers and only one of them is normal. */
  datasetsError: string | null;
  /** Rolling rows-per-second across every Dataset — the rail's "fleet throughput". Empty until two
   *  samples exist, because one sample is not a rate. */
  fleetSeries: number[];
  /** The newest sample of the above, as a number to print beside the line. */
  unitsPerSec: number;
  /** Per-dataset rows-per-second, keyed by `kind:name:version:dt`. */
  datasetSeries: Record<string, number[]>;
  /**
   * When the listing above was measured (epoch ms), `0` before the first poll.
   *
   * IT TRAVELS WITH THE NUMBERS BECAUSE THE NUMBERS ARE ABOUT A MOMENT. A Dataset a **Run** is
   * still appending to grows between two polls, so a row count from this listing is true as of
   * this stamp and not afterwards — and a count from a DIFFERENT statement (a run's ledger, a
   * provenance scan) may never be divided into one from this one. `datasets/scope.ts` makes that
   * refusal mechanical; this is the half of it the store owns.
   */
  datasetsAt: number;
  loadDatasets: () => Promise<void>;

  /**
   * The Dataset the Datasets surface should open when it next renders, and the Run to scope it to.
   *
   * THIS IS THE OTHER HALF OF THE ROUND TRIP. From a Dataset an operator reaches the Runs that
   * wrote it; from a Run they reach the Dataset's total — and landing on the Dataset already
   * scoped to the Run they came from is what makes "623 of 1,246 rows" a sentence about the run
   * they were just looking at rather than a number they have to find again. Same shape as
   * {@link AppState.focusTerminal}: consumed once and cleared, because it is a navigation and not
   * a selection.
   */
  datasetFocus: DatasetFocus | null;
  /** Go to the Datasets surface and open one Dataset. The two always happen together. */
  openDataset: (focus: DatasetFocus) => void;
  clearDatasetFocus: () => void;

  /**
   * The Terminal inventory itself, not just its size.
   *
   * KEPT BECAUSE TWO OTHER SURFACES ASK REAL QUESTIONS OF IT. The Actors page wants to know how
   * many Machines are running each Actor — which is the one live number about an Actor this system
   * can honestly report, since nothing on the registration path carries a call rate — and the
   * Monitor's filter bar builds its actor / address / session menus from it. Both used to be
   * impossible without mounting the Monitor, which is exactly the surface an operator is not on
   * when they ask.
   */
  panes: PaneTerminal[];

  wall: WallCounts;
  /** Partial, so the Monitor can give up its `live` claim on unmount without also blanking `panes`
   * — the inventory is still there, and a rail that flashed "0 panes" on every navigation away
   * would read as a Fleet that had just gone. */
  setWallCounts: (counts: Partial<WallCounts>) => void;
  /** Published by the Monitor while it is mounted — it holds a fresher inventory than the app-level
   *  poll, and two sources for one list would let the Actors page disagree with the wall. */
  setPanes: (panes: PaneTerminal[]) => void;
  /** Count the Terminals without mounting the Monitor, so the rail's `panes` is honest on a session
   * that never opens it. Best-effort: an unreachable streamer leaves the last count alone rather
   * than reporting zero Machines, which is a different and much more alarming statement. */
  loadPanes: () => Promise<void>;

  /**
   * The Terminal the Monitor should reveal when it next renders.
   *
   * THIS IS WHAT MAKES SERVE AND RUN RETURN A LINK. `kontra workflow serve` puts a worker in a tmux
   * session on the host, and that session IS a Terminal on the wall (`LOCAL_SESSION_PREFIX`) — so
   * "watch it" is a surface change plus an id, and the Monitor scrolls to the tile and flashes it.
   * Consumed once and cleared, because a reveal is a flash and not a selection.
   */
  focusTerminal: string | null;
  /** Go to the Monitor and reveal one Terminal. The two always happen together — a focus with no
   * surface change is a link that appears to do nothing. */
  watchTerminal: (id: string) => void;
  clearFocusTerminal: () => void;
}

/** Stable identity for a Dataset — the same triple that addresses it in the catalog. Duplicated
 *  from DatasetPage's `keyOf` deliberately: this module must not import a lazy-loaded surface. */
export function datasetKey(d: Pick<DatasetInfo, 'kind' | 'name' | 'version' | 'dt'>): string {
  return `${d.kind}:${d.name}:${d.version ?? ''}:${d.dt ?? ''}`;
}

/** The previous sample, held outside the store because nothing renders it — writing it into state
 *  would re-render every subscriber on each poll to no visible effect. */
let lastSample: { at: number; total: number; byKey: Map<string, number> } | null = null;

/**
 * Say in the address bar where the store just went.
 *
 * ONLY THE SEVEN NAVIGATION ACTIONS CALL THIS, AND THAT IS THE WHOLE DESIGN. The obvious
 * alternative — subscribe to the store and mirror every change into the bar — is the trap in this
 * seam: four pollers write this store every two to thirty seconds (`loadRuns`, `loadDatasets`,
 * `loadPanes`, `loadCatalog`, plus the Monitor's `setPanes`/`setWallCounts` on every socket
 * message), and a mirror that pushed for any of them would bury the operator's actual navigation
 * under hundreds of entries and make Back useless within a minute. A poll cannot reach this
 * function at all, which is stronger than a poll being filtered out by it.
 *
 * THE EQUALITY CHECK IS THE SECOND GUARD, and it is not redundant. Clicking the Actors rail item
 * while already on Actors, or `setRunId` firing on a page whose address does not carry a run id, or
 * `openDataset` for the Dataset already open, all end here with the address unchanged — and an
 * entry per idempotent click is the same broken Back by a slower route.
 *
 * PUSH, NEVER REPLACE. Every one of the seven is an operator asking to be somewhere else, which is
 * the definition of a history entry. The only `replace` in the app is `addressing.ts`
 * canonicalising an address on arrival — a correction to a place already visited, which is also
 * what turns a retired `/runs/<id>` into its new address without leaving the dead one behind it.
 */
function addressed(s: AddressedState): void {
  const bar = addressBar();
  const url = formatAddress(addressOf(s));
  if (url === bar.url()) return;
  bar.push(url);
}

/** Two parked-run entries that say the same thing. Field by field, because the whole point is to
 *  tell a re-read that found the same park from one that found a new question. */
function samePark(a: ParkedRun | null, b: ParkedRun | null): boolean {
  if (a === null || b === null) return a === b;
  return (
    a.runId === b.runId && a.workflow === b.workflow && a.pending === b.pending && a.since === b.since
  );
}

export const useAppStore = create<AppState>((set, get) => ({
  theme: 'dark',
  setTheme: (theme) => set({ theme }),

  // The cold-load default, and it is only ever what the app shows when the address says nothing
  // else: `addressing.ts` overwrites this from the URL before the first render.
  view: DEFAULT_VIEW,
  setView: (view) => {
    set({ view });
    addressed(get());
  },

  catalog: [],
  loadCatalog: async () => {
    // Server is the source of truth. A null result means the backend was unreachable — leave
    // the current catalog rather than blanking it on a transient failure; an empty array is a
    // real "nothing deployed" answer and does replace.
    const actors = await fetchCatalog();
    if (actors) set({ catalog: actors });
  },

  runs: [],
  runsError: null,
  // Same rule as the catalog, plus the error is KEPT: "Temporal is unreachable" and "no runs
  // yet" are different answers, and rendering an empty list for the first is the run-status
  // failure this whole surface was reshaped to stop telling.
  loadRuns: async () => {
    try {
      set({ runs: await fetchRuns(), runsError: null });
    } catch (err) {
      set({ runsError: err instanceof Error ? err.message : String(err) });
    }
  },

  pulse: null,
  pulseError: null,
  // THE LAST GOOD READING SURVIVES A FAILED POLL, and the error rides beside it rather than
  // replacing it — the reading then says "not known", and says what it last saw and how long ago.
  // Blanking the pulse instead would lose the one piece of context that makes an outage legible.
  loadPulse: async () => {
    try {
      set({ pulse: await fetchPulse(), pulseError: null });
    } catch (err) {
      set({ pulseError: err instanceof Error ? err.message : String(err) });
    }
  },

  parked: [],
  // ONE RUN'S ENTRY AT A TIME, REPLACED OR REMOVED. Keyed by run id rather than appended, so a
  // page re-publishing what it just read cannot grow the list, and answering the last pending ask
  // on a run clears its mark by passing `null` rather than by leaving a stale one behind.
  setParked: (runId, entry) =>
    set((s) => {
      const had = s.parked.find((p) => p.runId === runId) ?? null;
      // NOTHING CHANGED IS NOTHING PUBLISHED. zustand compares by identity, and the page that
      // feeds this re-publishes on every arrival — a fresh array each time would re-render the
      // rail, and the surface beside it, for a fact that did not move.
      if (samePark(had, entry)) return {};
      const rest = s.parked.filter((p) => p.runId !== runId);
      return { parked: entry ? [...rest, entry] : rest };
    }),

  fleetOps: [],
  fleetOpsError: null,
  // Same rule again, and it matters more here than anywhere: a fleet costs money by the hour, so
  // "the cluster could not be asked" must never render as "you have nothing running".
  loadFleetOps: async () => {
    try {
      set({ fleetOps: await fetchFleetOperations(), fleetOpsError: null });
    } catch (err) {
      set({ fleetOpsError: err instanceof Error ? err.message : String(err) });
    }
  },

  runId: null,
  // Still no surface change — see the doc comment — but it IS the last segment of a Workflows
  // address, so the bar follows it while that surface is the one on screen and ignores it while it
  // is not.
  setRunId: (runId) => {
    set({ runId, runPane: null });
    addressed(get());
  },
  openRun: (runId) => {
    set({ runId, runPane: null, view: 'workflows' });
    addressed(get());
  },

  runPane: null,
  focusRunPane: (runPane) => {
    set({ runPane });
    addressed(get());
  },

  workflowName: null,
  // OPENING A THREAD CLOSES THE CONVERSATION THAT WAS IN THE LAST ONE. A run id left over from the
  // previous workflow would address `/workflows/<new>/<old run>` — a URL that reads as a run of a
  // workflow that never produced it, which is worse than an empty panel because it looks like an
  // answer.
  openWorkflow: (workflowName) => {
    set({ workflowName, runId: null, runPane: null, view: 'workflows' });
    addressed(get());
  },

  workflowCount: null,
  setWorkflowCount: (workflowCount) => set({ workflowCount }),
  scratchCount: null,
  setScratchCount: (scratchCount) => set({ scratchCount }),
  actorFolderCount: null,
  setActorFolderCount: (actorFolderCount) => set({ actorFolderCount }),

  datasets: [],
  datasetsError: null,
  fleetSeries: [],
  unitsPerSec: 0,
  datasetSeries: {},
  datasetsAt: 0,
  datasetFocus: null,
  openDataset: (datasetFocus) => {
    set({ datasetFocus, view: 'datasets' });
    addressed(get());
  },
  // CONSUMING A NAVIGATION IS NOT A NAVIGATION, so this does NOT touch the bar — and that is the
  // difference between a Dataset link that survives a reload and one that does not. The Datasets
  // surface clears the focus the moment it has resolved it against the catalog; if the bar mirrored
  // that it would fall back to `/datasets` a poll-tick after arriving, and F5 on a pasted link would
  // land on the listing. The address is where you were sent, not what the page has finished doing
  // with it.
  clearDatasetFocus: () => set({ datasetFocus: null }),
  loadDatasets: async () => {
    let listing: DatasetInfo[];
    try {
      listing = await fetchDatasets();
    } catch (err) {
      set({ datasetsError: err instanceof Error ? err.message : String(err) });
      return;
    }
    const at = Date.now();
    const byKey = new Map(listing.map((d) => [datasetKey(d), d.rows]));
    const total = listing.reduce((sum, d) => sum + d.rows, 0);
    const prev = lastSample;
    lastSample = { at, total, byKey };
    if (!prev) {
      // FIRST SAMPLE, so there is no rate yet — and the series stays empty rather than starting
      // with a zero. A leading zero would be drawn as a measured trough, and the first thing an
      // operator would see on a page opened mid-sweep is a line climbing out of "nothing".
      set({ datasets: listing, datasetsError: null, datasetsAt: at });
      return;
    }
    const elapsed = at - prev.at;
    set((s) => {
      const series: Record<string, number[]> = {};
      for (const [key, rows] of byKey) {
        series[key] = push(s.datasetSeries[key] ?? [], rate(prev.byKey.get(key) ?? rows, rows, elapsed));
      }
      const fleet = push(s.fleetSeries, rate(prev.total, total, elapsed));
      return {
        datasets: listing,
        datasetsError: null,
        datasetsAt: at,
        datasetSeries: series, // departed datasets drop out with their series — no unbounded map
        fleetSeries: fleet,
        unitsPerSec: fleet[fleet.length - 1] ?? 0,
      };
    });
  },

  panes: [],
  wall: { panes: 0, live: 0 },
  setWallCounts: (counts) => set((s) => ({ wall: { ...s.wall, ...counts } })),
  setPanes: (panes) => set((s) => ({ panes, wall: { ...s.wall, panes: panes.length } })),
  loadPanes: async () => {
    try {
      const terminals = await fetchTerminals();
      set((s) => ({ panes: terminals, wall: { ...s.wall, panes: terminals.length } }));
    } catch {
      /* unreachable streamer — see the doc comment; the last inventory stands */
    }
  },

  focusTerminal: null,
  watchTerminal: (focusTerminal) => {
    set({ focusTerminal, view: 'monitor' });
    addressed(get());
  },
  /** Same rule as {@link AppState.clearDatasetFocus}: the reveal is finished, the address is not. */
  clearFocusTerminal: () => set({ focusTerminal: null }),
}));
