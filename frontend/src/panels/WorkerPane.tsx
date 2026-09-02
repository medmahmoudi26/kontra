/**
 * The tmux pane a served worker is running in, beside the editor.
 *
 * WHY THIS IS BESIDE AN EDITOR AT ALL. A worker fails in ways nothing else on either surface can
 * report. A stray tab is a `TabError` at import; a missing package is an ImportError; a queue
 * nobody serves is a Run that simply waits. In all three the file saves, Serve returns
 * successfully, Run starts a workflow, and every status stays green — the evidence is a traceback in
 * a pane an operator had to know to go and attach to. Putting the pane here is the difference
 * between "my workflow hangs" and reading the exception.
 *
 * BOTH CALLERS' WORKERS, ONE PANE. The Workflows page serves a caller's workflow and the Actor
 * workbench serves an Actor; the two mint their session names by different rules, so the NAME is
 * the caller's to derive and this component's job starts at finding it. A second terminal for the
 * Actors page would have drifted on the two things that are load-bearing here — the snapshot-only
 * rule below, and the patience while the streamer rediscovers a session.
 *
 * IT IS A SNAPSHOT, NEVER A LIVE ATTACH. ADR 0020's cost model: the wall is snapshots, and a live
 * attach is an sshd session, a PTY and a per-viewer tmux session on the node. A pane that quietly
 * went live because it happened to be on screen would spend that budget without anyone choosing to.
 * The Monitor is where a pane is promoted, and the link goes there.
 *
 * IT RESOLVES A SESSION NAME, NOT AN ID. A serve answers with the session it created, and the
 * streamer discovers that session on its own cadence — so a Terminal for a session created a second
 * ago does not exist yet. Polling briefly is the difference between "not yet" and reporting "no
 * pane" about a worker that is running perfectly well.
 *
 * UNLESS THE CALLER ALREADY HAS THE TERMINAL, which is the one thing added for the run-scoped
 * Monitor and is additive to every existing caller. A session name cannot address a MACHINE — nine
 * droplets running one Actor answer to one session name — so a surface that scopes panes to a run's
 * Machines picks by id and hands the object in. See the `terminal` prop; nothing else here changed.
 */

import { useEffect, useState } from 'react';
import { ExternalLink } from 'lucide-react';
import TerminalTile from './TerminalTile';
import { fetchTerminals, type Terminal as PaneTerminal } from './panelsClient';
import { paneForSession } from './paneForSession';
import { useSinglePane } from './useSinglePane';
import { tileRefFor } from './chrome/tileRef';
import { useTerminalStyle } from './chrome/useTerminalStyle';
import { useAppStore } from '../state/store';
import { Button } from '@/components/ui/button';

/** How often the inventory is re-asked while a pane is missing, and for how long. The streamer
 *  rediscovers local sessions on its own cadence; this is patience, not a retry loop. */
const FIND_MS = 3000;

/**
 * TWO WAYS TO SAY WHICH PANE, AND NEVER BOTH — a union rather than four loose props, so the wrong
 * combination does not compile.
 *
 *  - BY SESSION NAME, which is what the two editors want: they have just pressed Serve, or are
 *    looking at code served yesterday, and "the worker for this thing" is a name to resolve.
 *  - BY TERMINAL, which is what a Monitor scoped to a RUN wants. `paneForSession`'s own header
 *    states why the name will not do there: a fleet Machine's session is `<actor>-<version>` by the
 *    same rule the local one uses, so nine droplets running `probe` 0.1.0 all answer to one name and
 *    the resolver returns the local match, else the first. Right for "where is the worker I just
 *    served"; wrong when the operator has picked one of a run's Machines by id, where any other pane
 *    under that name is somebody else's screen.
 *
 * A `null` TERMINAL IS A CALLER SAYING IT HAS NOTHING TO SHOW, and is not the same as omitting the
 * prop: "resolve it yourself" and "I looked and there is none" would otherwise be one value with two
 * meanings, and the second must draw the empty state rather than start polling for a name nobody
 * asked about.
 */
export type WorkerPaneProps = {
  /** What the empty state is about. A pane that says "no pane for this workflow" beside an Actor's
   *  code sends the reader looking for a workflow they never opened. */
  subject?: 'workflow' | 'actor';
  className?: string;
} & (
  | {
      /** The Terminal to show. The caller resolved it, so nothing here searches for anything. */
      terminal: PaneTerminal | null;
      session?: never;
      derived?: never;
    }
  | {
      terminal?: undefined;
      /** The session `Serve` just created, when this page created one. */
      session: string | null;
      /**
       * The session a worker served in an EARLIER visit would be in, derived by the CALLER.
       *
       * The two pages derive it from different things and by different rules — a workflow's session
       * is its name (`workflowSessionOf`), an Actor's is `<name>-<version>` (`actorSession`) — so
       * this component takes the answer rather than a file to guess from. Guessing with the wrong
       * rule is the failure that matters: the name resolves to nothing, and a worker that is polling
       * perfectly well is drawn as one that never started.
       */
      derived: string;
    }
);

export function WorkerPane({
  session,
  derived,
  subject = 'workflow',
  className,
  terminal: given,
}: WorkerPaneProps): JSX.Element {
  const watchTerminal = useAppStore((s) => s.watchTerminal);
  const style = useTerminalStyle();

  /** The caller resolved it. Held as a boolean so the search effect depends on the FACT rather than
   *  on an object identity that changes on every inventory poll. */
  const addressed = given !== undefined;

  /**
   * THE NAME TO LOOK FOR, and it does not depend on this page having served anything.
   *
   * `session` is only set when Serve was pressed in THIS visit. Code served yesterday, or from a
   * terminal, has a pane too — and a page that only found its own would show an empty rectangle
   * beside a worker that is running.
   */
  const want = addressed ? (given ? tileRefFor(given).session : '') : (session ?? derived ?? '');
  const [found, setFound] = useState<PaneTerminal | null>(null);
  const [searched, setSearched] = useState(false);
  const terminal = addressed ? (given ?? null) : found;

  useEffect(() => {
    setFound(null);
    setSearched(false);
    // Nothing to resolve, and nothing to poll for: the caller holds the inventory this pane would
    // have re-fetched, so a second discovery loop here would be one `capture-pane` cadence per
    // mounted pane for an answer that is already on screen.
    if (addressed) return;
    if (!want) return;
    let cancelled = false;
    const tick = async (): Promise<void> => {
      try {
        const terminals = await fetchTerminals();
        if (cancelled) return;
        // `paneForSession`, not a `find`: a fleet Machine's session is named by the same rule, so
        // several Terminals can answer to this name and only the local one is the worker the button
        // beside this pane started.
        setFound(paneForSession(terminals, want));
      } catch {
        /* the Monitor's own error surface owns this; here it only means "not yet" */
      } finally {
        if (!cancelled) setSearched(true);
      }
    };
    void tick();
    const timer = setInterval(() => void tick(), FIND_MS);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [addressed, want]);

  const pane = useSinglePane(terminal?.id ?? null);

  return (
    <div className={`flex min-h-0 min-w-0 flex-col ${className ?? ''}`} data-testid="worker-pane">
      <div className="flex h-[31px] shrink-0 items-center gap-2 overflow-hidden border-b border-border bg-muted/60 pl-2.5 pr-2">
        <span
          className={`size-[7px] shrink-0 rounded-full ${
            pane.painting
              ? 'bg-emerald-400 [animation:kontra-pulse_1.8s_ease-in-out_infinite]'
              : 'border border-dashed border-muted-foreground'
          }`}
        />
        <span className="min-w-0 flex-1 truncate font-mono text-[11px] font-medium">
          {terminal ? `${terminal.host} · ${want}` : want || 'the worker’s pane'}
        </span>
        {terminal && (
          <Button
            size="sm"
            variant="ghost"
            className="h-5 shrink-0 px-1.5 text-[9.5px]"
            data-testid="watch-on-wall"
            title="open this pane on the Monitor, where it can be promoted to a live attach"
            onClick={() => watchTerminal(terminal.id)}
          >
            <ExternalLink size={10} className="mr-1" />
            Monitor
          </Button>
        )}
      </div>

      <div className="min-h-0 flex-1">
        {terminal ? (
          <div className="flex h-full flex-col" data-testid="worker-pane-tile">
            <TerminalTile
              terminal={terminal}
              mode={pane.mode}
              subscribe={pane.subscribe}
              /* WIRED, WHICH IS WHAT MAKES THIS PANE REFLOW. It used to be a no-op on ADR 0020's
                 cost model — see `useSinglePane`'s header for why that model is about a fleet
                 Machine and not about the worker on this host. The tile calls this from its own
                 `ResizeObserver`, settled 120 ms (`SETTLE_MS`), so a drag produces ONE re-attach at
                 the end rather than one per pointer move; `useSinglePane.focus` additionally drops a
                 re-ask at a size it already asked for. Blur stays a no-op: nothing here demotes a
                 pane, and dropping the attach on a stray blur would make the pane stop reflowing for
                 a reason the operator could not see. */
              onFocus={(_id, cols, rows) => pane.focus(cols, rows)}
              /* THE MEASUREMENT IS WHAT DRIVES IT, not `onFocus`, and the difference is a
                 chicken-and-egg the first attempt walked straight into: the tile only re-asks
                 through `onFocus` when it is ALREADY live (`modeRef.current === 'live'`), and
                 nothing here ever promoted it — so zero `focus` frames were sent, measured, and the
                 pane stayed a snapshot no matter how it was resized.

                 `onMeasure` fires on every settled measurement including the first, so this both
                 PROMOTES the pane and follows it. Re-asking is not free — the streamer tears the
                 attach down and opens another — so `useSinglePane.focus` drops a repeat at a size it
                 has already asked for, and `SETTLE_MS` means a drag reports once at the end. */
              onMeasure={(_id, cols, rows) => pane.focus(cols, rows)}
              onBlur={() => undefined}
              theme={style.theme}
              fontSize={style.fontSize}
              paletteIsDark={style.isDark}
              // The socket dropped and is reconnecting (slice 05): the screen below is frozen, so the
              // tile says so over it rather than looking like a worker that stopped printing.
              stale={pane.reconnecting}
              // The screen and nothing else: this component draws the identity and the link to the
              // Monitor above it, and the tile's four health signals are about a Machine — `ssh:
              // reachable` on a worker running on this host is noise beside a page that already
              // says whether the queue is being polled.
              bare
            />
          </div>
        ) : (
          <div className="flex h-full flex-col items-center justify-center gap-1.5 p-4 text-center text-[11.5px] text-muted-foreground">
            {!searched && !addressed ? (
              <span>looking for the worker’s pane…</span>
            ) : (
              <>
                <strong className="font-semibold text-foreground">
                  No pane for this {subject}.
                </strong>
                <span>
                  Press <span className="font-mono">Serve</span> and its pane appears here — the
                  traceback of a worker that boots and dies is only ever in it.
                </span>
                {/* MEASURED, and said rather than left to look broken. The streamer rediscovers
                    local sessions on a 30-second cadence (`DEFAULT_DISCOVER_MS`), so a worker
                    served a second ago is genuinely not there yet — and a blank rectangle for half
                    a minute after pressing Serve reads as a failure rather than as a wait. */}
                <span className="text-[10.5px] opacity-70">
                  Just served? The Monitor rediscovers sessions about every 30 seconds — it will
                  appear on its own.
                </span>
              </>
            )}
          </div>
        )}
      </div>

      {pane.reconnecting && !pane.error && (
        <p
          data-testid="worker-pane-reconnecting"
          className="m-0 shrink-0 border-t border-amber-500/40 bg-amber-500/10 px-2 py-1 font-mono text-[10.5px] text-amber-700 dark:text-amber-300"
        >
          the pane stream dropped — reconnecting… the screen above is frozen until it does.
        </p>
      )}

      {pane.error && (
        <p className="m-0 shrink-0 border-t border-destructive/40 bg-destructive/10 px-2 py-1 font-mono text-[10.5px] text-destructive">
          {pane.error}
        </p>
      )}
    </div>
  );
}
