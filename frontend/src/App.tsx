/** App shell: the nav rail down the left, then the current surface.
 *
 * FIVE SURFACES, and Workflows is the centre. A workflow is a thread, each of its runs is a
 * conversation, and everything else here is what a run calls (**Actors**), what it produces
 * (**Datasets**), the machines under it (**Monitor**), and what the whole appliance is configured
 * with (**Settings**).
 *
 * TWO SURFACES WERE RETIRED, and neither was deleted from the address space.
 *
 *  - **Runs.** The global list made a run id and the code that produced it two different searches,
 *    so a run is reached through its workflow now. `/runs/<id>` still parses, still names that run,
 *    and REDIRECTS — see `state/address.ts`. It does not 404, because that address is in somebody's
 *    notes.
 *  - **Scratch.** A drawing surface with no subject (ADR 0026), which is why nobody opened it. It
 *    comes back as a workflow's own design tab. `/scratch` redirects to the list.
 *
 * THEIR PAGE COMPONENTS ARE GONE NOW, and this note is what is left of them. `RunsPage.tsx` and
 * `ScratchPage.tsx` were kept unrouted while their replacements were built, so the material could be
 * moved rather than thrown away; the move is done. A run's full account — transcript, drill, fleet
 * turns — is `Transcript.tsx`/`TranscriptDrill.tsx` under `/workflows/<workflow>/<run>`, and the
 * sketch is `WorkflowSketch.tsx` on that same surface's design tab. Two answers to "where does a
 * run's event log render", one of them unreachable, was the reason to finish it.
 *
 * The three-pane graph editor — catalog | canvas | inspector — went with the interpreter (ADR 0023
 * §12). There is no graph to edit: a Run is one execution of a caller's workflow and the caller
 * owns its loop.
 *
 * QUERY IS NOT A SURFACE. It is what a Dataset looks like when you open it — see `state/store.ts`'s
 * `View`.
 */

import { lazy, useEffect } from 'react';
import { Shell } from './panels/Shell';
import { useAppStore } from './state/store';

// Every surface is lazy. The heavy ones earn it outright — Datasets pulls in AG Grid and the
// CodeMirror SQL editor, and Monitor pulls xterm.js plus its CSS and talks to a DIFFERENT origin,
// so a session that never opens it makes no cross-origin request at all. The light ones are lazy
// for consistency: which surface is heavy should not be visible in this file's structure.
const WorkflowsPage = lazy(() => import('./panels/WorkflowsPage'));
const ActorsPage = lazy(() => import('./panels/ActorsPage'));
const DatasetPage = lazy(() => import('./panels/DatasetPage'));
const MonitorPage = lazy(() => import('./panels/DashboardPage'));
const SettingsPage = lazy(() => import('./panels/SettingsPage'));

/** How often the nav rail's Terminal count is refreshed when the Monitor is not mounted. Matches
 * the streamer's own discovery cadence — a faster poll cannot see anything newer. */
const PANES_REFRESH_MS = 30_000;

/**
 * How often the Dataset catalog is re-read, for everyone.
 *
 * ONE POLLER FOR THE WHOLE APP. Three surfaces want this listing and each used to fetch it itself,
 * which is three catalog scans for a number that would then disagree between panels. It also makes
 * the throughput series possible at all: a rate needs two samples taken by the same clock, so the
 * cadence has to be a property of the app rather than of whichever page happens to be mounted.
 *
 * TWO CADENCES, because the listing is not free. It is a catalog scan — measured at 60–75 ms on the
 * 4 GB controller — and holding it at two seconds forever costs a few percent of a core for every
 * open tab, whether or not anything is happening. Two seconds is right while a sweep is running or
 * while an operator is on the Workflows page watching one, because that is when a slower poll makes
 * a working fleet look stalled. At rest, ten: nothing is being written, and the honest reading of a
 * resting lake at any cadence is the same reading.
 */
const DATASETS_REFRESH_MS = 2000;
const DATASETS_IDLE_MS = 10_000;

/**
 * How often the run LIST is re-read.
 *
 * ONE POLLER FOR THE RUN LIST, and it is deliberately the slower of the two clocks on the Workflows
 * surface. The list is an inventory: it costs a Temporal `describe` per open run it returns (up to
 * 200) plus that run's ledger read plus a visibility scan over every dispatch on the cluster, so
 * polling it at the detail's cadence would multiply the most expensive read in the app by the
 * number of runs on screen. The run you are WATCHING is refreshed at 2 s by `useRunTelemetry`,
 * which also refreshes this list the moment that run settles — so the one transition where a stale
 * row would be visibly wrong is the one that reconciles it.
 *
 * AND IT ONLY RUNS WHILE THE WORKFLOWS SURFACE IS OPEN, which is the change the pulse paid for. It
 * used to poll on every surface at 5–15 s, forever, because the nav rail derived "is anything
 * running" by filtering the array — so a session parked on the Datasets page all afternoon was
 * re-reading the entire run history of this controller for a two-digit number in the chrome. The
 * rail reads {@link PULSE_REFRESH_MS} now, which is two bounded reads that do not move with
 * history, and the list is polled by the one surface that draws it.
 */
const RUNS_REFRESH_MS = 5000;

/**
 * How often "is anything happening at all, anywhere" is re-read, for everyone.
 *
 * THE ONLY RUN-SHAPED POLL THAT RUNS ON EVERY SURFACE, and it is cheap enough to be one: a count
 * off Temporal's visibility index, plus the open runs' memos, which ride on their listing for free
 * (`backend/src/pulse.ts`). Nothing in it grows with how many runs this controller has ever
 * held.
 *
 * Two cadences for the same reason the catalog has two. Five seconds while something is running or
 * while an operator is on the Workflows page watching one, because that is when a slower poll makes
 * a working fleet look stalled and a fresh park look unnoticed. Fifteen at rest: the honest reading
 * of a resting appliance at any cadence is the same reading.
 */
const PULSE_REFRESH_MS = 5000;
const PULSE_IDLE_MS = 15_000;

export default function App(): JSX.Element {
  // The Actor catalog is auto-discovered from the server: whatever is deployed and registered
  // shows up. Load on mount and poll, so a newly-deployed actor appears without a reload.
  // Best-effort: an unreachable backend leaves the catalog untouched.
  const loadCatalog = useAppStore((s) => s.loadCatalog);
  useEffect(() => {
    void loadCatalog();
    const timer = setInterval(() => void loadCatalog(), 15_000);
    return () => clearInterval(timer);
  }, [loadCatalog]);

  const loadPanes = useAppStore((s) => s.loadPanes);
  const view = useAppStore((s) => s.view);

  /**
   * THE PULSE, ON EVERY SURFACE. This is the whole of what the chrome needs to answer "is anything
   * happening at all, anywhere" — see `panels/chrome/pulse.ts` for the four readings it becomes and
   * `backend/src/pulse.ts` for why it is two bounded reads rather than a listing.
   *
   * THE CADENCE IS DRIVEN BY THE PULSE'S OWN LAST ANSWER, not by the run list. `runs.some(running)`
   * was the old test and it required the run list to be polled everywhere for the cadence of the
   * poller that fed it, which is the circle this replaces.
   */
  const loadPulse = useAppStore((s) => s.loadPulse);
  const pulse = useAppStore((s) => s.pulse);
  // Workflows counts as busy even with nothing running: a run just started reaches Temporal's
  // visibility index a beat later, and the whole provisioning window would poll at the idle rate
  // while the operator watched an empty panel.
  const busy = view === 'workflows' || (pulse?.running ?? 0) > 0;
  useEffect(() => {
    void loadPulse();
    const timer = setInterval(() => void loadPulse(), busy ? PULSE_REFRESH_MS : PULSE_IDLE_MS);
    return () => clearInterval(timer);
  }, [busy, loadPulse]);

  // The run LIST, for the one surface that draws it. Not a chrome poller any more: see
  // RUNS_REFRESH_MS. Mounted-surface-scoped rather than store-scoped so the Workflows page keeps
  // exactly the freshness it had, and every other surface stops paying for it.
  const loadRuns = useAppStore((s) => s.loadRuns);
  useEffect(() => {
    if (view !== 'workflows') return;
    void loadRuns();
    const timer = setInterval(() => void loadRuns(), RUNS_REFRESH_MS);
    return () => clearInterval(timer);
  }, [loadRuns, view]);
  // Terminals, for the rail's `live` counter. Also loaded by the Monitor, which shows them in
  // full; this is what makes the counter honest on a session that never opens that surface, which
  // is exactly when an operator most needs to see that four Machines are attached.
  useEffect(() => {
    // Not while the Monitor is open: it holds the inventory itself and publishes both counters, so
    // polling here as well would be a second `capture-pane` discovery on the streamer for a number
    // that is already fresher.
    if (view === 'monitor') return;
    void loadPanes();
    const timer = setInterval(() => void loadPanes(), PANES_REFRESH_MS);
    return () => clearInterval(timer);
  }, [loadPanes, view]);

  const loadDatasets = useAppStore((s) => s.loadDatasets);
  useEffect(() => {
    void loadDatasets();
    const timer = setInterval(
      () => void loadDatasets(),
      busy ? DATASETS_REFRESH_MS : DATASETS_IDLE_MS
    );
    return () => clearInterval(timer);
  }, [busy, loadDatasets]);

  // Light/dark: the shadcn theme flips on the <html> `dark` class.
  const theme = useAppStore((s) => s.theme);
  useEffect(() => {
    document.documentElement.classList.toggle('dark', theme === 'dark');
  }, [theme]);

  // All five are full-bleed beside the rail. Monitor in particular must not sit in a narrower
  // column: a Terminal is sized to its container's measured cols/rows, so anything that shrinks the
  // wall shrinks every session on it.
  return (
    <Shell>
      {view === 'workflows' ? (
        <WorkflowsPage />
      ) : view === 'actors' ? (
        <ActorsPage />
      ) : view === 'datasets' ? (
        <DatasetPage />
      ) : view === 'monitor' ? (
        <MonitorPage />
      ) : (
        <SettingsPage />
      )}
    </Shell>
  );
}
