import { WallEmpty } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="bg-background text-foreground rounded-lg p-4">{children}</div>
    </div>
  );
}

/**
 * What an empty wall says.
 *
 * NOT A HOLE THE COLOUR OF A TERMINAL, which is what this replaced and which is
 * indistinguishable from a wall that failed to load — on the exact surface an operator opens to
 * find out whether anything is running. So it names both ways a Terminal comes into existence, and
 * then names the one failure that produces this screen while everything looks up: the panels
 * streamer is a forked child of `orchestrator-infra`, and that container being up does not mean
 * the child is.
 *
 * It is a separate export from `TileWall` for a MEASURED reason — the page renders it instead of
 * mounting a wall with an empty inventory, because a wall syncs its layout to the inventory it is
 * handed and the first paint's inventory is `[]`. Mounting one here wiped the operator's saved
 * arrangement to disk before the real Terminals arrived.
 */
export function NoTerminals() {
  return (
    <Frame>
      <WallEmpty />
    </Frame>
  );
}

/**
 * Where the Monitor actually draws it: inside the `terminal-pending` rectangle, on the terminal
 * palette rather than on the app's card colour. The page keeps that frame so that a Dashboard with
 * no Terminal is still visibly a page — and the dashed border has to hold its own against a black
 * ground, not just against `bg-background`.
 */
export function OnTheTerminalGround() {
  return (
    <Frame>
      <div
        className="flex flex-col items-stretch justify-start rounded border border-border p-3"
        style={{ background: '#0b0b0e', height: '200px' }}
      >
        <WallEmpty />
      </div>
    </Frame>
  );
}

/**
 * The width it gets with the sidebar tree open, which is when an operator is most likely to be
 * looking at it — they opened the tree to find the Machine that is missing. Four sentences and two
 * `<code>` spans in a 300-pixel column is the real test of this paragraph: it wraps, and no
 * command it names is allowed to break across a line into something un-copyable.
 */
export function Narrow() {
  return (
    <Frame>
      <div style={{ width: '300px' }}>
        <WallEmpty />
      </div>
    </Frame>
  );
}
