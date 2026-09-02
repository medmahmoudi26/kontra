/**
 * One tile's banner: who it is, the controls — and the grab point for the wall.
 *
 * ONE ROW, BECAUSE THE STATUS LINE WENT WHERE TMUX PUTS IT. This banner briefly had two: a
 * hand-rolled address line, then `chrome/StatusLine.tsx` in its place. The bar is now the last
 * element of the TILE rather than the second row of its header — "put the green banner on the
 * bottom, like tmux" — so everything about the address, the session, the pane's command, the pane's
 * geometry and the frame's age is down there, and this strip is back to identity plus controls.
 *
 * THE HEIGHT IS A BUDGET RATHER THAN A STYLE, and moving the bar spent none of it. `grid/wall.ts`'s
 * row height was MEASURED against the chrome a tile carries — 132 px minus the banner and the health
 * chips leaves about five rows of terminal — so every pixel here is a line of a Machine's output an
 * operator cannot see. Two header rows plus the chips became ONE header row plus the chips plus the
 * same bar at the foot — the same elements in the same flex column, so the sum is unchanged by
 * construction and `WALL_ROW_PX` did not have to move. What would have cost a row is the other
 * version of the change: bar at the bottom, header still two rows. A narrow tile still DROPS what it
 * can afford to lose rather than wrapping.
 *
 * THE BANNER IS THE DRAG HANDLE, which is why it is a `cursor-grab` region with `select-none` and a
 * `touch-action: none`. A separate drag strip above the tile would be a fifth row of chrome on every
 * tile on the wall. The controls inside it stop the pointer with `stopPropagation`, so clicking "Go
 * live" is never the start of a drag.
 *
 * GOING LIVE IS A LABELLED BUTTON THAT SAYS WHAT IT COSTS. ADR 0020: the wall is snapshots, and only
 * a focused or explicitly pinned Terminal is a live PTY attach, because each one is an `sshd`
 * session, a PTY and a per-viewer tmux session on an `s-1vcpu-2gb` Machine. Slice 2 amendment 9
 * makes it "never a click, never a stray tab-stop", so it is not the tile's `onClick`, not the
 * menu's default item, and not something the sidebar can trigger.
 *
 * NOTHING HERE CAN PUT BYTES ON THE SOCKET. Copy reads xterm's buffer; the drawer is local state;
 * the converge names a Machine and the streamer composes the workflow. There is no input, no key
 * handler and no message type that carries a payload toward a session.
 */

import { memo, type MutableRefObject } from 'react';
import { EyeOff, Maximize2, Minimize2 } from 'lucide-react';
import PaneThroughput from './PaneThroughput';
import TileMenu, { type TileMenuItem } from './TileMenu';
import { byteWords } from './format';
import { modeStakes, type TileRef } from './tileRef';

export interface TileHeaderProps {
  id: string;
  ref_: TileRef;
  /** What a person calls this Machine. Falls back to the node segment of the id when the inventory
   * carries no hostname — `local` Machines report the host as both. */
  host: string;
  live: boolean;
  /** How far back the scrollback is, from the wheel proxy. Zero means at the tail. */
  scrolledBack: number;
  /** True while snapshots are being HELD so the scrollback under the reader survives. The tile is
   * showing old output deliberately, which has to be said — a held tile and a Machine that stopped
   * printing look identical, and that is the confusion this whole badge exists to prevent. */
  held?: boolean;
  elided: number;
  /** A tile too narrow for both rows in full. The identity and the controls stay; the actor, the
   * geometry and the mode badge go. Anything that can only be reached by widening a tile is a
   * control an operator cannot find. */
  compact: boolean;
  /** Bytes this Terminal has received since the banner's sparkline last drained it. A REF, so a
   *  busy Machine's frames do not re-render the banner — see `PaneThroughput`. Absent on a tile
   *  with no byte path. */
  bytes?: MutableRefObject<number>;
  /** Pointer-down on the banner starts a wall drag. Absent when the tile is not on a wall — the
   * Workflows page embeds one and there is nothing there to drag it around. */
  onDragHandle?(e: React.PointerEvent<HTMLElement>): void;
  onGoLive(): void;
  onStopLive(): void;
  onCopy(): void;
  onConverge?(): void;
  onOpenDrawer?(): void;
  onJumpToTail?(): void;
  /** Take this pane off the wall until it is restored. Absent off the wall — an embedded tile has no
   *  wall to leave and no "N hidden" affordance to be restored from, and a hide with no way back is
   *  the disappearance this control exists to avoid looking like. */
  onHide?(): void;
  /** Zoom this pane to fill the wall, or restore it. Absent off the wall. tmux's `C-b z` as a click —
   * reachable by mouse alone, which is the whole of slice 05's criterion 1 for zoom. */
  onZoom?(): void;
  /** True while THIS pane is the zoomed one: the control flips to "restore". */
  zoomed?: boolean;
}

/** Swallow a pointer-down so a control inside the banner is not also the start of a drag. */
function notADrag(e: React.PointerEvent): void {
  e.stopPropagation();
}

export default memo(function TileHeader({
  id,
  ref_,
  host,
  live,
  scrolledBack,
  held = false,
  elided,
  compact,
  bytes,
  onDragHandle,
  onGoLive,
  onStopLive,
  onCopy,
  onConverge,
  onOpenDrawer,
  onJumpToTail,
  onHide,
  onZoom,
  zoomed = false,
}: TileHeaderProps): JSX.Element {
  const stakes = modeStakes(ref_.mode);

  const items: TileMenuItem[] = [
    {
      key: 'copy',
      label: 'Copy visible text',
      hint:
        'the rows on screen, right-trimmed. Not the scrollback and not a log — a Terminal is lossy by ' +
        'construction, and the Manifest, the journal on the Machine and the lake are the record.',
      run: onCopy,
    },
    live
      ? {
          key: 'stop-live',
          label: 'Back to snapshots',
          hint: 'ends the PTY attach and kills this viewer’s grouped tmux session on the node',
          emphasis: 'live',
          run: onStopLive,
        }
      : {
          key: 'go-live',
          label: 'Go live (a real PTY attach)',
          hint:
            'opens an sshd session, a PTY and a per-viewer tmux session on this node. The wall is ' +
            'snapshots; only what you promote is live, and the page holds a budget.',
          emphasis: 'live',
          run: onGoLive,
        },
    {
      key: 'converge',
      label: 'Converge session',
      hint: onConverge
        ? `runs the tmux-<node> workflow on the infra queue so the session and its windows exist. ${stakes.sentence}`
        : 'unavailable: this Dashboard has no converge for this Terminal',
      disabled: !onConverge,
      emphasis: 'danger',
      run: () => onConverge?.(),
    },
    {
      key: 'hide',
      label: 'Hide this pane',
      hint: onHide
        ? 'takes this WINDOW off the wall and stops paying for it — no `capture-pane` exec, no ' +
          'frames, and a live attach is dropped. The wall keeps the rectangle, and the "hidden" ' +
          'bar above the wall lists it and puts it back.'
        : 'unavailable: this tile is not on a wall, so there is nowhere to restore it from',
      disabled: !onHide,
      run: () => onHide?.(),
    },
    {
      key: 'drawer',
      label: 'Open detail drawer',
      hint: onOpenDrawer
        ? 'actor@version, tag, fleet, mode, every health sentence — including the axes this pane’s mode cannot answer — snapshot age, and the exact ssh/tmux commands to do it yourself'
        : 'the detail drawer is not wired up in this build',
      disabled: !onOpenDrawer,
      run: () => onOpenDrawer?.(),
    },
  ];

  return (
    <div
      className={`flex shrink-0 flex-col border-b border-border bg-muted ${
        onDragHandle ? 'cursor-grab select-none touch-none active:cursor-grabbing' : ''
      }`}
      data-testid={`tile-header-${id}`}
      data-live={live ? 'true' : undefined}
      {...(onDragHandle ? { onPointerDown: onDragHandle } : {})}
    >
      {/* WHO, and the controls. The only row: everything the address line and then the status line
          used to say from here is on the bar at the FOOT of the tile now, where tmux draws it. */}
      <div className="flex min-w-0 items-center gap-1.5 px-2 py-1">
        <span
          data-testid={`tile-dot-${id}`}
          aria-hidden
          className={`size-[7px] shrink-0 rounded-full ${
            live ? 'bg-emerald-400 [animation:kontra-pulse_1.8s_ease-in-out_infinite]' : 'bg-muted-foreground/40'
          }`}
        />
        <code className="truncate text-[11px] font-medium" title={`${host} — ${id}`}>
          {host}
        </code>

        <span className="ml-auto flex shrink-0 items-center gap-1">
          {/* Only when the tile is wide enough to spare 46 px. On a narrow tile the identity and the
              controls win — a chart nobody can read is not worth the hostname it truncates. */}
          {!compact && bytes && <PaneThroughput counter={bytes} id={id} />}

          {scrolledBack > 0 && (
            // A tile scrolled up is showing OLD output while its stream keeps arriving, which on a
            // wall of near-identical journals is indistinguishable from a Machine that stopped
            // printing. So it is stated, and it is the way back.
            <button
              type="button"
              data-testid={`tile-tail-${id}`}
              data-held={held ? 'true' : undefined}
              title={
                held
                  ? 'this tile is scrolled back, so its repaints are being held — the screen you ' +
                    'are reading will not be replaced under you. Click to return to the newest output.'
                  : 'this tile is scrolled back — click to return to the newest output'
              }
              className={`rounded border px-1 text-[10px] leading-4 ${
                // A held tile reads as amber, the same colour every other "showing you something
                // older than now" state on this surface uses.
                held
                  ? 'border-amber-500/50 bg-amber-500/10 text-amber-600 dark:text-amber-400'
                  : 'border-primary/50 bg-primary/10 text-primary'
              }`}
              onPointerDown={notADrag}
              onClick={() => onJumpToTail?.()}
            >
              ↓ {scrolledBack} back{held ? ' · held' : ''}
            </button>
          )}

          {elided > 0 && (
            <span
              data-testid={`tile-elided-${id}`}
              data-bytes={elided}
              title="output the streamer's byte cap dropped for this Terminal. Shown, never swallowed: a tile that silently skips output is a tile that lies about what a Worker printed."
              className="rounded border border-amber-500/60 bg-amber-500/10 px-1 text-[10px] leading-4 text-amber-700 dark:text-amber-300"
            >
              {byteWords(elided)} elided
            </span>
          )}

          {onZoom && (
            // tmux's `C-b z`, as a click. It does NOT go live and puts NOTHING on the socket — it is
            // page state that lays this pane out at full size (ADR 0020 decision 12: real geometry, never
            // a transform). `stopPropagation` so a zoom on the banner is not also the start of a drag.
            <button
              type="button"
              data-testid={`tile-zoom-${id}`}
              aria-label={zoomed ? `restore ${id} to the wall` : `zoom ${id} to fill the wall`}
              title={
                zoomed
                  ? 'restore this pane to the wall'
                  : 'zoom this pane to fill the wall — no keyboard, just a click (tmux C-b z, without tmux)'
              }
              className="rounded border border-border px-1 leading-4 text-muted-foreground hover:text-foreground"
              onPointerDown={notADrag}
              onClick={onZoom}
            >
              {zoomed ? <Minimize2 size={11} aria-hidden /> : <Maximize2 size={11} aria-hidden />}
            </button>
          )}

          {onHide && (
            // HIDE IS A BUTTON, not a menu item. It shipped inside the `⋯` menu and the operator
            // could not find it — three times — which for an affordance is the same as not having
            // built it. It sits beside zoom because they are the two things you do to a tile's
            // PRESENCE on the wall, and because a hidden pane costs nothing (it unsubscribes), so
            // reaching for it should cost nothing either. The way back is the strip above the wall.
            <button
              type="button"
              data-testid={`tile-hide-${id}`}
              aria-label={`hide ${id} from the wall`}
              title="take this pane off the wall — it stops being drawn AND stops being streamed. Restore it from the strip above the wall."
              className="rounded border border-border px-1 leading-4 text-muted-foreground hover:text-foreground"
              onPointerDown={notADrag}
              onClick={onHide}
            >
              <EyeOff size={11} aria-hidden />
            </button>
          )}

          <button
            type="button"
            data-testid={`focus-${id}`}
            title={
              live
                ? 'end the PTY attach on this node and go back to snapshots'
                : 'open a real PTY attach: an sshd session and a per-viewer tmux session on this node, inside the page’s live budget'
            }
            className={`rounded-full border px-1.5 text-[9.5px] font-medium uppercase leading-4 tracking-wide ${
              live
                ? 'border-emerald-500/50 bg-emerald-500/15 text-emerald-600 dark:text-emerald-400'
                : 'border-border bg-transparent text-muted-foreground'
            }`}
            onPointerDown={notADrag}
            onClick={() => (live ? onStopLive() : onGoLive())}
          >
            {live ? 'live' : 'snapshot'}
          </button>

          <span onPointerDown={notADrag}>
            <TileMenu scope={id} items={items} triggerLabel={`actions for ${id}`} />
          </span>
        </span>
      </div>

    </div>
  );
});
