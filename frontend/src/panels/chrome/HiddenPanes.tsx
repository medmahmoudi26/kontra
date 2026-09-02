/**
 * "3 panes hidden" — the affordance that keeps hidden from looking like gone.
 *
 * THIS COMPONENT IS THE PRICE OF THE HIDE CONTROL, not a decoration on it. A wall where a tile can
 * vanish and leave no trace is a wall where "my crawler is missing" has two indistinguishable causes:
 * somebody hid it, and the Worker died. `TileWall` already spends a row on GHOSTS for a Terminal that
 * left the inventory for that exact reason, and `paneHiding.ts` states it as the first of the three
 * properties hiding owes a reader. So this is always on the page while anything is hidden, it names
 * each pane rather than only counting them, and every name is the button that puts it back.
 *
 * IT SAYS WHAT WAS HIDDEN, WHICH IS A WINDOW. The wall's grain is `<mode>:<node>/<session>/<window>`
 * and the hide control lives on a tile, so `session:window` is the unit and the label prints it that
 * way — an operator who hid the handler still has the actor beside it, and the list has to make that
 * obvious rather than leave "a pane" ambiguous between a window, a session and a Machine.
 *
 * AN ID THE INVENTORY NO LONGER HAS IS STILL LISTED, marked, and still restorable. A hidden pane on a
 * Machine that has since left the Fleet would otherwise be an invisible entry that silently re-hides
 * the tile if that id ever comes back — a wall with a hole in it and nothing on the page to explain
 * the hole. Naming it is also the only way to clear it.
 */

import { parseTileRef } from './tileRef';

export interface HiddenPanesProps {
  /** In the order they were hidden — most recent last, so undoing reads backwards. */
  ids: readonly string[];
  /** Ids the inventory still reports. An id outside this set is listed as absent, never dropped. */
  known: ReadonlySet<string>;
  onShow(id: string): void;
  onShowAll(): void;
  className?: string;
}

/** What one hidden pane reads as: the node, then the string `tmux attach -t` takes. */
export function hiddenLabel(id: string): string {
  const ref = parseTileRef(id);
  return `${ref.node} ${ref.session}:${ref.window}`;
}

export default function HiddenPanes({
  ids,
  known,
  onShow,
  onShowAll,
  className,
}: HiddenPanesProps): JSX.Element | null {
  // Nothing hidden is nothing to say. The one case where silence is right: there is no absence to
  // explain, and a permanent "0 hidden" row is a line of the wall spent on a fact about nothing.
  if (ids.length === 0) return null;

  return (
    <div
      data-testid="hidden-panes"
      data-count={ids.length}
      className={`flex flex-wrap items-center gap-1.5 rounded border border-dashed border-amber-500/50 bg-amber-500/5 px-2 py-1 text-[11px] ${className ?? ''}`}
    >
      <span className="font-medium text-amber-700 dark:text-amber-300">
        {ids.length} pane{ids.length === 1 ? '' : 's'} hidden
      </span>
      <span className="text-muted-foreground">
        — one tmux window each, not drawn and not subscribed: no snapshot exec, no frames, no live
        attach. Click one to put it back.
      </span>

      {ids.map((id) => {
        const absent = !known.has(id);
        return (
          <button
            key={id}
            type="button"
            data-testid={`unhide-${id}`}
            data-absent={absent ? 'true' : undefined}
            title={
              absent
                ? `${id} — hidden, and the streamer is not reporting it at all right now. Restoring it ` +
                  'will draw a tile only when the inventory has it again; until then this entry is what ' +
                  'says the pane is being hidden rather than missing.'
                : `${id} — restore this pane to the wall and subscribe it again`
            }
            className={`rounded border px-1.5 py-0.5 font-mono leading-4 hover:bg-background ${
              absent
                ? 'border-muted-foreground/40 text-muted-foreground line-through'
                : 'border-amber-500/50 text-amber-700 dark:text-amber-300'
            }`}
            onClick={() => onShow(id)}
          >
            {hiddenLabel(id)}
            {absent ? ' (not in the inventory)' : ''} ✕
          </button>
        );
      })}

      <button
        type="button"
        data-testid="unhide-all"
        title="restore every hidden pane to the wall"
        className="ml-auto rounded border border-border px-1.5 py-0.5 leading-4 text-muted-foreground hover:text-foreground"
        onClick={onShowAll}
      >
        Show all
      </button>
    </div>
  );
}
