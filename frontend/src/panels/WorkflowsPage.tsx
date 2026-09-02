/**
 * Workflows: every caller workflow you have, what each one is DOING, and the pane its worker is in.
 *
 * THIS SURFACE CAME BACK, and the reason it was missing is worth keeping. It was folded into the
 * Catalog when Runs became addressable by id, on the reasoning that a definition and a run are
 * different things and only one of them needed a page. That was right about runs and wrong about
 * definitions: editing a workflow, serving it, and watching the worker you just started are one
 * activity, and they were spread across a page about files and a wall about Machines.
 *
 * THREE STATES, AND THE ANIMATION IS THE POINT. A workflow is `running`, `serving`, or `idle`, and
 * they come from two different authorities — Temporal's task queue for "is anyone polling", the run
 * list for "is anything open". Reading either alone gets it wrong, so `run/workflowState.ts` owns
 * the decision and this file only draws it. A fourth state, `unknown`, exists because a cluster
 * that cannot be asked must not render as "nothing is serving anything".
 *
 * THE DETAIL COLUMN IS A THREAD NOW — the open workflow's own runs beside five readings of the one
 * that is selected (`WorkflowThread.tsx`). What was here was a run table, a source pane, a worker
 * pane and a run log stacked down the page, each with a height the operator had dragged: four
 * things about one subject competing for the same 800 pixels, and no one selection governing any of
 * them. They are the same four things, under one. This file still owns the folder list, the header,
 * the serve/pause/run controls and the input form; it hands the viewer and the pane in as props
 * because neither can load under the test runner, which is what lets every state of the thread be
 * asserted in node.
 *
 * THE PANE IS STILL A TAB AWAY because the failure it catches is invisible anywhere else. A worker
 * that boots and dies — a TabError from a stray tab, a missing import, a queue nobody serves —
 * leaves a Run that simply waits, and no status on this page changes. The pane is where the
 * traceback is. It goes LIVE rather than staying a snapshot — see `useSinglePane`'s header for why
 * ADR 0020's cost model is about a fleet Machine and not about the worker on this host, and for
 * what the snapshot rule cost: a snapshot is a dump of the remote screen at THE REMOTE SCREEN'S
 * size, so resizing the pane could never rewrap a line.
 *
 * NO FOLDER, NO WORKFLOW — one row per registered folder, the same rule the Actors page follows.
 * The list used to draw two inventories: the files under `.kontra/workflows/` and the registrations
 * beneath them, which on an ordinary installation is the same two workflows twice, once by name
 * with a description and once by path with the name repeated. The FOLDER is the unit because it is
 * what this surface can act on — open it, serve it, start it, forget it — and a file row offers
 * none of those. A registration outside the default root is a workflow all the same, which is how
 * one comes to live in the checkout beside the actors it drives.
 *
 * THE FILE HALF IS STILL JOINED IN (`mergeWorkflowFolders`), because `description.md`'s first
 * paragraph is what a list of names cannot say. It contributes a sentence, never a row.
 *
 * A RUN'S OWN DETAIL IS HERE NOW, under `/workflows/<workflow>/<run>`, which is the address this
 * page already held. Its TRANSCRIPT — what it set out to do, what it called, what it wrote and how
 * it ended, in kontra's words rather than Temporal's — is the Transcript tab; what Temporal actually
 * recorded is the Event log beside it, which is where a drill from any turn lands. Its Datasets and
 * its sketch are tabs that are scoped and reachable and whose contents are their own slices.
 *
 * IT STILL MINTS NOTHING FROM A THREAD. A Run is one execution of a caller's workflow (ADR 0023
 * §12): the Run button here starts a REGISTERED folder by its declared type, with the queue derived
 * server-side, and the thread beside it only reads. There is no graph to interpret and no way to
 * launch an arbitrary one from a conversation.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { AlertTriangle, ChevronDown, ChevronRight, Pause, Play, Terminal } from 'lucide-react';
import CodeMirror from '@uiw/react-codemirror';
import { python } from '@codemirror/lang-python';
import {
  fetchExposure,
  fetchPollers,
  fetchWorkflowSource,
  fetchWorkflows,
  pauseWorkflow,
  serveWorkflow,
  startRun,
  stopRun,
  type RunRow,
  type ServeResult,
  type WorkflowDescriptor,
  type WorkflowFile,
} from '../run/api';
import {
  stateWords,
  strandedRun,
  workflowState,
  type PollerReport,
  type WorkflowState,
} from '../run/workflowState';
import { parseWorkflow, queueDigest, typeFromSource } from './workflowSource';
import {
  draftForInput,
  inputOf,
  type InputDraft,
} from './workflowInput';
import { WorkflowInputForm } from './WorkflowInputForm';
import { rowStatus } from './workflowRow';
import { elidePath } from './elidePath';
import { streakOf } from '../run/runState';
import { Streak } from '../components/Spark';
import { RegisterFolder } from './RegisterFolder';
import { FolderAbsent, FolderActions, useRegisteredFolders, type FolderShelf } from './RegisteredFolders';
import { mergeWorkflowFolders } from './sourceFolders';
import { StatePill } from './StatePill';
import { WorkflowThread, type TurnsFailure } from './WorkflowThread';
import { WorkflowSketch, useWorkflowSketch } from './WorkflowSketch';
import { DEFAULT_TAB, openDrill, threadOf, transcriptFor, type TabId } from './workflowThread';
import { RunDataTab, RunMonitor } from './RunMonitor';
import { RunStatsBar } from './RunStats';
import { runDatasets } from './runDatasets';
import { runStats } from './runStats';
import { focusedMachine, runMachines } from './runMachines';
import { answerRunAsk, type RunTurns } from '../run/turns';
import type { AskDeck } from './AskTurn';
import { answerOf, askForm, parkedRun } from './ask';
import { transcriptHub, useFollowedRun } from '../run/follow';
import { TurnDrill } from './TranscriptDrill';
import { turnKey } from './transcriptDrill';
import { SideDockControls, SideRail, SideResizer, useSideDock } from './chrome/SideDock';
import { WorkerPane } from './WorkerPane';
import { RunTail } from './RunTail';
import { SourceProvenance } from './SourceProvenance';
import { workflowSessionOf } from './workflowSession';
import { useAppStore } from '../state/store';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';

/**
 * Highlighting only — NO edit or save extensions (ADR 0030). `indentWithTab` and the four-space
 * indent unit belonged to an editor that wrote this file; the viewer is read-only, so what is left
 * is the Python grammar that colours it. `editable={false}` on the CodeMirror below is what actually
 * refuses input; this is what makes the refused input still readable.
 */
const editorExtensions = [python()];

/** How often the serving signal is re-read. One `DescribeTaskQueue` per tick, for the ONE workflow
 *  that is open — the list's dots ride the same answer as the header's pill. */
const POLLER_MS = 4000;

/** How often the Terminal inventory is re-asked while the Monitor tab is open. `WorkerPane`'s own
 *  `FIND_MS` verbatim: the streamer rediscovers sessions on a ~30 s cadence, so this is patience
 *  while a just-served worker appears, not a retry loop. */
const PANES_REFRESH_MS = 3000;

export default function WorkflowsPage(): JSX.Element {
  const runs = useAppStore((s) => s.runs);
  const loadRuns = useAppStore((s) => s.loadRuns);
  const setWorkflowCount = useAppStore((s) => s.setWorkflowCount);
  /**
   * WHICH WORKFLOW IS OPEN IS AN ADDRESS, NOT A `useState`.
   *
   * It was local, so `/workflows` was the only URL this surface ever had: a reload came back on the
   * first row whatever the operator had been reading, and there was no link to hand anybody. The
   * store holds it now and the URL is its projection (`state/address.ts`), which is what makes
   * `/workflows/dnssweep` survive an F5 and Back leave the thread it was in.
   *
   * ONE DIRECTION ONLY — store to page. A click calls `openWorkflow`, the store writes the bar, and
   * the effect below notices the change and does the local work (clear the last row's signals, read
   * the new source). Writing both ways would put a push in the effect's path and turn Back into a
   * loop that cannot leave.
   */
  const workflowName = useAppStore((s) => s.workflowName);
  const openWorkflow = useAppStore((s) => s.openWorkflow);
  const theme = useAppStore((s) => s.theme);

  const [dir, setDir] = useState('');
  const [files, setFiles] = useState<WorkflowFile[]>([]);
  /**
   * Whether the workflow listing has ANSWERED, which an empty array cannot say.
   *
   * ONLY THE SKETCH TAB READS IT, and it is the difference between "no caller workflow by that name
   * exists here" and "nobody has looked" — the first is a claim a design tab may make about
   * something an author drew, and the second is not. Everything else on this page draws `files`
   * directly, where an empty list before the fetch answers is a list with nothing in it yet and is
   * indistinguishable from the truth a moment later.
   */
  const [listed, setListed] = useState(false);
  /** What workers have REGISTERED, keyed by workflow type — not by file. See WorkflowContract. */
  const [registered, setRegistered] = useState<WorkflowDescriptor[]>([]);
  const [selected, setSelected] = useState('');
  const [source, setSource] = useState('');

  /**
   * The `@workflow.defn` type each folder declares, keyed by the name the row opens under.
   *
   * WHY THE PAGE CARRIES THIS AT ALL. A closed row has to join to its runs by the DECORATED type
   * (`dhmonitor` → `DockerLeakMonitor`), and that name lives only in the source — `GET /api/workflows`
   * lists files and never parses them. So the page reads each folder's source once and remembers the
   * type. Until a row's type is here, it is UNKNOWN, which is what the list says — never `never run`,
   * the lie #13 was about. A cheap local file read, not a Temporal RPC, so reading every row up front
   * is nothing like the per-row `DescribeTaskQueue` the serving signal deliberately refuses. */
  const [typeByFolder, setTypeByFolder] = useState<Record<string, string>>({});
  /** Folder names whose source has been requested, so the resolver fetches each exactly once and a
   *  render in flight does not stack a second read on the same name. */
  const requested = useRef<Set<string>>(new Set());
  /** Bumped on every selection; a source fetch that returns after the operator has moved on carries
   *  an older token and is DISCARDED rather than painted over the newer row (#13's racing clicks). */
  const selectToken = useRef(0);

  /* EMPTY, NOT `'recon'`. The default here was a literal queue name left over from an older
     run, so an operator who never opened the options drawer served and started on it — and
     Temporal accepts a start on ANY queue string, routes it there, and waits. That is the whole
     `no_poller_tasks{taskqueue="recon"} = 12` on the live cluster: twelve workflow tasks delivered
     to a queue with nobody on it, by a default nobody chose. Serve no longer sends this at all
     (the server derives `wf-<name>-<version>`); it is filled from the registered descriptor for
     `start`, and stays empty until there is one. */
  const [queue, setQueue] = useState('');
  const [type, setType] = useState('');
  /**
   * The run's ONE argument, as a form — a field per declared property, or the raw JSON box for a
   * workflow that declares nothing. It was `useState('{}')` + a `<Textarea>` + `JSON.parse`, which
   * asked an operator to hand-write the very object the descriptor already describes, and coerced
   * nothing: `"machines": "4"` went out as a string and failed at the far end of a serve. The draft
   * is rebuilt from the open workflow's declared input below; `workflowInput.ts` owns its shape.
   */
  const [draft, setDraft] = useState<InputDraft>(() => draftForInput(undefined));
  const [showOptions, setShowOptions] = useState(false);

  const [served, setServed] = useState<ServeResult | null>(null);
  const [paused, setPaused] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [exposure, setExposure] = useState<{ open: boolean; detail: string } | null>(null);
  /** Dismissed for THIS session only — see the banner below for why it is not persisted. */
  const [exposureHidden, setExposureHidden] = useState(false);
  /**
   * WHICH READING OF THE OPEN RUN IS ON SCREEN.
   *
   * LOCAL, DELIBERATELY, AND NOT IN THE ADDRESS. A tab is a lens on a subject the URL already
   * names: `/workflows/dnssweep/sweep-9` is the run, and Transcript or Data is how the operator
   * happens to be looking at it this second. Putting it in the path would mint six addresses for
   * one thing and make Back a tab-switcher.
   *
   * IT STILL FOLLOWS A `?pane=`, and that does not reverse the decision above. The tab is not IN the
   * address; it is INFERRED from what the address names — a URL that names a **Terminal** is a URL
   * about the Machines under the run, and landing it on the Transcript with an invisible selection
   * would be a link that arrives having apparently done nothing. Read here rather than in an effect so a cold
   * load paints the Monitor on the first frame; the effect below is for Back and Forward, which
   * arrive after one.
   */
  const [tab, setTab] = useState<TabId>(() =>
    useAppStore.getState().runPane === null ? DEFAULT_TAB : 'monitor'
  );
  /** The open run's account, once it has arrived. Never cleared on a switch — `transcriptFor`
   *  refuses a reading that belongs to another run, and leaving this alone is what keeps that
   *  guard load-bearing rather than decorative. */
  const [loaded, setLoaded] = useState<RunTurns | null>(null);
  const [turnsFailure, setTurnsFailure] = useState<TurnsFailure | null>(null);
  /** The run a stop has been asked for and not yet answered. */
  const [stopping, setStopping] = useState<string | null>(null);
  /**
   * The run this page is WATCHING — set by pressing Run, by opening one from the list below, and by
   * the address on a cold load.
   *
   * IT IS `store.runId` NOW, AND THE REASON IT WAS NOT IS GONE. It used to be local because
   * `openRun` carried a view change to a global Runs surface, so sharing one value made pressing
   * Run navigate away from the code, the input and the worker's pane at the exact moment an author
   * wants all four. That surface is retired: a run is reached through the workflow that produced it,
   * so "which run is this page showing" and "which run is addressed" are now the same question —
   * and keeping them apart would let the bar name one run while another was on screen.
   *
   * `openRun` rather than `setRunId` because it is the action that means "open this run" from
   * anywhere: on this page the surface is already right, and from a Dataset it is not.
   */
  const watching = useAppStore((s) => s.runId);
  const setWatching = useAppStore((s) => s.openRun);
  /**
   * CLOSING A CONVERSATION IS NOT OPENING ONE, and the store already has two verbs for that.
   *
   * `openRun` takes a `string` — it MEANS "go and look at this run", and it carries a surface
   * change with it. Closing keeps the thread and drops the last address segment, which is
   * `setRunId(null)`: no surface change, and the bar follows because a run is the last segment of a
   * Workflows address. Routing both through `openRun` type-checked only because a method-shorthand
   * prop is bivariant; the thread declares its handlers as properties now, and this is what that
   * caught.
   */
  const setRunIdOnly = useAppStore((s) => s.setRunId);
  const openOrClose = useCallback(
    (runId: string | null) => (runId === null ? setRunIdOnly(null) : setWatching(runId)),
    [setRunIdOnly, setWatching]
  );

  /** The serving signal for the OPEN workflow's queue, and when it was measured. */
  const [pollers, setPollers] = useState<PollerReport | null>(null);
  /** The previous tick's poller count, in a ref rather than in state: it is read by the poll loop
   *  to notice a worker COMING UP, and putting it in the effect's deps would restart the interval
   *  on every tick. */
  const polled = useRef<number | null>(null);
  const [now, setNow] = useState(() => Date.now());

  const folders = useRegisteredFolders('workflow');

  /**
   * THE OPEN WORKFLOW'S SKETCH, HELD HERE RATHER THAN IN THE TAB THAT DRAWS IT.
   *
   * The thread renders one tab at a time, so a Scratch that owned its own document would lose an
   * unsaved drawing the moment somebody glanced at the Transcript — and it is the only one of the
   * six readings that holds anything a person typed. The listing and the registered descriptors are
   * handed over rather than re-fetched: this page already holds both, and a second opinion about
   * what `.kontra/workflows/` contains would arrive at a different moment with nothing to say which
   * was right.
   */
  const sketch = useWorkflowSketch(selected, listed ? files : null, registered);

  const reload = useCallback(async () => {
    try {
      const got = await fetchWorkflows();
      setDir(got.dir);
      setFiles(got.workflows);
      setListed(true);
      setRegistered(got.registered);
      setWorkflowCount(got.workflows.length);
      return got.workflows;
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      return [];
    }
  }, [setWorkflowCount]);

  useEffect(() => {
    void reload();
    void loadRuns();
    void fetchExposure().then(setExposure);
  }, [reload, loadRuns]);

  /**
   * Selecting a row is IMMEDIATE and LOCAL; the source fetch fills the detail in afterwards.
   *
   * It used to `await fetchWorkflowSource` before `setSelected`, so a click did not highlight its row
   * — and the queue, the poller signal and the worker pane did not follow — until a round trip
   * returned, and rapid clicks raced: whichever fetch resolved last won, painting a row the operator
   * had already left. Selection now happens synchronously, every per-workflow signal is cleared for
   * the new row, and the fetch is fenced by a token so a late answer for an abandoned row is dropped.
   */
  const select = useCallback((name: string) => {
    setError(null);
    setNotice(null);
    setSelected(name);
    // Nothing from the previous file belongs to this one: its source, its type, the queue derived
    // from its contract, the poller reading of that queue, the worker it served, and the run this
    // page was watching. Cleared now so no stale value survives the switch (#13) — except the run,
    // which `openWorkflow` clears in the store: this function also runs when the ADDRESS is what
    // changed, and clearing the run here would blank the one a deep link had just named.
    setSource('');
    setType('');
    setQueue('');
    setPollers(null);
    polled.current = null;
    setServed(null);
    setPaused(false);

    // `''` is the list with nothing open — a real place (`/workflows`), reached by Back out of a
    // thread. Everything above has already been cleared; there is simply no source to read.
    if (name === '') return;

    const token = ++selectToken.current;
    requested.current.add(name);
    void fetchWorkflowSource(name)
      .then((src) => {
        if (token !== selectToken.current) return; // a newer row is selected; this answer is stale.
        setSource(src);
        // The TYPE comes from the file's own `@workflow.defn`, not from its name: the filename guess
        // reads `nscheck.py` as `Nscheck` while the class is `NsCheck`, and starting a type nothing
        // registered hangs instead of failing.
        const t = typeFromSource(src, name);
        setType(t);
        setTypeByFolder((m) => ({ ...m, [name]: t }));
      })
      .catch((err) => {
        // Let a transient read retry from the background resolver rather than pinning this row to
        // UNKNOWN forever.
        requested.current.delete(name);
        if (token !== selectToken.current) return;
        setError(err instanceof Error ? err.message : String(err));
      });
  }, []);

  /**
   * The address, applied.
   *
   * THIS REPLACED AN AUTO-SELECT OF THE FIRST ROW, and the swap is the point rather than a
   * casualty. Opening the first workflow on arrival made `/workflows` and `/workflows/<first>` the
   * same picture, so Back out of a thread appeared to do nothing and no reload could land anywhere
   * but row one. The list with nothing open is a place the address can say, and the detail column
   * already has words for it.
   *
   * A NAME THE CHECKOUT DOES NOT HAVE IS STILL APPLIED, deliberately: an id in an address is not a
   * promise that it exists, and this page's own header is where "there is no such workflow" gets
   * answered. Falling back to row one here would quietly show somebody a different workflow than
   * the one they pasted.
   */
  useEffect(() => {
    const wanted = workflowName ?? '';
    if (wanted !== selected) select(wanted);
  }, [select, selected, workflowName]);

  /**
   * Read every listed row's source once, to learn the type it declares.
   *
   * The list joins closed rows to their runs by the decorated type, which only the source carries.
   * Without this a row is truthful only after the operator has opened it; with it, `dhmonitor` reads
   * as `DockerLeakMonitor` and joins to its running run while some other row is selected — the exact
   * case #13 filed. `requested` makes each read happen once; a read that fails leaves the row UNKNOWN
   * and retriable on the next reload, which is honest, not a lie dressed as `never run`. */
  useEffect(() => {
    for (const row of mergeWorkflowFolders(files, folders.sources)) {
      const name = row.file?.name ?? row.name;
      if (requested.current.has(name)) continue;
      requested.current.add(name);
      void fetchWorkflowSource(name)
        .then((src) => setTypeByFolder((m) => ({ ...m, [name]: typeFromSource(src, name) })))
        .catch(() => requested.current.delete(name));
    }
  }, [files, folders.sources]);

  // THE SERVING SIGNAL, polled while a workflow is open. It stops when the tab is hidden for the
  // same reason every other poll here does: one `DescribeTaskQueue` every four seconds against a
  // page nobody is looking at is a cost with no reader.
  useEffect(() => {
    const q = queue.trim();
    if (!q) {
      setPollers(null);
      return;
    }
    let live = true;
    const tick = async (): Promise<void> => {
      const report = await fetchPollers(q);
      if (live) {
        // A WORKER THAT IS POLLING HAS REGISTERED — AND IN --watch MODE IT RE-REGISTERS ON EVERY
        // SAVE. The descriptor is pushed before `worker.run()`, so the first poll is the earliest
        // moment the contract can be there (`serve` returns as soon as tmux has the session, seconds
        // before the interpreter has imported anything). But a watch serve keeps the same worker up
        // and re-derives the descriptor when the file changes, so the contract — the form, or the
        // broken-file state — moves while the poller count never does. Re-fetching the descriptors
        // on every poll of a live worker is what makes the form track the editor without a page
        // reload; the draft is keyed on the serialized schema (below), so an unchanged schema does
        // not disturb what the operator is typing. Only when a worker is actually polling: a queue
        // nobody serves has no descriptor to re-read.
        if (report.pollers > 0) void reload();
        polled.current = report.pollers;
        setPollers(report);
        setNow(Date.now());
      }
    };
    void tick();
    const timer = setInterval(() => void tick(), POLLER_MS);
    return () => {
      live = false;
      clearInterval(timer);
    };
  }, [queue, reload]);

  const scan = useMemo(() => parseWorkflow(source), [source]);
  /**
   * THIS WORKFLOW'S THREAD: its own runs, and which one is open.
   *
   * IT REPLACED A SECOND COPY OF THE JOIN. This page used to filter `runs` by `type` here while the
   * LIST beside it joined through `rowStatus` — two spellings of the one thing #13 got wrong, and
   * the shorter one had no test. `threadOf` calls `rowStatus`, so both halves of the page now agree
   * by construction and both are pinned by `workflowRow.test.ts`.
   *
   * `type.trim() || undefined` IS NOT A TIDY-UP. An empty string means the source has not been read
   * yet, and `rowStatus` answers `unknown` to that rather than `never run` — feeding it `''` would
   * be handing it a type it could then find no runs for, which is the lie in its original disguise.
   */
  const thread = useMemo(
    () => threadOf(type.trim() || undefined, runs, watching),
    [runs, type, watching]
  );
  const mine = thread.runs;

  /* ── the run's Machines, and what it wrote ──────────────────────────────────────────────────
     BOTH ARE FILTERS OVER LISTINGS THE APP ALREADY HOLDS, and that is deliberate: the Terminal
     inventory and the Dataset catalog each have exactly one poller for the whole app (`App.tsx`
     says why), so scoping them to the open run costs a `useMemo` rather than two more requests per
     run on screen. */
  const panes = useAppStore((s) => s.panes);
  const runPane = useAppStore((s) => s.runPane);
  const focusRunPane = useAppStore((s) => s.focusRunPane);
  const catalog = useAppStore((s) => s.datasets);
  const catalogAt = useAppStore((s) => s.datasetsAt);
  const openDataset = useAppStore((s) => s.openDataset);
  /** THE ANTI-STALE RULE, APPLIED HERE TOO. The thread applies it for the tabs it renders itself;
   *  the Monitor's scope is derived from the transcript BEFORE it gets there, so it has to make the
   *  same refusal — a reading of the previous run names the previous run's Actors. */
  const scopeTurns = transcriptFor(loaded, thread.selected);
  /**
   * THE SESSION THE CALLER'S OWN WORKER IS IN — `WorkerPane`'s `session ?? derived` rule verbatim,
   * moved up to where the scoping now happens.
   *
   * THE SERVER'S ANSWER WINS WHEN THERE IS ONE. `serveWorkflow` reports the session the worker
   * actually landed in; `workflowSessionOf` is a FIFTH independent derivation of the same rule
   * (`workflowSession.ts` says so), and a derivation that has drifted would draw a worker that is
   * polling perfectly well as one that never started. `served` is only set by a Serve in THIS visit
   * and is cleared when the open workflow changes, so the derived name is what a worker served
   * yesterday is found by — which is the whole reason both exist.
   */
  const workerSession = served?.session ?? workflowSessionOf(selected);
  const scoped = useMemo(
    () => runMachines(scopeTurns, workerSession, panes),
    [panes, scopeTurns, workerSession]
  );
  const focusedPane = focusedMachine(scoped.machines, runPane);
  /* BACK AND FORWARD ONTO A `?pane=` OPEN THE TAB IT IS ABOUT — see the `tab` state above for why
     this is not the tab living in the address. Only ever forward: a pane that has been cleared says
     nothing about which reading the operator wants, so it never sends them back to the Transcript. */
  useEffect(() => {
    if (runPane !== null) setTab('monitor');
  }, [runPane]);
  /*
   * THE INVENTORY, AT THE PANE'S OWN CADENCE, WHILE THE PANE IS ON SCREEN.
   *
   * `WorkerPane` used to poll `/api/panels/terminals` every 3 s for itself, and it only ever
   * mounted on this tab — so this is the same request at the same rate in the same circumstances,
   * moved up to where the scoping happens. Without it a worker served a moment ago would take the
   * app-wide 30 s to appear, on the one surface whose whole job is watching the thing you just
   * started.
   *
   * IT CANNOT RACE THE MONITOR SURFACE, which is `App.tsx`'s rule and is satisfied here by
   * construction rather than by a guard: `App.tsx` mounts exactly one surface, so this page does not
   * exist while the Monitor does. The rule matters because that page publishes a fresher inventory
   * from its own socket, and a second discovery would be one more `capture-pane` round on the
   * streamer for a number that is already newer.
   */
  const loadPanes = useAppStore((s) => s.loadPanes);
  useEffect(() => {
    if (tab !== 'monitor') return;
    void loadPanes();
    const timer = setInterval(() => void loadPanes(), PANES_REFRESH_MS);
    return () => clearInterval(timer);
  }, [loadPanes, tab]);
  const wrote = useMemo(
    () => runDatasets(catalog, thread.selected?.runId ?? null, catalogAt),
    [catalog, catalogAt, thread.selected]
  );
  /** The contract for the TYPE that is open — joined here because this side is the only one that
   *  knows which type the file it is showing declares (`typeFromSource`). */
  const contract = useMemo(
    () => registered.find((w) => w.name === type.trim()),
    [registered, type]
  );
  /** The registered folder the OPEN workflow lives in — its `.path` is what the read-only viewer
   *  offers to copy and open in the operator's own editor (ADR 0030). The list already joins files
   *  to folders this way; the editor side needs the same join to name where the source on screen is
   *  on disk. */
  const openFolder = useMemo(() => {
    for (const row of mergeWorkflowFolders(files, folders.sources)) {
      if ((row.file?.name ?? row.name) === selected) return row.folder;
    }
    return undefined;
  }, [files, folders.sources, selected]);
  /**
   * THE QUEUE FOLLOWS THE WORKFLOW, from the worker that registered it.
   *
   * This field held one value for the whole session, seeded to `recon` and carried across every
   * workflow the operator opened. Temporal accepts a start on ANY queue name and routes it there;
   * only a worker polling that exact one takes the task. So opening `canary` and pressing Run sent
   * it to `recon` — MEASURED — where a worker that polls `recon` picked it up and failed the
   * workflow task with `Workflow class Canary is not registered on this worker`, repeatedly, while
   * the page and the run list both reported `running`. Nothing on any surface said "wrong queue".
   *
   * `descriptor.queue` is what the worker serving this type is polling, so it is the only value
   * here that is a fact rather than a habit — and it is now the ONLY writer. There is no guard for
   * an operator typing over it because there is nothing to type into: the queue is derived
   * (`wf-<name>-<digest12>`, bound to the folder's content — GitHub #15) and the drawer displays it.
   * What used to be an input was an invitation to override the one string guaranteed to be right.
   * The Run button no longer SENDS this queue either: it sends the folder, and the server re-derives
   * and refuses if nothing serves that digest — so a stale descriptor cannot aim a Run wrong.
   */
  useEffect(() => {
    const want = contract?.queue;
    if (want) setQueue(want);
  }, [contract?.queue]);

  /**
   * THE RUN FORM IS BUILT FROM THE OPEN WORKFLOW'S DECLARED INPUT, and rebuilt when that changes.
   *
   * Keyed on the SERIALIZED schema, not the descriptor's identity: the catalog is re-fetched on a
   * poll (a worker coming up), which mints a fresh `contract` object every few seconds, and depending
   * on that object would wipe what the operator is typing on every tick. The string only changes when
   * the declared input actually does — a workflow opened, or a descriptor arriving for the first time
   * as its worker serves (undeclared → fields), which is exactly the transition that should redraw
   * the box into a form.
   */
  const schemaKey = JSON.stringify(contract?.input ?? null);
  useEffect(() => {
    setDraft(draftForInput(contract?.input));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [type, schemaKey]);

  const state = workflowState(mine, pollers, now);
  const stranded = strandedRun(mine, pollers, now);
  const result = inputOf(draft);
  const inputOk = !('error' in result);

  /**
   * THE OPEN RUN'S ACCOUNT, FOLLOWED.
   *
   * KEYED ON `thread.selected`, NOT ON `watching`. They differ in exactly the case this page was
   * briefed against: the address still names the previous workflow's run for a render after the
   * operator switches threads, and reading that id would spend a round trip on a run this page has
   * already decided it is not showing.
   *
   * IT IS A READER, NOT AN EFFECT, AND THE DIFFERENCE IS THE WHOLE SLICE. A one-shot read made a
   * running run's transcript a stale snapshot with a badge on it, so "what is happening" and "what
   * happened" were two surfaces and only the second worked. `run/follow.ts` reads until the run
   * closes and then stops — a settled run is read once and never again, which is the property the
   * one-shot had and the one worth keeping — and every arrival is an actual change in the log, so
   * nothing on screen moves when nothing happened.
   *
   * THE RUN'S STATUS IS NO LONGER A DEPENDENCY. It used to be there so a run that SETTLED while it
   * was open got one final read; the reader now learns that from the log itself, which is the
   * authority (`Transcript.live` is "terminal event or nothing"), and re-keying on a list poll would
   * tear the reader down and rebuild it for a fact it already knew.
   */
  const openRunId = thread.selected?.runId ?? null;
  const following = useFollowedRun(openRunId);
  useEffect(() => {
    if (!following) return;
    // The reading is NOT cleared on a failed re-read: `transcriptFor` already refuses one that
    // belongs to another run, and blanking it here would lose a good account of the run still on
    // screen the moment one poll of it failed.
    if (following.turns) setLoaded(following.turns);
    setTurnsFailure(following.failure ? { gone: following.failure.gone, detail: following.failure.detail } : null);
  }, [following]);
  useEffect(() => {
    if (openRunId === null) {
      setLoaded(null);
      setTurnsFailure(null);
    }
  }, [openRunId]);

  /**
   * THE OPEN RUN'S OWN NUMBERS — the fourth reading of a run, which this page did not have.
   *
   * IT COSTS NO REQUEST, and that is why it is a `useMemo` here rather than a hook with a poll in
   * it. Every authority it needs is already on this page and already being kept fresh by the ONE
   * poller that owns it: the LEDGER rides on the run list row (`/api/runs` carries the
   * materialization dimension), TEMPORAL'S HISTORY is the log the follower is already reading for
   * the transcript, the LAKE's rate series is `App.tsx`'s single catalog poll, and the STREAK is
   * the run list again. A fifth poller for a number four existing ones can answer is exactly the
   * cost `useRunTelemetry` was built to refuse.
   *
   * IT READS `following.history` AND NOT `loaded`. Both are guarded, but they are guarded by
   * different things: `useFollowedRun` refuses a reading about a run it is no longer watching, and
   * that is the same object the drill takes its raw events from — so the numbers and the events
   * behind them cannot come from two different reads.
   *
   * THE CONCERNS COME THROUGH `scopeTurns`, which is `transcriptFor`'s refusal already applied: a
   * dropped-Unit warning belonging to the run you just left must never appear over the run you just
   * opened.
   */
  const datasetSeries = useAppStore((s) => s.datasetSeries);
  const stats = useMemo(
    () =>
      runStats({
        run: thread.selected,
        history: following?.history ?? null,
        siblings: mine,
        wrote,
        series: datasetSeries,
        concerns: scopeTurns?.named.concerns ?? [],
        now,
      }),
    [thread.selected, following, mine, wrote, datasetSeries, scopeTurns, now]
  );

  /**
   * WHICH TURN'S RAW EVENTS ARE OPEN — a key, never the turn.
   *
   * BECAUSE THE TURN IS A NEW OBJECT ON EVERY ARRIVAL. Holding the `NamedTurn` itself would pin the
   * drill to the reading it was opened from, so a loop that gained forty members while an operator
   * was reading it would still show the six events it folded when they clicked. The key is stable
   * across arrivals (`turnKey` is anchored on the group's first event), so the turn is looked up in
   * the CURRENT reading each render and the raw view grows with the row above it.
   */
  const [drilledTurn, setDrilledTurn] = useState<string | null>(null);
  const drilled = useMemo(
    () =>
      drilledTurn === null
        ? null
        : (following?.turns?.named.turns.find((t) => turnKey(t.turn) === drilledTurn) ?? null),
    [following, drilledTurn]
  );
  // A drill on a run that is no longer open is a panel about nothing. Closing it here rather than
  // leaving it to render empty keeps the tail — the thing that IS about the run on screen — visible.
  useEffect(() => setDrilledTurn(null), [openRunId]);

  /**
   * The runs this page has actually read a PENDING ask for.
   *
   * ONE RUN AT MOST, AND THAT IS THE HONEST CEILING. A park lives on the run's own memo, so knowing
   * which of two hundred rows is parked would cost two hundred reads; what is known is the account
   * that was just fetched. Every other row shows Temporal's word, which is `running` — true, and
   * never invented (see `runWord`).
   */
  const parked = useMemo(() => {
    const out = new Set<string>();
    // THE RUN'S OWN WORD, NOT `pending` ON THE TURN. `ParkedTurn.pending` is "nothing answered it",
    // which is also true of an ask that EXPIRED and of one a cancellation abandoned — so reading it
    // here painted a run as waiting on a human when nobody was waiting for anything. `state` is the
    // four-way ending the run declared (`hitl.ts`), and only one of the four is a park.
    const waiting = loaded?.asks.some((a) => a.state === 'pending') ?? false;
    // AND A CLOSED RUN IS NEVER PARKED, whatever its asks say. A run that failed never gets to
    // rewrite its memo, so its question sits at `pending` in the archive forever.
    if (loaded && loaded.live && waiting) out.add(loaded.runId);
    return out;
  }, [loaded]);

  /**
   * The same fact, published to the CHROME — where it reaches an operator who is not on this page.
   *
   * THIS PAGE UNMOUNTS. `App.tsx` renders one surface at a time, so a park known only here stops
   * being known the moment somebody opens Datasets — which is exactly the moment the requirement is
   * about. The store keeps it, the rail draws it, and `stillParked` prunes it against the run list
   * so a run that closed does not leave a mark behind.
   */
  const setParked = useAppStore((s) => s.setParked);
  useEffect(() => {
    if (!loaded) return;
    setParked(loaded.runId, loaded.live ? parkedRun(loaded.runId, selected, loaded.asks) : null);
  }, [loaded, selected, setParked]);

  /* ───────────────────────────── answering a parked run's questions ───────────────────────────── */

  /**
   * One draft per ask, and one operator label for the session.
   *
   * KEYED BY ASK ID AND CLEARED ON A RUN SWITCH. Ask ids are per-run and deterministic — `ask-1` on
   * every run that asks once (`hitl.py`: "a counter … reads as itself in a URL") — so a draft
   * carried across a switch would arrive prefilled with somebody else's answer to a different
   * question. That is the #13 shape of bug, one level down, and it is cleared rather than guessed at.
   *
   * THE LABEL IS NOT KEYED BY ANYTHING. It is the person at the keyboard, not a property of an ask,
   * and it is deliberately not persisted: this appliance's own `KONTRA_OPERATOR` is the durable
   * default (`defaultOperator` in `hitl.ts`), and a name this page remembered would outlive whoever
   * typed it on a shared box.
   */
  const [drafts, setDrafts] = useState<Record<string, InputDraft>>({});
  const [answerBy, setAnswerBy] = useState('');
  const [answering, setAnswering] = useState<string | null>(null);
  const [refused, setRefused] = useState<Record<string, string>>({});
  useEffect(() => {
    setDrafts({});
    setRefused({});
    setAnswering(null);
  }, [openRunId]);

  const answer = useCallback(
    (askId: string) => {
      const runId = loaded?.runId;
      const ask = loaded?.asks.find((a) => a.id === askId);
      if (!runId || !ask) return;
      const draft = drafts[askId] ?? askForm(ask).draft;
      const got = answerOf(draft);
      if ('error' in got) {
        setRefused((was) => ({ ...was, [askId]: got.error }));
        return;
      }
      setAnswering(askId);
      setRefused((was) => {
        const { [askId]: _gone, ...rest } = was;
        return rest;
      });
      void answerRunAsk(runId, askId, { value: got.value, by: answerBy })
        .then((sent) => {
          if (sent.ok) {
            // THE ACCOUNT IS RE-READ RATHER THAN PATCHED. The answer becomes a turn because the RUN
            // recorded it — the memo moves to `answered`, the signal lands in history — and writing
            // an optimistic turn here would be this page asserting something it did not witness.
            setDrafts((was) => {
              const { [askId]: _done, ...rest } = was;
              return rest;
            });
            void transcriptHub.poll(runId);
            return;
          }
          // BESIDE THE FORM, NOT IN THE PAGE NOTICE. A run with three asks can have one refused and
          // two perfectly answerable, and the field the schema objected to is named.
          setRefused((was) => ({
            ...was,
            [askId]: sent.field && sent.field !== '/' ? `${sent.field}: ${sent.refused}` : sent.refused,
          }));
          // A 409 means the ask is no longer pending — somebody answered first, or the deadline
          // went by. Re-read so the row stops offering a form for a question that is over.
          if (sent.state) void transcriptHub.poll(runId);
        })
        .finally(() => setAnswering(null));
    },
    [loaded, drafts, answerBy]
  );

  /** Everything the transcript needs to let this run be answered — one prop, built once. */
  const deck = useMemo<AskDeck>(
    () => ({
      drafts,
      onDraft: (askId, draft) => setDrafts((was) => ({ ...was, [askId]: draft })),
      by: answerBy,
      onBy: setAnswerBy,
      onAnswer: answer,
      sending: answering,
      refused,
    }),
    [drafts, answerBy, answer, answering, refused]
  );

  const stop = useCallback(
    (runId: string, escalate: boolean) => {
      setStopping(runId);
      void stopRun(runId, { escalate })
        .then((res) => {
          setNotice(res.detail);
          void loadRuns();
        })
        .catch((err: unknown) => setNotice(err instanceof Error ? err.message : String(err)))
        .finally(() => setStopping(null));
    },
    [loadRuns]
  );

  async function act(what: string, run: () => Promise<void>): Promise<void> {
    setBusy(what);
    setError(null);
    setNotice(null);
    try {
      await run();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(null);
    }
  }

  return (
    <main className="flex min-h-0 min-w-0 flex-1 overflow-hidden">
      <WorkflowList
        files={files}
        dir={dir}
        runs={runs}
        selected={selected}
        openState={state}
        openType={type.trim()}
        typeByFolder={typeByFolder}
        folders={folders}
        onOpen={openWorkflow}
      />

      <section className="flex min-w-0 flex-1 flex-col overflow-hidden">
        {/*
          DISMISSIBLE, NOT SILENT, AND NOT AUTO-FIXED. This banner is TRUE — with no
          `KONTRA_RUN_TOKEN` set, anything that can reach this API can start a workflow that
          provisions cloud machines. It is not noise and it does not get deleted.

          It is also not something the operator can act on from here, and that is the part worth
          writing down: this SPA has NO way to send a bearer token — there is no `runToken` anywhere
          in `frontend/src`. So setting the variable the sentence asks for makes
          `checkOptionalBearer` 401 every serve and start, and locks the console out of the surface
          the banner is protecting. Until the SPA can authenticate, "set KONTRA_RUN_TOKEN" is advice
          that breaks the product, and a permanent banner for it is a warning nobody can clear.

          So: read once, dismissed per browser, and it comes BACK on a new session — because the
          exposure has not gone anywhere, only the operator's attention to it.
        */}
        {exposure?.open && !exposureHidden && (
          <div className="flex shrink-0 items-start gap-2 border-b border-amber-500/40 bg-amber-500/10 p-2.5 text-[12px]">
            <AlertTriangle size={15} className="mt-px shrink-0 text-amber-500" />
            <div className="min-w-0 flex-1">
              <strong className="font-semibold">This control surface is open.</strong>{' '}
              {exposure.detail}
            </div>
            <button
              type="button"
              data-testid="exposure-dismiss"
              aria-label="Dismiss the open-control-surface warning for this session"
              title="Dismiss for this session — the surface is still open"
              className="shrink-0 rounded px-1.5 py-0.5 text-muted-foreground hover:bg-amber-500/20 hover:text-foreground"
              onClick={() => setExposureHidden(true)}
            >
              ✕
            </button>
          </div>
        )}

        {!selected ? (
          <p className="p-5 text-[13px] text-muted-foreground">
            {files.length === 0
              ? 'Nothing here yet. Run `kontra init`, then drop a workflow in — or copy one from examples/python/workflows/.'
              : 'Pick a workflow to view, serve and run it.'}
          </p>
        ) : (
          <>
            <header className="shrink-0 border-b border-border px-5 pt-3.5">
              <div className="flex items-start justify-between gap-4">
                <div className="min-w-0">
                  <div className="flex min-w-0 items-center gap-2.5">
                    <h1 className="m-0 truncate font-mono text-[16px] font-semibold">
                      {selected}
                    </h1>
                    <StateDot state={state} />
                    <StatePill
                      state={stateWords(state).label}
                      testid="workflow-state"
                      title={stateWords(state).title}
                    />
                  </div>
                  <p className="m-0 mt-1.5 flex flex-wrap items-center gap-1.5 text-[11.5px] text-muted-foreground">
                    <code className="font-mono">{dir || '.kontra/workflows/'}</code>
                    {scan.refs.map((r) => (
                      <span
                        key={`${r.kind}:${r.name}:${r.version ?? ''}`}
                        className="rounded border border-border px-1.5 py-px font-mono"
                        title={
                          r.kind === 'actor'
                            ? 'an Actor this workflow dispatches to — something must be serving its queue'
                            : r.kind === 'fleet'
                              ? 'this workflow provisions its own Machines; its Bundle must be published (kontra build)'
                              : 'a Dataset it reads or writes'
                        }
                      >
                        <span className="opacity-60">{r.kind}</span> {r.name}
                        {r.version ? `@${r.version}` : ''}
                      </span>
                    ))}
                  </p>
                </div>

                <div className="flex shrink-0 flex-wrap justify-end gap-2">
                  <Button
                    size="sm"
                    variant="outline"
                    data-testid="serve-button"
                    onClick={() =>
                      void act('serve', async () => {
                        setServed(await serveWorkflow(selected));
                        setPaused(false);
                      })
                    }
                    /* No queue in the condition: there is nothing to type any more, so Serve is
                       available as soon as a workflow is selected. It used to be disabled until the
                       queue field was non-empty — a gate on a value the operator could not get
                       right, since the only correct one is derived from the manifest. */
                    disabled={busy !== null || !selected}
                  >
                    <Terminal size={14} className="mr-1.5" />
                    {busy === 'serve' ? 'Serving…' : 'Serve'}
                  </Button>

                  {/* PAUSE IS OFFERED ONLY WHEN THERE IS A WORKER TO PAUSE. It interrupts the
                      process in the pane, so a button that appeared for a workflow nobody is
                      serving would do nothing and say nothing. */}
                  {(state === 'serving' || state === 'running' || paused) && (
                    <Button
                      size="sm"
                      variant={paused ? 'default' : 'outline'}
                      data-testid="pause-button"
                      title={
                        paused
                          ? 'restart the worker; the run continues from history'
                          : 'interrupt the worker. The run makes no progress — but activities already ' +
                            'dispatched keep running, and its timeouts keep ticking, so a long pause ' +
                            'fails a run rather than holding it.'
                      }
                      onClick={() =>
                        void act('pause', async () => {
                          const res = await pauseWorkflow(selected, paused);
                          setPaused(!paused);
                          setNotice(res.detail);
                        })
                      }
                      disabled={busy !== null}
                    >
                      {paused ? <Play size={14} className="mr-1.5" /> : <Pause size={14} className="mr-1.5" />}
                      {busy === 'pause' ? '…' : paused ? 'Resume' : 'Pause'}
                    </Button>
                  )}

                  <Button
                    size="sm"
                    data-testid="run-button"
                    onClick={() =>
                      void act('run', async () => {
                        // The form is the contract for what starts. A required field left blank, or a
                        // value that will not coerce, is refused HERE — named — rather than posted and
                        // failed minutes later in a tmux pane.
                        if ('error' in result) throw new Error(result.error);
                        const started = await startRun(selected, type.trim(), result.value);
                        void loadRuns();
                        /* STAYS ON THIS PAGE. It used to leave for a global Runs surface — so the
                           one moment an author most wants their code, their input, the worker's
                           pane and the run all in view at once was the moment the page took four of
                           them away. That surface is retired; the run is watched HERE, and the only
                           thing that moves is the address, which now names the run so it can be
                           pasted or reloaded. */
                        setWatching(started.runId);
                      })
                    }
                    disabled={busy !== null || !type.trim() || !queue.trim() || !inputOk}
                  >
                    <Play size={14} className="mr-1.5" />
                    {busy === 'run' ? 'Starting…' : 'Run'}
                  </Button>
                </div>
              </div>

              <RunOptions
                open={showOptions}
                onToggle={() => setShowOptions((v) => !v)}
                queue={queue}
                type={type}
                setType={setType}
                defns={scan.defns}
                draft={draft}
                onDraft={setDraft}
                result={result}
              />
            </header>

            {/* A RUN IS OPEN AND NOTHING IS SERVING IT. The one state worth interrupting somebody
                for: it looks exactly like a slow run and is not one. Temporal reports it `running`
                the whole time it is making no progress. */}
            {stranded && (
              <div
                className="flex shrink-0 items-start gap-2 border-b border-rose-500/40 bg-rose-500/10 p-2.5 text-[12px]"
                data-testid="stranded-run"
              >
                <AlertTriangle size={15} className="mt-px shrink-0 text-rose-500" />
                <div>
                  <strong className="font-semibold">A Run is open and nothing is serving it.</strong>{' '}
                  Its worker is gone — a pause nobody resumed, a crashed process, a rebooted host —
                  so it is making no progress and will make none. Serve it again, or stop the run.
                </div>
              </div>
            )}

            {notice && (
              <p
                className="m-0 shrink-0 border-b border-border bg-muted/40 px-5 py-2 text-[11.5px] text-muted-foreground"
                data-testid="workflow-notice"
              >
                {notice}
              </p>
            )}

            {/* THE THREAD. This workflow's own runs down one side, six readings of the open one
                beside them — see `WorkflowThread.tsx`. What used to be here was a run TABLE, a
                source pane, a worker pane and a run log stacked down the page, each with its own
                dragged height: four things about one subject, competing for the same 800 pixels.
                They are the same four things, under one selection. */}
            {/* THE VIEWER AND THE PANE ARE HANDED IN, unchanged. Neither can load under the test
                runner (CodeMirror, xterm), and the thread has states worth pinning — so it takes
                what it frames, exactly as `Shell` does one level up. */}
            <WorkflowThread
              workflow={selected}
              thread={thread}
              tab={tab}
              onTab={setTab}
              onOpenRun={openOrClose}
              parked={parked}
              loaded={loaded}
              failure={turnsFailure}
              {...(following ? { follow: following.phase } : {})}
              onDrillTurn={(named) => {
                // WHICH TURN — and only that. Carrying the reader to the Event log is the thread's
                // own doing (`drillHandler`), because this file imports CodeMirror and cannot be
                // asserted in node: that half of the click must not live where nothing can check it.
                setDrilledTurn(openDrill(named).openTurn);
              }}
              openTurn={
                // MARKED ONLY WHILE THE PANEL IS REALLY THERE. `drilledTurn` can name a turn the
                // current reading has not produced yet — a frame during a switch — and a row
                // highlighted as open over nothing is a control that lies about its own state.
                drilled ? drilledTurn : null
              }
              drill={
                drilled && thread.selected ? (
                  <TurnDrill
                    runId={thread.selected.runId}
                    named={drilled}
                    history={following?.history ?? null}
                    onClose={() => setDrilledTurn(null)}
                  />
                ) : null
              }
              onStop={stop}
              stopping={stopping}
              deck={deck}
              now={now}
              code={
                <>
                  <div className="flex min-w-0 flex-[3] flex-col overflow-hidden">
                  <div className="flex h-[31px] shrink-0 items-center border-b border-border bg-muted/60">
                    <span className="flex h-full items-center gap-1.5 border-r border-border bg-background px-2.5 font-mono text-[11px]">
                      <span className="size-[5px] rounded-full bg-sky-300" />
                      {selected}
                    </span>
                    <span className="ml-auto px-2.5 font-mono text-[9.5px] text-muted-foreground">
                      {source.split('\n').length} lines · Python · read-only
                    </span>
                  </div>
                  {/* WHAT IS ON THIS DISK, AT THIS DIGEST (ADR 0030). The viewer is read-only, so the
                      panel's job is to confirm what is deployed rather than to change it: the folder's
                      path (to open in the operator's own editor) beside the digest of the code a worker
                      actually registered. Empty until this workflow is served. */}
                  <SourceProvenance
                    path={openFolder?.path || dir || '.kontra/workflows/'}
                    digest={queueDigest(contract?.queue)}
                    digestAbsent="not served yet"
                    testid="workflow-source-provenance"
                  />
                  {/* READ-ONLY, ON PURPOSE (ADR 0030). `editable={false}` refuses input while leaving
                      the text selectable and scrollable, so it stays a viewer and not a disabled
                      editor. A dashboard that wrote to disk let the file and the registered digest
                      disagree with nothing on screen; the strip above is where that shows now. ADR 0020
                      made the same call for Terminals. */}
                  <div className="min-h-0 flex-1 overflow-hidden">
                    <CodeMirror
                      value={source}
                      height="100%"
                      className="workflow-editor h-full text-[12px]"
                      theme={theme === 'dark' ? 'dark' : 'light'}
                      extensions={editorExtensions}
                      editable={false}
                    />
                  </div>
                  </div>
                  {/* THE CODE TAB IS THE CODE, AND NOTHING ELSE. Two things used to sit under the
                      editor: `WorkflowContract`'s field table in a `max-h-[40%]` box, and a copy of
                      the serve/start commands. Both cost height on the one tab whose entire job is
                      reading the source, and neither was the only place it could be found — the
                      descriptor still draws the Run form and the Scratch inspector, and the two
                      commands are what the Serve and Run buttons in this page's own header do. A
                      tab that shows a file should show the file. */}
                </>
              }
              monitor={
                /* SCOPED TO THE OPEN RUN, and the pane inside it is the SAME `WorkerPane` this tab
                   always held — handed the Terminal the operator picked instead of a session name
                   to go and resolve. A session name cannot address a Machine (nine droplets running
                   one Actor answer to one name), which is the whole reason the id travels. */
                <RunMonitor
                  run={thread.selected}
                  scoped={scoped}
                  focused={focusedPane}
                  onFocus={focusRunPane}
                  inventory={panes.length}
                  pane={
                    focusedPane ? (
                      <WorkerPane
                        key={focusedPane.terminal.id}
                        terminal={focusedPane.terminal}
                        className="flex-1"
                      />
                    ) : null
                  }
                />
              }
              data={
                /* THE OUTPUT, ONE CLICK FROM THE RUN THAT MADE IT — and it arrives on the Datasets
                   surface already scoped to this run, so the count there is a sentence about the
                   run you came from rather than a number to find again. */
                <RunDataTab
                  run={thread.selected}
                  datasets={wrote}
                  onOpen={(info) =>
                    openDataset({
                      name: info.name,
                      kind: info.kind,
                      ...(thread.selected ? { run: thread.selected.runId } : {}),
                    })
                  }
                />
              }
              scratch={
                /* THE DESIGN TAB, KEYED TO THE WORKFLOW AND NOT TO THE RUN (ADR 0026). Its state is
                   the page's (`useWorkflowSketch`, above) rather than the tab's, because the thread
                   renders one tab at a time and an unsaved drawing must survive a look at the
                   Transcript. */
                <WorkflowSketch {...sketch} />
              }
              stats={<RunStatsBar stats={stats} />}
              tail={thread.selected ? <RunTail runId={thread.selected.runId} /> : null}
            />
          </>
        )}

        {error && (
          <p
            className="m-5 mt-0 shrink-0 whitespace-pre-wrap rounded border border-destructive/40 bg-destructive/10 p-2 font-mono text-[12px] text-destructive"
            data-testid="workflow-error"
          >
            {error}
          </p>
        )}
      </section>
    </main>
  );
}

// --- the listing --------------------------------------------------------------------------------

/**
 * A dot that animates when something is happening, and does not when nothing is.
 *
 * THE ANIMATION IS A CLAIM, which is why `stateWords().live` decides it rather than the colour
 * doing double duty. `unknown` is drawn in a muted colour and does NOT pulse: a page insisting
 * something is happening while admitting it cannot tell is worse than one that says nothing.
 */
function StateDot({ state, small }: { state: WorkflowState; small?: boolean }): JSX.Element {
  const words = stateWords(state);
  const colour =
    state === 'running'
      ? 'bg-emerald-400'
      : state === 'serving'
        ? 'bg-sky-400'
        : state === 'unknown'
          ? 'border border-dashed border-muted-foreground'
          : 'bg-muted-foreground/40';
  return (
    <span
      data-testid={`state-dot-${state}`}
      data-live={words.live ? 'true' : undefined}
      title={words.title}
      className={`shrink-0 rounded-full ${small ? 'size-1.5' : 'size-2'} ${colour} ${
        words.live ? '[animation:kontra-pulse_1.8s_ease-in-out_infinite]' : ''
      }`}
    />
  );
}

function WorkflowList({
  files,
  dir,
  runs,
  selected,
  openState,
  openType,
  typeByFolder,
  folders,
  onOpen,
}: {
  files: WorkflowFile[];
  dir: string;
  runs: RunRow[];
  selected: string;
  /** The measured state of the workflow that is OPEN. See the row below for why only that one. */
  openState: WorkflowState;
  openType: string;
  /** Each row's `@workflow.defn` type, keyed by the name it opens under — the join key a closed row
   *  needs and cannot read for itself. A name missing here is a row whose type is UNKNOWN. */
  typeByFolder: Record<string, string>;
  /** The registered folders, held by the page so the register form and this list share one copy. */
  folders: FolderShelf;
  onOpen(name: string): void;
}): JSX.Element {
  /* ONE ROW PER REGISTERED FOLDER. NO FOLDER, NO WORKFLOW. The folder is what this list can act on
     — open, serve, start, forget — and drawing the files beside it put most workflows on screen
     twice in two vocabularies. The file half is joined in for its `description.md` sentence, which
     is the one thing a folder cannot say. */
  const rows = mergeWorkflowFolders(files, folders.sources);
  const dock = useSideDock('workflows', { width: 264, side: 'left', collapsed: false });

  if (dock.collapsed) {
    return (
      <div className="flex min-h-0" style={{ order: dock.side === 'left' ? -1 : 1 }}>
        <SideRail
          label="Workflows"
          badge={rows.length}
          testid="workflows-rail"
          onExpand={() => dock.setCollapsed(false)}
        />
      </div>
    );
  }

  const aside = (
    <aside
      /* THE WIDTH IS THE OPERATOR'S NOW. It was `w-[264px]` — one number, chosen once, against one
         screen — and a registered path like `/root/kontra-local/.claude/worktrees/…/nscheck` wraps
         to four lines inside it. */
      style={{ flex: `0 0 ${dock.width}px`, width: dock.width }}
      className="flex min-h-0 flex-col"
      data-testid="workflows-aside"
      data-side={dock.side}
    >
      <div className="shrink-0 border-b border-border px-3.5 py-2.5">
        <div className="flex items-baseline gap-2">
          <span className="text-[9.5px] uppercase tracking-wide text-muted-foreground">
            Workflows
          </span>
          {/* NOT "files". A workflow is a folder holding `workflow.py` and its `description.md`, so
              the count is of workflows — and counting files would have counted two per workflow.
              It counts ROWS, which includes a folder registered outside `dir`: that one is a
              workflow this console can serve and is not in the directory the title names. */}
          <span
            className="font-mono text-[10px] tabular-nums text-muted-foreground"
            title={dir || '.kontra/workflows/'}
          >
            {rows.length} {rows.length === 1 ? 'workflow' : 'workflows'}
          </span>
          <SideDockControls dock={dock} label="the workflow list" testid="workflows-dock" />
        </div>
        <div className="mt-1.5">
          <RegisterFolder
            kind="workflow"
            defaultRoot={folders.defaultRoot}
            onRegistered={folders.registered}
          />
        </div>
      </div>
      {/* `flex-1 min-h-0` so this scrolls INSIDE the aside. It sized to its content while it was the
          only thing here, which was invisible; with the folders under it, a checkout of more than a
          screenful of workflows would push them off the bottom instead of scrolling. */}
      <div className="flex min-h-0 flex-1 flex-col gap-0.5 overflow-y-auto p-1.5">
        {rows.length === 0 ? (
          <p className="m-0 p-2 text-[11px] text-muted-foreground">
            No folders registered, so there is nothing to serve. Run{' '}
            <code className="font-mono">kontra init</code> and drop a workflow folder under{' '}
            <code className="font-mono">{dir || '.kontra/workflows/'}</code>, or register one holding{' '}
            <code className="font-mono">workflow.py</code> from anywhere on this disk.
          </p>
        ) : (
          rows.map(({ name: rowName, file, folder }) => {
            const f = file ?? { name: rowName, description: folder.description };
            const isOpen = f.name === selected;
            /**
             * THE JOIN IS ON THE DECORATED TYPE, NOT THE FILENAME. `dhmonitor`'s class is
             * `DockerLeakMonitor`; the old `guessTypeFromFilename` read it as `Dhmonitor`, matched
             * no run, and painted a running workflow as NEVER RUN (#13). The open row already knows
             * its type from the source it is showing (`openType`); a closed row gets it from
             * `typeByFolder`, and a row whose type is not resolved yet is UNKNOWN — never `never run`.
             */
            const rowType = isOpen ? openType : typeByFolder[f.name];
            const info = rowStatus(rowType, runs);
            const mine = info.runs;
            /**
             * ONLY THE OPEN WORKFLOW GETS A MEASURED STATE, and the rest get what the run list
             * alone can say.
             *
             * The serving signal is one `DescribeTaskQueue` per QUEUE, and a queue is chosen per
             * session rather than declared in a file — so polling every row would mean guessing a
             * queue for each of them and asking Temporal about strings nobody serves. A row that
             * has a run open says so; a row that does not is left plain rather than labelled
             * `idle`, because this list cannot tell idle from unserved.
             */
            const rowState: WorkflowState = isOpen ? openState : info.state;
            const last = info.last;
            return (
              <div
                /* KEYED BY THE FOLDER — two checkouts of `nscheck` are two rows with one name, and
                   keying by that name would collapse them into one React element. */
                key={folder.id}
                className={`rounded ${isOpen ? 'bg-accent' : 'hover:bg-accent/50'}`}
                data-testid={`workflow-file-${f.name}`}
                data-state={isOpen || rowState === 'running' ? rowState : undefined}
              >
                {/* THE ROW OPENS IT; the forget control is a SIBLING of that button rather than
                    inside it. A button inside a button is invalid markup and the inner one stops
                    receiving clicks in some browsers — which is how the merged row would quietly
                    lose the affordance the merge was supposed to keep. */}
                <button
                  className="w-full px-2 pt-2 text-left"
                  onClick={() => onOpen(f.name)}
                  title={f.description ? `${f.name} — ${f.description}` : f.name}
                >
                  <div className="flex min-w-0 items-center gap-1.5">
                    <StateDot state={rowState} small />
                    <span className="min-w-0 flex-1 truncate font-mono text-[12px]">{f.name}</span>
                    <span className="shrink-0 font-mono text-[9.5px] tabular-nums text-muted-foreground">
                      {last ? new Date(last.startedAt).toLocaleDateString() : ''}
                    </span>
                  </div>
                  {/* WHAT IT IS FOR, from the folder's `description.md`. The whole reason a workflow
                      became a folder: a list of names says which workflows exist and nothing about
                      which one you want. Two lines, then it stops — the long form is the file. */}
                  {f.description && (
                    <p
                      className="m-0 mt-1 line-clamp-2 text-[10.5px] leading-snug text-muted-foreground"
                      data-testid={`workflow-description-${f.name}`}
                    >
                      {f.description}
                    </p>
                  )}
                  <div className="mt-1.5 flex items-end gap-2">
                    <StatePill
                      state={isOpen ? stateWords(openState).label : info.label}
                      testid={`file-state-${f.name}`}
                      title={isOpen ? stateWords(openState).title : undefined}
                      small
                    />
                    <span className="min-w-0 flex-1 text-muted-foreground">
                      <Streak bars={streakOf(mine)} width={112} height={16} />
                    </span>
                  </div>
                </button>

                {/* WHERE IT IS, on the row that names it. An operator with two checkouts of one
                    workflow is here to find out which one the console serves, and the row IS the
                    folder — so the path is always drawn, never a tooltip. */}
                <div className="flex min-w-0 items-start gap-1 px-2 pb-1.5">
                  <p
                    className="m-0 min-w-0 flex-1 truncate font-mono text-[10px] leading-snug text-muted-foreground"
                    data-testid={`workflow-path-${f.name}`}
                    title={folder.path}
                  >
                    {elidePath(folder.path)}
                  </p>
                  <FolderAbsent source={folder} testid={`workflow-${f.name}`} />
                  {/* THE SAME COMPONENT THE ACTOR CARD DRAWS. Two copies would eventually disagree
                      about the rule that costs a 400 — a discovered folder has no registration to
                      remove. */}
                  <FolderActions
                    source={folder}
                    busy={folders.forgetting !== null}
                    testid={`workflow-${f.name}`}
                    onForget={folders.forget}
                  />
                </div>
              </div>
            );
          })
        )}
      </div>

      {folders.error && (
        <p
          className="m-0 shrink-0 border-t border-border px-3.5 py-2 text-[10.5px] text-destructive"
          data-testid="folders-error-workflow"
        >
          {folders.error}
        </p>
      )}

      {openType && (
        <div className="shrink-0 border-t border-border px-3.5 py-2 text-[9.5px] text-muted-foreground">
          the dot on the open workflow is measured; the others report only their runs
        </div>
      )}
    </aside>
  );

  /* THE HANDLE IS ON THE EDGE THE PANEL IS NOT DOCKED TO, which is why the two are ordered here and
     not fixed in the markup: a left-docked panel is resized from its right edge and a right-docked
     one from its left, and a handle on the wrong side would drag the panel away from the pointer.
     `order` moves the whole group across the page without the parent knowing which side it is on. */
  const handle = (
    <SideResizer
      width={dock.width}
      onWidth={dock.setWidth}
      side={dock.side}
      label="the workflow list"
      testid="workflows-resizer"
    />
  );
  return (
    <div
      className={`flex min-h-0 ${dock.side === 'left' ? 'border-r' : 'border-l'} border-border`}
      style={{ order: dock.side === 'left' ? -1 : 1 }}
    >
      {dock.side === 'left' ? (
        <>
          {aside}
          {handle}
        </>
      ) : (
        <>
          {handle}
          {aside}
        </>
      )}
    </div>
  );
}

// --- this workflow's runs -------------------------------------------------------------------

// THE RUN TABLE THAT LIVED HERE IS `RunRail` IN `WorkflowThread.tsx` NOW, and cancel/terminate
// went with it — onto the scope bar, beside the open run's own name, rather than two buttons per
// row of a list. What each of them COSTS is unchanged and is still on the button: cancel is
// cooperative, so the workflow's scopes run their exits and a fleet it holds is destroyed;
// terminate skips them, so that fleet keeps billing, which is why the server cancels first.

/**
 * Queue, type and the run's input — folded because the first two are set once and the third is a
 * form now rather than a box to hand-write.
 *
 * THE INPUT IS A GENERATED FORM (`WorkflowInputForm`), built from the open workflow's declared input
 * the same way the Actors page builds a Batch. A workflow that declares its fields gets one input per
 * field, typed and coerced; a workflow that declares nothing keeps the raw JSON box, and says which.
 * It was a `<Textarea>` an operator hand-wrote `{"machines": 4}` into, with `JSON.parse` the only
 * check and no coercion — so `"4"` typed as a string went out as a string and failed at the worker.
 */
function RunOptions({
  open,
  onToggle,
  queue,
  type,
  setType,
  defns,
  draft,
  onDraft,
  result,
}: {
  open: boolean;
  onToggle(): void;
  queue: string;
  type: string;
  setType(v: string): void;
  defns: string[];
  draft: InputDraft;
  onDraft(next: InputDraft): void;
  /** What the form currently coerces to — drives the folded summary and the block error inside. */
  result: ReturnType<typeof inputOf>;
}): JSX.Element {
  return (
    <div className="pb-2.5 pt-2.5">
      <button
        type="button"
        className="flex items-center gap-1 font-mono text-[10.5px] text-muted-foreground hover:text-foreground"
        onClick={onToggle}
        data-testid="run-options"
      >
        {open ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
        queue <span className="text-foreground">{queue || '—'}</span> · type{' '}
        <span className="text-foreground">{type || '—'}</span> · input{' '}
        <span className={'error' in result ? 'text-destructive' : 'text-foreground'}>
          {inputSummary(result)}
        </span>
      </button>
      {open && (
        <div className="mt-2 flex flex-wrap items-start gap-3">
          {/* READ-ONLY, BECAUSE IT IS NO LONGER A CHOICE. The queue is derived from the folder's
              manifest — `wf-<name>-<version>`, `workflowControl.ts:workflowQueue` — so an input here
              would offer to override the one string that is guaranteed correct. It offered exactly
              that until now, prefilled with the literal `'recon'`, which is how a run went out to a
              queue nobody served and sat `running` forever. Shown rather than removed: where a start
              routes is worth knowing, and it is the first thing to check when a run does not move.
              Empty until a worker has registered this workflow — that is "nobody has served this
              yet", not "no queue". */}
          <label className="basis-52 text-[11px] text-muted-foreground">
            Queue <span className="opacity-60">(derived)</span>
            <output
              className="mt-1 block rounded border border-border bg-muted/40 px-2 py-1.5 font-mono text-[12px] text-foreground"
              data-testid="run-queue"
              title="derived from workflow.json as wf-<name>-<version>; the worker that registered this workflow reported polling it"
            >
              {queue || 'set when this workflow is served'}
            </output>
          </label>
          <label className="basis-44 text-[11px] text-muted-foreground">
            Type <span className="opacity-60">(@workflow.defn)</span>
            <Input
              className="mt-1 font-mono text-[12px]"
              value={type}
              onChange={(e) => setType(e.target.value)}
              spellCheck={false}
              list="workflow-defns"
            />
            <datalist id="workflow-defns">
              {defns.map((d) => (
                <option key={d} value={d} />
              ))}
            </datalist>
          </label>
          {/* THE FORM, from the SAME reading `WorkflowContract` draws its field table from — so it can
              never offer a field the contract below does not show. A declared input is fields; a
              `dict` or an undeclared one is the raw box, which is where the old hand-written JSON
              lives on as the fallback it was always meant to be, not a mode to delete. */}
          <div className="min-w-56 flex-1 text-[11px] text-muted-foreground">
            Input
            <div className="mt-1">
              <WorkflowInputForm draft={draft} onDraft={onDraft} result={result} />
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

/** The folded summary of the run's input: what it will send, or that it is not startable yet. */
function inputSummary(result: ReturnType<typeof inputOf>): string {
  if ('error' in result) return 'incomplete';
  return result.value === undefined ? 'none' : JSON.stringify(result.value);
}
