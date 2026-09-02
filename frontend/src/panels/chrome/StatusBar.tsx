/**
 * The status bar: what the wall is costing, and how stale it is (slice 7a).
 *
 * ITS JOB IS TO MAKE THE LIVENESS POLICY LEGIBLE. ADR 0020's mechanism is easy to state and invisible in
 * a screenshot: the wall is SNAPSHOTS — one `capture-pane` exec per node every few seconds paints every
 * tile on that node — and only a focused or explicitly pinned Terminal is a live PTY attach, because
 * twelve Machines cost twelve periodic execs rather than twenty-four persistent PTYs and twenty-four
 * `sshd` sessions on `s-1vcpu-2gb` boxes. An operator who does not know that reads a snapshot wall as a
 * broken live one ("why is it three seconds behind?") and reads the live budget as an arbitrary limit.
 * Four numbers and one sentence fix both.
 *
 * IT TICKS ITSELF, AND THAT IS A PERFORMANCE DECISION. Snapshot age has to advance once a second. If the
 * page owned that clock, every tick would re-render `DashboardPage`, `DashboardGrid` and up to
 * `WALL_TILE_CAP` tiles — a wall re-reconciled every second forever, for a number in a footer. So the
 * page hands over a GETTER that reads its refs, and the interval lives here: the clock re-renders exactly
 * this component. The same reason the frame path uses a subscription instead of React state.
 *
 * SNAPSHOT AGE IS OBSERVED IN THE BROWSER, NOT READ FROM THE INVENTORY. `Terminal.lastSnapshotAt` exists
 * and is the streamer's own timestamp, but it only arrives with `GET /api/panels/terminals` — which this
 * page polls every 30 seconds — so an age derived from it would itself be up to 30 seconds stale, and
 * "the oldest snapshot is 28s old" would be indistinguishable from "the inventory is 28s old". What the
 * page can state without qualification is when it last RECEIVED a frame for a Terminal, so that is what
 * is measured, and the label says `no frame yet` rather than inventing a zero.
 */

import { memo, useEffect, useState } from 'react';
import { ageWords, byteWords } from './format';

export interface WallStats {
  /** Tiles on the wall, which is the Terminals this page is SUBSCRIBED to — panes an operator hid
   *  are not among them. `painted` is compared against this, and a hidden pane never paints because
   *  it is not subscribed, so counting it here would report a deliberate tidy-up as N broken tiles. */
  tiles: number;
  /** Terminals in the inventory — the wall may show fewer, and the difference matters. */
  inventory: number;
  live: number;
  budget: number;
  /** Ids that have received at least one frame. */
  painted: number;
  /** Milliseconds since the OLDEST tile's last frame; `null` when no tile has ever had one. */
  oldestFrameAgeMs: number | null;
  /** …and which Terminal that is, so "something is stale" has an address. */
  oldestId: string | null;
  /** Bytes the streamer's cap dropped, across the wall. */
  elided: number;
}

export interface StatusBarProps {
  /** Read on every tick. A getter rather than props because the values it reads change at frame rate and
   * must not re-render the wall — see the header. */
  stats(): WallStats;
  /** What the page's socket is doing, in one word. */
  phase: string;
  /** Milliseconds between ticks. Injectable so a test does not have to wait a second. */
  tickMs?: number;
}

/**
 * When a snapshot age stops being normal.
 *
 * The streamer's default snapshot cadence is a few seconds, so anything under ~10 s is the wall working.
 * 20 s is roughly four missed passes: long enough that a slow `ssh` is not the explanation and something
 * — the ControlMaster, the node, the exec — is worth looking at. Amber rather than red because the wall
 * is lossy BY DESIGN and a red footer that is sometimes just a busy Machine teaches an operator to ignore
 * a red footer.
 */
export const STALE_SNAPSHOT_MS = 20_000;

export default memo(function StatusBar({ stats, phase, tickMs = 1000 }: StatusBarProps): JSX.Element {
  const [snapshot, setSnapshot] = useState<WallStats>(stats);

  useEffect(() => {
    // Read once immediately as well as on the interval: a bar that waits a full second before its first
    // reading shows "0 Terminals" on a wall that is already painting.
    setSnapshot(stats());
    const timer = setInterval(() => setSnapshot(stats()), tickMs);
    return () => clearInterval(timer);
  }, [stats, tickMs]);

  const stale = snapshot.oldestFrameAgeMs !== null && snapshot.oldestFrameAgeMs > STALE_SNAPSHOT_MS;
  const atBudget = snapshot.live >= snapshot.budget;

  return (
    <div
      className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded border bg-card px-2 py-1 text-[11px] text-muted-foreground"
      data-testid="status-bar"
      // DELIBERATELY NOT A LIVE REGION. `role="status"` here would be the obvious accessible choice and it
      // is the wrong one: the snapshot age changes every second, so a polite live region would have a screen
      // reader announcing "oldest snapshot 4s ago… 5s ago… 6s ago" over whatever the operator was actually
      // doing, forever. A labelled group that a reader can visit on purpose is the honest shape for a
      // continuously-changing readout; the things that DO warrant an announcement are events, and those go
      // to the notices list.
      aria-label="wall status"
    >
      <span data-testid="status-phase" data-phase={phase} className="font-medium">
        {phase === 'streaming' ? 'snapshots' : phase}
      </span>

      <span
        data-testid="status-terminals"
        data-tiles={snapshot.tiles}
        data-inventory={snapshot.inventory}
        title="Terminals this wall is subscribed to, out of every Terminal the Fleet inventory reports. A wall is allowed to show part of a Fleet — panes an operator has hidden are the difference, they are listed above the wall, and the sidebar tree shows all of it either way."
      >
        {snapshot.tiles} Terminal{snapshot.tiles === 1 ? '' : 's'} of {snapshot.inventory}
      </span>

      <span
        // The one number on this page with a REMOTE cost, and the tooltip is the cost model.
        data-testid="live-budget"
        data-live={snapshot.live}
        data-budget={snapshot.budget}
        className={atBudget ? 'text-primary' : undefined}
        title={
          'The wall is snapshots: one capture-pane exec per node paints every tile on it. A live tile is ' +
          'a PTY, an sshd session and a per-viewer tmux session on a 2 GB Machine, so the page holds a ' +
          'budget — promoting past it demotes the oldest rather than quietly attaching to everything. ' +
          'Going live is always a per-tile control, never a side effect of clicking.'
        }
      >
        {snapshot.live}/{snapshot.budget} live
      </span>

      <span
        data-testid="status-snapshot-age"
        data-age-ms={snapshot.oldestFrameAgeMs ?? ''}
        data-stale={stale ? 'true' : undefined}
        className={stale ? 'text-amber-700 dark:text-amber-300' : undefined}
        title={
          snapshot.oldestId
            ? `Oldest frame on the wall: ${snapshot.oldestId}. Measured in this browser — when a frame last arrived — not from the inventory, which is only re-read every 30 seconds.`
            : 'No tile has received a frame yet. A subscription waits for a real measurement, so a tile that has not measured itself has not asked for one.'
        }
      >
        oldest snapshot {ageWords(snapshot.oldestFrameAgeMs)}
        {snapshot.painted < snapshot.tiles && ` · ${snapshot.tiles - snapshot.painted} not painted yet`}
      </span>

      <span
        data-testid="status-elided"
        data-bytes={snapshot.elided}
        className={snapshot.elided > 0 ? 'text-amber-700 dark:text-amber-300' : undefined}
        title="output the streamer's per-subscription byte cap dropped. Reported rather than swallowed: a wall that silently skips output is a wall that lies about what a Worker printed."
      >
        {snapshot.elided > 0 ? `${byteWords(snapshot.elided)} elided` : 'nothing elided'}
      </span>

      <span className="ml-auto text-right">
        {/* Said here as well as in the page header, because this is the line an operator reads when they
            are deciding whether to trust what a tile shows. ADR 0020: a Terminal is not a record. */}
        lossy by construction — the Manifest, the journal on the node and the lake are the record
      </span>
    </div>
  );
});
