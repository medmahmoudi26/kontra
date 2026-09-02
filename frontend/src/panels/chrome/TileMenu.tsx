/**
 * A tile's context menu (slice 7a).
 *
 * HAND-ROLLED, AND THE REASON IS NOT BUNDLE SIZE. The reference implementation's `PaneContextMenu` uses
 * `@szhsin/react-menu`, which is a fine library and the wrong shape here for the same reason
 * `grid/DashboardGrid.tsx` refuses a drag-and-drop dependency: a menu library owns its own portal and
 * positioning, and several of them position with a CSS `transform` on an ancestor wrapper. ADR 0020
 * decision 12 forbids a Terminal inside a transformed container, because `addon-fit`'s measurement is
 * what `stty` receives. A menu is ~90 lines; auditing a library's portal for transforms on every upgrade
 * is forever.
 *
 * EVERY ITEM IS READ-ONLY OR OUT-OF-BAND. Copy reads xterm's own buffer. Go live / stop live are
 * `focus`/`blur`, which carry a geometry and no bytes. Converge is a Temporal workflow the STREAMER
 * composes — the client picks which Machine, never what the converge contains. Open the drawer is local
 * state. There is deliberately no "send keys", no "restart", no "kill session": ADR 0020's finding (3)
 * measured `run-shell` executing as root through a read-only tmux client, so the guarantee has to be that
 * there is nothing to write to, and a menu is where a fourth item quietly becomes a command channel.
 *
 * ACCESSIBILITY IS NOT DECORATION ON THIS ONE. The menu is `role="menu"` with roving focus, Escape and
 * outside-click close, and focus returns to the trigger — because the alternative on a wall is an
 * operator whose keyboard focus is lost somewhere inside a grid of twenty-four terminals.
 */

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';

export interface TileMenuItem {
  key: string;
  label: string;
  hint?: string;
  disabled?: boolean;
  /** Rendered apart from the rest, for the one item that costs a Machine something. */
  emphasis?: 'live' | 'danger';
  run(): void;
}

/** Kept in one place because the positioning arithmetic and the rendered box have to agree — a menu
 * measured at one width and drawn at another is a menu that clips itself off the right of the viewport. */
const MENU_WIDTH = 230;
/** An upper bound, not a measurement: enough to decide "below or above" before the menu has been laid out.
 * Four items plus padding is well under this. */
const MENU_MAX_HEIGHT = 160;

export interface TileMenuProps {
  /** The Terminal id, for the test hooks — a wall has one of these per tile. */
  scope: string;
  items: TileMenuItem[];
  /** Label for the trigger's accessible name; the glyph alone is not one. */
  triggerLabel: string;
}

export default function TileMenu({ scope, items, triggerLabel }: TileMenuProps): JSX.Element {
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const menuRef = useRef<HTMLDivElement | null>(null);
  /**
   * Where to put the menu, in VIEWPORT coordinates.
   *
   * `position: fixed`, MEASURED PER OPEN — and the reason it can be is a consequence of ADR 0020 rather
   * than a coincidence. A tile's container is `overflow: hidden` (xterm has to be clipped to its box), so
   * an absolutely-positioned menu inside it is clipped by the tile: on a four-column wall that is a
   * half-menu with the converge item cut off. `fixed` escapes ancestor overflow — but only while no
   * ancestor has a `transform`, `filter` or `will-change` establishing a containing block, and decision 12
   * already forbids exactly those anywhere near a Terminal, because a transform makes `addon-fit`'s
   * measurement a lie. The rule that protects the geometry is what makes this positioning reliable.
   *
   * The cost of `fixed` is that the menu does not travel with a scroll, so a scroll closes it — which is
   * what a popover should do anyway.
   */
  const [at, setAt] = useState<{ top: number; left: number } | null>(null);

  const close = useCallback((restoreFocus: boolean): void => {
    setOpen(false);
    if (restoreFocus) triggerRef.current?.focus();
  }, []);

  useLayoutEffect(() => {
    if (!open) {
      setAt(null);
      return;
    }
    const rect = triggerRef.current?.getBoundingClientRect();
    if (rect) {
      // Right-aligned to the trigger by default, flipped and clamped against the viewport: a tile in the
      // right-hand column of a four-column wall has only a few hundred pixels to its right.
      const left = Math.max(4, Math.min(rect.right - MENU_WIDTH, window.innerWidth - MENU_WIDTH - 4));
      // Below unless there is no room below, in which case above — a tile in the last row of the wall.
      const below = rect.bottom + 4;
      const top = below + MENU_MAX_HEIGHT > window.innerHeight ? Math.max(4, rect.top - 4 - MENU_MAX_HEIGHT) : below;
      setAt({ top, left });
    }
    // The first item takes focus, so the menu is usable from the keyboard the moment it opens.
    setActive(0);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (e: PointerEvent): void => {
      const target = e.target as Node | null;
      if (target && (menuRef.current?.contains(target) || triggerRef.current?.contains(target))) return;
      close(false);
    };
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        close(true);
      }
    };
    // A `fixed` menu does not travel with a scroll, so a scroll dismisses it rather than leaving it
    // hovering over a tile that has moved out from under it. Capture phase, because the wall's scroll
    // container is an ancestor and `scroll` does not bubble.
    const onScroll = (): void => close(false);
    // Capture, so a click anywhere — including on another tile's xterm, which stops propagation for its
    // own selection handling — still dismisses this.
    document.addEventListener('pointerdown', onPointerDown, true);
    document.addEventListener('keydown', onKey, true);
    document.addEventListener('scroll', onScroll, true);
    window.addEventListener('resize', onScroll);
    return () => {
      document.removeEventListener('pointerdown', onPointerDown, true);
      document.removeEventListener('keydown', onKey, true);
      document.removeEventListener('scroll', onScroll, true);
      window.removeEventListener('resize', onScroll);
    };
  }, [close, open]);

  const enabled = items.filter((item) => !item.disabled);

  // Move real DOM focus to the active item — once per change of `active`, and only while open. An
  // aria-only cursor would leave the browser's focus on the trigger, so Enter would reopen the menu
  // instead of running the item under the highlight.
  useEffect(() => {
    if (!open) return;
    menuRef.current?.querySelector<HTMLElement>('[data-active="true"]')?.focus();
  }, [active, open]);

  const onMenuKeyDown = (e: React.KeyboardEvent<HTMLDivElement>): void => {
    if (enabled.length === 0) return;
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      const step = e.key === 'ArrowDown' ? 1 : -1;
      setActive((prev) => (prev + step + enabled.length) % enabled.length);
      return;
    }
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      const item = enabled[active];
      if (item) {
        item.run();
        close(true);
      }
      return;
    }
    if (e.key === 'Tab') {
      // A menu is not part of the tab ring: tabbing out of it closes it rather than leaving an open
      // popover over a tile nobody is looking at any more.
      close(false);
    }
  };

  return (
    <span className="relative inline-flex">
      <button
        ref={triggerRef}
        type="button"
        data-testid={`tile-menu-${scope}`}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={triggerLabel}
        title={triggerLabel}
        className="rounded border bg-black/40 px-1 text-xs leading-5 text-muted-foreground hover:bg-accent hover:text-accent-foreground"
        onClick={() => setOpen((prev) => !prev)}
        onKeyDown={(e) => {
          if (e.key === 'ArrowDown' && !open) {
            e.preventDefault();
            setOpen(true);
          }
        }}
      >
        ⋯
      </button>

      {open && at && (
        <div
          ref={menuRef}
          role="menu"
          aria-label={triggerLabel}
          data-testid={`tile-menu-items-${scope}`}
          tabIndex={-1}
          // `fixed`, at measured viewport coordinates — see the `at` state above for why that is both
          // necessary (the tile clips its overflow) and safe (nothing near a Terminal may carry a
          // transform). Still in the tile's own DOM subtree rather than portalled, so it needs no portal
          // root and inherits the page's theme tokens.
          style={{ position: 'fixed', top: at.top, left: at.left, width: MENU_WIDTH }}
          className="z-30 rounded border bg-popover p-1 text-popover-foreground shadow-lg"
          onKeyDown={onMenuKeyDown}
        >
          {items.map((item) => {
            const index = enabled.indexOf(item);
            return (
              <button
                key={item.key}
                type="button"
                role="menuitem"
                data-testid={`tile-menu-${item.key}-${scope}`}
                disabled={item.disabled}
                title={item.hint}
                // Roving tabindex: exactly one item is reachable by Tab, and the arrow keys move which.
                // The FOCUS itself is moved by the effect below rather than by a ref callback — a ref
                // callback fires on every render and would drag focus back mid-typing.
                tabIndex={index === active ? 0 : -1}
                data-active={index === active ? 'true' : undefined}
                className={`flex w-full items-baseline gap-2 rounded px-2 py-1 text-left text-xs hover:bg-accent disabled:cursor-default disabled:opacity-50 ${
                  item.emphasis === 'live' ? 'text-primary' : ''
                } ${item.emphasis === 'danger' ? 'text-amber-700 dark:text-amber-300' : ''}`}
                onClick={() => {
                  item.run();
                  close(true);
                }}
              >
                <span className="flex-1">{item.label}</span>
              </button>
            );
          })}
        </div>
      )}
    </span>
  );
}
