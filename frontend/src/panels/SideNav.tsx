/**
 * The nav rail: five surfaces, what each holds, and what the fleet is doing right now.
 *
 * A RAIL RATHER THAN A TOP BAR, and it is a measurement decision. Most of these surfaces are
 * full-height columns of their own — the Workflows list, the run transcript, the Monitor's wall,
 * the Datasets schema — so a horizontal bar spent a whole row of a 900-pixel screen to say five
 * words, and every one of those pages then started 40 px lower. Vertically the same words cost
 * width nothing else wanted, and the counters at the bottom get a home that is not somebody's page.
 *
 * THE COUNT BESIDE EACH ITEM IS THE INVENTORY OF THAT SURFACE, so "how many workflows / actors /
 * panes / datasets" is answerable without visiting all four. They are the numbers a session starts
 * by wanting and then never asks again, which is exactly the shape a rail is for.
 *
 * THE PULSE IS THE DEBT THE RETIREMENT LEFT, AND IT IS PAID IN TWO PLACES.
 *
 * Retiring the global Runs view removed the one page that answered "is anything happening at all,
 * anywhere" — a question an operator asks from whichever surface they happen to be on, which is
 * precisely when the answer has to be in the chrome and not on a page. So:
 *
 *   THE DOTS ON THE WORKFLOWS ICON are the glanceable half. An inventory of RUNS, not of workflows,
 *     so they are marks beside the count rather than the count itself — `3` workflows of which some
 *     are running is two facts and must read as two. They survive collapsing, because a 48-pixel
 *     rail is exactly the state an operator is in when they ask.
 *   THE PULSE BLOCK IN THE FOOTER is the readable half: the word, the numbers, the sentence behind
 *     them, and the way in. It is a COUNT AND A WAY IN, never a table — restoring a global list of
 *     runs through the chrome would undo the decision the retirement was paid for. Clicking reaches
 *     a run the way every route to a run now does, through the Workflows surface.
 *
 * `idle` IS A WORD HERE, NOT A ZERO. The footer used to carry `in flight  0` beside `live panes  0`
 * beside `units/s  —`, and three zeroes in a column read as a page that failed to load rather than
 * as an appliance at rest. And a fourth reading — `not known` — exists at all because "we could not
 * ask" and "nothing is happening" must never render the same; see `chrome/pulse.ts`, which is where
 * all four are decided and where they can be tested.
 *
 * THE REST OF THE FOOTER IS THE OTHER THING STILL HAPPENING WHILE YOU LOOK ELSEWHERE. `units/s` and
 * its line are MEASURED — rows landing in the lake between two catalog samples, never an animation
 * (see `components/spark.ts`). `live` is how many Terminals hold a real PTY attach: an sshd session,
 * a PTY and a per-viewer tmux session on an `s-1vcpu-2gb` Machine each (ADR 0020), and it is the
 * number an operator most needs from a page that is NOT the Monitor, because that is exactly when
 * they have forgotten about it.
 *
 * SPLIT PROPS-IN / MARKUP-OUT. {@link NavRail} is drawn from its arguments and nothing else, so a
 * node test can render it with any surface marked, at either width, with counts taken and counts
 * not taken, idle, running, parked, mixed and unreachable. {@link SideNav} is the few lines that
 * read the store — zustand hands `renderToStaticMarkup` the SERVER snapshot, so a `setState` in a
 * test cannot move anything the container reads, and every state worth pinning would have been
 * unreachable through it.
 */

import {
  Boxes,
  Database,
  LayoutGrid,
  Moon,
  PanelLeftClose,
  PanelLeftOpen,
  Settings,
  Sun,
  Workflow,
  type LucideIcon,
} from 'lucide-react';
import { Spark, SPARK_COLORS } from '../components/Spark';
import { stillParked } from './ask';
import { useAppStore, type Theme, type View, type WallCounts } from '../state/store';
import { SURFACES } from '../state/surfaces';
import { NAV_COLLAPSED_KEY, usePersistedFlag } from './chrome/persistedFlag';
import { readPulse, type PulseReading, type PulseTone } from './chrome/pulse';

/** Expanded. Was `w-60` (240px) — five short words never needed that, and every surface here is a
 *  full-height column that wanted the pixels more than the rail did. */
const RAIL_W = 'w-44';
/** Collapsed: icons only, wide enough for a 16px glyph and its hit target and nothing else. */
const RAIL_W_COLLAPSED = 'w-12';

/** One glyph per surface. A `Record<View, …>` on purpose: a sixth surface added to `surfaces.ts`
 *  and given no icon fails to compile, rather than drawing an item with a hole in it. */
const ICONS: Record<View, LucideIcon> = {
  workflows: Workflow,
  actors: Boxes,
  datasets: Database,
  monitor: LayoutGrid,
  settings: Settings,
};

/**
 * One colour per reading, and the resting one is deliberately the quietest thing on the rail.
 *
 * NOTHING BLINKS AND NOTHING IS RED. A run that is grinding is emerald because it is working; a run
 * waiting on a person is sky because a durable workflow blocked on a human is working as designed
 * and can legitimately do it for hours; `idle` and `not known` are the muted foreground, because an
 * appliance at rest is not a warning and an appliance that could not be asked is not one either —
 * it is a sentence, and the sentence is in the tooltip.
 */
const TONE_TEXT: Record<PulseTone, string> = {
  unread: 'text-muted-foreground',
  idle: 'text-muted-foreground',
  running: 'text-emerald-500',
  parked: 'text-sky-500',
  unknown: 'text-muted-foreground',
};

/** The 6-pixel mark beside each fact. `idle` is a hollow ring rather than a filled dot: at rest
 *  there is nothing to point at, and an outline says "read, and empty" where a grey disc would say
 *  "something, greyed out". */
const TONE_DOT: Record<PulseTone, string> = {
  unread: 'border border-border',
  idle: 'border border-muted-foreground/60',
  running: 'bg-emerald-500',
  parked: 'bg-sky-500',
  unknown: 'border border-dashed border-muted-foreground/60',
};

/**
 * The tooltip on one nav item, as a function of what the rail can currently show.
 *
 * COLLAPSING IS MEANT TO COST WIDTH, NOT KNOWLEDGE. A collapsed rail hides each item's label and
 * its inventory count, so both move into the tooltip — which is the whole rule, and it is three
 * lines.
 *
 * IT WAS `chrome/navTitle.ts`, AND IT IS HERE NOW BECAUSE THE REASON IT WAS NOT IS GONE. That file
 * carried nine lines explaining that it could not be tested any other way: the render suites used
 * `renderToStaticMarkup`, zustand's React adapter feeds the SERVER snapshot down that path, and a
 * `useAppStore.setState` in a test therefore moved nothing the markup could see. The suite has a
 * DOM now, `setState` is visible through a real render, and the tooltip is asserted where an
 * operator meets it — as the `title` on the button (`sideNav.render.test.ts`). A module whose only
 * justification was the reach of the test runner stops being a module when the runner changes.
 */
function navTitle(label: string, hint: string, count: number | null, collapsed: boolean): string {
  if (!collapsed) return hint;
  // `null` means NOT COUNTED YET, never zero — a surface publishes its own count when it first
  // loads, and `Workflows (0)` about a directory nobody has read is a false statement, not a
  // pessimistic one. The label alone is the honest answer until then.
  return count === null ? `${label} — ${hint}` : `${label} (${count}) — ${hint}`;
}

export interface NavRailProps {
  view: View;
  /** The inventory of each surface. `null` is NOT COUNTED YET and is not zero — see {@link navTitle}. */
  counts: Record<View, number | null>;
  /**
   * What is happening across everything, right now — already read (`chrome/pulse.ts`).
   *
   * A READING RATHER THAN THE NUMBERS, because the interesting part is not the count: it is which
   * of idle, running, parked and not-known this is, which of those may be said at the same time,
   * and what the sentence behind the mark is. All of that is decided in a module a node test can
   * drive from a literal, and none of it is decided in this file.
   *
   * IT IS IN THE CHROME BECAUSE THAT IS THE ONLY PLACE IT WORKS. An operator is told the fleet is
   * grinding, or that they are the bottleneck, while they are looking at the Datasets surface, or
   * the Monitor, or nothing at all; a mark on the page they would have to already be on says
   * nothing to the person it is for.
   */
  pulse: PulseReading;
  collapsed: boolean;
  theme: Theme;
  wall: WallCounts;
  /** Rows-per-second across every Dataset. Empty until two samples exist — one sample is not a rate. */
  fleetSeries: readonly number[];
  unitsPerSec: number;
  onView(view: View): void;
  /** The way in. Called with the ONE run the pulse can point at unambiguously; everything else
   *  reaches the Workflows surface through {@link NavRailProps.onView}. */
  onOpenRun(runId: string): void;
  onCollapsed(collapsed: boolean): void;
  onTheme(theme: Theme): void;
}

export function NavRail({
  view,
  counts,
  pulse,
  collapsed,
  theme,
  wall,
  fleetSeries,
  unitsPerSec,
  onView,
  onOpenRun,
  onCollapsed,
  onTheme,
}: NavRailProps): JSX.Element {
  return (
    <nav
      className={`flex ${collapsed ? RAIL_W_COLLAPSED : RAIL_W} shrink-0 flex-col border-r border-border bg-card`}
      data-testid="side-nav"
      data-collapsed={collapsed ? 'true' : undefined}
    >
      <div
        className={`flex items-center border-b border-border py-3 ${collapsed ? 'flex-col gap-2 px-0' : 'gap-2.5 px-3.5'}`}
      >
        <span className="size-2.5 shrink-0 rounded-[3px] bg-gradient-to-br from-emerald-400 to-sky-300 shadow-[0_0_10px_rgba(52,211,153,.55)]" />
        {/* The wordmark is the first thing to go: it is the one element here that identifies
            rather than navigates, and the dot already does that in 10 pixels. */}
        {!collapsed && (
          <span className="font-mono text-[13px] font-semibold tracking-tight">kontra</span>
        )}
        <button
          type="button"
          data-testid="nav-collapse"
          className={`rounded p-1 text-muted-foreground hover:bg-accent hover:text-foreground ${collapsed ? '' : 'ml-auto'}`}
          onClick={() => onCollapsed(!collapsed)}
          title={collapsed ? 'Expand the rail' : 'Collapse the rail to icons'}
          aria-label={collapsed ? 'expand navigation' : 'collapse navigation'}
          aria-expanded={!collapsed}
        >
          {collapsed ? <PanelLeftOpen size={14} /> : <PanelLeftClose size={14} />}
        </button>
        {!collapsed && (
          <button
            type="button"
            className="rounded p-1 text-muted-foreground hover:bg-accent hover:text-foreground"
            onClick={() => onTheme(theme === 'dark' ? 'light' : 'dark')}
            title="Toggle theme"
            aria-label="toggle theme"
          >
            {theme === 'dark' ? <Sun size={14} /> : <Moon size={14} />}
          </button>
        )}
      </div>

      <div className={`flex flex-col gap-0.5 ${collapsed ? 'p-1.5' : 'p-2'}`}>
        {SURFACES.map((surface) => {
          const Icon = ICONS[surface.id];
          const count = counts[surface.id];
          /* THE ONE SURFACE THAT CARRIES A LIVE FACT. Nothing else in the rail is about right now:
             an actor count and a dataset count are inventories that change by the hour.

             `pulse.running` IS `null` WHEN THE APPLIANCE COULD NOT BE ASKED, and a `null > 0` is
             false — which is the behaviour wanted: no dot rather than a green one over a cluster
             nobody can see into. The footer says "not known" in words; a dot cannot. */
          const inFlight = surface.id === 'workflows' && (pulse.running ?? 0) > 0;
          /* THE SECOND LIVE FACT, AND THE ONLY ONE ABOUT THE READER. `running` says the machine is
             busy; this says the machine has stopped and is waiting for a person. They ride on the
             same icon because they are both about runs, and they are two marks rather than one
             because a run that is grinding and a run that is parked are opposite situations. */
          const asking = surface.id === 'workflows' && pulse.parked > 0;
          return (
            <button
              key={surface.id}
              type="button"
              data-testid={`nav-${surface.id}`}
              data-active={view === surface.id ? 'true' : undefined}
              title={navTitle(surface.label, surface.hint, count, collapsed)}
              className={`flex items-center rounded text-left text-[12.5px] ${
                collapsed ? 'justify-center px-0 py-2' : 'justify-between gap-2 px-2.5 py-1.5'
              } ${
                view === surface.id
                  ? 'bg-accent font-semibold text-foreground'
                  : 'text-muted-foreground hover:bg-accent/50'
              }`}
              onClick={() => onView(surface.id)}
            >
              <span className="flex items-center gap-2">
                <span className="relative flex shrink-0 items-center">
                  <Icon size={15} className="shrink-0" />
                  {/* IT SURVIVES COLLAPSING, unlike every other number here. "Is anything running"
                      is the question the retired Runs surface used to answer from anywhere, and a
                      48-pixel rail is exactly the state an operator is in when they ask it. */}
                  {inFlight && (
                    <span
                      className="absolute -right-1 -top-0.5 size-1.5 rounded-full bg-emerald-500"
                      data-testid="nav-running"
                      data-value={pulse.running ?? undefined}
                      title={pulse.because}
                    />
                  )}
                  {/* BELOW THE RUNNING DOT, NEVER INSTEAD OF IT. A run can be parked while three
                      others grind, and a mark that replaced the other would make the busy fleet
                      disappear the moment one question was asked. */}
                  {asking && (
                    <span
                      className="absolute -bottom-0.5 -right-1 size-1.5 rounded-full bg-sky-500"
                      data-testid="nav-parked"
                      data-value={pulse.parked}
                      title={pulse.because}
                    />
                  )}
                </span>
                {!collapsed && <span>{surface.label}</span>}
              </span>
              {!collapsed && (
                <span
                  className="font-mono text-[10px] tabular-nums opacity-60"
                  data-testid={`nav-inventory-${surface.id}`}
                >
                  {count ?? ''}
                </span>
              )}
            </button>
          );
        })}
      </div>

      {/* COLLAPSED KEEPS THE TWO THINGS THAT ARE ABOUT RIGHT NOW, and nothing else. The pulse,
          because "is anything happening" is the question a 48-pixel rail is least able to send you
          somewhere else to answer; and `live`, which counts Terminals holding a real PTY attach —
          an sshd session, a PTY and a per-viewer tmux session per Machine — and is the number an
          operator most needs from a page that is NOT the Monitor, precisely because that is when
          they have forgotten about it. The spark and `units/s` are answered by visiting a surface;
          a leaking attach and a stopped fleet are not. */}
      {collapsed ? (
        <div
          className="mt-auto flex flex-col items-center gap-1 border-t border-border py-3"
          data-testid="nav-counts"
        >
          <Pulse reading={pulse} collapsed onView={onView} onOpenRun={onOpenRun} />
          <span className="text-[9px] uppercase tracking-wide text-muted-foreground">live</span>
          <span
            className={`font-mono text-[11px] tabular-nums ${wall.live > 0 ? 'text-emerald-500' : 'text-foreground'}`}
            data-testid="nav-count-livepanes"
            data-value={wall.live}
            title="Terminals holding a real PTY attach. Zero while the Monitor is closed, because closing it kills them."
          >
            {wall.live}
          </span>
        </div>
      ) : (
        <div
          className="mt-auto flex flex-col gap-2 border-t border-border px-3.5 py-3"
          data-testid="nav-counts"
        >
          {/* FIRST IN THE FOOTER, above the throughput it explains. `units/s` says how fast rows
              are landing; this says whether anything is putting them there at all, and a rate over
              a fleet you have not been told is idle is a number with nothing to stand on. */}
          <Pulse reading={pulse} collapsed={false} onView={onView} onOpenRun={onOpenRun} />
          <div className="text-[9px] uppercase tracking-wide text-muted-foreground">
            Fleet throughput
          </div>
          <div className="h-[26px] text-muted-foreground" data-testid="fleet-spark">
            {/* Nothing until two samples exist. A flat line before the first measurement would read
                as a fleet that is running and writing nothing — the single most alarming thing this
                rail can say, and it would be saying it about a page that just loaded. */}
            <Spark
              series={fleetSeries}
              color={SPARK_COLORS.throughput}
              width={132}
              height={26}
              fill
              title="rows committed per second across every Dataset, sampled from the catalog"
            />
          </div>
          <Count
            label="units/s"
            value={fleetSeries.length === 0 ? '—' : Math.round(unitsPerSec).toLocaleString()}
            title="rows landing in the lake per second, measured between two catalog samples"
          />
          <Count
            label="live panes"
            value={wall.live}
            // Never dressed up when it is zero: a wall of snapshots is the resting state and the
            // cheap one, and an operator should be able to tell at a glance that nothing is attached.
            className={wall.live > 0 ? 'text-emerald-500' : undefined}
            title="Terminals holding a real PTY attach — an sshd session, a PTY and a per-viewer tmux session on each Machine. Zero while the Monitor is closed, because closing it kills them."
          />
        </div>
      )}
    </nav>
  );
}

/**
 * What is happening across everything, and the way in.
 *
 * A BUTTON ONLY WHEN THERE IS SOMEWHERE TO GO. An idle appliance has nothing to open and a control
 * that navigates nowhere is worse than a line of text that never claimed it would — so `entry.kind
 * === 'none'` renders a `div`, which is also what keeps a resting rail from offering a hover state
 * over the word `idle`.
 *
 * ONE RUN IS A DESTINATION; TWO ARE A SURFACE. When exactly one run is waiting on a person there is
 * nothing to choose between, so "in" means that conversation — reached through the Workflows
 * surface, which is how every route to a run works now. Past one, the honest destination is the
 * surface that holds them: picking the first silently would be a guess, and listing them here would
 * be the retired Runs page rebuilt inside the chrome.
 *
 * NOTHING DRAWN BEFORE THE FIRST ANSWER. `facts` is empty then, and a rail that flashed a word it
 * was about to correct on every page load is a rail nobody reads.
 */
function Pulse({
  reading,
  collapsed,
  onView,
  onOpenRun,
}: {
  reading: PulseReading;
  collapsed: boolean;
  onView(view: View): void;
  onOpenRun(runId: string): void;
}): JSX.Element | null {
  if (reading.facts.length === 0) return null;
  const { entry } = reading;
  const enter =
    entry.kind === 'none'
      ? undefined
      : entry.kind === 'run'
      ? () => onOpenRun(entry.runId)
      : () => onView('workflows');

  const body = collapsed ? (
    <>
      {reading.facts.map((fact) => (
        <span key={fact.key} className="flex flex-col items-center">
          <span className="text-[9px] uppercase tracking-wide text-muted-foreground">
            {fact.short}
          </span>
          {fact.value === null ? (
            <span className={`size-1.5 rounded-full ${TONE_DOT[fact.tone]}`} />
          ) : (
            <span
              className={`font-mono text-[11px] tabular-nums ${TONE_TEXT[fact.tone]}`}
              data-testid={`nav-pulse-${fact.key}`}
              data-value={fact.value}
            >
              {fact.value}
            </span>
          )}
        </span>
      ))}
    </>
  ) : (
    <>
      {reading.facts.map((fact) => (
        <span key={fact.key} className="flex items-center gap-2 text-[11px]">
          <span className={`size-1.5 shrink-0 rounded-full ${TONE_DOT[fact.tone]}`} />
          <span
            className={TONE_TEXT[fact.tone]}
            data-testid={`nav-pulse-${fact.key}`}
            data-value={fact.value ?? undefined}
          >
            {fact.text}
          </span>
        </span>
      ))}
    </>
  );

  const shape = collapsed
    ? 'flex flex-col items-center gap-1'
    : 'flex flex-col items-start gap-1 rounded text-left';
  const shared = {
    'data-testid': 'nav-pulse',
    'data-tone': reading.tone,
    'data-entry': entry.kind,
    title: reading.because,
  };

  return enter === undefined ? (
    <div {...shared} className={shape}>
      {body}
    </div>
  ) : (
    <button
      {...shared}
      type="button"
      className={`${shape} hover:bg-accent/50 ${collapsed ? '' : '-mx-1 px-1 py-0.5'}`}
      onClick={enter}
      aria-label={
        entry.kind === 'run'
          ? `open ${entry.runId}, which is waiting on you`
          : 'open the Workflows surface'
      }
    >
      {body}
    </button>
  );
}

/** The rail, wired to the store. Everything it decides is in {@link NavRail}. */
export function SideNav(): JSX.Element {
  const view = useAppStore((s) => s.view);
  const setView = useAppStore((s) => s.setView);
  const openRun = useAppStore((s) => s.openRun);
  const theme = useAppStore((s) => s.theme);
  const setTheme = useAppStore((s) => s.setTheme);
  const wall = useAppStore((s) => s.wall);
  const runs = useAppStore((s) => s.runs);
  const parked = useAppStore((s) => s.parked);
  const pulse = useAppStore((s) => s.pulse);
  const pulseError = useAppStore((s) => s.pulseError);
  const actorFolderCount = useAppStore((s) => s.actorFolderCount);
  const datasets = useAppStore((s) => s.datasets);
  const workflowCount = useAppStore((s) => s.workflowCount);
  const fleetSeries = useAppStore((s) => s.fleetSeries);
  const unitsPerSec = useAppStore((s) => s.unitsPerSec);
  const [collapsed, setCollapsed] = usePersistedFlag(NAV_COLLAPSED_KEY);

  // PRUNED HERE RATHER THAN IN THE STORE, because the run list is what makes an entry stale and
  // the list is polled by the surface that shows it. A closed run is never parked (`runWord`), and
  // this mark OUTLIVES the page that set it — so without this it would sit on the rail forever
  // after the run it was about failed. `readPulse` prunes the same set again on the one statement
  // that needs no list at all: nothing running means nothing parked.
  const waiting = stillParked(parked, runs);

  const counts: Record<View, number | null> = {
    // `null` where the number is not known yet — the Workflows page publishes its own file count
    // when it loads, and a rail that printed 0 before then would be stating something false about
    // a directory it has not read.
    workflows: workflowCount,
    // REGISTERED FOLDERS, not the catalog. It was `catalog.length` — 23 here, over a page drawing
    // one card — because the Actors surface lists what is on THIS disk and the catalog is what any
    // worker anywhere ever said about itself. Same rule as `datasets` below.
    actors: actorFolderCount,
    // DATASETS, not listing rows. `/api/datasets` returns one row per `version=…/dt=…` partition, so
    // this counted DISPATCHES — the rail said 54 over a page showing 11 names, and the inventory of
    // a surface has to be the thing that surface lists.
    datasets: new Set(datasets.map((d) => `${d.kind}:${d.name}`)).size,
    monitor: wall.panes,
    // SETTINGS HAS NO INVENTORY. A count of secrets would be a number about a store that does not
    // exist yet, and `0` next to it would read as "none set" rather than "nothing to set them in".
    settings: null,
  };

  return (
    <NavRail
      view={view}
      counts={counts}
      // THE BROWSER'S CLOCK, and it only feeds the "waiting 4m" clause in the tooltip. The server
      // timestamps its own reading (`Pulse.at`), so nothing here dates a park by when this render
      // happened to run.
      pulse={readPulse({ pulse, error: pulseError, known: waiting, now: Date.now() })}
      collapsed={collapsed}
      theme={theme}
      wall={wall}
      fleetSeries={fleetSeries}
      unitsPerSec={unitsPerSec}
      onView={setView}
      onOpenRun={openRun}
      onCollapsed={setCollapsed}
      onTheme={setTheme}
    />
  );
}

function Count({
  label,
  value,
  title,
  className,
}: {
  label: string;
  value: number | string;
  title: string;
  className?: string;
}): JSX.Element {
  return (
    <div
      className="flex justify-between font-mono text-[10px] text-muted-foreground"
      title={title}
      data-testid={`nav-count-${label.replace(/[^a-z]/g, '')}`}
      data-value={value}
    >
      <span>{label}</span>
      <span className={`tabular-nums ${className ?? 'text-foreground'}`}>{value}</span>
    </div>
  );
}
