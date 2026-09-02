/** Actors: what is deployed and registered, as a grid of cards.
 *
 * WHAT ONE CARD SAYS is `ActorCard.tsx` — the Methods, their signatures and where an Actor's code
 * is. This file is the page around them: the two inventories, the join between them, and which one
 * the workbench is open over.
 *
 * THE ONE LIVE NUMBER ON THIS PAGE IS MACHINES, and that is not a compromise — it is the only live
 * number an Actor has. Nothing on either SDK's registration path carries a call rate, a p95 or an
 * in-flight count, so a card claiming one would be claiming it from nowhere. What IS real is the
 * Monitor's Terminal inventory: every Machine running this Actor has a pane, tagged with the
 * actor and version it is running, and its health probe says whether the worker is polling. That
 * answers the question this page is actually opened with — "is anything serving this queue" —
 * which is the difference between a dispatch that runs and one that waits forever.
 *
 * NO FOLDER, NO ACTOR. This page drew the CATALOG — every Actor any worker ever registered about
 * itself, on any machine — which after one real run was twenty-three cards, most of them saying
 * `source unknown` and `no registered folder`. Those are deployments the reader cannot open, edit,
 * serve, call or forget: a page of things you do not control, which is a page you stop opening. And
 * it fails the other way too — the catalog is a LIVE inventory that empties when the workers stop,
 * so the same page went blank on an installation with folders registered and nothing serving them.
 * A registered folder is a path somebody pointed at on THIS disk, and the four things this surface
 * can do all need one — so the folder is the unit, and a catalog row with no folder is not drawn.
 *
 * WHAT THE CATALOG STILL ADDS is everything a running worker said about that code: its Methods and
 * their signatures, the Machines serving it, the digest, and what the version broke. That is why
 * the join survives the inversion — `catalogForFolder` reads it from the folder's side, and takes
 * the version ON DISK when a directory has several deploys behind it, so four `cachebuster` rows
 * are one card rather than four.
 *
 * AND A FOLDER NOBODY HAS SERVED IS STILL A CARD, marked `unserved`, because register → edit →
 * serve starts there. It claims no deployment: its Methods are unknown rather than absent, and the
 * card says which.
 *
 * THE FILTER IS FOR WHAT IS LEFT. Even folders-only, an installation with several checkouts and
 * several versions is a grid to scan, so one row narrows it: a name menu that collapses the
 * versions, a box over every field including each Method's name and its author's sentence, and two
 * toggles for the states worth asking about. The count beside them is never conditional — a filter
 * that hides eight cards must not read as a page that lost eight.
 *
 * AND THE WORKBENCH SERVES IT — locally, on the machine the orchestrator is on, and nowhere else.
 * `kontra serve --actor` can also start managed containers or place a Worker on Machines that keep
 * billing after the tab is closed; neither is reachable from anywhere on this page, and that is a
 * decision (`backend/src/actorControl.ts`) rather than an unfinished dropdown. What the
 * workbench does have beside the editor is the worker's own tmux pane, because a worker that starts
 * and registers nothing is this system's hardest failure to see: the file saves, the serve returns,
 * the counts above stay exactly as they were, and the traceback is in a pane nobody thought to
 * attach to.
 *
 * AND CALLING A METHOD CALLS THE METHOD (ADR 0033). This page used to hand over code instead — a
 * caller workflow you saved into a folder, served and started — on the premise that the server
 * could not dispatch. What ADR 0023 §12 actually removed was a general INTERPRETER executing
 * user-composed topologies; the invariant that survived is narrower and still holds, which is that
 * the orchestrator starts workflows and does not execute Batches. So `call` on a Method row starts
 * one execution of a kontra-owned workflow that makes exactly ONE Method call over the Batch you
 * typed, through the same Nexus operation production uses, into an untagged Dataset.
 *
 * ONE ACTOR, ONE VERSION, ONE METHOD, ONE BATCH — and the count is the line. There is no control
 * here that names a second Method, because the request has no field for one. Two is a topology, and
 * the way to run one is the caller shown read-only beside the button: copy it, own the dispatch, and
 * compose inside your own workflow, which is where composition has always belonged.
 *
 * THE GRID STAYS THE LANDING VIEW. The Methods and their descriptions are what this page is opened
 * for; the workbench and the call panel are clicks through and take the whole surface when they are
 * open, because an editor in a card-sized box is neither.
 */

import { useCallback, useEffect, useMemo, useState } from 'react';
import { RefreshCw } from 'lucide-react';
import { sharedQueue } from '@core/panels/pollers';
import { useAppStore } from '../state/store';
import { fetchPollers, fetchSlots, type ActorSlots } from '../run/api';
import type { PollerReport } from '../run/workflowState';
import type { Terminal as PaneTerminal } from './panelsClient';
import { terminalIsHealthy } from './chrome/tileRef';
import { ActorCard } from './ActorCard';
import { RegisterFolder } from './RegisterFolder';
import { useRegisteredFolders } from './RegisteredFolders';
import { FolderWorkbench } from './FolderWorkbench';
import { MethodCall } from './MethodCall';
import { catalogForFolder, folderForActor } from './sourceFolders';
import { ActorFilterBar } from './ActorFilterBar';
import { actorNames, matchActor, EMPTY_ACTOR_FILTER, type ActorFilter } from './actorFilter';
import { Button } from '@/components/ui/button';

/**
 * How often the page asks Temporal who is polling each Actor's queue.
 *
 * SLOWER THAN THE WORKFLOWS PAGE (4 s), and deliberately: that one follows ONE open workflow's
 * queue and uses a poller appearing as the cue to re-read a descriptor, while this asks about every
 * card on the page at once. What bounds it is the signal's own resolution — `POLL_FRESH_MS` is two
 * minutes, so a worker that dies is drawn stale within at most the window plus one tick, and a
 * faster cadence would buy a few seconds of latency on a two-minute measurement at the cost of a
 * request per card per tick, forever, on every open tab.
 */
const POLLERS_MS = 10_000;

export default function ActorsPage() {
  const catalog = useAppStore((s) => s.catalog);
  const loadCatalog = useAppStore((s) => s.loadCatalog);
  const loadPanes = useAppStore((s) => s.loadPanes);
  const setActorFolderCount = useAppStore((s) => s.setActorFolderCount);
  const panes = useAppStore((s) => s.panes);
  const [open, setOpen] = useState<Record<string, boolean>>({});
  /** The folder whose workbench is open, by id — NOT the Source itself. The list is re-read after
   *  every save (a new description.md changes what the row says), and holding the object would keep
   *  drawing the copy from before the reload. An id that is no longer listed closes the workbench,
   *  which is what forgetting the registration while editing it should do. */
  const [editing, setEditing] = useState<string | null>(null);
  /** The Method being called, by catalog key and Method name. Held here rather than in the card so
   *  it survives the card re-rendering, and so the panel gets the whole surface — a Batch table, a
   *  run report and a source block do not fit in a 452px column. */
  const [calling, setCalling] = useState<{ key: string; method: string } | null>(null);
  const [filter, setFilter] = useState<ActorFilter>(EMPTY_ACTOR_FILTER);
  /**
   * WHO IS POLLING EACH ACTOR'S QUEUE, by queue name — the only authority on "can this run".
   *
   * A queue with NO ENTRY is not the same as one nobody polls: the card draws a missing entry as
   * `unknown`, which is what it is before the first tick answers. The map is replaced rather than
   * merged on every tick so a folder that is forgotten stops carrying a report that nothing will
   * refresh again.
   */
  const [pollers, setPollers] = useState<Record<string, PollerReport>>({});
  /** When the reports above were read. Every age on the page is measured against THIS, not against
   *  render time — a card re-rendered for an unrelated reason must not age a poll it did not
   *  re-read. */
  const [polledAt, setPolledAt] = useState(0);
  /** Bumped by the refresh button. It is in the poll effect's deps, so pressing refresh restarts the
   *  interval with an immediate tick — the alternative was a second, unguarded copy of the fetch
   *  that could set state after the page had gone. */
  const [pollNonce, setPollNonce] = useState(0);
  /**
   * WHAT EACH ACTOR WILL ASK FOR — its declared credential slots, joined against the operator's
   * bindings (issue 20), keyed by actor name.
   *
   * ONE FETCH FOR THE WHOLE GRID rather than one per card: the surface is a handful of names and
   * states, and a request per card would put a burst on the credential API every time this page
   * opens. A missing entry is `null` on the card, which draws nothing — most actors ask for no
   * credential, and that is not a state worth a line on every card.
   *
   * READ ONCE, NOT POLLED. A slot's state changes when an operator binds something, which happens
   * on the Settings page, not here; the refresh button re-reads it with everything else.
   */
  const [slots, setSlots] = useState<Record<string, ActorSlots>>({});
  const folders = useRegisteredFolders('actor');

  useEffect(() => {
    void loadCatalog();
    void loadPanes();
  }, [loadCatalog, loadPanes]);

  /* SWALLOWS ITS FAILURE, and the reason is the same one `useSecrets` gives for guarding its shape:
     the credential surface is a section of this page, and a store that cannot be read must not take
     the Actors grid down with it. An empty map draws no strips, which is the state of every
     installation that has never declared a slot. */
  useEffect(() => {
    let live = true;
    void fetchSlots()
      .then((got) => {
        if (!live) return;
        const rows = Array.isArray(got.actors) ? got.actors : [];
        setSlots(Object.fromEntries(rows.map((a) => [a.actor, a])));
      })
      .catch(() => undefined);
    return () => {
      live = false;
    };
  }, [pollNonce]);

  // THE RAIL COUNTS WHAT THIS PAGE DRAWS, and this is where the number is known. It read
  // `catalog.length` — 23 against a page of 1 — because the rail could see the catalog and not the
  // folders. Published on every change rather than on first load, so forgetting a folder moves the
  // badge with the grid.
  useEffect(() => {
    setActorFolderCount(folders.sources.length);
  }, [folders.sources.length, setActorFolderCount]);

  /** Machines per actor, keyed the way the catalog keys them. The streamer tags each pane with the
   *  actor and version it is running, so this is a join over two real inventories rather than a
   *  guess from a name. */
  const machines = useMemo(() => {
    const by = new Map<string, PaneTerminal[]>();
    for (const p of panes) {
      if (!p.actor) continue;
      const key = `${p.actor}@${p.version}`;
      by.set(key, [...(by.get(key) ?? []), p]);
    }
    return by;
  }, [panes]);

  /**
   * ONE CARD PER REGISTERED FOLDER. NO FOLDER, NO ACTOR.
   *
   * The page used to draw the CATALOG — every Actor any worker ever registered about itself, on any
   * machine — which after one run was twenty-three cards, most of them `source unknown` and `no
   * registered folder`: deployments the reader cannot open, edit, serve or forget. Nothing on that
   * page was theirs to act on, so the page stopped being worth opening. And when those workers
   * stopped, the catalog emptied and the page went blank with the folders still registered.
   *
   * A folder is a path somebody pointed at on this disk. It can be edited, served, called and
   * forgotten, and that is the whole list of things this surface can do — so it is the unit. What
   * the catalog adds is what a RUNNING worker said about that code: its Methods, their signatures,
   * the Machines serving it, the digest, what the version broke. A folder nothing has served is
   * still a card, marked `unserved`, because register → edit → serve starts there.
   *
   * Built before filtering, so the count's denominator is the whole page and "showing 4 of 23" is
   * true rather than a fraction of a fraction.
   */
  const cards = useMemo(
    () =>
      folders.sources.map((s) => {
        const entry = catalogForFolder(s, catalog);
        /* THE STAND-IN'S KEY IS COMPOSED THE WAY THE CATALOG COMPOSES IT — `name@version` — and not
           from the folder's registration id, because the Machine join below is keyed by it and an
           `actor:probe:1lg4m74` there would match no pane at all.

           IT IS NOT THE TASK QUEUE, and this comment used to say it was. The shared queue is
           `probe-0.1.0` (`@core/panels/pollers:sharedQueue`); `probe@0.1.0` is not even a legal
           queue name — `workflowControl.QUEUE_RE` has no `@` — so the card derives the queue itself
           now rather than printing this. */
        const key = entry?.key ?? `${s.name}@${s.version}`;
        return {
          kind: entry ? ('catalogued' as const) : ('unserved' as const),
          actor: entry ?? {
            key,
            name: s.name,
            version: s.version,
            schemaVersion: '',
            operations: [],
            source: s.path,
          },
          folder: s,
          /* MACHINES BY QUEUE, whether or not the catalog has caught up. A worker can be polling for
             `probe@0.1.0` while the catalog read is still in flight, and a card that showed `—`
             because of the ORDER two fetches returned in would be reporting nothing is serving. */
          machines: machines.get(key) ?? ([] as PaneTerminal[]),
        };
      }),
    [catalog, folders.sources, machines]
  );

  /**
   * THE QUEUES THIS PAGE HAS TO ASK ABOUT, as one stable string.
   *
   * A joined string rather than an array, because the effect below depends on it: `cards` is a new
   * array on every catalog or pane tick, and an array in the deps would tear down and restart the
   * interval a few times a minute — which, on a page whose whole job is measuring how long ago
   * something happened, is how the measurement never gets old enough to be interesting.
   */
  const queueList = useMemo(
    () => [...new Set(cards.map((c) => sharedQueue(c.actor.name, c.actor.version)))].sort().join('\n'),
    [cards]
  );

  /**
   * ASK TEMPORAL WHO IS POLLING, on a timer.
   *
   * `fetchPollers` NEVER THROWS — an unreachable cluster comes back as `pollers: 0` WITH an `error`,
   * which the card draws as `unknown` rather than as "nothing is serving". That is the whole reason
   * the route answers 200 with a shape instead of a 502, and a `catch` here that dropped the report
   * would throw the distinction away one layer above where it was preserved.
   */
  useEffect(() => {
    const queues = queueList === '' ? [] : queueList.split('\n');
    if (queues.length === 0) {
      setPollers({});
      return;
    }
    let live = true;
    const tick = async (): Promise<void> => {
      const reports = await Promise.all(queues.map((q) => fetchPollers(q)));
      if (!live) return;
      setPollers(Object.fromEntries(reports.map((r) => [r.queue, r])));
      setPolledAt(Date.now());
    };
    void tick();
    const timer = setInterval(() => void tick(), POLLERS_MS);
    return () => {
      live = false;
      clearInterval(timer);
    };
  }, [queueList, pollNonce]);

  const names = useMemo(() => actorNames(cards.map((c) => c.actor)), [cards]);
  const shown = useMemo(
    () =>
      cards.filter((c) =>
        matchActor(c.actor, filter, {
          // In the pane's OWN mode: `isHealthy(m.health)` alone demands `loads === 'ok'`, which only
          // a fleet Machine's vmagent ever produces, so this filter used to report every local and
          // docker Worker as not serving. See `chrome/tileRef.terminalIsHealthy`.
          serving: c.machines.filter(terminalIsHealthy).length,
          machines: c.machines.length,
          onDisk: !c.folder.absent,
        })
      ),
    [cards, filter]
  );

  /** Forget by id, because a card holds the folder and the shelf holds the Source. Closing the
   *  workbench when its own registration goes is what forgetting the folder you are editing means. */
  const forget = useCallback(
    (id: string) => {
      const source = folders.sources.find((s) => s.id === id);
      if (!source) return;
      if (editing === id) setEditing(null);
      folders.forget(source);
    },
    [editing, folders]
  );

  const workbench = editing ? folders.sources.find((s) => s.id === editing) : undefined;
  if (workbench) {
    // THE DEPLOYED DIGEST, beside the source the viewer shows (ADR 0030). Read from the catalog the
    // same way a card is — `catalogForFolder` — so the read-only workbench can say which registered
    // code the files on disk are being compared against. Absent (unserved, or unpinned) is drawn as
    // its own state by `SourceProvenance`.
    return (
      <FolderWorkbench
        source={workbench}
        digest={catalogForFolder(workbench, catalog)?.digest}
        onClose={() => setEditing(null)}
      />
    );
  }

  /* THE ACTOR, THE METHOD AND THE FOLDER ARE RESOLVED FRESH, not captured when the button was
     pressed. The catalog reloads and the folder list reloads; a held Method object would draw the
     schema an Actor had before its worker re-registered, and a held Source would keep offering a
     folder that has since been forgotten. Any of the three going missing closes the panel, which is
     what forgetting the registration while calling into it should do. */
  const callingActor = calling ? catalog.find((a) => a.key === calling.key) : undefined;
  const callingOp = callingActor?.operations.find((o) => o.name === calling?.method);
  const callingFolder = callingActor ? folderForActor(callingActor, folders.sources) : undefined;
  if (callingActor && callingOp && callingFolder && !callingFolder.absent) {
    return (
      <MethodCall
        /* KEYED BY THE METHOD, so a different Method is a different component and not the same one
           holding the last Batch. Reaching a second Method goes through the grid today, which
           unmounts this — but the form's initial state is built once from the schema, and the day
           something opens one from another it would silently keep `head`'s fields under `title`. */
        key={`${callingActor.key}/${callingOp.name}`}
        actor={callingActor}
        op={callingOp}
        folder={callingFolder}
        /* WHO IS POLLING THIS ACTOR'S QUEUE, threaded down rather than re-fetched. The page already
           asks once per queue on a timer, and a call panel that asked again would be a second
           answer to one question — the one where the two can disagree about whether a worker is
           stale. `?? null` keeps `unknown` distinct from "nothing is serving", which is what makes
           the panel refuse to offer a call rather than report one. */
        pollers={pollers[sharedQueue(callingActor.name, callingActor.version)] ?? null}
        polledAt={polledAt}
        onClose={() => setCalling(null)}
      />
    );
  }

  return (
    <main className="dataset-page">
      <header className="flex flex-wrap items-baseline gap-3">
        <h1 className="m-0 text-lg font-semibold">Actors</h1>
        <span className="text-[11.5px] text-muted-foreground">
          One card per registered folder on this disk, carrying what a worker has told the catalog
          about the code inside it: the Methods it declares and how many Machines are serving them.
          Nothing is uploaded — a folder is a path, and no folder means no Actor here.
        </span>
        {/* REGISTERING IS A HEADER ACTION, beside the refresh and not buried in the list below,
            because on an installation with nothing registered the list is one line of prose and a
            button inside it would be the only thing on the page to press. `text-right` puts the
            closed button back on the right margin — it is inline-flex, so it does not fill the
            box the open form needs. */}
        <div className="ml-auto w-[340px] shrink-0 text-right">
          <RegisterFolder
            kind="actor"
            defaultRoot={folders.defaultRoot}
            onRegistered={folders.registered}
          />
        </div>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => {
            void loadCatalog();
            void loadPanes();
            folders.reload();
            // The pollers too. "Refresh from server" that left the one live signal on the page at
            // its last tick would be the button lying about what it did.
            setPollNonce((n) => n + 1);
          }}
          title="Refresh from server"
        >
          <RefreshCw size={14} />
        </Button>
      </header>

      {folders.error && (
        <p className="m-0 text-[11px] text-destructive" data-testid="folders-error-actor">
          {folders.error}
        </p>
      )}

      {cards.length === 0 ? (
        <p className="text-[13px] text-muted-foreground">
          No folders registered, so there is nothing here yet. Point the register button at a
          directory holding <code className="font-mono">actor.json</code> —{' '}
          <code className="font-mono">examples/python/beacon</code> is one — and it becomes a card
          you can edit, serve and call, before anything has served it. Workers that registered
          themselves from code that is not on this disk are in the catalog, not on this page.
        </p>
      ) : (
        <>
          <ActorFilterBar
            filter={filter}
            names={names}
            onChange={setFilter}
            shown={shown.length}
            total={cards.length}
          />

          {shown.length === 0 ? (
            // NOT AN EMPTY PAGE. A grid that simply went blank reads as a catalog that emptied
            // itself; the sentence names the filter as the cause and the count above it still says
            // how many are there.
            <p className="text-[13px] text-muted-foreground" data-testid="actor-none-match">
              No Actor matches this filter. {cards.length}{' '}
              {cards.length === 1 ? 'folder is' : 'folders are'} registered — clear it to see them.
            </p>
          ) : (
            <div className="grid items-start gap-3 [grid-template-columns:repeat(auto-fill,minmax(452px,1fr))]">
              {/* KEYED BY THE FOLDER, which is what a card IS now. Two checkouts of one Actor at
                  the same version resolve to the SAME catalog entry — `catalogForFolder` scores by
                  name when no path matches — so keying by `actor.key` would put two cards under one
                  React key and lose the second's open/edit state to the first's. */}
              {shown.map((c) => (
                <ActorCard
                  key={c.folder.id}
                  actor={c.actor}
                  machines={c.machines}
                  folder={c.folder}
                  open={open[c.folder.id] ?? false}
                  catalogued={c.kind === 'catalogued'}
                  forgetting={folders.forgetting !== null}
                  /* A QUEUE WITH NO REPORT YET IS `unknown`, not un-served — `?? null` is that
                     distinction, and it is the state every card is in until the first tick lands. */
                  pollers={pollers[sharedQueue(c.actor.name, c.actor.version)] ?? null}
                  now={polledAt}
                  slots={slots[c.actor.name] ?? null}
                  onToggle={() =>
                    setOpen((prev) => ({ ...prev, [c.folder.id]: !prev[c.folder.id] }))
                  }
                  onEdit={setEditing}
                  onForget={forget}
                  onCall={(method) =>
                    c.kind === 'catalogued' ? setCalling({ key: c.actor.key, method }) : undefined
                  }
                />
              ))}
            </div>
          )}
        </>
      )}
    </main>
  );
}
