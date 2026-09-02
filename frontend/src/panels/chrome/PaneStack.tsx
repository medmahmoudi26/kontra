/**
 * A column of panes an operator can reorder and fold.
 *
 * THIN, LIKE `PaneResizer` AND `SideDock`. Every decision is a pure function in `paneOrder.ts`; this
 * file is storage, the drag, and the header a pane is grabbed by.
 *
 * THE HEADER IS THE HANDLE, and the whole header rather than a grip on it. A six-pixel dot is a
 * target people miss, and a pane whose entire title bar drags is the convention every tiling editor
 * already taught them. The fold caret and any per-pane action sit inside it and stop the drag from
 * starting (`stopPropagation` on their own pointer-down), so pressing a button never moves a pane.
 *
 * HTML5 DRAG AND DROP, NOT POINTER CAPTURE, and the difference from `PaneResizer` is the point: a
 * resize is one element changing one number, and a reorder is a gesture BETWEEN elements. The native
 * API is what gives a drop target per pane without a global pointer listener deciding which element
 * the cursor is over.
 *
 * A DROP INDICATOR AND NOT A LIVE REORDER. Panes that rearrange under the cursor mid-drag are how
 * you drop something one slot from where you meant: the stack holds still, a line shows where it
 * would land, and the move happens on release.
 */

import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import { ChevronDown, ChevronRight, GripVertical } from 'lucide-react';
import {
  moveBefore,
  nudge,
  paneOrderKey,
  parsePaneOrder,
  reconcileOrder,
  toggleFolded,
  type PaneOrder,
} from './paneOrder';

export interface StackedPane {
  /** Stable across renders AND across releases — it is what the stored order is keyed by. */
  key: string;
  title: string;
  /** Drawn in the header, right of the title: a count, a state, a path. */
  meta?: ReactNode;
  /** Drawn at the right of the header. Buttons here must not start a drag — the stack stops
   *  propagation for the whole group, so nothing extra is needed per button. */
  actions?: ReactNode;
  body: ReactNode;
  /** A pane that has no folded state worth having — a one-line bar, say. */
  unfoldable?: boolean;
}

/** The stored order for one stack, reconciled against the panes it is given. */
export function usePaneOrder(stack: string, keys: readonly string[]): {
  order: string[];
  folded: string[];
  setOrder(next: string[]): void;
  fold(key: string): void;
} {
  const [stored, set] = useState<PaneOrder>(() => {
    try {
      return parsePaneOrder(globalThis.localStorage?.getItem(paneOrderKey(stack)) ?? null);
    } catch {
      // Storage can throw on ACCESS, not only on the value. The page still has to come up.
      return { order: [], folded: [] };
    }
  });

  useEffect(() => {
    try {
      globalThis.localStorage?.setItem(paneOrderKey(stack), JSON.stringify(stored));
    } catch {
      /* see above */
    }
  }, [stack, stored]);

  // RECONCILED ON EVERY RENDER, not once on mount: the panes a stack holds can change while it is on
  // screen — the Workflows sidecar grows a worker pane the moment something is served — and an order
  // computed at mount would leave the new pane out until a reload.
  const order = useMemo(() => reconcileOrder(stored.order, keys), [stored.order, keys]);

  const setOrder = useCallback((next: string[]) => set((prev) => ({ ...prev, order: next })), []);
  const fold = useCallback(
    (key: string) => set((prev) => ({ ...prev, folded: toggleFolded(prev.folded, key) })),
    []
  );

  return { order, folded: stored.folded, setOrder, fold };
}

export function PaneStack({
  stack,
  panes,
  className,
}: {
  /** Where this stack's order is remembered. One per surface. */
  stack: string;
  panes: StackedPane[];
  className?: string;
}): JSX.Element {
  const keys = useMemo(() => panes.map((p) => p.key), [panes]);
  const { order, folded, setOrder, fold } = usePaneOrder(stack, keys);
  const [dragging, setDragging] = useState<string | null>(null);
  const [over, setOver] = useState<string | null>(null);

  const byKey = useMemo(() => new Map(panes.map((p) => [p.key, p])), [panes]);

  return (
    <div className={`flex min-h-0 flex-col ${className ?? ''}`} data-testid={`stack-${stack}`}>
      {order.map((key, index) => {
        const pane = byKey.get(key);
        if (!pane) return null;
        const isFolded = folded.includes(key);
        return (
          <section
            key={key}
            data-testid={`pane-${key}`}
            data-folded={isFolded ? 'true' : undefined}
            data-dropping={over === key && dragging !== key ? 'true' : undefined}
            className={`flex min-h-0 flex-col border-b border-border last:border-b-0 ${
              isFolded ? 'shrink-0' : 'min-h-0 flex-1'
            } ${over === key && dragging !== key ? 'border-t-2 border-t-primary' : ''} ${
              dragging === key ? 'opacity-50' : ''
            }`}
            onDragOver={(e) => {
              // Without this the drop never fires: the default is "reject".
              e.preventDefault();
              setOver(key);
            }}
            onDrop={(e) => {
              e.preventDefault();
              if (dragging && dragging !== key) setOrder(moveBefore(order, dragging, key));
              setDragging(null);
              setOver(null);
            }}
          >
            <header
              draggable
              data-testid={`pane-header-${key}`}
              title={`${pane.title} — drag to reorder, or focus and use ↑ ↓`}
              className="flex shrink-0 cursor-grab items-center gap-1.5 bg-muted/40 px-2 py-1 text-[9.5px] uppercase tracking-wide text-muted-foreground active:cursor-grabbing"
              onDragStart={(e) => {
                // Firefox starts no drag at all without data on the transfer.
                e.dataTransfer.setData('text/plain', key);
                e.dataTransfer.effectAllowed = 'move';
                setDragging(key);
              }}
              onDragEnd={() => {
                setDragging(null);
                setOver(null);
              }}
              tabIndex={0}
              onKeyDown={(e) => {
                const towards = e.key === 'ArrowDown' ? 1 : e.key === 'ArrowUp' ? -1 : 0;
                if (towards === 0) return;
                // The sidecar scrolls on an arrow key otherwise, taking the pane being moved out of
                // view of the person moving it.
                e.preventDefault();
                setOrder(nudge(order, key, towards as -1 | 1));
              }}
            >
              <GripVertical size={11} className="shrink-0 opacity-50" aria-hidden="true" />
              {pane.unfoldable ? (
                <span className="w-3 shrink-0" />
              ) : (
                <button
                  type="button"
                  data-testid={`pane-fold-${key}`}
                  title={isFolded ? `show ${pane.title}` : `fold ${pane.title} away`}
                  className="shrink-0 rounded outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
                  // A pointer-down on a control inside the handle must not start a drag: pressing
                  // fold would otherwise pick the pane up instead.
                  onPointerDown={(e) => e.stopPropagation()}
                  onClick={() => fold(key)}
                >
                  {isFolded ? <ChevronRight size={11} /> : <ChevronDown size={11} />}
                </button>
              )}
              <span className="min-w-0 truncate">{pane.title}</span>
              {pane.meta && <span className="shrink-0 normal-case tracking-normal">{pane.meta}</span>}
              {pane.actions && (
                <span
                  className="ml-auto flex shrink-0 items-center gap-0.5 normal-case tracking-normal"
                  onPointerDown={(e) => e.stopPropagation()}
                  onDragStart={(e) => e.preventDefault()}
                >
                  {pane.actions}
                </span>
              )}
              <span className="sr-only">{`pane ${index + 1} of ${order.length}`}</span>
            </header>
            {!isFolded && <div className="flex min-h-0 flex-1 flex-col overflow-hidden">{pane.body}</div>}
          </section>
        );
      })}
    </div>
  );
}
