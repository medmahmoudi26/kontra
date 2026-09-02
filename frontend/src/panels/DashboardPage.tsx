/**
 * The Monitor — a wall of read-only Terminals over tmux (ADR 0020, slices 1–4).
 *
 * THE WALL IS THE INVENTORY, FREELY ARRANGED. Every Terminal the streamer reports gets a tile, and
 * a tile is a rectangle an operator drags and resizes (`grid/wall.ts`). It used to be a list of
 * SLOTS holding selectors, with tiles materialised by evaluating them; `grid/wall.ts`'s header
 * records what that traded away and why the trade reversed. What has not changed is the property
 * the slots existed to protect: this page re-reads the inventory on its own cadence and `syncWall`
 * places whatever is new, so `fleet up --count 10` still fills the wall with no edit and no reload.
 *
 * THE PAGE OWNS THE TRANSPORT; A TILE OWNS ITS SCREEN. One socket per tab carries every Terminal, and
 * a tagged binary frame is routed to whichever tile registered for that id. Everything about xterm,
 * the measured geometry and the repaint rule lives in {@link TerminalTile}; everything about where
 * tiles sit lives in `grid/TileWall.tsx`. This file is what connects the two, plus the four things
 * that can only be decided centrally: the subscription set, the live budget, the inventory refresh
 * and the notices.
 *
 * THE WALL IS SNAPSHOTS. ADR 0020: one `capture-pane` exec per Machine paints every tile on it, and
 * only a focused Terminal is a live PTY attach. So this page must never promote a wall: 24 tiles going
 * live would be 24 `sshd` sessions and 24 PTYs on `s-1vcpu-2gb` Machines, which is the cost model the
 * ADR exists to avoid. Going live takes a tile's own control (slice 2 amendment 9 — never a click,
 * never a tab-stop), it is honoured only inside {@link LIVE_BUDGET}, and promoting past the budget
 * DEMOTES the oldest and says so. Nothing on this page promotes automatically any more: pinned slots
 * were the only thing that could, and they went with the slot document.
 *
 * YOU CANNOT TYPE IN A TERMINAL. No `onData`, no key handler, and no message in the outbound union
 * carries bytes — ADR 0020's finding (3) measured `run-shell` and `send-keys` both executing through a
 * read-only tmux client with no error, so read-only is a property of this code and the streamer's
 * message table, never of tmux.
 *
 * A TERMINAL IS NOT A RECORD, and the page says so. Snapshots are lossy by construction (a screen, not
 * a log), a live attach is lossier still, and the Manifest, the journal on the Machine and the lake are
 * the record.
 */

import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import TerminalTile, { type TileMode } from './TerminalTile';
import TileWall, { WallEmpty } from './grid/TileWall';
import { useWall } from './grid/useWall';
import StyleControls from './chrome/StyleControls';
import SidebarTree from './chrome/SidebarTree';
import PaneFilterBar from './chrome/PaneFilterBar';
import HiddenPanes from './chrome/HiddenPanes';
import { EMPTY_FILTER, hiddenPanes, paneOptions, type PaneFilter } from './paneFilter';
import { useHiddenPanes, wantedSubscriptions } from './paneHiding';
import { disconnectedNotice, reconnectDelay, resubscribePlan } from './reconnect';
import { Button } from '@/components/ui/button';
import StatusBar, { type WallStats } from './chrome/StatusBar';
/**
 * Slice 7b's per-Terminal detail drawer. It landed while this slice was being integrated, so it is WIRED
 * rather than left as a mount point; its prop surface is `{ terminal, feed, elided, onClose }` and nothing
 * about it is this slice's to change.
 *
 * LAZY, AND THIS ONE IS MEASURED. `DetailDrawer` renders through `widgets/Markdown`, which imports
 * `react-markdown` and `remark-gfm` statically — a hundred-odd modules of unified/micromark. A static
 * import here puts all of it on the path between clicking "Dashboard" and seeing a wall, which is the same
 * mistake `App.tsx` documents avoiding for AG Grid and DuckDB. The browser suite caught it: with the drawer
 * imported eagerly, the first spec spent 39 s waiting for the Dashboard heading on a cold dev-server
 * dependency cache and timed out. A drawer that is opened on demand should be loaded on demand.
 * (`MermaidBlock` already does the same thing for `mermaid`, one layer further in — slice 7b's own note.)
 */
const DetailDrawer = lazy(() => import('./DetailDrawer'));
import { useTerminalStyle } from './chrome/useTerminalStyle';
import { useAppStore } from '../state/store';
import {
  decodeTagged,
  fetchTerminals,
  fetchTicket,
  panelBase,
  panelSocketUrl,
  type ClientMessage,
  type ServerMessage,
  type Terminal,
  type TerminalHealth,
} from './panelsClient';

type Phase = 'idle' | 'connecting' | 'streaming' | 'error' | 'reconnecting';

/**
 * How many Terminals this tab will hold live at once.
 *
 * Each one is a PTY, an `sshd` session and a per-viewer grouped tmux session on a 2 GB Machine
 * (ADR 0020), so this is a remote cost paid per tile, not a browser one. Four is enough to compare a
 * pair of Machines or an actor against its handler, and small enough that a wall of pinned slots
 * cannot become a wall of attaches. Exceeding it demotes the oldest promotion rather than refusing the
 * new one: an operator who just clicked "Go live" is telling you what they want to watch.
 */
export const LIVE_BUDGET = 4;

/**
 * How often the inventory is re-read.
 *
 * Matches the streamer's own `KONTRA_PANEL_DISCOVER_MS` default (30 s): a faster poll cannot see
 * anything newer, because that is the cadence at which the streamer re-discovers and re-probes. This is
 * what makes "`fleet up --count 10` fills the wall with no edit" true without a reload — a selector
 * that matches the new Machines materialises tiles for them on the next pass. Health does not wait for
 * it: `{t:'state'}` arrives on the socket.
 */
export const INVENTORY_REFRESH_MS = 30_000;

/**
 * What this page can put on the socket.
 *
 * `focus` and `blur` are in CONTRACT.md's client→server table but not in `panelsClient`'s
 * `ClientMessage` union, which slice 1 owns; this local widening keeps slice 4 out of that file. Note
 * what is NOT here and cannot be: no member of this union carries a payload bound for a session.
 */
type OutboundMessage =
  | ClientMessage
  | { t: 'focus'; id: string; cols: number; rows: number }
  | { t: 'blur'; id: string };

interface Size {
  cols: number;
  rows: number;
}

/**
 * One socket, plus the subscriptions made ON IT.
 *
 * The set travels with the socket, and that is a bug fix rather than tidiness. MEASURED in the browser
 * suite: `main.tsx` renders under `React.StrictMode`, so the connect effect runs mount → cleanup →
 * mount and two `connect()` calls interleave. With one page-level `subscribed` set and sends addressed
 * to "the current socket", the first socket's `onopen` reached the SECOND socket — which was not open
 * yet — so every send returned false, the set was already populated, and the surviving socket never
 * subscribed at all. The symptom was one blank tile out of four, indistinguishable from a Machine that
 * had stopped printing. Bound to a connection, an `onopen` can only ever subscribe its own socket.
 */
interface Conn {
  socket: WebSocket;
  subscribed: Set<string>;
}

/** Health from `{t:'state'}` overrides what the last inventory fetch carried — the socket is the
 * fresher of the two, and it is the one that arrives when a session goes away. */
function withLiveHealth(terminals: Terminal[], live: Record<string, TerminalHealth>): Terminal[] {
  let changed = false;
  const out = terminals.map((t) => {
    const health = live[t.id];
    if (!health || health === t.health) return t;
    changed = true;
    return { ...t, health };
  });
  return changed ? out : terminals;
}

/** Did the inventory really change? A fetch every 30 seconds that replaces the array by identity would
 * re-materialise the wall — same tiles, new objects — for nothing. */
function sameInventory(a: Terminal[], b: Terminal[]): boolean {
  if (a.length !== b.length) return false;
  return a.every((t, i) => {
    const other = b[i];
    return (
      other !== undefined &&
      t.id === other.id &&
      t.actor === other.actor &&
      t.tag === other.tag &&
      t.fleet === other.fleet &&
      t.version === other.version &&
      JSON.stringify(t.health) === JSON.stringify(other.health)
    );
  });
}

export default function DashboardPage(): JSX.Element {
  // The streamer's origin is still needed for the WebSocket; the ticket that authorises it is minted
  // same-origin by orchestrator-api, so this page holds no credential of its own.
  const base = useMemo(() => panelBase(), []);
  const wall = useWall();
  /** Palette and font size, persisted separately from the layout document (`theme.ts`). */
  const style = useTerminalStyle();
  /** The Terminal the sidebar last revealed. Cleared after the ring has faded — a reveal is a flash, not
   * a selection this wall has to keep in sync. */
  const [revealed, setRevealed] = useState<string | null>(null);
  /** Which Terminal's detail drawer is open. One at a time: the drawer takes width from the wall, and two
   * of them would leave a 24-tile wall as a column. */
  const [drawerId, setDrawerId] = useState<string | null>(null);

  /** The live connection. Superseded connections are closed on sight and their handlers ignored. */
  const connRef = useRef<Conn | null>(null);

  /**
   * The reconnect timer and how many attempts have been made (slice 05).
   *
   * A dropped socket is retried on {@link reconnectDelay}'s backoff rather than left idle — the whole
   * point of criterion 3 is that the wall comes back on its own. `attempt` resets to 0 on a clean open;
   * the timer is cleared on unmount so a torn-down page does not resurrect its socket.
   */
  const reconnectRef = useRef<{ attempt: number; timer: ReturnType<typeof setTimeout> | null }>({
    attempt: 0,
    timer: null,
  });
  /** True once this page has unmounted, so an in-flight `onclose` does not schedule a reconnect for a
   *  page that is gone. */
  const unmountedRef = useRef(false);
  /** The latest `connect`, reachable from the reconnect timer without making the timer a dependency of
   *  the effect that owns the socket. */
  const connectRef = useRef<() => Promise<void>>(async () => {});
  /** What the reconnecting banner shows, or `null` while the socket is up. State, not the ref above, so
   *  the banner is reactive; cleared on a clean open. */
  const [reconnectAt, setReconnectAt] = useState<{ attempt: number; delay: number } | null>(null);

  /** Which Terminal is zoomed to fill the wall, or `null` for the wall (slice 05). tmux's `C-b z` as
   *  page state — the zoomed pane is laid out at real size, never scaled (ADR 0020 decision 12). */
  const [zoomedId, setZoomedId] = useState<string | null>(null);

  const [phase, setPhase] = useState<Phase>('idle');
  const [error, setError] = useState<string | null>(null);
  const [notices, setNotices] = useState<string[]>([]);
  const [terminals, setTerminals] = useState<Terminal[]>([]);
  const [liveHealth, setLiveHealth] = useState<Record<string, TerminalHealth>>({});
  /** What the streamer says each Terminal is, from `{t:'state'}`. Absent means snapshots — the wall's
   * resting state. */
  const [modes, setModes] = useState<Record<string, TileMode>>({});
  const [elided, setElided] = useState<Record<string, number>>({});
  /** Terminals this page has ASKED to be live, oldest first. The page's own accounting, because a
   * promotion that has not arrived yet costs the Machine as much as one that has. */
  const [promoted, setPromoted] = useState<string[]>([]);

  /** Where a tagged frame goes: one writer per Terminal id, registered by the tile that owns it. */
  const writersRef = useRef(new Map<string, (payload: Uint8Array) => void>());
  /** The last geometry each tile reported. A tile that subscribes 0×0 asks a Machine for a zero-sized
   * screen, so a subscription waits for a real measurement. Outlives a reconnect: the tiles did not
   * move. */
  const sizesRef = useRef(new Map<string, Size>());
  /**
   * When a frame last arrived for each Terminal, and the tile elements the sidebar reveals.
   *
   * REFS, NOT STATE, AND THAT IS THE WHOLE DESIGN OF THE STATUS BAR. A live journal delivers up to fifteen
   * frames a second per tile; putting an arrival timestamp into React state would re-render this page —
   * and therefore the grid and up to `WALL_TILE_CAP` tiles — at that rate, to move a number in a footer.
   * So the arrivals go into a ref, and `chrome/StatusBar.tsx` owns a one-second clock that reads it, which
   * confines the tick to the one component that has to show it.
   */
  const lastFrameRef = useRef(new Map<string, number>());
  const tileElsRef = useRef(new Map<string, HTMLElement>());
  const revealTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const note = useCallback((line: string) => {
    // Bounded: a streamer that is unhappy every 3 seconds must not grow the page.
    setNotices((prev) => (prev[0] === line ? prev : [line, ...prev].slice(0, 6)));
  }, []);

  /** Send on a specific connection — the live one unless a handler names its own. */
  const send = useCallback((msg: OutboundMessage, conn = connRef.current): boolean => {
    if (!conn || conn.socket.readyState !== WebSocket.OPEN) return false;
    conn.socket.send(JSON.stringify(msg));
    return true;
  }, []);

  // --- the wall ----------------------------------------------------------------------------------

  const inventory = useMemo(() => withLiveHealth(terminals, liveHealth), [terminals, liveHealth]);

  /** A stable string, so the effects below fire when the SET of tiles changes and not when a health
   * update produced new Terminal objects. */
  const wallKey = inventory.map((t) => t.id).join('\n');

  /** Which ids are on the wall, and which are live — what the sidebar needs to mark its leaves, and what
   * the tree's rollups count. Sets rather than arrays because the tree asks per Terminal.
   *
   * Every Terminal is on the wall now, so this is the whole inventory — kept as a set rather than
   * inlined as `true` because the tree's rollups still count it, and because a wall that ever grows a
   * cap again should have one place to express it. */
  const onWallIds = useMemo(() => new Set(inventory.map((t) => t.id)), [inventory]);
  const liveIds = useMemo(
    // The STREAMER's answer, not the page's optimism: `promoted` includes a promotion that has been asked
    // for and not yet granted, and a sidebar leaf marked live for a Terminal the streamer refused is the
    // page disagreeing with itself in public.
    () => new Set(Object.entries(modes).filter(([, m]) => m === 'live').map(([id]) => id)),
    [modes]
  );

  /**
   * The filter, and what it hides.
   *
   * IT HIDES; IT DOES NOT UNSUBSCRIBE, and that is deliberate rather than an oversight: typing a
   * hostname into a search box must not tear down and rebuild a live PTY attach on every keystroke,
   * and each of those is an sshd session and a per-viewer tmux session on a 2 GB Machine (ADR 0020).
   * Filtering the wall is a way of LOOKING, and looking must not cost the Fleet anything.
   *
   * WHAT IT DOES COST IS THE SNAPSHOT. `TileWall` draws only what is not hidden, so a filtered tile's
   * component and xterm are gone (an earlier version of this note claimed otherwise) — but its id
   * stays in `sync`'s wanted set, so the streamer keeps taking its `capture-pane` and keeps sending
   * bytes that nothing paints. That is the right trade for a search box and the wrong one for a pane
   * an operator has decided they do not want, which is why hiding is a separate concept below.
   */
  const [filter, setFilter] = useState<PaneFilter>(EMPTY_FILTER);
  const hiddenBase = useMemo(
    () => hiddenPanes(inventory, filter, liveIds),
    [filter, inventory, liveIds]
  );
  /**
   * Panes an operator HID, which is a different act from a filter that stopped matching them.
   *
   * THE ONE THAT COSTS LESS. A filter is a way of looking and must not cost the Fleet anything, so it
   * deliberately leaves everything subscribed; hiding is a statement that this pane is not worth
   * paying for, and `sync` below subtracts it from the wanted set. It also persists, because a wall
   * an operator curated and a reload threw away is a wall they curate once and then stop curating.
   */
  const hides = useHiddenPanes();
  /**
   * What the WALL hides: the filter's set, the panes an operator hid, plus the zoomed Terminal
   * (slice 05).
   *
   * The zoomed pane is drawn full-bleed OVER the wall, so its wall tile must not also be rendered — two
   * tiles for one id would fight over the frame writer (last mount wins), and one would receive nothing.
   * Hiding is a view, not an edit (`TileWall.hidden`), so the wall keeps the tile in its saved document
   * and the other tiles keep streaming behind the overlay; unzooming just draws it again.
   */
  const hidden = useMemo(() => {
    const set = new Set<string>(hiddenBase);
    for (const id of hides.ids) set.add(id);
    if (zoomedId) set.add(zoomedId);
    return set;
  }, [hiddenBase, hides.ids, zoomedId]);
  const options = useMemo(() => paneOptions(inventory), [inventory]);

  // Latest-value refs, so the socket callbacks and `sync` can act on what is true WHEN THEY FIRE. This
  // is what lets `connect` keep a stable identity — a `connect` that changed on every wall edit would
  // tear the socket down and rebuild it on every drag.
  const tilesRef = useRef<readonly Terminal[]>(inventory);
  tilesRef.current = inventory;
  /** What an operator has hidden, readable from `sync` — which must keep a stable identity, or the
   *  socket would be torn down and rebuilt every time somebody hid a tile. */
  const hiddenRef = useRef<ReadonlySet<string>>(hides.ids);
  hiddenRef.current = hides.ids;
  const promotedRef = useRef(promoted);
  promotedRef.current = promoted;
  // Two more for the status bar's own clock, which reads through `wallStats` rather than through props.
  const inventoryCountRef = useRef(inventory.length);
  inventoryCountRef.current = inventory.length;
  const elidedTotalRef = useRef(0);
  elidedTotalRef.current = Object.values(elided).reduce((sum, n) => sum + n, 0);

  // --- liveness ----------------------------------------------------------------------------------

  const demote = useCallback(
    (id: string, why: string): void => {
      if (!send({ t: 'blur', id })) return;
      setPromoted((prev) => prev.filter((x) => x !== id));
      note(`${id}: back to snapshots — ${why}`);
    },
    [note, send]
  );

  /**
   * Promote one Terminal to a live PTY attach, inside the budget.
   *
   * The budget is enforced HERE rather than in the tile, because it is a property of the tab: the
   * streamer would happily open one attach per tile, and the Machines would pay for it.
   */
  const promote = useCallback(
    (id: string, size: Size): void => {
      if (!size.cols || !size.rows) {
        note(`${id}: not going live — this tile has not measured a size yet`);
        return;
      }
      const current = promotedRef.current;
      if (current.includes(id)) return;
      if (current.length >= LIVE_BUDGET) {
        const oldest = current[0];
        if (!oldest) return;
        demote(oldest, `the live budget is ${LIVE_BUDGET} Terminals and ${id} was promoted`);
      }
      if (!send({ t: 'focus', id, cols: size.cols, rows: size.rows })) return;
      setPromoted((prev) => (prev.includes(id) ? prev : [...prev.filter((x) => x !== id), id]));
      note(`${id}: live attach requested at ${size.cols}×${size.rows}`);
    },
    [demote, note, send]
  );

  /**
   * Bring the socket's subscriptions in line with the wall.
   *
   * Called whenever any input changes: the socket opens, a tile measures itself, or the inventory
   * changes. Idempotent, because "what should be subscribed" is a function of state and not of the
   * event that brought us here.
   *
   * NOTHING IS PROMOTED AUTOMATICALLY, and that is simpler than it used to be for a reason worth
   * keeping: a pinned slot could match twelve Machines, so this function carried a guard against a
   * tick-box becoming twelve `sshd` sessions. With no pins, going live is only ever an operator
   * pressing the control on one tile, which is what slice 2 amendment 9 asked for in the first place.
   */
  const sync = useCallback((conn = connRef.current): void => {
    if (!conn || conn.socket.readyState !== WebSocket.OPEN) return;
    // A superseded socket must not subscribe: it is on its way out and its execs would be paid for by
    // the Machines for nothing.
    if (connRef.current !== conn) return;
    const tiles = tilesRef.current;
    /*
      Wanted = on the wall AND measured AND not hidden.

      A tile sized 0×0 has not laid out, and a 0×0 subscribe asks a Machine for a zero-sized screen,
      so it is not wanted yet.

      HIDING IS SUBTRACTED HERE, AND THAT IS WHERE ITS VALUE IS. A hidden pane falls out of `wanted`,
      so `resubscribePlan` produces an `unsubscribe` for it and the loop below sends one. WHAT THE
      STREAMER THEN STOPS DOING, read out of `backend/src/panels/server.ts`:

        · `snapshotRound` builds its exec list from live subscriptions only, and per NODE-AND-SESSION
          with the set of WINDOWS as an argument. An unsubscribed window leaves that argument list;
          the last window of a session takes the whole `capture-pane` exec with it, every 3 s,
          forever. On a wall of many tiles that is the whole point.
        · `fanOut` delivers to subscribers, so the bytes, the byte budget and the elided accounting
          for that Terminal stop.
        · `announceHealth` walks subscriptions, so its `{t:'state'}` messages stop.
        · a LIVE attach is given up: the tile unmounts and blurs itself (`TerminalTile`'s unmount
          effect), and `demote` below covers the case where the id leaves `wanted` while its tile is
          still mounted. Either way the `sshd` session, the PTY and the per-viewer grouped tmux
          session on the node are killed and the `LIVE_BUDGET` slot is freed.

      WHAT IT DOES NOT STOP, and this is the honest half: the streamer's own 30 s pass. `refresh()`
      discovers and PROBES every Machine, and measures fleet health, whether or not anybody is
      subscribed to anything — that is `list-panes`/`has-session` per node plus one Temporal and one
      metrics query per fleet, and it is what `GET /api/panels/terminals` and the sidebar tree are
      made of. Hiding a pane is not a way to stop probing a Machine, and nothing here pretends it is.
      A hidden pane also stops having its exit banner read, because `paneExitFromScreen` runs on the
      snapshots that are no longer being taken.
    */
    const wanted = wantedSubscriptions(tiles, sizesRef.current, hiddenRef.current);

    // The delta, computed once. On a FRESH socket after a reconnect `conn.subscribed` is empty, so this
    // subscribes each wanted Terminal exactly once — no loss, no duplication (slice 05, criterion 3).
    const plan = resubscribePlan(wanted, conn.subscribed);

    // No longer wanted — it left the inventory, or an operator hid it. Either way: stop the snapshot
    // exec it was costing, and give up any attach it held. The tile's own unmount blurs a live one
    // too (slice 2), but a Terminal can leave `wanted` between renders while its tile is still
    // mounted, and a demote that has already happened is a no-op on both sides.
    for (const id of plan.unsubscribe) {
      if (promotedRef.current.includes(id)) {
        demote(id, hiddenRef.current.has(id) ? 'you hid this pane' : 'it left the wall');
      }
      send({ t: 'unsubscribe', id }, conn);
      conn.subscribed.delete(id);
      sizesRef.current.delete(id);
      // Its frame timestamp goes too, or the status bar's "oldest snapshot" would be answered forever by
      // a Terminal that is no longer on the wall — the wall would look stale because of a tile that is
      // not on it.
      lastFrameRef.current.delete(id);
    }

    for (const id of plan.subscribe) {
      const size = sizesRef.current.get(id);
      if (!size) continue; // measured first, always — the plan only names measured ids, but be exact
      // What addon-fit measured. It is also what a live attach `stty`s before attaching, which is
      // why a Terminal must not live inside a transformed or zoomable canvas.
      if (!send({ t: 'subscribe', id, cols: size.cols, rows: size.rows }, conn)) return;
      conn.subscribed.add(id);
    }
  }, [demote, send]);

  // The inventory changed shape.
  useEffect(() => {
    sync();
  }, [sync, wallKey]);

  /**
   * …and so did what an operator is paying for.
   *
   * A stable key rather than the set, for the reason `wallKey` is a string: this must fire when the
   * MEMBERSHIP changes and not when a re-render produced a new Set with the same ids in it. Hiding
   * without this would take the tile off screen and leave the streamer taking its `capture-pane`
   * until something else happened to call `sync` — which is precisely the `display:none` failure this
   * feature exists to avoid.
   */
  const hiddenKey = hides.order.join('\n');
  useEffect(() => {
    sync();
  }, [hiddenKey, sync]);

  // --- tile callbacks ---------------------------------------------------------------------------

  const subscribeFrames = useCallback(
    (id: string, write: (payload: Uint8Array) => void): (() => void) => {
      writersRef.current.set(id, write);
      return () => {
        // Only if it is still ours: a tile that unmounts AFTER its replacement registered would
        // otherwise delete the live writer and leave a mounted tile receiving nothing.
        if (writersRef.current.get(id) === write) writersRef.current.delete(id);
      };
    },
    []
  );

  /** The tile containers, for the sidebar's reveal. See `grid/TileWall`'s `registerTile`. */
  const registerTile = useCallback((id: string, el: HTMLElement | null): void => {
    if (el) tileElsRef.current.set(id, el);
    else tileElsRef.current.delete(id);
  }, []);

  /**
   * Scroll a Terminal's tile into view and flash it. IT DOES NOT GO LIVE.
   *
   * ADR 0020's cost model is why, and it is worth stating at the call site as well as in the sidebar: a
   * live tile is an `sshd` session, a PTY and a per-viewer tmux session on an `s-1vcpu-2gb` Machine, and
   * slice 2 amendment 9 makes promotion "never a click, never a stray tab-stop". A tree of clickable
   * Machine names is the easiest place in this UI to break that by accident, so the reveal deliberately
   * touches nothing but the scroll position, `revealed`, and DOM focus.
   *
   * `block: 'nearest'` so a tile that is ALREADY visible does not get scrolled for no reason — the flash
   * is what tells the operator where it is, and a jump would move a wall they were reading.
   */
  const reveal = useCallback(
    (id: string): void => {
      const el = tileElsRef.current.get(id);
      if (!el) {
        // A leaf with no tile. Every Terminal in the inventory gets one, so this now means the tree
        // and the wall disagree — a Terminal that arrived between the tree's render and the wall's
        // sync. Words rather than silence, because a reveal that does nothing reads as a broken
        // sidebar.
        note(`${id}: it has no tile on the wall yet — it may have only just been discovered.`);
        return;
      }
      el.scrollIntoView({ behavior: 'smooth', block: 'nearest', inline: 'nearest' });
      // Focus the CONTAINER (`tabIndex={-1}`), never the terminal: xterm keeps a helper textarea even
      // with `disableStdin`, and focusing into a tile would put a caret inside a read-only screen.
      el.focus({ preventScroll: true });
      setRevealed(id);
      if (revealTimerRef.current !== null) clearTimeout(revealTimerRef.current);
      // Matches `chrome.css`'s `kontra-reveal` duration: the attribute drives the animation, so it has to
      // outlive it and then stop, or a re-render would replay the ring on a tile nobody just revealed.
      revealTimerRef.current = setTimeout(() => {
        revealTimerRef.current = null;
        setRevealed((prev) => (prev === id ? null : prev));
      }, 1300);
    },
    [note]
  );

  useEffect(
    () => () => {
      if (revealTimerRef.current !== null) clearTimeout(revealTimerRef.current);
    },
    []
  );

  /**
   * The link Serve and Run hand back: a Terminal id to reveal once this wall has a tile for it.
   *
   * WAITING FOR THE TILE IS THE WHOLE JOB. `kontra workflow serve` returns as soon as the tmux
   * session exists, but the streamer only re-discovers `kontra-*` sessions on its own cadence, so
   * for up to `KONTRA_PANEL_DISCOVER_MS` the wall genuinely does not have it yet. Consuming the
   * focus immediately would scroll to nothing and clear it, and the operator would arrive at a wall
   * with their worker somewhere on it and no indication which one. So the request is held until the
   * id shows up in the inventory, and released the moment it does.
   */
  const focusTerminal = useAppStore((s) => s.focusTerminal);
  const clearFocusTerminal = useAppStore((s) => s.clearFocusTerminal);
  useEffect(() => {
    if (focusTerminal === null) return;
    if (!inventory.some((t) => t.id === focusTerminal)) return;
    reveal(focusTerminal);
    clearFocusTerminal();
  }, [clearFocusTerminal, focusTerminal, inventory, reveal]);

  /**
   * What the nav rail's counters read.
   *
   * Published from here rather than polled there, because this page holds the fresher answer for
   * both: the inventory it re-reads on its own clock, and the live set, which only exists while
   * this socket does. The cleanup zeroing `live` is not tidiness — closing the socket kills every
   * grouped session on every node, so a rail still reporting four attaches would be reporting four
   * sessions that no longer exist.
   */
  const setWallCounts = useAppStore((s) => s.setWallCounts);
  // The INVENTORY itself, not only its size: the Actors page counts Machines per Actor from it, and
  // that number is the one live fact an Actor has. Publishing from here keeps the two surfaces from
  // disagreeing — this page's copy is always the fresher one.
  const setPanes = useAppStore((s) => s.setPanes);
  useEffect(() => {
    setWallCounts({ panes: inventory.length, live: promoted.length });
  }, [inventory.length, promoted.length, setWallCounts]);
  useEffect(() => {
    setPanes(inventory);
  }, [inventory, setPanes]);
  useEffect(
    () => () => {
      setWallCounts({ live: 0 });
    },
    [setWallCounts]
  );

  /**
   * "Show me this node" — the commonest thing an operator wants after finding a Machine in the tree.
   *
   * It reveals the node's FIRST Terminal rather than filtering the wall to it. On a selector wall
   * this added a slot, because the wall was a query; on a wall that already holds every Terminal
   * there is nothing to add, and hiding the other tiles to answer "where is this one" would throw
   * away the arrangement the operator built.
   */
  const revealNode = useCallback(
    (machine: string): void => {
      const first = tilesRef.current.find((t) => t.machine === machine);
      if (!first) {
        note(`${machine}: no Terminal on this Machine is in the inventory`);
        return;
      }
      reveal(first.id);
    },
    [note, reveal]
  );

  /**
   * What the status bar reads, once a second.
   *
   * A GETTER over refs and the latest render's values, not a props object: see `lastFrameRef` above. It is
   * rebuilt when the wall or the live set changes — which is what the status bar's `useEffect` depends on —
   * and reads the frame arrivals fresh on every call.
   */
  const wallStats = useCallback((): WallStats => {
    const now = Date.now();
    /*
      HIDDEN PANES ARE NOT ON THE WALL, so they are not in this count.

      The footer renders `tiles - painted` as "N not painted yet", and a hidden pane never paints —
      by design, because it is not subscribed. Counting it would make the status bar report three
      broken tiles for three tiles an operator deliberately put away, on the one surface whose whole
      job is telling those two apart. `inventory` below still reports the whole Fleet, so the pair
      reads "2 Terminals of 5" and the difference is exactly what the hidden strip lists.
    */
    const tiles = tilesRef.current.filter((t) => !hiddenRef.current.has(t.id));
    let oldestFrameAgeMs: number | null = null;
    let oldestId: string | null = null;
    let painted = 0;
    for (const terminal of tiles) {
      const at = lastFrameRef.current.get(terminal.id);
      if (at === undefined) continue;
      painted += 1;
      const age = now - at;
      if (oldestFrameAgeMs === null || age > oldestFrameAgeMs) {
        oldestFrameAgeMs = age;
        oldestId = terminal.id;
      }
    }
    return {
      tiles: tiles.length,
      inventory: inventoryCountRef.current,
      live: promotedRef.current.length,
      budget: LIVE_BUDGET,
      painted,
      oldestFrameAgeMs,
      oldestId,
      elided: elidedTotalRef.current,
    };
  }, []);

  const measured = useCallback(
    (id: string, cols: number, rows: number): void => {
      sizesRef.current.set(id, { cols, rows });
      /**
       * ALWAYS sync, even when the numbers did not change.
       *
       * MEASURED: an early return on an unchanged size left tiles permanently blank. `sizesRef`
       * outlives a remount on purpose, so a tile that measures 90×15, is remounted by StrictMode, and
       * measures 90×15 again reports a size that is already known — and if the socket opened in
       * between, that second measurement was the ONLY remaining chance to subscribe it. The wall then
       * showed the top tile of each slot as a Machine printing nothing, with frames flowing for every
       * other tile.
       *
       * A re-sync is cheap and idempotent: it sends a `subscribe` only for an id this connection does
       * not have. It deliberately does NOT re-subscribe on a size change either — the streamer's
       * `subscribe` ignores cols/rows (a snapshot is whatever size the remote pane is), and a live
       * tile re-asks for itself through `TerminalTile`'s own resize handler.
       */
      sync();
    },
    [sync]
  );

  const onFocus = useCallback(
    (id: string, cols: number, rows: number): void => promote(id, { cols, rows }),
    [promote]
  );

  const onBlur = useCallback((id: string): void => demote(id, 'you asked for snapshots'), [demote]);

  const converge = useCallback(
    (id: string): void => {
      // The client picks WHICH Machine; the streamer decides what the converge contains.
      if (send({ t: 'converge', id })) note(`converge requested for ${id}`);
    },
    [note, send]
  );

  /**
   * Zoom a pane to fill the wall, or restore it — tmux's `C-b z`, as a mouse click (ADR 0020, slice 05).
   *
   * READ-ONLY IS WHAT MAKES THIS ORDINARY UI. With no keystrokes to forward there is no "active pane"
   * to move focus between, so the whole tmux shortcut vocabulary collapses into clicks: the sidebar
   * tree switches which pane you are looking at, the wheel scrolls scrollback, and zoom is just "show
   * this one big". The operator never types `C-b z`. It puts NOTHING on the socket, so it cannot break
   * read-only.
   *
   * PAGE STATE, NEVER A TRANSFORM. ADR 0020 decision 12 forbids a Terminal living inside a transformed
   * or zoomable canvas: `addon-fit`'s measured cols/rows is what the streamer `stty`s before `tmux
   * attach`, and a CSS `scale()` would make that measurement a lie about the remote pane's shape. So
   * the zoomed pane is REAL geometry — laid out at full size, re-fitted by its own `ResizeObserver` —
   * and this holds only WHICH pane, not how it is drawn.
   *
   * CLICKING THE ZOOMED PANE RESTORES THE WALL; clicking any other MOVES the zoom to it. There is only
   * ever one, which is the point — a "zoom" that showed several would just be a smaller wall. This was
   * `chrome/zoom.ts`, a one-line `toggleZoom` under twenty-five lines of the rationale above; the
   * ternary now sits where it is used and `dashboardPage.test.ts` asserts the toggle through the
   * control, against the overlay it produces — including that the zoomed tile leaves the wall, which
   * the pure function could not express and which is the half that matters.
   */
  const zoomPane = useCallback(
    (id: string): void => setZoomedId((prev) => (prev === id ? null : id)),
    []
  );

  /**
   * Take one WINDOW off the wall until it is put back.
   *
   * IT SAYS SO IN THE NOTICES, on top of the persistent bar above the wall. Two affordances for one
   * act, deliberately: the notice is what an operator sees at the moment the tile disappears — the
   * instant the "did I just kill that Worker?" question is asked — and the bar is what is still there
   * tomorrow. The `sync` effect above is what makes it stop costing anything.
   *
   * A ZOOMED PANE THAT IS HIDDEN IS UNZOOMED FIRST. The zoom overlay draws the tile at full bleed
   * OVER the wall and is not part of the wall's `hidden` set, so hiding without this would leave the
   * hidden pane as the only thing on screen.
   */
  const hidePane = useCallback(
    (id: string): void => {
      setZoomedId((prev) => (prev === id ? null : prev));
      hides.hide(id);
      note(`${id}: hidden — no snapshot exec, no frames, no attach. Restore it above the wall.`);
    },
    [hides, note]
  );

  const showPane = useCallback(
    (id: string): void => {
      hides.show(id);
      // A restored pane re-subscribes through the ordinary path: its tile mounts, measures itself,
      // and `measured` calls `sync`. Said here because the tile appears a moment before its first
      // frame does, and an empty rectangle is the thing this whole surface refuses to leave silent.
      note(`${id}: restored — it re-subscribes as soon as its tile has measured itself.`);
    },
    [hides, note]
  );

  /** A zoomed Terminal that leaves the Fleet restores the wall — there is nothing left to zoom. */
  useEffect(() => {
    if (zoomedId && !inventory.some((t) => t.id === zoomedId)) setZoomedId(null);
  }, [inventory, zoomedId]);

  /** The Terminal being zoomed, resolved from the live inventory so its health tracks `{t:'state'}`. */
  const zoomTerminal = zoomedId === null ? null : (inventory.find((t) => t.id === zoomedId) ?? null);
  /** The socket is down and reconnecting: every tile's screen is frozen, so every tile says so. */
  const disconnected = reconnectAt !== null;

  /**
   * Make every tile re-measure after the layout changed.
   *
   * `TerminalTile` refits on `window.resize` and on nothing else, so a slot that was resized or a
   * density that changed would leave its xterm sized for the old rectangle: a snapshot tile letterboxes
   * and a live tile keeps streaming at the geometry it was `stty`d at. A synthetic event is blunt — it
   * refits every tile, not just the ones that moved — but it is correct, it costs one fit per tile per
   * commit, and the alternative is a `ResizeObserver` inside a file this slice does not own.
   */
  const onLayoutCommit = useCallback((): void => {
    if (typeof window === 'undefined') return;
    // Next frame: React has committed the new geometry to the DOM by then, so what the tiles measure
    // is the layout they are actually in.
    window.requestAnimationFrame(() => window.dispatchEvent(new Event('resize')));
  }, []);

  // --- the socket -------------------------------------------------------------------------------

  const loadInventory = useCallback(
    async (why: 'mount' | 'refresh'): Promise<Terminal[] | null> => {
      try {
        const next = await fetchTerminals();
        setTerminals((prev) => (sameInventory(prev, next) ? prev : next));
        return next;
      } catch (e) {
        // A refresh failure is a notice, not an error page: the socket is still streaming, and
        // blanking a working wall because one poll failed is worse than a stale inventory.
        if (why === 'refresh') note(`could not refresh the Fleet inventory: ${String(e)}`);
        else throw e;
        return null;
      }
    },
    [note]
  );

  /**
   * Schedule a reconnect after a socket drop, on the backoff (slice 05).
   *
   * Called from the SOCKET's own `onclose`/`onerror` — a socket that existed and went away — never from
   * the ticket-mint failure, which is "the Dashboard is switched off" rather than "a dropped socket" and
   * keeps its error page. Guarded so `onerror` immediately followed by `onclose` schedules once, and so
   * an unmounted page does not resurrect a socket.
   */
  const scheduleReconnect = useCallback((): void => {
    if (unmountedRef.current) return;
    if (reconnectRef.current.timer !== null) return;
    const attempt = reconnectRef.current.attempt;
    const delay = reconnectDelay(attempt);
    setPhase('reconnecting');
    setReconnectAt({ attempt, delay });
    note(disconnectedNotice(attempt, delay));
    reconnectRef.current.timer = setTimeout(() => {
      reconnectRef.current.timer = null;
      reconnectRef.current.attempt = attempt + 1;
      void connectRef.current();
    }, delay);
  }, [note]);

  const connect = useCallback(async (): Promise<void> => {
    // No credential to check for any more: the ticket is minted same-origin by orchestrator-api,
    // which holds KONTRA_PANEL_TOKEN. An unconfigured deployment surfaces as the mint's own 503,
    // carrying a message that names the process to fix — see the catch below.
    setPhase('connecting');
    setError(null);
    try {
      const found = await loadInventory('mount');
      if (found && found.length === 0) {
        setPhase('error');
        setError(
          'No Terminals: the Fleet inventory is empty. `kontra fleet up` creates Machines; a ' +
            'Machine with no session is still a tile, so an empty list means no Machines at all.'
        );
        return;
      }

      const { ticket } = await fetchTicket();
      const socket = new WebSocket(panelSocketUrl(base, ticket));
      socket.binaryType = 'arraybuffer';
      const conn: Conn = { socket, subscribed: new Set() };
      // Exactly one live connection, always. Under StrictMode two `connect()` calls interleave and the
      // effect cleanup can run before either socket exists, so the second one closes the first here
      // rather than leaving a socket nobody holds — which the streamer would keep taking snapshots for.
      connRef.current?.socket.close();
      connRef.current = conn;
      setPromoted([]);

      socket.onopen = () => {
        if (connRef.current !== conn) {
          socket.close();
          return;
        }
        // A clean open: the backoff resets and the reconnecting banner clears (slice 05).
        reconnectRef.current.attempt = 0;
        setReconnectAt(null);
        setPhase('streaming');
        sync(conn);
      };
      socket.onmessage = (event: MessageEvent<unknown>) => {
        if (connRef.current !== conn) return;
        if (event.data instanceof ArrayBuffer) {
          const { id, payload } = decodeTagged(event.data);
          // WHEN, recorded here and nowhere else. This is the only place the page can see a frame arrive,
          // and it is what the status bar's "oldest snapshot" is measured from — the browser's own
          // observation rather than `Terminal.lastSnapshotAt`, which only refreshes with the 30-second
          // inventory poll and would make a stale INVENTORY indistinguishable from a stale snapshot. A
          // `Map.set` on a ref, deliberately: at fifteen frames a second per tile, React state here would
          // re-render the wall at frame rate.
          lastFrameRef.current.set(id, Date.now());
          // One socket carries every Terminal, so a frame goes to the tile whose id it carries and
          // nowhere else. A frame for a Terminal that has left the wall has no writer and is dropped.
          writersRef.current.get(id)?.(payload);
          return;
        }
        if (typeof event.data !== 'string') return;
        const msg = JSON.parse(event.data) as ServerMessage;
        if (msg.t === 'state') {
          // Mode and health arrive together, so a tile is never told a stale one of either.
          setLiveHealth((prev) => ({ ...prev, [msg.id]: msg.health }));
          setModes((prev) => (prev[msg.id] === msg.mode ? prev : { ...prev, [msg.id]: msg.mode }));
          if (msg.mode !== 'live') {
            // An attach that ended, failed, or was never opened. The page's live accounting follows
            // the streamer's answer rather than its own optimism, or the budget would fill with
            // Terminals that are not live.
            setPromoted((prev) => (prev.includes(msg.id) ? prev.filter((x) => x !== msg.id) : prev));
          }
          if (msg.mode === 'error') note(`${msg.id}: the streamer cannot stream this Terminal`);
        } else if (msg.t === 'error') {
          note(msg.message);
          /**
           * AN ERROR CARRYING AN ID MEANS THAT TERMINAL IS NOT LIVE.
           *
           * MEASURED against the stub streamer, which implements the wire independently: a `focus` is
           * answered with `{t:'error', id}` and NOTHING ELSE (slice 1 amendment 6 — "accepted and
           * answered with an error naming slice 2"). Waiting for a `{t:'state'}` that never comes left
           * the page reporting "1/4 live" for a Terminal the streamer had refused, while the tile's own
           * control still said "Go live" — the page and the tile disagreeing about the same Terminal.
           *
           * Safe for every documented use of the type: a refused focus, a failed attach, an attach that
           * ended (which slice 2 amendment 7 pairs WITH this error), and "no such Terminal". None of
           * them describes a Terminal that is live. An error with no id is about the socket, not a
           * Terminal, and is left alone.
           */
          if (msg.id) {
            setPromoted((prev) => (prev.includes(msg.id!) ? prev.filter((x) => x !== msg.id) : prev));
          }
        } else if (msg.t === 'elided') {
          // Output dropped by the byte cap. Shown, never swallowed: a tile that silently skips output
          // is a tile that lies about what a Worker printed.
          setElided((prev) => ({ ...prev, [msg.id]: (prev[msg.id] ?? 0) + msg.bytes }));
        }
      };
      socket.onerror = () => {
        if (connRef.current !== conn) return;
        setError(
          `the Dashboard socket failed (${base}). Is 8090 published, and is this origin in KONTRA_PANEL_ORIGIN?`
        );
        // A dropped socket is retried, not left as a dead-end error (slice 05, criterion 3). `onclose`
        // usually follows; the guard in `scheduleReconnect` makes the pair schedule once.
        scheduleReconnect();
      };
      socket.onclose = () => {
        // A superseded socket closing is expected and says nothing about the live one — clearing the
        // page's state here would blank a wall that is streaming perfectly well.
        if (connRef.current !== conn) return;
        connRef.current = null;
        // Nothing is live once the socket is gone: the streamer kills every grouped session when it
        // drops, so a tile still offering "back to snapshots" would be offering to blur a Machine that
        // has already forgotten it.
        setPromoted([]);
        setModes({});
        // SAY SO, do not freeze silently: schedule a reconnect and let the banner and the tiles' stale
        // badges announce it (slice 05, criteria 3 and 4).
        scheduleReconnect();
      };
    } catch (e) {
      // The ticket mint or the first inventory read failed — "the Dashboard is switched off", not a
      // dropped socket. It keeps its error page and does NOT reconnect (the 503 fail-closed path relies
      // on no socket being opened); the operator fixes the config and reloads.
      setPhase('error');
      setError(String(e));
    }
  }, [base, loadInventory, note, scheduleReconnect, sync]);
  connectRef.current = connect;

  useEffect(() => {
    unmountedRef.current = false;
    void connect();
    return () => {
      unmountedRef.current = true;
      if (reconnectRef.current.timer !== null) {
        clearTimeout(reconnectRef.current.timer);
        reconnectRef.current.timer = null;
      }
      connRef.current?.socket.close();
      connRef.current = null;
    };
  }, [connect]);

  // The inventory, on its own clock. This is what makes `fleet up --count 10` fill a selector-driven
  // wall with no edit and no reload.
  useEffect(() => {
    const timer = setInterval(() => {
      void loadInventory('refresh');
    }, INVENTORY_REFRESH_MS);
    return () => clearInterval(timer);
  }, [loadInventory]);

  const tileCount = inventory.length;
  /**
   * The Terminal the drawer is describing, resolved from the INVENTORY rather than remembered.
   *
   * So the drawer's health sentences and snapshot age track `{t:'state'}` instead of being a snapshot of
   * whatever was true when it was opened — a drawer that says "session: present" about a session that has
   * since gone is the exact class of stale reading ADR 0020's four-signal rule exists to prevent. A drawer
   * whose Terminal has left the Fleet closes itself, because there is nothing left to describe.
   */
  const drawer = drawerId === null ? null : (inventory.find((t) => t.id === drawerId) ?? null);

  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col gap-2 overflow-hidden p-3">
      {/*
        ONE BAR, and what is on it is what the wall's geometry cannot express by itself. Compact and
        Reset are the two operations that are not a drag; the style controls are one appearance for
        the whole wall, because a per-tile palette is a wall nobody can read across.

        THE PHASE AND THE COUNTS ARE NOT HERE — they are the status bar's (slice 7a), and this is not
        only tidiness: the header used to render the word `snapshots` for the socket's phase and the
        status bar renders it too, which is two matches for one strict-mode selector.
      */}
      <div className="flex flex-wrap items-center gap-2">
        <h1 className="text-sm font-semibold">Monitor</h1>
        <span className="text-[11px] text-muted-foreground">
          {promoted.length} live · drag a tile by its banner, drag the corner to resize
        </span>

        <span className="ml-auto flex items-center gap-1">
          <Button
            variant="outline"
            size="sm"
            data-testid="wall-compact"
            title="close every gap without moving a tile sideways"
            onClick={() => {
              wall.compact();
              onLayoutCommit();
            }}
          >
            Compact
          </Button>
          <Button
            variant="outline"
            size="sm"
            data-testid="wall-reset"
            title="throw the arrangement away and lay the Terminals out in inventory order"
            onClick={() => {
              wall.reset(inventory.map((t) => t.id));
              onLayoutCommit();
            }}
          >
            Reset layout
          </Button>
          <StyleControls style={style} />
        </span>
      </div>

      <PaneFilterBar
        filter={filter}
        options={options}
        onChange={setFilter}
        // The FILTER's count, not the wall's — a zoomed pane is still shown, just bigger, so it must
        // not read as one the filter hid.
        shown={inventory.length - hiddenBase.size}
        total={inventory.length}
      />

      {/*
        WHAT IS HIDDEN, ALWAYS ON THE PAGE while anything is.

        HIDDEN MUST NOT LOOK LIKE GONE. A tile that vanishes with nothing to show for it is
        indistinguishable from a Worker that died — the same confusion the ghosts, the `no session`
        scrim and the chip row all exist to end — so the count, the names and the way back are here
        rather than behind a menu. Above the wall, not below it: an operator looking for a pane that
        is not on the wall is looking at the top of the page.
      */}
      <HiddenPanes
        ids={hides.order}
        known={onWallIds}
        onShow={showPane}
        onShowAll={() => {
          hides.showAll();
          note('every hidden pane restored — each re-subscribes once its tile has measured itself.');
        }}
      />

      {hides.warning && (
        // A hidden set that could not be read or written is said out loud, for `useWall`'s reason: a
        // set that silently reset looks exactly like one that never saved.
        <p className="text-xs text-amber-700 dark:text-amber-300" data-testid="hidden-warning">
          {hides.warning}{' '}
          <button className="underline" onClick={hides.dismissWarning}>
            dismiss
          </button>
        </p>
      )}

      {/* Said in the UI, not only in the ADR: a wall that looks like a log invites someone to treat a
          Terminal as evidence for what a Worker did. */}
      <p className="text-xs text-muted-foreground">
        Read-only, and lossy by construction — a Terminal is a screen, not a log. The Manifest, the
        journal on the Machine and the lake are the record.
      </p>

      {error && (
        <p className="text-xs text-destructive" data-testid="dashboard-error">
          {error}
        </p>
      )}

      {reconnectAt && (
        // The socket dropped and the wall is coming back on its own (slice 05). Said rather than left as
        // a silently frozen wall; the tiles carry their own stale badges over their last frames.
        <p className="text-xs text-amber-700 dark:text-amber-300" data-testid="dashboard-reconnecting">
          {disconnectedNotice(reconnectAt.attempt, reconnectAt.delay)}
        </p>
      )}

      {wall.warning && (
        // A layout that silently reset looks like a layout that never saved.
        <p className="text-xs text-amber-700 dark:text-amber-300" data-testid="layout-warning">
          {wall.warning}{' '}
          <button className="underline" onClick={wall.dismissWarning}>
            dismiss
          </button>
        </p>
      )}

      {/*
        THE SIDEBAR IS A FLEX SIBLING OF THE WALL, NEVER AN OVERLAY, and that is a measurement decision
        rather than a layout preference (the one thing adopted verbatim from the reference implementation's
        `Sidebar`). Overlaid, the wall keeps its full width, so every xterm measures cols for a rectangle
        that is partly underneath the sidebar and every line wraps in the wrong column — and those cols are
        what the streamer `stty`s before a live attach. As a sibling the wall's container shrinks, each
        tile's `ResizeObserver` fires, and the geometry stays true.
      */}
      <div className="flex min-h-0 min-w-0 flex-1 gap-2">
        <SidebarTree
          inventory={inventory}
          onWall={onWallIds}
          live={liveIds}
          onReveal={reveal}
          onSelectNode={revealNode}
          // Persisted, and CLOSED by default: the tree takes its width off the wall, and `theme.ts`'s
          // `SIDEBAR_DEFAULT_OPEN` records the measurement that made that the right default — the browser
          // suite caught a 66-column tile wrapping a 66-character journal line.
          open={style.sidebar}
          onToggle={style.toggleSidebar}
        />

        {/*
          THE WALL AND THE ZOOMED PANE SHARE THIS SLOT. The zoomed pane is a full-bleed overlay OVER the
          wall (slice 05): the wall stays mounted underneath — that one tile hidden — so the other tiles
          keep streaming and unzooming is instant. The overlay is REAL geometry (`absolute inset-0`, no
          transform), so the zoomed tile's addon-fit measures a true large screen — ADR 0020 decision 12.
        */}
        <div className="relative flex min-h-0 min-w-0 flex-1 flex-col">
        {tileCount > 0 ? (
          <TileWall
            wall={wall}
            inventory={inventory}
            onLayoutCommit={onLayoutCommit}
            registerTile={registerTile}
            revealed={revealed}
            hidden={hidden}
            /*
              The tile owns its xterm, its measured geometry, its health chips, its banner and the
              repaint rule — twenty-four of these on a wall is exactly why the page does not reach
              into any of them. What the page still decides is what only it can: the palette and font
              size (one appearance for the whole wall), and the four callbacks that touch the socket.
            */
            renderTile={(terminal, { compact, dense, dragHandle }) => (
              <TerminalTile
                terminal={terminal}
                mode={modes[terminal.id] ?? 'snapshot'}
                subscribe={subscribeFrames}
                onMeasure={measured}
                onFocus={onFocus}
                onBlur={onBlur}
                // Per tile, as CONTRACT.md slice 2 amendment 10 said it would be: on a wall,
                // "converge the Machine I am looking at" is the only unambiguous version of this
                // button, and a page-level one would have been a second button with the same name.
                onConverge={converge}
                elided={elided[terminal.id] ?? 0}
                theme={style.theme}
                fontSize={style.fontSize}
                paletteIsDark={style.isDark}
                compact={compact}
                dense={dense}
                onNotice={note}
                onOpenDrawer={setDrawerId}
                // Take this window off the wall AND off the socket. Only on the wall: the zoomed
                // overlay below has no "N hidden" bar to be restored from and would be hiding the
                // one tile it is showing.
                onHide={hidePane}
                // Zoom this pane to fill the wall — tmux's `C-b z`, reachable by mouse alone (slice 05).
                onZoom={zoomPane}
                // When the socket is down, every tile's screen is frozen, so every tile says so.
                stale={disconnected}
                // The banner is the grab point, so the strip an operator reads the hostname off is
                // the strip they drag. See `grid/TileWall.tsx`.
                onDragHandle={dragHandle}
              />
            )}
          />
        ) : (
          /*
            An empty wall SAYS SO, rather than being a hole the colour of a terminal.

            MEASURED: `orchestrator-infra` came back from a reboot with its forked streamer child
            dead, so the inventory was empty — and this branch drew a bare rectangle, which is
            indistinguishable from a wall that failed to load, on the exact surface an operator
            opens to find out whether anything is running.

            `WallEmpty`, NOT a `TileWall` with an empty inventory. A wall syncs its layout to the
            inventory it is handed, and on first paint that is `[]` because the fetch has not
            returned — so mounting one here wiped the saved arrangement to disk before the real
            Terminals arrived. The `terminal-pending` hook stays: a page with no Terminal must
            still be visibly a page, and the browser suite selects on it.
          */
          <div
            className="flex min-h-0 min-w-0 flex-1 flex-col items-stretch justify-start rounded border p-3"
            style={{ background: style.background }}
            data-testid="terminal-pending"
            data-cols={0}
            data-rows={0}
            data-mode={phase}
          >
            <WallEmpty />
          </div>
        )}

        {zoomTerminal && (
          <div
            data-testid={`zoom-${zoomTerminal.id}`}
            className="absolute inset-0 z-30 flex min-h-0 min-w-0 flex-col overflow-hidden rounded border bg-card"
          >
            <TerminalTile
              terminal={zoomTerminal}
              mode={modes[zoomTerminal.id] ?? 'snapshot'}
              subscribe={subscribeFrames}
              onMeasure={measured}
              onFocus={onFocus}
              onBlur={onBlur}
              onConverge={converge}
              elided={elided[zoomTerminal.id] ?? 0}
              theme={style.theme}
              fontSize={style.fontSize}
              paletteIsDark={style.isDark}
              onNotice={note}
              onOpenDrawer={setDrawerId}
              // Its own header carries the "restore" control (`Minimize2`); clicking it toggles zoom off.
              onZoom={zoomPane}
              zoomed
              stale={disconnected}
            />
          </div>
        )}
        </div>

        {drawer && (
          // A FLEX SIBLING, like the sidebar and for the same reason: an overlaid drawer would leave the
          // wall measuring a width it does not have, and those cols are what a live attach was `stty`d to.
          // The wall shrinks, the tiles re-fit, and the geometry stays true.
          //
          // The fallback keeps the WIDTH the drawer is about to take, so opening it moves the wall once
          // rather than twice — a second reflow when the chunk lands would re-fit every xterm again, and on
          // a live tile that is a second re-`stty`.
          <Suspense
            fallback={
              <aside className="w-full max-w-md shrink-0 rounded border p-3 text-xs text-muted-foreground">
                Loading details…
              </aside>
            }
          >
            <DetailDrawer
              terminal={drawer}
              feed={modes[drawer.id] ?? 'snapshot'}
              elided={elided[drawer.id] ?? 0}
              onClose={() => setDrawerId(null)}
              className="shrink-0"
            />
          </Suspense>
        )}
      </div>

      {/* What the wall costs and how stale it is. It owns its own one-second clock so the wall is not
          re-rendered to advance a number — see `chrome/StatusBar.tsx`. */}
      <StatusBar stats={wallStats} phase={phase} />

      {notices.length > 0 && (
        <ul className="max-h-24 overflow-y-auto text-xs text-muted-foreground" data-testid="notices">
          {notices.map((line, i) => (
            <li key={`${i}-${line}`}>{line}</li>
          ))}
        </ul>
      )}
    </div>
  );
}
