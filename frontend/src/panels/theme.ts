/**
 * How a Terminal LOOKS: palette, font family, font size — persisted (ADR 0020, slice 7a).
 *
 * A SEPARATE DECISION FROM THE APP'S LIGHT/DARK, and deliberately. `state/store.ts` already persists
 * a `theme` for the SPA and `App.tsx` flips `.dark` on `<html>`; that is the chrome. What this file
 * owns is the sixteen-plus-two colours a *terminal* paints with, which is not the same choice: an
 * operator reading `journalctl -fu` on a bright office monitor wants light chrome around dark tiles
 * far more often than they want a light terminal, because a Machine's output was coloured for a dark
 * background by whoever wrote the ANSI. So the default here is `follow`, which tracks the app for the
 * chrome-ish parts and still keeps a legible terminal, and an explicit choice overrides it and sticks.
 *
 * WHY A PALETTE AT ALL, WHEN THE OLD CODE PASSED `{ background: '#0b0b0e' }`. Because that one line was
 * the whole terminal theme, and xterm then fell back to its own defaults for the sixteen ANSI colours —
 * defaults tuned against xterm's white background. `capture-pane -e` carries the SGR a Worker emitted,
 * so those sixteen are the colours the Fleet actually prints in; the pair that mattered most in
 * practice was `brightBlack` (used by systemd for timestamps, near-invisible at 0.4 contrast on
 * `#0b0b0e`) and `red` (a scanner finding, which must not be the same red as a failing health chip or
 * the wall stops distinguishing "the Machine says this is bad" from "we say this Machine is bad").
 *
 * FONT SIZE IS NOT A FREE CONTROL, and this is the one thing in this file with a remote cost. Cell
 * metrics decide cols/rows, cols/rows are what the streamer puts into `stty` before `tmux attach`, so
 * a font-size bump re-`stty`s and re-attaches every LIVE tile — up to `LIVE_BUDGET` PTYs on
 * `s-1vcpu-2gb` Machines. `TerminalTile` coalesces the refit that follows (see its `SETTLE_MS`), which
 * is what turns a five-step drag from five re-attaches into one.
 *
 * EVERY EXPORT IS PURE OR TOTAL. The parse never throws and never returns something the renderer has
 * to guard, because the thing that would let an operator fix a bad stored value is the page that would
 * not have mounted — the same rule `grid/layout.ts`'s `parseLayoutStore` states.
 */

import type { ITheme } from '@xterm/xterm';
// The @font-face lives here rather than in `styles.css`: `App.tsx` lazy-loads `DashboardPage`, so a
// tab that never opens the Dashboard never fetches 1.08 MB of font. See `fonts/README.md`.
import './fonts/nerd-font.css';

/** One key, versioned in the VALUE like `grid/layout.ts` does, so clearing a stale key by hand is
 * never the migration path. */
export const TERMINAL_THEME_STORAGE_KEY = 'kontra-dashboard-terminal-theme';

export const TERMINAL_THEME_VERSION = 1;

/**
 * The font stack, Nerd Font first.
 *
 * DOCUMENTED FALLBACK, not a decoration: `fonts/` can be deleted (or a build can fail to emit the
 * asset) and the wall still renders, in system monospace, losing box-drawing and powerline glyphs to
 * tofu. The tail is ordered by what is actually installed where an operator sits — macOS, then
 * Windows, then the Linux boxes — and ends at the generic keyword, which is the only entry guaranteed
 * to resolve.
 *
 * `monospace` LAST AND ALWAYS: a stack that ends at a named family renders proportional text in a
 * terminal if that family is missing, and a proportional terminal is unreadable rather than merely
 * plain.
 */
export const TERMINAL_FONT_STACK =
  "'FiraCode Nerd Font', 'FiraCode Nerd Font Mono', 'Fira Code', ui-monospace, SFMono-Regular, " +
  "'SF Mono', Menlo, Consolas, 'DejaVu Sans Mono', monospace";

/** What `whenTerminalFontReady` asks the browser to load. A `document.fonts.load` specifier needs a
 * size and one family — the size is irrelevant to whether the file is fetched. */
const FONT_PROBE = "12px 'FiraCode Nerd Font'";

export const MIN_FONT_SIZE = 9;
export const MAX_FONT_SIZE = 20;
/**
 * 12 px, which is what slice 2 hardcoded, kept as the default on purpose.
 *
 * MEASURED constraint rather than taste: `grid/layout.ts`'s row heights were measured against a 12 px
 * cell (`wall` density gives ~8 rows of terminal in a 200 px row), so raising the default would silently
 * shrink every tile's row count on an existing saved layout. An operator can still raise it; the
 * DEFAULT is what a fresh browser inherits, and it has to agree with the density ladder.
 */
export const DEFAULT_FONT_SIZE = 12;

export type PaletteName = 'follow' | 'night' | 'paper';

/**
 * Two palettes and a follow.
 *
 * TWO, not twelve. A theme picker with a dozen entries is a decision an operator makes once and a
 * maintenance surface forever, and the point of this control is legibility, not decoration: one dark
 * palette that agrees with the ANSI a Fleet actually emits, one light one for a bright room, and
 * `follow` so the wall does not fight the chrome around it by default.
 */
export interface PaletteSpec {
  label: string;
  hint: string;
  /** `null` for `follow`: resolved against the app's light/dark at apply time. */
  theme: ITheme | null;
}

/**
 * The dark palette, and the reasoning behind the two entries that are not obvious.
 *
 * `background` is slice 2's `#0b0b0e` unchanged — a tile has to read as a terminal at a glance across
 * a wall, and near-black against the card background is what does that in both app themes.
 *
 * `brightBlack` is lifted well above a true grey. systemd and `journalctl` print timestamps and
 * de-emphasised fields in bright black, so on the fleet it is not an accent colour, it is most of the
 * left-hand column of every line; at xterm's default `#666` it is 2.3:1 on this background and an
 * operator reads the wall by squinting at it.
 *
 * `red` is deliberately a warmer, lighter red than `--destructive`. A red the WORKER printed and a red
 * the DASHBOARD printed must not be the same red, or the wall stops being able to say "this Machine is
 * failing" as distinct from "this Machine says something failed".
 */
const NIGHT: ITheme = {
  background: '#0b0b0e',
  foreground: '#d7dae0',
  cursor: '#d7dae0',
  cursorAccent: '#0b0b0e',
  selectionBackground: '#2b4b6f',
  selectionForeground: '#f2f4f8',
  black: '#22252b',
  red: '#f07178',
  green: '#8bd49c',
  yellow: '#e2c08d',
  blue: '#6cb6ff',
  magenta: '#d3a0f0',
  cyan: '#76d6d6',
  white: '#c8ccd4',
  brightBlack: '#8b93a1',
  brightRed: '#ff8f96',
  brightGreen: '#a4e8b3',
  brightYellow: '#f2d5a4',
  brightBlue: '#8fcaff',
  brightMagenta: '#e3b8ff',
  brightCyan: '#93e6e6',
  brightWhite: '#f2f4f8',
};

/**
 * The light palette.
 *
 * A light terminal is a compromise and the code should say so: the sixteen ANSI colours a Worker emits
 * were chosen against a dark background, so every one of them has to be darkened here to stay legible,
 * and a Machine printing bright white text disappears. `brightWhite` is therefore NOT white — it is
 * the darkest foreground in the set, because "bright" on a light background has to mean "more
 * emphatic", not "closer to the paper".
 */
const PAPER: ITheme = {
  background: '#fbfbfd',
  foreground: '#22252b',
  cursor: '#22252b',
  cursorAccent: '#fbfbfd',
  selectionBackground: '#bcd6f5',
  selectionForeground: '#11131a',
  black: '#3b4048',
  red: '#c2373f',
  green: '#2f7d45',
  yellow: '#8a6100',
  blue: '#1f5fbf',
  magenta: '#8a3fb0',
  cyan: '#136f74',
  white: '#5a616c',
  brightBlack: '#6b7280',
  brightRed: '#a12930',
  brightGreen: '#236236',
  brightYellow: '#6d4c00',
  brightBlue: '#164a99',
  brightMagenta: '#6d2f8c',
  brightCyan: '#0e565a',
  brightWhite: '#11131a',
};

export const PALETTES: Record<PaletteName, PaletteSpec> = {
  follow: {
    label: 'Follow app',
    hint: 'the terminal palette tracks the SPA’s light/dark toggle',
    theme: null,
  },
  night: { label: 'Night', hint: 'dark tiles — what the Fleet’s ANSI was written for', theme: NIGHT },
  paper: { label: 'Paper', hint: 'light tiles — for a bright room; every ANSI colour is darkened', theme: PAPER },
};

export const PALETTE_ORDER: PaletteName[] = ['follow', 'night', 'paper'];

export function isPaletteName(value: unknown): value is PaletteName {
  return typeof value === 'string' && value in PALETTES;
}

export interface TerminalStyle {
  palette: PaletteName;
  fontSize: number;
  /** Is the Fleet tree open? See {@link SIDEBAR_DEFAULT_OPEN} for why the default is `false`. */
  sidebar: boolean;
}

/**
 * THE TREE STARTS CLOSED, AND THIS IS A MEASURED DECISION RATHER THAN A TASTE ONE.
 *
 * The sidebar is a flex SIBLING of the wall (it has to be — an overlay would leave every xterm measuring a
 * width it does not have), so while it is open the wall is narrower by its width. `grid/layout.ts` states
 * the property the two-column default exists for: "wide enough that a journal line does not wrap". That
 * property is quantitative, and the arithmetic is tight on the 1280-px window the browser suite runs at:
 *
 *   1280 − 44 (page padding) = 1236; two columns of ~608 leave a tile ~600 px wide, which at a 7.2-px cell
 *   is ~83 columns. The fake fleet's longest journal line is 68 characters, and a real
 *   `kontra-handler[1421]:` prefix plus a URL is longer.
 *
 * Subtract a 248-px sidebar and the tile is ~478 px, which is ~66 columns — and the browser suite CAUGHT
 * that: `repaints a snapshot rather than appending it` reported `1 marker(s), 5 line(s)`, one screen with
 * one wrapped line. The repaint was fine; the tile was too narrow. So the tree opens when an operator wants
 * it, the wall keeps its width by default, and the choice persists — which is also the honest shape for what
 * the tree is FOR: answering "where is kf-crawl-07", not being read continuously.
 */
export const SIDEBAR_DEFAULT_OPEN = false;

/**
 * How wide the tree is when it is open — narrowed from 248 for the same arithmetic.
 *
 * At 216 px a two-column tile is ~68 columns, which still holds the fake fleet's longest line. An operator
 * who wants both the tree and unwrapped output has `focused` density, which is one column.
 */
export const SIDEBAR_WIDTH_PX = 216;

export const DEFAULT_TERMINAL_STYLE: TerminalStyle = {
  palette: 'follow',
  fontSize: DEFAULT_FONT_SIZE,
  sidebar: SIDEBAR_DEFAULT_OPEN,
};

export function clampFontSize(n: unknown): number {
  const value = typeof n === 'number' ? n : Number(n);
  if (!Number.isFinite(value)) return DEFAULT_FONT_SIZE;
  const rounded = Math.round(value);
  if (rounded < MIN_FONT_SIZE) return MIN_FONT_SIZE;
  if (rounded > MAX_FONT_SIZE) return MAX_FONT_SIZE;
  return rounded;
}

/**
 * Read the stored style. Total: `null`, garbage, a future version and a hand-edited value in devtools
 * all produce a usable style.
 *
 * A newer `version` falls back to the default rather than being read field by field, for the same
 * reason `parseLayoutStore` drops one: a build that renamed `fontSize` would otherwise lay the wall out
 * at 9 px and look broken instead of looking reset. Nothing warns, unlike a layout — a reset palette
 * costs an operator one click, where a reset wall costs them ten minutes.
 */
export function parseTerminalStyle(raw: string | null | undefined): TerminalStyle {
  if (raw === null || raw === undefined || raw.trim() === '') return DEFAULT_TERMINAL_STYLE;
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return DEFAULT_TERMINAL_STYLE;
  }
  if (!parsed || typeof parsed !== 'object') return DEFAULT_TERMINAL_STYLE;
  const r = parsed as Record<string, unknown>;
  if (r.version !== TERMINAL_THEME_VERSION) return DEFAULT_TERMINAL_STYLE;
  return {
    palette: isPaletteName(r.palette) ? r.palette : DEFAULT_TERMINAL_STYLE.palette,
    fontSize: clampFontSize(r.fontSize),
    // `=== true`, not truthiness: a stored `"yes"` or `1` from a hand edit must fall back to the default
    // rather than open a sidebar that narrows every tile on the wall.
    sidebar: r.sidebar === true,
  };
}

export function serializeTerminalStyle(style: TerminalStyle): string {
  return JSON.stringify({
    version: TERMINAL_THEME_VERSION,
    palette: style.palette,
    fontSize: style.fontSize,
    sidebar: style.sidebar,
  });
}

/**
 * The `ITheme` a tile should paint with.
 *
 * `appIsDark` is passed in rather than read from the DOM so this stays pure and testable — the caller
 * already knows, because `App.tsx` is what sets the class.
 */
export function resolvePalette(palette: PaletteName, appIsDark: boolean): ITheme {
  const spec = PALETTES[palette] ?? PALETTES.follow;
  return spec.theme ?? (appIsDark ? NIGHT : PAPER);
}

/** The tile's own background, for the chrome AROUND the xterm — the rounded border, the "no session"
 * scrim and the pending placeholder all have to sit on the same colour the terminal paints, or a tile
 * has a visible seam between its frame and its screen. */
export function paletteBackground(palette: PaletteName, appIsDark: boolean): string {
  return resolvePalette(palette, appIsDark).background ?? '#0b0b0e';
}

/** Is the resolved palette a dark one? The scrims and badges over a tile need to know, and asking the
 * palette is more honest than asking the app: a `night` terminal inside a light SPA still needs a
 * light-on-dark badge. */
export function paletteIsDark(palette: PaletteName, appIsDark: boolean): boolean {
  return palette === 'night' || (palette === 'follow' && appIsDark);
}

/**
 * Resolve when the Nerd Font is usable, or immediately when it cannot be.
 *
 * NEVER REJECTS, and never blocks a mount. This exists only so a tile can RE-measure after the swap:
 * `font-display: swap` paints the first frame in the fallback stack, whose cell metrics differ from the
 * Nerd Font's, and a tile that measured itself in the fallback reported cols/rows that the streamer
 * then `stty`d a remote pane to. One extra `fit()` when the font lands is the whole fix; awaiting the
 * font BEFORE opening the terminal would have been the other one, and it trades a correct measurement
 * for up to a second of blank wall on a cold cache.
 *
 * `document.fonts` is absent in the vitest node environment and in older browsers, hence the guard.
 */
export function whenTerminalFontReady(): Promise<void> {
  const fonts = typeof document === 'undefined' ? undefined : document.fonts;
  if (!fonts?.load) return Promise.resolve();
  return fonts
    .load(FONT_PROBE)
    .then(() => undefined)
    .catch(() => undefined);
}
