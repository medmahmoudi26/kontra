/**
 * One Actor's card: a registered folder on this disk, and what a running worker has said about it.
 *
 * THE FOLDER IS THE SUBJECT, NOT THE CATALOG ENTRY. `folder` is required, and the card no longer has
 * a branch for an Actor without one — that branch drew a row with no editor, no forget, no call and
 * no sentence the reader could act on, which is the noise the Actors page was inverted to remove.
 * What the catalog still supplies is everything a running worker knows and a directory cannot: the
 * Methods, their signatures, the Machines, the digest, and what the version broke.
 *
 * IT IS NOT IN `ActorsPage.tsx` FOR A MECHANICAL REASON, and the reason is worth stating so nobody
 * folds it back in: the page imports the workbench, the workbench imports the worker's pane, and
 * that imports xterm — which touches `self` at MODULE LOAD. `actorCard.test.ts` runs in node with
 * no jsdom, so importing this component from the page fails the whole file before a test runs,
 * with `ReferenceError: self is not defined`. What the card SAYS is the contract; it lives where a
 * test can draw it.
 *
 * THE METHOD IS THE UNIT, not the Actor. An Actor has many (ADR 0023 §9) and each self-registers
 * as its own operation with its own signature — `shout` taking a message says nothing about what
 * `title` takes — so a card that showed only `name@version` would leave a caller to guess the half
 * of the call that varies, and guessing wrong is a dispatch that fails at the actor rather than in
 * the editor. The methods are therefore always visible; only their FIELD tables fold away.
 *
 * SCHEMAS ARE RENDERED AS FIELDS, not as raw JSON Schema. The catalog derives them from the
 * author's own types (`takes=`/`emits=` in Python, `Takes()`/`Emits()` in Go), and a field table is
 * what a caller actually needs: the names and types to put in a Batch.
 *
 * AND AN UNDECLARED SCHEMA SAYS SO. Not every actor declares its types — `examples/go/dnsfacts`
 * and `examples/go/nscheck` do not — and an empty field table is indistinguishable from a Method
 * that takes nothing. That distinction is the whole reason the Actors page is worth opening, so the
 * absent case is drawn as absent, never as blank.
 *
 * AND A METHOD CAN BE CALLED FROM ITS ROW, which on this page now means it is CALLED (ADR 0033):
 * the row opens a panel that collects a Batch and starts one execution of a kontra-owned workflow
 * making exactly ONE Method call over it. One Actor, one version, one Method, one Batch — the row
 * offers no way to name a second, because the request has no field for one. It is drawn only when
 * the Actor's folder is registered and on disk, because both routes behind it are keyed by that
 * folder, and the folder is what pins the call to one Actor at one version.
 *
 * WHAT A METHOD IS, IS NOT DRAWN HERE ANY MORE. `MethodRow` and the field tables moved to
 * `MethodContract.tsx` the day a second surface needed them: Scratch's inspector lists an Actor's
 * Methods so an author can see which one a node uses and change it, and a second rendering of "what
 * this Method takes" would be the one that goes stale unnoticed. This page still decides WHICH rows
 * to draw and whether each one can be called; what a row says is one module's answer for both.
 *
 * AND A VERSION THAT BROKE A CALLER SAYS SO, ON THE VERSION THAT DID IT. The catalog compares a new
 * version's per-Method schemas against the one immediately before it (`@core/compat`, at
 * registration) and stores what it found; this is where that lands, because the version is what a
 * caller pins and the card is the only surface that shows one. Registration is NOT refused for it —
 * a breaking change in a new version is what versions are for — so if this strip is not drawn, the
 * break is invisible everywhere and the first thing to notice it is a run, in the shape of data
 * that does not fit.
 *
 * NO "COMPATIBLE" BADGE, EVER. The absence of this strip means nothing was reported, which covers a
 * first version, a Method that is new here and a schema that declares no fields — none of which were
 * compared. A green tick would be the one claim the check cannot support.
 *
 * DESCRIPTIONS TRAVEL NOW, and this comment used to say they could not. It said "a description is
 * not in the catalog — nothing on the registration path carries one", which was true until `209b0ad`
 * put a Method's own words on it: Python reads the docstring's first paragraph, Go types the same
 * sentence into `Does("…")`. So a Method with no description is an author who wrote none, and the
 * row says THAT — a fixable omission by the person reading it, not a field the system lacks.
 */

import { ChevronDown, ChevronRight, Code2 } from 'lucide-react';
import { sharedQueue } from '@core/panels/pollers';
import type { Terminal as PaneTerminal } from './panelsClient';
import { terminalIsHealthy } from './chrome/tileRef';
import { MethodRow } from './MethodContract';
import { ActorWorkers } from './ActorWorkers';
import { SlotStrip } from './SlotStrip';
import { FolderActions } from './RegisteredFolders';
import type { ActorSlots, Source } from '../run/api';
import type { PollerReport } from '../run/workflowState';
import type { CatalogActor } from '../types';
import { Button } from '@/components/ui/button';

/** What the card SAYS about an Actor's code is the contract, and it is markup rather than a value
 *  a helper could return — which is why `actorCard.test.ts` draws it. */
export function ActorCard({
  actor,
  machines,
  folder,
  open,
  catalogued = true,
  forgetting = false,
  pollers = null,
  now = 0,
  slots = null,
  onToggle,
  onEdit,
  onForget,
  onCall,
}: {
  actor: CatalogActor;
  machines: PaneTerminal[];
  /**
   * The registered folder this card IS. Required — no folder, no Actor.
   *
   * It was optional, and the card drew "no registered folder — catalogued by a worker, not
   * registered from disk" for every Actor that had none: a row with no editor, no forget, no call
   * and nothing to do about any of it. The page maps over folders now, so the state cannot occur;
   * the prop is required rather than merely unused so it cannot come back by accident.
   */
  folder: Source;
  open: boolean;
  /**
   * False when NOTHING has registered this Actor and the card is drawn from the folder alone.
   *
   * THE TWO EMPTY METHOD LISTS ARE DIFFERENT FACTS. A catalogued Actor with no operations is a
   * load-only deployment — a worker ran, registered itself, and declared no `@actor.method`. A
   * folder nobody has served has no operations because nothing has ever looked, and calling that
   * "load-only" would report a deployment that does not exist. The whole page turns on the
   * difference between what a worker said about itself and what is on this disk, so the one card
   * that now draws both has to keep saying which it is holding.
   */
  catalogued?: boolean;
  /** True while this card's own registration is being forgotten. */
  forgetting?: boolean;
  /**
   * Who is polling this Actor's shared queue — `GET /api/queues/:queue/pollers`, read by the page.
   *
   * `null` IS "NOT ASKED YET", never "nothing is serving", and defaulting it that way is what keeps
   * a card drawn before the first poll honest: the worker strip says `unknown`, which is the state
   * the route and `run/workflowState.ts` both go to trouble to keep apart from zero.
   */
  pollers?: PollerReport | null;
  /** When {@link pollers} was read, epoch ms. `0` with a `null` report is the pre-poll state; a
   *  component that read the clock itself could not be drawn twice and compared. */
  now?: number;
  onToggle: () => void;
  onEdit: (id: string) => void;
  /** Drop the registration. Absent for a discovered folder — `SourceStore.forget` refuses an `at:`
   *  id, so a button would be an affordance that exists only to 400. */
  onForget?: (id: string) => void;
  /**
   * WHAT THIS ACTOR WILL ASK FOR — its declared credential slots, joined against the operator's
   * bindings (issue 20). `null` is "nothing has declared", which is most actors and draws nothing.
   *
   * A PROP, NOT A FETCH, for the reason `pollers` is one: this component is drawn by a node test
   * with no jsdom and no network, and every state a slot can be in has to be a render rather than
   * a mock. The page reads it once for all cards.
   */
  slots?: ActorSlots | null;
  /** Open the call panel for one Method — the Batch form, the Run button and the caller. */
  onCall: (method: string) => void;
}) {
  // A Machine whose health probe is unhappy is not serving, whatever the catalog says. Counted
  // separately rather than folded in, because "four Machines, one of them dead" and "three
  // Machines" are different situations and only one of them needs somebody.
  //
  // READ IN THE PANE'S OWN MODE (`terminalIsHealthy`). It used to be `isHealthy(m.health)`, which
  // required `loads === 'ok'` — a reading vmagent produces and vmagent runs only on fleet Machines —
  // so a `local` or `docker` Worker could never be counted, and this card reported `0 serving` for an
  // actor that was answering calls.
  const serving = machines.filter(terminalIsHealthy).length;
  // Absent on every row registered before the check existed, and on every version nothing was
  // reported about — the two read the same here, and both mean "no finding", never "compatible".
  const findings = actor.incompatibilities ?? [];
  /* THE SHARED QUEUE, DERIVED — and this chip used to print `actor.key`, which is `name@version`
     and is not a task-queue name at all: `@` is not in `workflowControl.QUEUE_RE`, so the string on
     the card was one no queue has ever been called and one `kontra workers list` never prints. The
     rule is `@core/panels/pollers:sharedQueue`, the same derivation `DetailDrawer` and the streamer's
     poller signal use, so the name on the card is the name the poller strip below asks about. */
  const queue = sharedQueue(actor.name, actor.version);
  const dot =
    machines.length === 0
      ? 'border border-dashed border-muted-foreground'
      : serving === machines.length
        ? 'bg-emerald-400 animate-pulse'
        : serving > 0
          ? 'bg-amber-400'
          : 'bg-rose-400';

  return (
    <article
      className="overflow-hidden rounded-xl border border-border bg-card"
      data-testid={`actor-${actor.name}`}
    >
      <div className="flex items-start gap-3 px-3.5 py-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <span className={`size-[7px] shrink-0 rounded-full ${dot}`} />
            <span className="font-mono text-[12.5px] font-semibold">{actor.name}</span>
            <span className="font-mono text-[10px] text-muted-foreground">v{actor.version}</span>
            <span
              className="rounded-full bg-zinc-500/10 px-1.5 font-mono text-[8.5px] uppercase tracking-wider text-muted-foreground"
              title={
                !catalogued
                  ? 'a registered folder on this disk that no worker has served — the catalog holds nothing about it yet'
                  : actor.operations.length === 0
                    ? 'a worker registered this Actor and it declared no @actor.method'
                    : undefined
              }
            >
              {!catalogued ? 'unserved' : actor.operations.length === 0 ? 'load-only' : 'actor'}
            </span>
          </div>
          {/* WHERE THE CODE IS: THE REGISTERED FOLDER, which is the one path on this card the
              reader controls. It is always drawn, never a tooltip — an operator with two checkouts
              of `probe` is here to find out which one this console is holding, and the answer
              cannot be behind a hover. */}
          <p
            className="m-0 mt-1.5 truncate font-mono text-[10.5px] text-muted-foreground"
            data-testid={`actor-source-${actor.name}`}
            title={`the registered folder this card is: ${folder.path}`}
          >
            {folder.path}
          </p>

          {/* AND WHERE THE RUNNING COPY CAME FROM, only when it disagrees. `actor.source` is where
              the WORKER loaded from, which on a fleet Machine is a path on the MACHINE
              (`/opt/kontra/actor/<name>`) and names nothing on this disk. Two paths that differ is
              a real finding — the code you are editing is not the code that ran — and printing the
              same path twice when they agree is noise. */}
          {actor.source && actor.source !== folder.path && (
            <p
              className="m-0 mt-1 truncate font-mono text-[10px] text-amber-500/80"
              data-testid={`actor-loaded-${actor.name}`}
              title={`A worker registered this Actor from ${actor.source}, which is not this folder. On a fleet Machine that is a path on the MACHINE, not on this host — so the two disagreeing is normal for a fleet deploy and worth a second look for a local one.`}
            >
              a worker loaded it from {actor.source}
            </p>
          )}

          {folder.absent ? (
            // A registered folder whose directory is gone reads every file as a 400. Offering the
            // editor anyway would answer a click with a stack of refusals — but FORGET is exactly
            // the control this state wants, so it is drawn beside the sentence rather than only on
            // a healthy row.
            <div className="mt-1.5 flex flex-wrap items-center gap-1.5">
              <p
                className="m-0 text-[10.5px] italic text-rose-500/80"
                data-testid={`actor-folder-absent-${actor.name}`}
                title={`${folder.path} is registered but is not on the orchestrator's disk any more`}
              >
                registered folder is not on disk — nothing to edit until it is back
              </p>
              <FolderActions
                source={folder}
                busy={forgetting}
                testid={`actor-${actor.name}`}
                onForget={onForget && ((s) => onForget(s.id))}
              />
            </div>
          ) : (
            <div className="mt-1 flex flex-wrap items-center gap-1">
              <Button
                variant="ghost"
                size="sm"
                className="h-6 px-1.5 text-[10px]"
                data-testid={`actor-edit-${actor.name}`}
                title={`open the files in ${folder.path}`}
                onClick={() => onEdit(folder.id)}
              >
                <Code2 size={11} className="mr-1" />
                edit code
              </Button>
              <FolderActions
                source={folder}
                busy={forgetting}
                testid={`actor-${actor.name}`}
                onForget={onForget && ((s) => onForget(s.id))}
              />
            </div>
          )}
        </div>
        <div className="w-[104px] shrink-0 text-right" data-testid={`actor-machines-${actor.name}`}>
          <div
            className={`font-mono text-[15px] tabular-nums ${
              machines.length === 0 ? 'text-muted-foreground' : 'text-emerald-400'
            }`}
          >
            {machines.length === 0 ? '—' : serving}
          </div>
          <div className="text-[8px] uppercase tracking-wide text-muted-foreground">
            {machines.length === 0
              ? 'no machines'
              : serving === machines.length
                ? 'machines serving'
                : `serving of ${machines.length}`}
          </div>
        </div>
      </div>

      <div className="flex flex-wrap gap-1.5 px-3.5 pb-2.5">
        {/* THE QUEUE, and the tense is the difference. A catalogued Actor IS served on it; a folder
            nothing has run yet WILL BE, on the queue its name and version compose to — and saying
            "is served on" there would claim a deployment the card has just marked `unserved`. */}
        <Chip
          k="queue"
          v={queue}
          title={
            catalogued
              ? 'the shared task queue every Method of this Actor is served on'
              : 'the shared task queue this Actor will be served on once something serves this folder'
          }
        />
        <Chip
          k="methods"
          v={String(actor.operations.length)}
          title="operations this Actor self-registered — one per @actor.method"
        />
        {/* The digest is the actor's content-pinned identity (ADR 0011). Absent means pinning is
            off for this one, which is a real state and not an error. */}
        <Chip
          k="image"
          v={actor.digest ? `${actor.digest.slice(0, 19)}…` : 'unpinned'}
          title="the registered OCI image digest (ADR 0011) — a rebuild changes it, which is what trips the drift check"
        />
        {machines.length > 0 && (
          <Chip
            k="hosts"
            v={[...new Set(machines.map((m) => m.publicIp || m.host))].join(' ')}
            title="the Machines the Monitor can see running this Actor"
          />
        )}
      </div>

      {/*
        WHO CAN ACTUALLY SERVE THIS, above the Methods and above what the version broke.

        Everything over it is REGISTRATION — a folder, a digest, a queue name, Methods a worker once
        declared — and all of it survives the process that made it. Read alone it says "this Actor
        exists" in a way that reads as "this Actor works", and the gap between those two is the
        dispatch that sits on a queue nobody polls while Temporal reports the run as running.
        `ActorWorkers` is the only thing on this card that answers the question the page is opened
        with, so it goes where a reader lands rather than under a fold.
      */}
      <ActorWorkers
        actor={actor.name}
        queue={queue}
        report={pollers}
        now={now}
        machines={machines}
      />

      {/* WHAT IT WILL ASK FOR, above what this version broke and above the Methods: both of those
          are about a caller's contract, and this is about whether the actor can run at all. An
          unbound credential is the one thing on this card that refuses a run BEFORE it starts. */}
      {slots && slots.slots.length > 0 && (
        <div className="border-t border-border px-3.5 py-2">
          <SlotStrip view={slots} />
        </div>
      )}

      {/* WHAT THIS VERSION BROKE, above the Methods rather than under them: it is a fact about the
          version in the header, and a caller pins the version. Drawn only when there is something
          to draw — see the header comment on why there is no compatible badge. */}
      {findings.length > 0 && (
        <div
          className="border-t border-amber-500/40 bg-amber-500/5 px-3.5 py-2"
          data-testid={`actor-incompat-${actor.name}`}
        >
          <div
            className="text-[9px] uppercase tracking-wider text-amber-500"
            title={`Registering ${actor.version} was compared against ${findings[0]?.previous}. An input schema must still read data shaped for the older one (BACKWARD); a caller of the older one must still read this one's output (FORWARD).\n\nRegistration was NOT refused: a breaking change in a new version is what versions are for. This is a structural comparison of two JSON Schema documents — top-level fields, whether they are required, and their declared types — not a proof that one can stand in for the other.`}
          >
            breaks a caller of {findings[0]?.previous}
          </div>
          <ul className="m-0 mt-1 list-none p-0">
            {findings.map((f) => (
              <li
                key={`${f.method}.${f.field}`}
                className="text-[10.5px] leading-snug"
                data-testid={`actor-incompat-${actor.name}-${f.method}-${f.field}`}
              >
                {/* THE METHOD AND THE DIRECTION ARE BOTH NAMED. "probe 0.2.0 is incompatible" sends
                    a reader to diff two descriptors; `head() output FORWARD` says which half of
                    which signature moved and which way, which is the sentence they can act on. */}
                <code className="font-mono">{f.method}()</code>{' '}
                <span className="font-mono text-amber-500">
                  {f.field} {f.rule}
                </span>{' '}
                <span className="text-muted-foreground">{f.detail}</span>
              </li>
            ))}
          </ul>
        </div>
      )}

      {actor.operations.length === 0 ? (
        <p
          className="m-0 border-t border-border px-3.5 py-2.5 text-[11px] italic text-muted-foreground"
          data-testid={`actor-nomethods-${actor.name}`}
        >
          {catalogued
            ? 'no methods registered — a load-only Actor is still a real deployment'
            : 'nothing has served this folder yet, so no Methods are registered — serve it and its Methods appear here'}
        </p>
      ) : (
        <div className="border-t border-border">
          <div className="flex gap-3 bg-muted px-3.5 py-1.5 text-[9px] uppercase tracking-wider text-muted-foreground">
            <span className="w-[118px] shrink-0">method</span>
            <span className="flex-1">takes → emits</span>
          </div>
          {actor.operations.map((op) => (
            <MethodRow
              key={op.name}
              actor={actor.name}
              op={op}
              expanded={open}
              /* CALLING READS THE FOLDER, because BOTH routes are keyed by it — the caller
                 (`POST /api/sources/actor/:id/caller`) and the probe that actually makes the call
                 (`…/probe`, ADR 0033). The folder is also what pins the probe to ONE Actor at ONE
                 version: the request cannot name either. So a Method offers `call` exactly when its
                 Actor offers `edit`, and the sentence above about a folder that is gone is the
                 reason for both — a button drawn over a missing directory would 400 on a fact the
                 card is already telling the reader. */
              onCall={folder.absent ? undefined : () => onCall(op.name)}
            />
          ))}
          <button
            className="flex w-full items-center gap-1 px-3.5 py-1.5 text-[11px] text-muted-foreground hover:bg-accent/40"
            onClick={onToggle}
            data-testid={`actor-signatures-${actor.name}`}
          >
            {open ? <ChevronDown size={12} /> : <ChevronRight size={12} />}
            {open ? 'hide field tables' : 'show field tables'}
          </button>
        </div>
      )}
    </article>
  );
}

function Chip({ k, v, title }: { k: string; v: string; title: string }): JSX.Element {
  return (
    <span
      className="max-w-full truncate rounded bg-muted px-1.5 py-0.5 font-mono text-[9.5px] text-muted-foreground"
      title={title}
    >
      <span className="opacity-60">{k}</span> <span className="text-foreground">{v}</span>
    </span>
  );
}
