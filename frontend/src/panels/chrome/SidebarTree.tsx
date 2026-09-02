/**
 * The sidebar: mode → node → session → window, beside the wall (slice 7a).
 *
 * IT SHOWS THE INVENTORY, THE WALL SHOWS THE SELECTORS. That is the whole reason it exists. A
 * selector-driven wall is allowed to be a view of part of a Fleet, so "where is kf-crawl-07" is a
 * question the wall structurally cannot answer — it either matched a slot or it did not, and both look
 * like a tile that is not there. Every Terminal the inventory reports gets a row here, whether or not a
 * slot matched it, and a row that is not on the wall says so.
 *
 * CLICKING A LEAF REVEALS; IT DOES NOT GO LIVE. ADR 0020's cost model is the reason and it is not
 * negotiable: a live tile is an `sshd` session, a PTY and a per-viewer tmux session on an
 * `s-1vcpu-2gb` Machine, so twenty-four of them is twenty-four of each. Slice 2 amendment 9 states the
 * rule as "never a click, never a stray tab-stop", and a tree of clickable Machine names is the single
 * easiest place in this UI to break it by accident. So a leaf scrolls its tile into view and flashes it,
 * and going live stays the tile header's own labelled control. The tooltip on every leaf says so, because
 * an operator who expects a click to attach will read the absence of an attach as a bug.
 *
 * A FLEX SIBLING, NEVER AN OVERLAY — see `chrome.css`. An overlaid sidebar leaves the wall measuring a
 * width it does not have, and every tile then wraps its lines in the wrong column.
 *
 * KEYBOARD: one tab-stop for the whole tree (`tabIndex` on the list, `aria-activedescendant`-style cursor
 * on a row), then arrows to move, Enter to reveal, Left/Right to collapse and expand. A per-row tab-stop
 * would put twenty-four tab-stops between the wall's controls and anything after it, and — worse on this
 * page — it would put a focusable element next to a terminal for every Terminal in the Fleet.
 */

import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { SideResizer, useSideDock } from './SideDock';
import { Button } from '@/components/ui/button';
import { SIDEBAR_WIDTH_PX } from '../theme';
import type { Terminal } from '../panelsClient';
import {
  allBranchKeys,
  buildTree,
  describeBranch,
  flattenTree,
  type BranchHealth,
  type RollupState,
  type TreeRow,
} from './tree';
import './chrome.css';

export interface SidebarTreeProps {
  inventory: readonly Terminal[];
  /** Ids materialised onto the wall right now. */
  onWall: ReadonlySet<string>;
  /** Ids the streamer says are a live PTY attach. */
  live: ReadonlySet<string>;
  /** Scroll a tile into view and flash it. Never promotes it — see the header. */
  onReveal(id: string): void;
  /** Narrow the wall to what a branch holds, by writing a slot selector. The tree offers it on a node
   * row because "show me only this Machine" is the commonest thing an operator wants after finding a
   * Machine in a tree, and doing it by hand is: open the selector editor, type the name, close it. */
  onSelectNode?(machine: string): void;
  open: boolean;
  onToggle(): void;
}

/**
 * Four channels of difference per state, matching `HealthChips` exactly — hue, fill, border style AND
 * glyph.
 *
 * `unknown` is dashed and unfilled for the reason slice 3 records: a lighter green is how "we don't
 * know" reads as "fine" at a glance, and a sidebar is skimmed even harder than a wall. The vocabulary is
 * deliberately identical to the chips' so that one visual language covers the whole page.
 */
const STATE_CLASS: Record<RollupState, string> = {
  ok: 'border-solid border-emerald-500/60 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300',
  bad: 'border-solid border-red-500/70 bg-red-500/15 text-red-700 dark:text-red-300',
  unknown: 'border-dashed border-zinc-400/80 bg-transparent text-zinc-500 dark:text-zinc-400',
};

const STATE_GLYPH: Record<RollupState, string> = { ok: '●', bad: '▲', unknown: '?' };

/** The rollup, never alone: the glyph carries the state and the number beside it carries the reading, so
 * a branch can never be summarised as a bare colour (ADR 0020's "never collapsed into one light"). */
function Rollup({ health, scope }: { health: BranchHealth; scope: string }): JSX.Element {
  const label =
    health.failing > 0
      ? `${health.failing}/${health.total}`
      : health.unmeasured > 0
        ? `${health.unmeasured}?`
        : `${health.total}`;
  return (
    <span
      data-testid={`tree-rollup-${scope}`}
      data-state={health.state}
      data-total={health.total}
      data-failing={health.failing}
      data-live={health.live}
      title={describeBranch(health)}
      className={`inline-flex shrink-0 items-center gap-1 rounded border px-1 text-[10px] leading-4 ${STATE_CLASS[health.state]}`}
    >
      <span aria-hidden="true">{STATE_GLYPH[health.state]}</span>
      <span>{label}</span>
    </span>
  );
}

export default memo(function SidebarTree({
  inventory,
  onWall,
  live,
  onReveal,
  onSelectNode,
  open,
  onToggle,
}: SidebarTreeProps): JSX.Element {
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());
  /** The keyboard cursor, as a ROW KEY rather than an index: an index moves under the operator every
   * time the 30-second inventory refresh adds a Machine above it. */
  const [cursor, setCursor] = useState<string | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);

  const tree = useMemo(() => buildTree({ inventory, onWall, live }), [inventory, onWall, live]);
  const rows = useMemo(() => flattenTree(tree, collapsed), [tree, collapsed]);

  const cursorIndex = rows.findIndex((r) => r.key === cursor);

  const toggleBranch = useCallback((key: string, force?: boolean): void => {
    setCollapsed((prev) => {
      const next = new Set(prev);
      const shouldCollapse = force ?? !prev.has(key);
      if (shouldCollapse) next.add(key);
      else next.delete(key);
      return next;
    });
  }, []);

  const activate = useCallback(
    (row: TreeRow): void => {
      setCursor(row.key);
      if (row.kind === 'window') onReveal(row.node.id);
      else toggleBranch(row.key);
    },
    [onReveal, toggleBranch]
  );

  // Keep the cursor on a row that still exists. A Machine that left the Fleet must not leave the arrow
  // keys pointing at nothing — which presents as a tree that has stopped responding.
  useEffect(() => {
    if (cursor !== null && !rows.some((r) => r.key === cursor)) setCursor(rows[0]?.key ?? null);
  }, [cursor, rows]);

  const onKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLDivElement>): void => {
      if (rows.length === 0) return;
      const at = cursorIndex < 0 ? 0 : cursorIndex;
      const row = rows[at];
      const move = (to: number): void => {
        const clamped = Math.max(0, Math.min(rows.length - 1, to));
        setCursor(rows[clamped]?.key ?? null);
        e.preventDefault();
      };
      switch (e.key) {
        case 'ArrowDown':
        case 'j':
          return move(cursorIndex < 0 ? 0 : at + 1);
        case 'ArrowUp':
        case 'k':
          return move(cursorIndex < 0 ? 0 : at - 1);
        case 'Home':
          return move(0);
        case 'End':
          return move(rows.length - 1);
        case 'ArrowRight':
          if (row?.expandable) {
            toggleBranch(row.key, false);
            e.preventDefault();
          }
          return;
        case 'ArrowLeft':
          if (row?.expandable) {
            toggleBranch(row.key, true);
            e.preventDefault();
          }
          return;
        case 'Enter':
        case ' ':
          if (row) {
            activate(row);
            e.preventDefault();
          }
          return;
        default:
          return;
      }
    },
    [activate, cursorIndex, rows, toggleBranch]
  );

  /* WIDTH AND SIDE ONLY. Whether the tree is open is the PAGE's state and stays there (`open` /
     `onToggle`): the wall re-fits every tile when it changes, so the Dashboard has to know, and a
     second copy of that fact in storage is how the two would eventually disagree. */
  const dock = useSideDock('monitor-tree', {
    width: SIDEBAR_WIDTH_PX,
    side: 'left',
    collapsed: false,
  });

  if (!open) {
    // CLOSED BY DEFAULT, and the toggle is a persistent affordance rather than a hidden one. `theme.ts`'s
    // `SIDEBAR_DEFAULT_OPEN` has the measurement: an open tree narrows every tile by its own width, and the
    // two-column density exists precisely to keep a journal line from wrapping. The Terminal count rides on
    // the button so the tree is worth opening before it is opened.
    return (
      <Button
        variant="outline"
        size="sm"
        data-testid="sidebar-open"
        title={
          `Show the Machines tree: all ${inventory.length} Terminal${inventory.length === 1 ? '' : 's'} the ` +
          'inventory reports, whether or not a slot matched them. It takes its width from the wall, so ' +
          'tiles get narrower while it is open.'
        }
        className="h-full w-7 self-stretch px-0 text-[11px]"
        onClick={onToggle}
      >
        <span className="[writing-mode:vertical-rl]">
          ☰ Machines {tree.reduce((n, mode) => n + mode.nodes.length, 0)}
        </span>
      </Button>
    );
  }

  const branches = allBranchKeys(tree);
  const everythingCollapsed = branches.length > 0 && branches.every((k) => collapsed.has(k));
  /** Machines, which is what the header says: one per NODE, across every mode. */
  const machineCount = tree.reduce((n, mode) => n + mode.nodes.length, 0);

  const aside = (
    <aside
      // THE WIDTH IS THE OPERATOR'S, not `theme.ts`'s. The constant is still the DEFAULT and its
      // arithmetic still holds — a sidebar takes its width off the wall, and the two-column density
      // exists to keep a journal line from wrapping — but which trade an operator wants depends on
      // whether they are reading machine names or reading journals, and only they know that.
      style={{ flex: `0 0 ${dock.width}px`, width: dock.width, minWidth: dock.width }}
      className="kontra-sidebar flex flex-col gap-1 rounded border p-1"
      data-testid="sidebar"
      data-rows={rows.length}
      data-side={dock.side}
      aria-label="Machines tree"
    >
      <div className="flex items-center gap-1 px-1">
        {/* NOT "Fleet". This tree holds every Terminal the inventory reports, across all three
            execution modes — so the header that said Fleet put `local` inside a panel named after
            the one mode it is not, and the count beside the word read as part of a name. A fleet
            is a thing a run provisions and it appears BELOW, under its own mode row, carrying the
            name the stack gave it. */}
        <span className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
          Machines
        </span>
        {/* THE NOUN THE HEADER SAYS. This counted TERMINALS — so a single host running five panes
            read as "MACHINES 5", and the number under a word it does not count is worse than no
            number: it is an inventory figure an operator will quote. Machines here, Terminals in
            the title, because both are worth knowing and only one of them is what the word means. */}
        <span
          className="text-[11px] text-muted-foreground"
          data-testid="sidebar-total"
          data-machines={machineCount}
          data-terminals={inventory.length}
          title={
            `${machineCount} machine${machineCount === 1 ? '' : 's'}, ` +
            `${inventory.length} terminal${inventory.length === 1 ? '' : 's'} across them`
          }
        >
          {machineCount}
        </span>
        <span className="ml-auto flex items-center gap-1">
          <Button
            variant="ghost"
            size="sm"
            data-testid="sidebar-collapse-all"
            title={everythingCollapsed ? 'expand every branch' : 'collapse every branch'}
            className="h-6 px-1 text-[11px]"
            onClick={() => setCollapsed(everythingCollapsed ? new Set() : new Set(branches))}
          >
            {everythingCollapsed ? '＋' : '－'}
          </Button>
          <Button
            variant="ghost"
            size="sm"
            data-testid="sidebar-flip"
            title={`move the tree to the ${dock.side === 'left' ? 'right' : 'left'} of the wall`}
            className="h-6 px-1 text-[11px]"
            onClick={dock.flip}
          >
            {dock.side === 'left' ? '⇥' : '⇤'}
          </Button>
          <Button
            variant="ghost"
            size="sm"
            data-testid="sidebar-close"
            title="hide the tree (the wall gets its width back, and the tiles re-fit)"
            className="h-6 px-1 text-[11px]"
            onClick={onToggle}
          >
            ✕
          </Button>
        </span>
      </div>

      {inventory.length === 0 ? (
        <p className="px-1 text-[11px] text-muted-foreground">
          No Terminals. A Machine with no session is still a Terminal, so an empty tree means no
          Machines anywhere — `kontra serve` puts one on this host, and a workflow that provisions a
          fleet puts them on Machines.
        </p>
      ) : (
        <div
          ref={listRef}
          role="tree"
          aria-label="Terminals by mode, node, session and window"
          tabIndex={0}
          // ONE tab-stop for the tree, and the keys are handled here rather than per row. On a Fleet of
          // twelve Machines a per-row tab-stop would be ~40 stops, every one of them a focusable
          // element sitting beside a read-only terminal.
          onKeyDown={onKeyDown}
          className="flex flex-col outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
        >
          {rows.map((row) => {
            const isCursor = row.key === cursor;
            const expanded = row.expandable ? !collapsed.has(row.key) : undefined;
            const label = row.node.label;
            const stakes = row.kind === 'mode' ? row.node.stakes : null;
            /**
             * A FLEET IS NAMED BY THE RUN THAT PROVISIONED IT, and the name only exists when one
             * did. `machinesFromStack` reads it off the Pulumi stack; `local.ts` leaves it empty on
             * purpose. So the row says `fleet · sweep-aug` where a workflow made a fleet, and the
             * bare mode word where the answer is not one name — a second fleet in the same
             * inventory is a real state, and picking one of the two to display would be a guess.
             */
            const fleets = row.kind === 'mode' ? row.node.fleets : [];
            const shown = fleets.length === 1 ? `${label} · ${fleets[0]}` : label;
            return (
              <div
                key={row.key}
                role="treeitem"
                aria-level={row.depth + 1}
                aria-expanded={expanded}
                aria-selected={isCursor}
                data-testid={`tree-row-${row.key}`}
                data-kind={row.kind}
                data-depth={row.depth}
                data-on-wall={row.kind === 'window' ? String(row.node.onWall) : undefined}
                data-live={row.kind === 'window' && row.node.live ? 'true' : undefined}
                data-cursor={isCursor ? 'true' : undefined}
                className={`kontra-tree-row cursor-pointer rounded px-1 py-[1px] text-[11px] hover:bg-accent ${
                  isCursor ? 'bg-accent' : ''
                }`}
                style={{ paddingLeft: `${4 + row.depth * 10}px` }}
                onClick={() => activate(row)}
              >
                <span className="flex min-w-0 items-center gap-1">
                  {row.expandable && (
                    <span aria-hidden="true" className="w-2 shrink-0 text-muted-foreground">
                      {expanded ? '▾' : '▸'}
                    </span>
                  )}
                  {row.kind === 'window' && (
                    // Which leaves are actually on the wall, at a glance. A filled marker is a tile you
                    // can reveal; a hollow one is a Terminal no slot matched, and the fix for that is a
                    // selector, not a Machine.
                    <span
                      aria-hidden="true"
                      className={`w-2 shrink-0 ${row.node.onWall ? 'text-foreground' : 'text-muted-foreground/50'}`}
                    >
                      {row.node.onWall ? '▪' : '▫'}
                    </span>
                  )}
                  <span
                    className={`truncate ${row.kind === 'window' ? 'font-normal' : 'font-medium'} ${
                      row.kind === 'window' && !row.node.onWall ? 'text-muted-foreground' : ''
                    }`}
                    title={
                      row.kind === 'window'
                        ? `${row.node.id}\n\nClick to scroll to this tile and flash it. It does NOT go live — a live attach is an sshd session, a PTY and a tmux session on the Machine, so it takes the tile's own "Go live" control.${row.node.onWall ? '' : '\n\nNo slot on this Dashboard matches this Terminal, so it has no tile yet.'}`
                        : row.kind === 'mode'
                          ? `${stakes?.sentence ?? ''}${
                              fleets.length > 1
                                ? `\n\nTwo fleets are in this inventory: ${fleets.join(', ')}.`
                                : ''
                            }\n\n${describeBranch(row.node.health)}`
                          : describeBranch(row.node.health)
                    }
                  >
                    {shown}
                  </span>
                  {row.kind === 'window' && row.node.live && (
                    <span
                      data-testid={`tree-live-${row.node.id}`}
                      title="a live PTY attach: an sshd session and a tmux session on this Machine right now"
                      className="shrink-0 rounded border border-primary/60 bg-primary/10 px-1 text-[9px] leading-4 text-primary"
                    >
                      live
                    </span>
                  )}
                  {row.kind === 'mode' && stakes?.level === 'worker' && (
                    // ADR 0020's asymmetry, stated in the UI as CONTRACT.md's slice 6 amendment
                    // requires: on the fleet a crashed tmux server costs the view; here it costs a
                    // running Worker.
                    <span
                      data-testid={`tree-stakes-${row.key}`}
                      title={stakes.sentence}
                      className="shrink-0 rounded border border-amber-500/60 bg-amber-500/10 px-1 text-[9px] leading-4 text-amber-700 dark:text-amber-300"
                    >
                      holds Workers
                    </span>
                  )}
                </span>

                <span className="flex shrink-0 items-center gap-1">
                  {row.kind === 'node' && onSelectNode && (
                    <button
                      type="button"
                      data-testid={`tree-only-${row.key}`}
                      title={`add a slot showing only ${label}`}
                      className="rounded border px-1 text-[9px] leading-4 text-muted-foreground hover:bg-accent"
                      onClick={(e) => {
                        e.stopPropagation();
                        onSelectNode(label);
                      }}
                    >
                      only
                    </button>
                  )}
                  {row.kind === 'window' ? (
                    <span
                      data-testid={`tree-state-${row.node.id}`}
                      data-state={row.node.state}
                      title={row.node.terminal.health.detail ?? `all four signals: ${row.node.state}`}
                      className={`inline-flex shrink-0 items-center rounded border px-1 text-[9px] leading-4 ${STATE_CLASS[row.node.state]}`}
                    >
                      <span aria-hidden="true">{STATE_GLYPH[row.node.state]}</span>
                    </span>
                  ) : (
                    <Rollup health={row.node.health} scope={row.key} />
                  )}
                </span>
              </div>
            );
          })}
        </div>
      )}

      <p className="mt-auto px-1 pt-1 text-[10px] leading-tight text-muted-foreground">
        Clicking a window scrolls to its tile. It does not go live — that is the tile’s own control,
        because an attach costs the Machine an <code>sshd</code> session and a PTY.
      </p>
    </aside>
  );

  /* The handle is on the edge the tree is NOT docked to — see `SideDock.tsx`. `order` moves the
     whole group across the wall without `DashboardPage` knowing which side it is on. */
  const handle = (
    <SideResizer
      width={dock.width}
      onWidth={dock.setWidth}
      side={dock.side}
      label="the Machines tree"
      testid="sidebar-resizer"
    />
  );
  return (
    <div className="flex min-h-0 self-stretch" style={{ order: dock.side === 'left' ? -1 : 1 }}>
      {dock.side === 'left' ? (
        <>
          {aside}
          {handle}
        </>
      ) : (
        <>
          {handle}
          {aside}
        </>
      )}
    </div>
  );
});
