import { SidebarTree } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="bg-background text-foreground rounded-lg p-4">{children}</div>
    </div>
  );
}

const noop = () => {};

/** Ids follow the slice-6 grammar `<mode>:<node>/<session>/<window>` — the tree's four
 *  levels are mode → node → session → window, and two of them exist only inside the id. */
function terminal(
  id: string,
  actor: string,
  health: Record<string, string>,
  extra: Record<string, unknown> = {},
) {
  const [, rest = ''] = id.split(':');
  const node = rest.split('/')[0] ?? '';
  return {
    id,
    machine: node,
    host: `${node}.kontra.internal`,
    publicIp: '10.124.0.7',
    role: 'worker',
    campaign: 'full-scope',
    actor,
    version: 'v0.3.1',
    window: '0',
    health,
    lastSnapshotAt: 1_760_000_000_000,
    ...extra,
  } as never;
}

const inventory = [
  terminal('fleet:kontra-1/kontra-subfinder/0', 'subfinder', {
    reachable: 'ok',
    session: 'present',
    poller: 'live',
    loads: 'ok',
  }),
  terminal('fleet:kontra-2/kontra-subfinder/0', 'subfinder', {
    reachable: 'ok',
    session: 'present',
    poller: 'live',
    loads: 'ok',
  }),
  terminal('fleet:kontra-9/kontra-httpx/0', 'httpx', {
    reachable: 'ok',
    session: 'present',
    poller: 'none',
    loads: 'failing',
    detail: 'No poller on task queue kontra-httpx for 4m12s',
  }),
  terminal('fleet:kontra-11/kontra-crawler/0', 'crawler', {
    reachable: 'unknown',
    session: 'unknown',
    poller: 'unknown',
    loads: 'unknown',
  }),
  terminal('local:laptop/kontra-crawler/0', 'crawler', {
    reachable: 'ok',
    session: 'present',
    poller: 'live',
    loads: 'ok',
  }),
];

/** The tree open: mode → node → session → window, each branch carrying its own rollup. */
export function Open() {
  return (
    <Frame>
      <div className="h-[420px]">
        <SidebarTree
          inventory={inventory}
          onWall={new Set(['fleet:kontra-1/kontra-subfinder/0', 'fleet:kontra-9/kontra-httpx/0'])}
          live={new Set(['fleet:kontra-1/kontra-subfinder/0'])}
          onReveal={noop}
          onSelectNode={noop}
          open
          onToggle={noop}
        />
      </div>
    </Frame>
  );
}

/**
 * Collapsed — the default. `theme.ts`'s SIDEBAR_DEFAULT_OPEN has the measured reason:
 * a tree open by default costs the wall width an operator came for.
 */
export function Collapsed() {
  return (
    <Frame>
      <div className="h-[220px]">
        <SidebarTree
          inventory={inventory}
          onWall={new Set()}
          live={new Set()}
          onReveal={noop}
          onSelectNode={noop}
          open={false}
          onToggle={noop}
        />
      </div>
    </Frame>
  );
}

/**
 * A branch is never summarised as a bare colour: the glyph carries the state and the
 * number beside it carries the reading — `1/4` failing, `2?` unmeasured, `4` all fine.
 */
export function BranchRollups() {
  return (
    <Frame>
      <div className="h-[420px]">
        <SidebarTree
          inventory={[
            ...inventory,
            terminal('fleet:kontra-12/kontra-crawler/0', 'crawler', {
              reachable: 'fail',
              session: 'no-tmux',
              poller: 'none',
              loads: 'unknown',
              detail: 'SSH to 10.124.0.12 timed out after 10s',
            }),
          ]}
          onWall={new Set()}
          live={new Set()}
          onReveal={noop}
          onSelectNode={noop}
          open
          onToggle={noop}
        />
      </div>
    </Frame>
  );
}
