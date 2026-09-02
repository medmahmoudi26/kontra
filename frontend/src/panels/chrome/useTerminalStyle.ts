/**
 * The terminal palette and font size, as React state, persisted (slice 7a).
 *
 * Thin on purpose, exactly like `grid/useLayout.ts`: every decision is a pure function in `theme.ts`, so
 * what a test drives is the style and not a hook. This file is the two things that cannot be pure —
 * `localStorage` and the `.dark` class the SPA sets on `<html>`.
 *
 * WRITES ARE BEST-EFFORT AND SILENT, WHICH IS THE OPPOSITE OF `useLayout`. A layout that stops saving
 * costs an operator ten minutes of arranging, so `useLayout` surfaces the failure as a warning on the
 * wall. A palette that stops saving costs them one click on the next reload, and a permanent warning
 * banner for that would be noise on a page whose warnings need to mean something.
 *
 * WHY IT WATCHES `<html>`'s CLASS. The default palette is `follow`, which resolves against the SPA's
 * light/dark — and that toggle lives in `state/store.ts` and `App.tsx`, which are not this slice's files.
 * Rather than reach into another slice's store, this observes the ONE thing `App.tsx` actually does with
 * it (`classList.toggle('dark', …)`), which is a smaller coupling than importing a store and a stabler
 * one than duplicating the media query.
 */

import { useCallback, useEffect, useMemo, useState } from 'react';
import type { ITheme } from '@xterm/xterm';
import {
  clampFontSize,
  MAX_FONT_SIZE,
  MIN_FONT_SIZE,
  paletteBackground,
  paletteIsDark,
  parseTerminalStyle,
  resolvePalette,
  serializeTerminalStyle,
  TERMINAL_THEME_STORAGE_KEY,
  type PaletteName,
  type TerminalStyle,
} from '../theme';

export interface TerminalStyleApi extends TerminalStyle {
  /** The resolved xterm palette — what a tile passes to `new XTerm({ theme })`. */
  theme: ITheme;
  /** The tile chrome's background, so the frame around a screen matches the screen. */
  background: string;
  /** Is the resolved palette dark? The scrims and badges over a tile need to know. */
  isDark: boolean;
  setPalette(palette: PaletteName): void;
  setFontSize(size: number): void;
  /** `+1`/`-1`, clamped. A stepper rather than a slider: cell metrics are integral, and a slider invites
   * dragging — which on a live tile is a re-`stty` and a re-attach per step. */
  nudgeFontSize(by: number): void;
  canGrow: boolean;
  canShrink: boolean;
  /** Open or close the Fleet tree. Persisted, and closed by default — `theme.ts`'s
   * `SIDEBAR_DEFAULT_OPEN` has the measured reason. */
  toggleSidebar(): void;
}

function read(): TerminalStyle {
  try {
    return parseTerminalStyle(globalThis.localStorage?.getItem(TERMINAL_THEME_STORAGE_KEY) ?? null);
  } catch {
    // Reading storage can itself throw: a browser with site data blocked throws on ACCESS, not on the
    // value. The wall still has to come up, with the default palette.
    return parseTerminalStyle(null);
  }
}

/** Is the SPA in dark mode right now? Observed rather than stored — see the header. */
function appIsDarkNow(): boolean {
  if (typeof document === 'undefined') return true;
  return document.documentElement.classList.contains('dark');
}

export function useTerminalStyle(): TerminalStyleApi {
  // Read once, lazily: this touches `localStorage`, and a hook body runs on every render.
  const [style, setStyle] = useState<TerminalStyle>(read);
  const [appIsDark, setAppIsDark] = useState<boolean>(appIsDarkNow);

  useEffect(() => {
    if (typeof document === 'undefined') return;
    const observer = new MutationObserver(() => setAppIsDark(appIsDarkNow()));
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] });
    // Re-read on mount too: `App.tsx`'s effect may have run after this hook's first render.
    setAppIsDark(appIsDarkNow());
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    try {
      globalThis.localStorage?.setItem(TERMINAL_THEME_STORAGE_KEY, serializeTerminalStyle(style));
    } catch {
      /* see the header: a palette that does not persist is worth one click, not a banner */
    }
  }, [style]);

  const setPalette = useCallback((palette: PaletteName): void => {
    setStyle((prev) => (prev.palette === palette ? prev : { ...prev, palette }));
  }, []);

  const setFontSize = useCallback((size: number): void => {
    setStyle((prev) => {
      const fontSize = clampFontSize(size);
      return prev.fontSize === fontSize ? prev : { ...prev, fontSize };
    });
  }, []);

  const toggleSidebar = useCallback((): void => {
    setStyle((prev) => ({ ...prev, sidebar: !prev.sidebar }));
  }, []);

  const nudgeFontSize = useCallback(
    (by: number): void => setStyle((prev) => {
      const fontSize = clampFontSize(prev.fontSize + by);
      return prev.fontSize === fontSize ? prev : { ...prev, fontSize };
    }),
    []
  );

  return useMemo<TerminalStyleApi>(
    () => ({
      ...style,
      theme: resolvePalette(style.palette, appIsDark),
      background: paletteBackground(style.palette, appIsDark),
      isDark: paletteIsDark(style.palette, appIsDark),
      setPalette,
      setFontSize,
      nudgeFontSize,
      canGrow: style.fontSize < MAX_FONT_SIZE,
      canShrink: style.fontSize > MIN_FONT_SIZE,
      toggleSidebar,
    }),
    [appIsDark, nudgeFontSize, setFontSize, setPalette, style, toggleSidebar]
  );
}
