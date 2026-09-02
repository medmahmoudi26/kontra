/**
 * One Terminal, drawn (ADR 0020, slices 2 and 7a).
 *
 * A tile is a screen of one window of one node's tmux session. It paints from snapshots by default and
 * from a live PTY attach while it is focused, and it is the thing that decides how big the remote pane
 * should be: `addon-fit` measures the tile, and those cols/rows are what the streamer puts into `stty`
 * before `tmux attach`. That is also why a Terminal must not live inside a transformed or zoomable
 * canvas — a CSS scale would make the measurement a lie and the remote pane the wrong shape.
 *
 * YOU CANNOT TYPE IN A TERMINAL. There is no `onData` handler, no key handler, and no message on the
 * wire that carries bytes toward a node. ADR 0020's finding (3) measured `run-shell` and a keystroke
 * injection both executing through a *read-only* tmux client with no error, so read-only is a property
 * of this code and the streamer's message table, never of tmux. `disableStdin` is xterm's own guard on
 * top of that, and slice 7a closed the last code path that even assembled bytes: xterm's wheel handler
 * builds cursor keys for a terminal with no scrollback, so `chrome/useTileScroll.ts` takes xterm's whole
 * wheel path off the table with `attachCustomWheelEventHandler`. See that file's header.
 *
 * GOING LIVE IS AN EXPLICIT ACT, never a click or a stray tab-stop. A focus opens an `sshd` session, a
 * PTY and a per-viewer tmux session on a 2 GB Machine, so it takes a button that says so. The browser
 * suite pins the other half of that rule — clicking a tile and typing into it puts NOTHING on the socket
 * — and an implicit focus-on-click would break it while also attaching to a Machine every time an
 * operator clicked to read something.
 *
 * THE DOM RENDERER, NOT WEBGL. Browsers cap WebGL contexts near 16 and silently drop the oldest when a
 * wall exceeds it, so a 24-tile Dashboard of WebGL terminals loses tiles for reasons that look like a
 * broken stream. xterm 5.5's default renderer is the DOM one; this file's job is not to load
 * `addon-webgl`, and the reason is written here so nobody adds it for a frame rate a journal follower
 * will never need.
 *
 * A TERMINAL IS NOT A RECORD. Snapshots are a screen and a live attach is lossy by construction; the
 * Manifest, the journal on the node and the lake are the record.
 */

import { lazy, memo, Suspense, useCallback, useEffect, useRef, useState } from 'react';
import { Terminal as XTerm, type ITheme } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { Button } from '@/components/ui/button';
import HealthChips from './HealthChips';
import TileHeader from './chrome/TileHeader';
import StatusLine from './chrome/StatusLine';
import { copyText, visibleText } from './chrome/copy';
import { tileRefFor } from './chrome/tileRef';
import { useTileScroll } from './chrome/useTileScroll';
import { TERMINAL_FONT_STACK, whenTerminalFontReady } from './theme';
import { parseWidgetFrame, type Widget } from './widgets/parseWidget';
import type { Terminal, TerminalHealth } from './panelsClient';

/**
 * How many lines this pane's own scrollback ring keeps — xterm's `scrollback` option, named.
 *
 * TMUX'S SCROLLBACK IS UNREACHABLE FROM A READ-ONLY PANE. tmux keeps history in copy-mode, which is
 * a KEYBOARD feature — entered with `C-b [`, scrolled with the arrow keys. A read-only browser pane
 * forwards no keystrokes (that is the whole guarantee), so "scroll back through what a Worker
 * printed" cannot be tmux's job here. It is the frontend's: the xterm below holds a ring of the
 * stream, capped at this. Exported because the detail drawer STATES the cap to the operator, and a
 * `2000` written twice is how the sentence and the buffer come to disagree.
 *
 * WHAT SURVIVES A RECONNECT is a real design item, not something tmux answers for us. A snapshot
 * pane's ring is thrown away on every repaint (`applyFrame`'s `reset()` — that discard is exactly
 * what makes a snapshot idempotent), so on a snapshot pane the ring is only ever the last screen
 * plus the backlog a `capture-pane -S` carries. A live attach accumulates up to the cap. Either way
 * a dropped-and-reattached socket reseeds from a fresh screen and the browser holds no bytes across
 * the gap — which is the same property that lets the reconnect promise never to duplicate output.
 *
 * It was `panels/scrollback.ts`, whose one other export was the drawer's sentence about all this.
 * The constant belongs to the component that passes it to xterm; the sentence belongs to the one
 * that says it, and neither needed a file of its own once a test could render either.
 */
export const SCROLLBACK_LINES = 2_000;

/**
 * SLICE 7b, MOUNT POINT 3 OF 3 — pane content as a widget.
 *
 * LAZY for the same measured reason `DashboardPage` lazies the drawer: `WidgetView` renders through
 * `widgets/Markdown`, which pulls `react-markdown` (and, behind its own `import()`, mermaid). A wall
 * of journal followers must not carry a markdown pipeline in the chunk that draws it. `parseWidget`
 * is imported eagerly on purpose — it is pure string work with no React and no dependencies, and it
 * is what decides whether the lazy chunk is ever needed.
 */
const WidgetView = lazy(() => import('./widgets/WidgetView'));

export type TileMode = 'snapshot' | 'live' | 'error';

/**
 * Home + clear, the prefix the streamer puts in front of a full screen.
 *
 * It is the ONLY thing that distinguishes a repaint from an append on this wire, and it has to be:
 * the snapshot path sends a whole screen every few seconds and a live attach sends increments, over
 * one binary frame type.
 */
export const CLEAR_HOME = '\x1b[H\x1b[2J';

const CLEAR_HOME_BYTES = Uint8Array.from(CLEAR_HOME, (c) => c.charCodeAt(0));

/**
 * The narrowest and widest a tile may scale its type to make the source pane fit.
 *
 * The floor is where scaling stops helping: below about 5px the glyphs stop resolving and a wrapped
 * line you can read beats an unwrapped one you cannot. The ceiling stops a narrow pane — an 80-column
 * `claude` window in a wide tile — from being blown up to a size that reads as a mistake.
 */
const MIN_FIT_FONT_PX = 5;
const MAX_FIT_FONT_PX = 22;

/**
 * How many times the fit may iterate, and how finely it may step at the end.
 *
 * ONE DIVISION IS NOT ENOUGH, which is only obvious once measured. Cell width is not linear in font
 * size — xterm rounds a cell to whole pixels — so `font * cols / sourceCols` lands NEAR the target
 * and not on it, and near is not good enough when one column short means every long line wraps.
 * Measured on this box before the loop existed: a 200-column pane in a 666px tile settled at 162
 * columns, still wrapping by 38.
 *
 * So it converges instead: a few proportional passes to get close, then quarter-point steps down to
 * clear any remaining shortfall. Both are bounded — each pass costs a reflow, and this runs on a
 * settled resize.
 */
const FIT_PASSES = 4;
const FIT_TRIM_STEPS = 12;
const FIT_TRIM_PX = 0.25;

/**
 * Scale the type so the SOURCE pane's full width lands in the tile — no wrapping, no dead column.
 *
 * THE PROBLEM THIS SOLVES, in the operator's words: "there is still empty space, the text must
 * automatically fill it. There seems to be some kind of asymmetry in the dimensions."
 *
 * A snapshot tile shows what `capture-pane` drew at the FAR END's geometry, and the two ends did not
 * agree. Panes on this box right now are 80x25, 115x44 and 200x50, while a default wall tile fits
 * about 110 columns at 12px — so the same wall had a pane wrapping every long line (200 into 110)
 * and another leaving a third of its tile blank (80 in 110), which is the asymmetry. `addon-fit`
 * cannot fix it: it sizes the GRID to the box, which is the right answer only when the grid is also
 * what the far end is drawing at.
 *
 * The far end's width is therefore the fixed quantity and the type scales to it. `fit()` has just
 * run, so `term.cols` is how many columns this box holds at the requested size — the ratio to
 * `sourceCols` is exactly the factor the font needs, with no cell-metric arithmetic and no
 * dependency on the font's aspect ratio.
 *
 * NOT IN LIVE MODE. A live attach is `stty`d to the tile's own measurement, so the source grid IS
 * the tile's grid and there is nothing to reconcile; scaling there would fight the re-attach.
 *
 * ROWS ARE LEFT ALONE. Fitting height as well would mean choosing between honouring the width and
 * honouring the height, and a terminal that shows its last N lines is what a terminal has always
 * been — vertical overflow scrolls, horizontal overflow mangles.
 */
export function fitSourceWidth(
  term: { cols: number; options: { fontSize?: number } },
  fit: { fit(): void },
  sourceCols: number,
  mode: TileMode
): void {
  if (mode === 'live' || sourceCols <= 0 || !term.cols) return;
  const requested = term.options.fontSize ?? 0;
  if (requested <= 0) return;

  const clamp = (px: number): number =>
    Math.min(MAX_FIT_FONT_PX, Math.max(MIN_FIT_FONT_PX, px));
  const apply = (px: number): void => {
    term.options.fontSize = px;
    fit.fit();
  };

  let font = requested;
  // Proportional passes. `term.cols / sourceCols` is the factor the type needs — the box is fixed,
  // so columns vary inversely with size — and repeating it walks out the rounding.
  for (let pass = 0; pass < FIT_PASSES && term.cols !== sourceCols; pass += 1) {
    const next = clamp((font * term.cols) / sourceCols);
    // Converged, clamped, or moving by less than a viewer could see.
    if (Math.abs(next - font) < 0.1) break;
    font = next;
    apply(font);
  }

  // The proportional passes can still settle a column or two SHORT, and short means wrapping — the
  // one outcome this function exists to prevent. Trim until it does not, or until the floor does.
  for (let step = 0; step < FIT_TRIM_STEPS && term.cols < sourceCols; step += 1) {
    if (font <= MIN_FIT_FONT_PX) break;
    font = Math.max(MIN_FIT_FONT_PX, font - FIT_TRIM_PX);
    apply(font);
  }

  // Nothing moved: put the requested size back rather than leaving a rounding artefact behind.
  if (Math.abs(font - requested) < 0.1 && term.options.fontSize !== requested) apply(requested);
}

/**
 * How long a tile's box must be still before it re-measures. THIS ONE HAS A REMOTE COST.
 *
 * MEASURED PROBLEM, not a micro-optimisation. `grid/TileWall.tsx` re-lays a tile out LIVE during a
 * resize drag (its document write is on pointer-up, but the geometry follows the pointer), so the
 * `ResizeObserver` below fires on every pointer move. Unthrottled that is a `fit()` per move — and for a
 * LIVE tile, `onFocus` per move, which is the streamer tearing down and re-opening an `sshd` session, a
 * PTY and a grouped tmux session on an `s-1vcpu-2gb` Machine sixty times a second. The same storm arrives
 * from a font-size change, a density change and the sidebar being toggled.
 *
 * So a measurement waits for the box to be QUIET. 120 ms is under the ~200 ms at which an operator starts
 * to feel a control lag, and comfortably longer than one animation frame, so a drag produces exactly one
 * measurement — at the end, which is also the only geometry that was ever real.
 *
 * The FIRST measurement is not debounced (see `remeasure(immediate)`): a tile that has not measured itself
 * has not subscribed, so delaying that one would delay the tile's first frame for no benefit.
 */
export const SETTLE_MS = 120;

/** Test hooks are part of the contract (CONTRACT.md amendments 12 and 14), not a convenience: a
 * Playwright spec must not select on Tailwind classes, or restyling a tile would read as a broken
 * stream. The `chip-<signal>` hooks belong to {@link HealthChips}, which owns them; `focus-<id>` and the
 * tile menu's hooks belong to `chrome/TileHeader.tsx`, which renders them. */
export const testid = {
  tile: (id: string) => `terminal-${id}`,
  empty: (id: string) => `tile-empty-${id}`,
  /** The pane's process is over while its session is still present — a different tile state from
   *  `empty`, and a different thing for an operator to do about it. */
  exited: (id: string) => `tile-exited-${id}`,
  focus: (id: string) => `focus-${id}`,
  converge: (id: string) => `converge-${id}`,
} as const;

/**
 * KNOWN OPEN BUG, and what is established about it — do not "fix" this by guessing.
 *
 * Disposing an xterm instance from an effect cleanup throws, asynchronously and out of xterm's own
 * code: `TypeError: Cannot read properties of undefined (reading 'dimensions')` from
 * `Viewport.syncScrollArea` reading a `RenderService` that has already gone. `main.tsx` renders under
 * `React.StrictMode`, so in dev every mount is mount → cleanup → mount and hits it every time;
 * measured DEV-ONLY (a `vite build` served by `vite preview` mounts with zero page errors). The wall
 * hits the same race for real, on every tile that leaves it.
 *
 * What is established, so the next attempt starts further along:
 *   - the trigger is in `Terminal.open()`, which ends with `setTimeout(() => this.syncScrollArea())`
 *     — so the pending work is a TIMER, not only an animation frame;
 *   - deferring the dispose by one animation frame does NOT clear it (measured: the browser suite's
 *     page error still fired, 8/8 otherwise green);
 *   - a `setTimeout`-then-frame deferral is UNVERIFIED.
 *
 * Left as the plain synchronous dispose deliberately: that is the state the browser suite is green
 * against, with the throw exempted by exactly one narrow pattern (`KNOWN_MOUNT_RACE` in
 * `e2e/fixtures.ts`). Slice 7a's leave animation was built as a GHOST (`chrome/useTileGhosts.ts`)
 * specifically so it would not need to hold a disposing terminal open for the length of an animation —
 * see that file.
 */

/** Is this payload a whole screen? */
export function isRepaint(payload: Uint8Array): boolean {
  if (payload.length < CLEAR_HOME_BYTES.length) return false;
  for (let i = 0; i < CLEAR_HOME_BYTES.length; i += 1) {
    if (payload[i] !== CLEAR_HOME_BYTES[i]) return false;
  }
  return true;
}

/**
 * A cheap fingerprint of one screen — FNV-1a, 32-bit.
 *
 * It answers ONE question: is this repaint the same picture as the last one? A snapshot arrives every
 * few seconds whether or not the pane printed anything, so without this a tile could not tell "the
 * Worker is quiet" from "the Worker is busy", and the status line would have to imply one of them.
 *
 * Whole-payload rather than sampled: a 200×50 screen is a few KiB and a wall repaints a few times a
 * second, which is nothing next to writing those same bytes into xterm. A collision costs a late
 * `quiet`, never a wrong `exited`.
 */
export function screenHash(payload: Uint8Array): number {
  let hash = 0x811c9dc5;
  for (let i = 0; i < payload.length; i += 1) {
    hash ^= payload[i] as number;
    // `Math.imul` because the FNV prime overflows 32 bits in float arithmetic, and `>>> 0` keeps it
    // an unsigned int so two runs over identical bytes are identical numbers.
    hash = Math.imul(hash, 0x01000193) >>> 0;
  }
  return hash;
}

/** The part of xterm a frame needs. An interface so the rule below is testable without a DOM. */
export interface TerminalSink {
  reset(): void;
  write(data: Uint8Array): void;
}

/**
 * Write one frame: a snapshot REPLACES the tile, a live frame appends to it.
 *
 * The reset is the mechanism, not belt-and-braces. Two things would otherwise accumulate: a repaint's
 * outgoing screen scrolls into xterm's scrollback, so a tile repainting every three seconds fills
 * 2000 lines of history with stale screens an operator can scroll back through; and a live attach's
 * first bytes were measured to be `\x1b[?1049h` (alternate screen) followed by `\x1b[1;50r` (a
 * 50-row scroll region), so a stream that ends leaves the terminal in a mode a later snapshot would
 * paint into sideways. `reset()` is the only primitive that puts the buffer, the scrollback, the
 * cursor, the SGR state, the alternate-buffer flag and the scroll region back to a known state —
 * which is what makes a repaint idempotent whatever the previous stream left behind.
 *
 * It is also why the wheel proxy has nothing to scroll on a snapshot tile: `reset()` throws the
 * scrollback away every few seconds, so scrollback is a property of a LIVE tile. `chrome/scroll.ts`
 * states the consequence — a snapshot tile absorbs no wheel and the wall scrolls instead.
 */
export function applyFrame(sink: TerminalSink, payload: Uint8Array): 'repaint' | 'append' {
  if (isRepaint(payload)) {
    sink.reset();
    sink.write(payload);
    return 'repaint';
  }
  sink.write(payload);
  return 'append';
}

/**
 * Should this frame be HELD because somebody is reading back through the tile?
 *
 * THE BUG THIS ENDS. A snapshot carries 50 lines above the visible screen (`SNAPSHOT_LINES`), so a
 * snapshot tile has scrollback and can be scrolled — and then, three seconds later, the next
 * snapshot arrives and {@link applyFrame} calls `reset()`, which is documented above as throwing the
 * scrollback away on purpose. The viewport lands back at the tail. Reading anything older than the
 * visible screen was therefore impossible: the wall pulled you to the bottom faster than you could
 * read a line.
 *
 * ONLY A REPAINT IS HELD, and only while the tile is actually scrolled back. A live frame is an
 * APPEND, and xterm already leaves a scrolled-up viewport where it is when output arrives — so
 * holding those would stall a live attach to fix a problem it does not have. A tile at the tail
 * (`scrolledBack === 0`) is every tile on a wall nobody is touching, and it repaints as before.
 *
 * HELD, NOT DROPPED-AND-FORGOTTEN. The next snapshot after the operator returns to the tail is a
 * whole screen — that is what a snapshot IS — so the tile catches up on its own within one cadence.
 * Nothing has to be buffered, which is the property that makes this safe on a wall of sixty tiles.
 */
export function holdsRepaint(payload: Uint8Array, scrolledBack: number): boolean {
  return scrolledBack > 0 && isRepaint(payload);
}

export interface TerminalTileProps {
  terminal: Terminal;
  /** What the streamer last said about this Terminal, from `{t:'state', mode}`. */
  mode: TileMode;
  /**
   * Register for this Terminal's bytes; the returned function unregisters.
   *
   * A subscription rather than a `frames` prop on purpose: a live journal delivers up to fifteen
   * frames a second, and routing those through React state would re-render the page at that rate to
   * hand bytes to an imperative canvas anyway.
   */
  subscribe(id: string, write: (payload: Uint8Array) => void): () => void;
  /** What addon-fit measured, whenever it changes. The page reports it to the streamer, which is what
   * stops a tile from asking a node for a zero-sized screen. */
  onMeasure?(id: string, cols: number, rows: number): void;
  /** Ask for a live attach at the measured size. */
  onFocus(id: string, cols: number, rows: number): void;
  /** Ask to go back to snapshots — which is also what kills the grouped session on the node. */
  onBlur(id: string): void;
  /** Offered on a tile whose session does not exist, and from the tile menu. */
  onConverge?(id: string): void;
  /**
   * Take this pane off the wall until it is put back.
   *
   * ONE WINDOW, WHICH IS WHAT A TILE IS. The wall's grain is `<mode>:<node>/<session>/<window>` and
   * this control belongs to a tile, so it hides a WINDOW and not a session or a Machine — the two
   * windows of one `kontra` session are an actor and its handler, and hiding both to hide one would
   * be a control that does more than it says.
   *
   * The page owns what hiding MEANS (unsubscribing, persisting, and the "N hidden" bar that puts it
   * back); the tile only names the id. Absent off the wall.
   */
  onHide?(id: string): void;
  /**
   * Zoom this pane to fill the wall, or restore it (slice 05). Absent off the wall — the Workflows
   * page embeds one pane and there is nothing to zoom it over.
   *
   * tmux's `C-b z`, as a click: read-only means no active pane to switch focus between, so zoom is
   * ordinary page state and the operator never learns the shortcut. The handle only DECIDES which pane;
   * the page lays the zoomed one out at real size, because a CSS scale would make addon-fit's geometry a
   * lie about the remote pane (ADR 0020 decision 12).
   */
  onZoom?(id: string): void;
  /** True when THIS tile is the zoomed one, so its control reads "restore" rather than "zoom". */
  zoomed?: boolean;
  /**
   * The page's socket is down and reconnecting (slice 05), so the screen below is FROZEN at its last
   * frame. Said over the screen rather than left silent: a frozen tile and a Machine that stopped
   * printing are indistinguishable otherwise, which is the confusion ADR 0020's badges exist to end.
   */
  stale?: boolean;
  /** Bytes this tile was told were dropped by the byte cap. Shown, never swallowed. */
  elided?: number;
  /** The resolved terminal palette (`theme.ts`). Changing it re-themes in place — xterm's `options.theme`
   * is a live setter, so a palette change must NOT remount the terminal and drop its screen. */
  theme: ITheme;
  /** Font size in px. Changes cell metrics, hence cols/rows, hence a live tile's `stty` — see
   * {@link SETTLE_MS}. */
  fontSize: number;
  /** True when the resolved palette is a dark one: the scrims and badges over the screen need to know,
   * and asking the palette is more honest than asking the app. */
  paletteIsDark: boolean;
  /** A tile too NARROW for the banner in full — it drops what it can and keeps every control. */
  compact?: boolean;
  /**
   * A tile too SHORT to spend rows on prose. `TileWall` still computes and passes it — it is a
   * property of the RECTANGLE, not of what happens to be drawn in it — but nothing here reads it any
   * more: its only consumer was the health chip row, which is gone. Kept rather than ripped out of
   * `TileWall` and `DashboardPage` because the next thing that needs to know a tile is short will
   * want exactly this, and because removing a prop three files up to delete one unused argument is a
   * worse trade than saying so here.
   */
  dense?: boolean;
  /**
   * Draw the SCREEN and nothing else — no banner, no health chips.
   *
   * For a surface that already carries the identity and the controls itself. The Workflows page's
   * `WorkerPane` has its own header with the session name and the link to the Monitor, so the
   * tile's banner would be a second one saying the same thing, and its four health signals are
   * about a Machine — `ssh: reachable` on a worker running on this host is noise beside a page that
   * already reports whether the queue is being polled.
   *
   * NOT A DENSITY. `compact` and `dense` drop what a narrow tile cannot afford; this drops what the
   * EMBEDDER has already said, which is a different decision and would be wrong to infer from size.
   */
  bare?: boolean;
  /** Told when a copy succeeds or fails, and when the drawer is asked for. The page owns the notices, so
   * a tile never renders a toast of its own. */
  onNotice?(line: string): void;
  /** The per-Terminal detail drawer. Absent on a surface that has no room for one (the Workflows
   * page embeds a tile), and the menu item is then disabled with a sentence saying so. */
  onOpenDrawer?(id: string): void;
  /**
   * Pointer-down on the BANNER starts a wall drag.
   *
   * Threaded through the tile rather than drawn by the wall, because the alternative is a drag strip
   * above the banner — a fifth row of chrome on every tile, on a wall where `grid/wall.ts` records
   * that a tile has about five rows of terminal to begin with. Absent off the wall.
   */
  onDragHandle?(e: React.PointerEvent<HTMLElement>): void;
}

/** A session that is anything but `present` has no live screen behind it, and the tile says so over
 * whatever it last painted — that screen is now stale. */
export function sessionGone(health: TerminalHealth): boolean {
  return health.session !== 'present' && health.session !== 'unknown';
}

/**
 * The pane's process is over, while the session it lives in is still there.
 *
 * A SEPARATE PREDICATE FROM {@link sessionGone}, and the separation is the feature. They are the two
 * halves of "this tile is not going to change again", they have different causes, different actions
 * and different chrome — a scrim for a session that is gone, a badge for a Worker that finished —
 * and folding them together would put a "converge the session" button in front of an operator whose
 * session is present and whose Worker exited with 143.
 */
export function processExited(health: TerminalHealth): boolean {
  return health.process === 'exited';
}

export default memo(function TerminalTile({
  terminal,
  mode,
  subscribe,
  onMeasure,
  onFocus,
  onBlur,
  onConverge,
  onHide,
  onZoom,
  zoomed = false,
  stale = false,
  elided = 0,
  theme,
  fontSize,
  paletteIsDark,
  compact = false,
  bare = false,
  onNotice,
  onOpenDrawer,
  onDragHandle,
}: TerminalTileProps): JSX.Element {
  const id = terminal.id;
  const wrapperRef = useRef<HTMLDivElement | null>(null);
  const mountRef = useRef<HTMLDivElement | null>(null);
  const termRef = useRef<XTerm | null>(null);
  /** Bytes received since the banner's sparkline last drained this. A ref, never state — see the
   *  byte path below and `chrome/PaneThroughput.tsx`. */
  const bytesRef = useRef(0);
  /**
   * WHEN a frame last arrived, and when the screen it paints last CHANGED.
   *
   * TWO CLOCKS BECAUSE THEY ARE TWO FACTS, and the status line acts on them differently. A snapshot
   * arrives every few seconds whether or not the pane printed anything — `capture-pane` always
   * returns a screen — so a growing FRAME age means the feed has stalled, while a growing CHANGE age
   * means the Worker is running and silent. Collapsing them would report a healthy quiet Worker as a
   * broken tile, which is the same class of lie in the other direction.
   *
   * REFS, not state, for the reason `DashboardPage` gives for its own arrival map: a live journal
   * delivers up to fifteen frames a second and this must not re-render the tile. `chrome/StatusLine`
   * owns the once-a-second clock that reads them.
   */
  const lastFrameRef = useRef<number | null>(null);
  const lastChangeRef = useRef<number | null>(null);
  /** A cheap hash of the last repainted SCREEN, so an identical repaint does not count as a change.
   *  FNV-1a over the payload: one pass, no allocation, and collisions cost at most a late "quiet". */
  const screenHashRef = useRef<number | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const [size, setSize] = useState<{ cols: number; rows: number }>({ cols: 0, rows: 0 });
  /** How far back the wheel proxy has scrolled this tile, in lines. */
  const [scrolledBack, setScrolledBack] = useState(0);
  /**
   * The same number, readable from the BYTE PATH.
   *
   * The frame handler is registered once per Terminal (re-registering it per state change would
   * drop frames mid-stream), so it cannot close over `scrolledBack` — it has to read what is true
   * when a frame arrives. This is the ref half of that pair, written by the same reporter.
   */
  const scrolledBackRef = useRef(0);
  /** True while a repaint is being held for a reader — the tile is showing OLD output ON PURPOSE,
   *  which the header has to say or it is indistinguishable from a Machine that stopped printing. */
  const [held, setHeld] = useState(false);
  /** The document this pane is currently announcing, if it is announcing one. Set only from a
   * repaint; see the byte path. */
  const [widget, setWidget] = useState<Widget | null>(null);

  // Latest-value refs, so the effects below can act on what is true WHEN THEY FIRE rather than on
  // what was true when they were registered — and so a new callback identity does not tear down an
  // xterm instance and its stream.
  const modeRef = useRef<TileMode>(mode);
  modeRef.current = mode;
  const onBlurRef = useRef(onBlur);
  onBlurRef.current = onBlur;
  const onFocusRef = useRef(onFocus);
  onFocusRef.current = onFocus;
  const onMeasureRef = useRef(onMeasure);
  onMeasureRef.current = onMeasure;
  const onNoticeRef = useRef(onNotice);
  onNoticeRef.current = onNotice;
  /** The SOURCE pane's width, in columns — what the far end is actually drawing at. Read by
   *  `measureNow`; a ref and not a dependency because `measureNow` feeds `remeasure`, which the
   *  xterm mount effect depends on, so making this a dependency would dispose and recreate the
   *  terminal every time a pane was resized on the node. */
  const paneColsRef = useRef(terminal.paneCols ?? 0);
  // Zero when the streamer has not reported a pane width yet, which `fitSourceWidth` reads as "no
  // source geometry to reconcile against" and leaves `addon-fit`'s answer alone.
  paneColsRef.current = terminal.paneCols ?? 0;
  /** The font size the VIEWER asked for. `measureNow` overwrites `term.options.fontSize` with a
   *  derived one, so it needs the requested value to derive from — reading it back off the terminal
   *  would compound the previous scaling on every measurement. */
  const fontSizeRef = useRef(fontSize);
  fontSizeRef.current = fontSize;

  /** The pending settle timer — see {@link SETTLE_MS}. */
  const settleRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  /**
   * Has this tile ever produced a NON-ZERO measurement?
   *
   * THE DEBOUNCE MUST NEVER DELAY THE FIRST ONE, and this ref is what guarantees it. A tile that has not
   * reported a size has not been subscribed — `DashboardPage.sync` waits for a real measurement, because a
   * subscription at 0×0 asks a node for a zero-sized screen — so until the first success every measurement
   * runs immediately and only later ones coalesce.
   *
   * MEASURED, and this is what it cost to find out: with every measurement debounced, a tile that mounted
   * before layout (0×0, so nothing reported) had its first real measurement deferred by {@link SETTLE_MS},
   * and each `ResizeObserver` callback arriving during layout and the font swap RESET that timer. On a
   * loaded box the browser suite's `repaints a snapshot rather than appending it` then found `.xterm-rows`
   * holding a single space for 25 seconds — a tile indistinguishable from a Machine that had stopped
   * printing, which is the exact failure `DashboardPage`'s `measured` comment already records a different
   * cause for. Coalescing is only ever worth doing to a tile that is already streaming.
   */
  const measuredOnceRef = useRef(false);

  /** Re-measure, and report the measurement upward. Zero is never reported: a tile mounted before
   * layout, or inside a hidden parent, measures 0×0 — and a remote pane sized 0×0 is a blank tile
   * with no error anywhere. */
  const measureNow = useCallback((): { cols: number; rows: number } | null => {
    const term = termRef.current;
    const fit = fitRef.current;
    if (!term || !fit) return null;
    try {
      // Always fit from the REQUESTED font, never from whatever the last pass derived — otherwise
      // each measurement scales the previous scaling and the text walks away to nothing.
      if (term.options.fontSize !== fontSizeRef.current) {
        term.options.fontSize = fontSizeRef.current;
      }
      fit.fit();
      fitSourceWidth(term, fit, paneColsRef.current, modeRef.current);
    } catch {
      /* a tile with no geometry yet has nothing to fit to */
    }
    if (!term.cols || !term.rows) return null;
    measuredOnceRef.current = true;
    const measured = { cols: term.cols, rows: term.rows };
    setSize((prev) =>
      prev.cols === measured.cols && prev.rows === measured.rows ? prev : measured
    );
    onMeasureRef.current?.(id, measured.cols, measured.rows);
    return measured;
  }, [id]);

  /**
   * Measure once the box has been still for {@link SETTLE_MS} — or right now, for the first one.
   *
   * `reask` is what makes a LIVE tile follow a resize: a live attach's size is fixed by `stty` before
   * the attach and cannot be changed without a channel to write on, which by design does not exist, so
   * a resized live tile has to ask for a new attach. The re-ask rides on the SETTLED measurement and
   * never on an intermediate one, which is the whole point of the debounce.
   */
  const remeasure = useCallback(
    (options: { immediate?: boolean; reask?: boolean } = {}): void => {
      if (settleRef.current !== null) {
        clearTimeout(settleRef.current);
        settleRef.current = null;
      }
      const run = (): void => {
        const measured = measureNow();
        if (options.reask && measured && modeRef.current === 'live') {
          onFocusRef.current(id, measured.cols, measured.rows);
        }
      };
      // Immediately when asked, and ALWAYS while this tile has never measured itself — see
      // `measuredOnceRef`. Until that first success the tile is unsubscribed and blank, so there is nothing
      // to coalesce and everything to lose by waiting.
      if (options.immediate || !measuredOnceRef.current) {
        run();
        return;
      }
      settleRef.current = setTimeout(() => {
        settleRef.current = null;
        run();
      }, SETTLE_MS);
    },
    [id, measureNow]
  );

  // The xterm instance, created once per tile. Nothing writes to it except the subscription below.
  //
  // NEITHER `theme` NOR `fontSize` IS A DEPENDENCY, deliberately. Both are live setters on
  // `term.options` (applied by the effect further down), and putting them here would dispose and
  // recreate the terminal on a palette change — which clears the screen, drops the scrollback, and on a
  // live tile leaves a grouped session behind on the node. A theme change must be a repaint, not a
  // remount.
  useEffect(() => {
    if (!mountRef.current || termRef.current) return;
    const term = new XTerm({
      convertEol: true,
      cursorBlink: false,
      // Read-only, on top of there being no path for bytes to leave this page at all.
      disableStdin: true,
      // The frontend's OWN scrollback ring, capped (slice 05). tmux copy-mode is a keyboard feature a
      // read-only pane cannot reach, so xterm holds the history and `scrollback.ts` names the cap in
      // one place so the drawer can state it.
      scrollback: SCROLLBACK_LINES,
      fontSize,
      // The Nerd Font, with the documented fallback chain behind it (`theme.ts`). Box-drawing and
      // powerline glyphs are most of what makes terminal output look right, and a `monospace` default
      // renders them as tofu.
      fontFamily: TERMINAL_FONT_STACK,
      theme,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(mountRef.current);
    // ONE OWNER FOR THE WHEEL. `chrome/useTileScroll.ts` explains this in full: returning false is
    // xterm's documented "I handled it", and it also makes unreachable the branch in xterm's own wheel
    // listener that assembles cursor keys for a scrollback-less terminal.
    term.attachCustomWheelEventHandler(() => false);
    termRef.current = term;
    fitRef.current = fit;
    remeasure({ immediate: true });

    // The Nerd Font arrives after the first paint (`font-display: swap`), and the swap changes the cell
    // size under a terminal that has already measured itself — which would leave the streamer having
    // `stty`d a pane to the FALLBACK font's geometry. One extra measurement when it lands is the fix.
    let disposed = false;
    void whenTerminalFontReady().then(() => {
      if (!disposed) remeasure({ reask: true });
    });

    const onWindowResize = (): void => remeasure({ reask: true });
    window.addEventListener('resize', onWindowResize);

    // A slot resized by DRAG never fires `window.resize`, so on a wall the tile would keep the
    // geometry it mounted with and every line would wrap wrongly against the pane's real width. This is
    // also the observer that fires per pointer move during a drag — see {@link SETTLE_MS} for why the
    // measurement it triggers is debounced and the live re-attach rides on the settled one.
    const observer =
      typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(() => onWindowResize());
    if (observer && mountRef.current) observer.observe(mountRef.current);

    return () => {
      disposed = true;
      if (settleRef.current !== null) clearTimeout(settleRef.current);
      observer?.disconnect();
      window.removeEventListener('resize', onWindowResize);
      // Cleared first, so nothing writes to a terminal that is on its way out.
      termRef.current = null;
      fitRef.current = null;
      // See the KNOWN OPEN BUG note above before changing this line.
      term.dispose();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id, remeasure]);

  // Palette and font size, applied IN PLACE. `options` is a live setter in xterm 5.5, so this repaints
  // rather than remounting; the font size then changes the cell metrics, so it re-measures — settled, and
  // re-asking for a live attach at the new geometry, because that is what `stty` was given.
  useEffect(() => {
    const term = termRef.current;
    if (!term) return;
    term.options.theme = theme;
    term.options.fontFamily = TERMINAL_FONT_STACK;
    if (term.options.fontSize !== fontSize) {
      term.options.fontSize = fontSize;
      remeasure({ reask: true });
    }
  }, [fontSize, remeasure, theme]);

  // The byte path.
  useEffect(
    () =>
      subscribe(id, (payload) => {
        // Counted BEFORE the early return, and into a ref rather than into state: this is what the
        // banner's sparkline drains once a second (see `chrome/PaneThroughput.tsx`), and a state
        // update here would re-render the tile — and reach into an xterm — per frame.
        bytesRef.current += payload.length;
        // …and so is the arrival, for the same reason and one more: a frame that is HELD below still
        // ARRIVED, and a status line that stopped ageing because somebody scrolled up would report a
        // working feed as a dead one.
        const at = Date.now();
        lastFrameRef.current = at;
        if (isRepaint(payload)) {
          // A snapshot repaints on a cadence whether or not anything changed. Only a DIFFERENT screen
          // is the pane printing something.
          const hash = screenHash(payload);
          if (screenHashRef.current !== hash) {
            screenHashRef.current = hash;
            lastChangeRef.current = at;
          }
        } else if (payload.length > 0) {
          // A live attach only sends bytes when the pane produces them — tmux's own status line was
          // the one thing that ticked on its own, and the attach turns it off (`panels/attach.ts`).
          lastChangeRef.current = at;
        }
        const term = termRef.current;
        if (!term) return;
        // SOMEBODY IS READING. A repaint resets the terminal, which is what makes a snapshot
        // idempotent and also what would throw away the scrollback under their cursor — so it waits
        // until they are back at the tail. The bytes are already counted above, so the throughput
        // chart keeps telling the truth about a Machine that is still printing.
        if (holdsRepaint(payload, scrolledBackRef.current)) {
          setHeld(true);
          return;
        }
        setHeld(false);
        const kind = applyFrame(
          { reset: () => term.reset(), write: (data) => term.write(data) },
          payload
        );
        // A widget is a WHOLE SCREEN that announces itself, so only a repaint can start or end one:
        // a producer writing `__KONTRA_WIDGET__:markdown` into its pane means "render this document",
        // and the next screen without the marker means it has gone back to being a terminal.
        //
        // An `append` deliberately clears it. Appends only arrive from a live attach, which streams
        // increments of a screen this tile is no longer painting in full — a rich document cannot be
        // maintained from fragments, and showing a stale one over a live stream would be a lie about
        // what the pane currently holds. The bytes still reached xterm above, so going live on a
        // widget pane shows the terminal, which is the honest fallback.
        if (kind === 'repaint') setWidget(parseWidgetFrame(payload));
        else setWidget((prev) => (prev === null ? prev : null));
      }),
    [id, subscribe]
  );

  // A tile that goes away while live must not leave a grouped session behind on the node. The
  // streamer also kills it when the socket drops, but a tile can be unmounted (a re-layout, a
  // filter, the grid) without the socket going anywhere.
  useEffect(
    () => () => {
      if (modeRef.current === 'live') onBlurRef.current(id);
    },
    [id]
  );

  // The wheel proxy and touch scrolling. Its listeners live on the WRAPPER — a non-scrolling element,
  // so the browser has nowhere to put a delta before the handler sees it.
  const scroll = useTileScroll({
    wrapperRef,
    termRef,
    onScrollback: (lines) => {
      scrolledBackRef.current = lines;
      setScrolledBack(lines);
      // Back at the tail: the hold is over the moment the reader returns, rather than one cadence
      // later when the next snapshot happens to arrive. Otherwise the badge would still say "held"
      // over a tile that is already live again.
      if (lines === 0) setHeld(false);
    },
  });

  const goLive = useCallback(() => {
    // Measured immediately rather than settled: an operator who just clicked "Go live" is waiting, and
    // the geometry they are looking at is the one to attach at.
    const term = termRef.current;
    measureNow();
    onFocus(id, term?.cols ?? 0, term?.rows ?? 0);
  }, [id, measureNow, onFocus]);

  const copyVisible = useCallback((): void => {
    const term = termRef.current;
    if (!term) return;
    const text = visibleText(term.buffer.active, term.rows);
    void copyText(text).then((ok) => {
      onNoticeRef.current?.(
        ok
          ? `${id}: copied ${text.split('\n').length} visible line(s)` +
              (elided > 0 ? ` — note ${elided} byte(s) were elided by the byte cap` : '')
          : `${id}: could not copy — this browser blocked the clipboard (a plain-HTTP origin has no ` +
              'secure clipboard, and the legacy path was refused too)'
      );
    });
  }, [elided, id]);

  /** What the status line reads once a second. A stable getter over the two refs above, so the bar's
   *  clock never becomes a reason to re-render this tile. */
  const frames = useCallback(
    () => ({ lastFrameAt: lastFrameRef.current, lastChangeAt: lastChangeRef.current }),
    []
  );

  const live = mode === 'live';
  const gone = sessionGone(terminal.health);
  /** The pane's process is over. A DIFFERENT state from `gone`, and it gets different chrome: the
   *  session is still there and the last screen is the most valuable thing on the tile (it holds the
   *  traceback), so this is a badge over the corner rather than a scrim over the evidence. */
  const exited = processExited(terminal.health);
  const ref_ = tileRefFor(terminal);
  /** Scrims and badges over the screen have to contrast with the PALETTE, not with the app. */
  const scrim = paletteIsDark ? 'bg-black/70 text-amber-300' : 'bg-white/80 text-amber-800';

  return (
    <>
      {/*
        WHO this tile is, WHERE it is, WHAT it is doing, and the menu — two rows, because
        `grid/wall.ts`'s row height was measured against the chrome a tile carries and every pixel
        here is a line of output an operator cannot see. It replaced slice 2's overlay controls,
        which cost no height and sat on top of the first thing a `journalctl` line printed.

        It is also the wall's grab point (`onDragHandle`), so the hostname an operator reads and the
        strip they drag are the same strip.

        `bare` drops it, for a surface that already carries the identity and the controls — see the
        prop. A second banner saying the same thing costs the rows of output it was added to save.
      */}
      {!bare && (
      <TileHeader
        id={id}
        ref_={ref_}
        // The inventory's hostname, falling back to the node segment of the id: a `local` Machine
        // reports both as the same string, and a malformed id still has a usable node.
        host={terminal.host || terminal.machine || ref_.node}
        live={live}
        scrolledBack={scrolledBack}
        held={held}
        elided={elided}
        compact={compact}
        bytes={bytesRef}
        onGoLive={goLive}
        onStopLive={() => onBlur(id)}
        onCopy={copyVisible}
        onJumpToTail={scroll.toBottom}
        zoomed={zoomed}
        {...(onZoom ? { onZoom: () => onZoom(id) } : {})}
        {...(onConverge ? { onConverge: () => onConverge(id) } : {})}
        {...(onOpenDrawer ? { onOpenDrawer: () => onOpenDrawer(id) } : {})}
        {...(onHide ? { onHide: () => onHide(id) } : {})}
        {...(onDragHandle ? { onDragHandle } : {})}
      />
      )}

      {/*
        The four signals are slice 3's component, HOSTED here rather than reimplemented: it owns the
        `chip-<signal>` hooks, the `ok|bad|unknown` tri-state and the four channels of visual
        difference that keep `unknown` from reading as `ok`. The per-tile suffix is what a wall makes
        necessary — four tiles with an unsuffixed `chip-session` are four elements matching one
        strict-mode selector.
      */}
      {/*
        At `wall` density the health block is CLIPPED to one row rather than edited: four full-text
        signals wrap to four lines, which in a 200-pixel tile is more label than terminal. The text stays
        in the DOM and in each chip's `title`, so nothing that reads it — an operator hovering, or a spec
        — loses the reading. Applied from here (the tile knows its density) instead of from the page,
        which used to reach in with a `:first-child` selector that the header row has now displaced.
      */}
      {/*
        THE CHIP ROW IS GONE FROM THE TILE, on the operator's instruction, after three rounds of
        trying to make it quiet enough.

        It cost a row on every tile to say `session: present`, `process: unknown` and
        `n/a: reachable, loads` — three restatements of what a healthy local pane always is — and the
        one chip that ever carried news, `poller`, was for a while simply WRONG on this host: a local
        node calls itself `localhost` while its Worker identifies by `os.hostname()`, so a polling
        handler rendered as a red NONE (fixed in `pollers.ts`, but the row had already earned its
        reputation).

        NOTHING IS LOST, and that is the condition on removing it. ADR 0020's rule is that the signals
        stay independent and that `unknown` never renders as healthy — not that they must all be on
        screen at all times. They remain in two places that were built for them: the status line at
        the FOOT of this tile names the leading finding in the same words (`HealthChips.leadingFinding`
        is the one derivation both use), and the detail drawer still lists every signal with its own
        sentence. A tile with nothing wrong now says nothing; a tile with something wrong says the one
        thing that is wrong, where tmux puts its own status.
      */}

      {/*
        The tile. `data-cols`/`data-rows` carry what addon-fit measured, because a terminal that
        mounted at 0×0 renders nothing while every type checks; `data-mode` is what the streamer last
        said this Terminal is. The xterm mount is a CHILD: xterm owns its container's contents, so the
        empty-state marker has to be its sibling.

        This element is ALSO the wheel proxy's wrapper, and it must not be scrollable itself — if it
        could scroll, the browser would have somewhere to put a delta before the handler decided who
        should move (`chrome/useTileScroll.ts`).
      */}
      <div
        ref={wrapperRef}
        data-testid={testid.tile(id)}
        data-cols={size.cols}
        data-rows={size.rows}
        data-mode={mode}
        data-scrolled-back={scrolledBack > 0 ? scrolledBack : undefined}
        // No border of its own: the wall's tile container owns the frame, and a second one inside it
        // would draw a doubled edge on every tile. `m-1 mt-0` keeps the screen off the frame without
        // costing the row of padding a nested border would.
        className="relative m-1 mt-0 min-h-0 flex-1 overflow-hidden rounded"
        style={{ background: theme.background ?? '#0b0b0e' }}
      >
        <div ref={mountRef} className="h-full w-full" />

        {stale && !gone && (
          // Slice 05, criterion 4 in the socket-drop sense: the stream dropped, so the screen under
          // this badge is frozen at its last frame. A corner badge rather than a full scrim — the last
          // screen is still the most useful thing to show while the page reconnects — but never silence.
          <div
            data-testid={`tile-stale-${id}`}
            title="the panel stream dropped; this screen is frozen at its last frame until the page reconnects"
            className={`pointer-events-none absolute right-1 top-1 rounded px-1 text-[10px] leading-4 ${scrim}`}
          >
            stream dropped — reconnecting
          </div>
        )}

        {exited && !gone && (
          /*
            THE FOURTH THING A TILE CAN BE, and the one that used to look like the healthiest.
            `.scratch/instrument-panel/issues/05` asks that a pane which has gone away say so rather
            than freeze on its last frame — and only a missing SESSION was ever detected. A Worker
            that EXITED leaves the session present, the tile painting, and the four health chips
            green-ish, because `cli/tmux.go` holds the window open on purpose so the exit status
            stays readable.

            A BADGE, NOT A SCRIM. `gone` covers the screen because there is nothing behind it worth
            reading; here the screen is the Worker's last output — the traceback, the panic, the
            reason — and covering it to announce that it is final would throw away the only thing
            an operator opened this tile for.
          */
          <div
            data-testid={testid.exited(id)}
            data-exit-status={terminal.exitStatus || undefined}
            title={
              terminal.health.detail ??
              'the process in this pane has exited; the window is held open so its last output stays readable'
            }
            // Top LEFT: the stale badge owns the top right, and both can be true at once — a socket
            // that dropped while the Worker was already over.
            className={`pointer-events-none absolute left-1 top-1 rounded px-1 text-[10px] leading-4 ${
              paletteIsDark ? 'bg-black/70 text-red-300' : 'bg-white/85 text-red-700'
            }`}
          >
            {terminal.exitStatus ? `process exited ${terminal.exitStatus}` : 'process exited'} — final
            screen
          </div>
        )}

        {widget && (
          // OVER the mount, never instead of it: xterm keeps measuring and `data-cols`/`data-rows`
          // keep meaning something, so a document that stops announcing itself reveals a terminal
          // that is already the right shape. The fallback below shows the pane as text while the
          // markdown chunk loads — a widget tile is never a blank rectangle.
          <Suspense
            fallback={
              <div className="absolute inset-0 overflow-auto p-2 font-mono text-[11px] text-zinc-400">
                {widget.body.slice(0, 2000)}
              </div>
            }
          >
            <WidgetView
              widget={widget}
              terminalId={id}
              fallbackTitle={`${terminal.machine} · ${terminal.window}`}
              className="absolute inset-0"
            />
          </Suspense>
        )}

        {gone && (
          // Acceptance criterion 5: a session that is gone is words and an action, never a quiet
          // tile. It sits OVER the terminal because the last screen it painted is now stale.
          <div
            data-testid={testid.empty(id)}
            className={`absolute inset-0 flex flex-col items-center justify-center gap-1 p-3 text-center text-xs ${scrim}`}
          >
            <strong>no session on {terminal.machine}</strong>
            <span>{terminal.health.detail ?? 'converge to create it'}</span>
            {onConverge && (
              <Button
                data-testid={testid.converge(id)}
                variant="outline"
                size="sm"
                onClick={() => onConverge(id)}
              >
                Converge session
              </Button>
            )}
          </div>
        )}
      </div>

      {/*
        THE STATUS LINE, AT THE FOOT OF THE TILE — where tmux puts it.

        "And put the green banner on the bottom, like tmux." It was row 2 of the header, which is the
        one place tmux never draws it, so the bar an operator was supposed to recognise did not read
        as one. It is the last element of the tile now, edge to edge under the screen, and styled
        like `status-style bg=green,fg=black`.

        IT COSTS THE WALL NOTHING. `grid/wall.ts`'s `WALL_ROW_PX` is measured against the chrome a
        tile carries: the header gave back the 15 px row this occupied, so the total is unchanged and
        the wall's packing did not have to move. See the header's own note.

        AFTER the screen and OUTSIDE it, not an overlay on it: the screen's last line is a Worker's
        newest output and a band floating over it would cover exactly the row an operator is reading.

        `bare` drops it with the rest of the chrome — a surface that draws its own identity and
        controls (`WorkerPane`) would otherwise get a second bar saying where it is.
      */}
      {!bare && (
        <StatusLine
          id={id}
          terminal={terminal}
          ref_={ref_}
          feed={mode}
          frames={frames}
          tileCols={size.cols}
          tileRows={size.rows}
          stale={stale}
          compact={compact}
        />
      )}
    </>
  );
});
