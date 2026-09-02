import { TileMenu } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="bg-background text-foreground rounded-lg p-4">{children}</div>
    </div>
  );
}

const items = [
  { key: 'copy', label: 'Copy attach command', hint: 'tmux attach -t kontra-subfinder', run: () => {} },
  { key: 'drawer', label: 'Open detail', hint: 'health, ids, last frame', run: () => {} },
  { key: 'tail', label: 'Jump to tail', disabled: true, run: () => {} },
  {
    key: 'converge',
    label: 'Converge this Machine',
    hint: 'restarts the pane',
    emphasis: 'danger' as const,
    run: () => {},
  },
];

/**
 * The trigger at rest, at the right edge of a tile header — one per tile, which is the
 * only way this component is ever used.
 *
 * The OPEN menu cannot be captured statically: it opens on click, and it is positioned
 * `fixed` and measured per open so a tile's `overflow: hidden` cannot clip it. Building
 * with it: pass `items` with a stable `key` each, and mark the one action that costs a
 * Machine something with `emphasis: 'danger'` so it renders apart from the rest.
 */
export function InATileHeader() {
  return (
    <Frame>
      <div className="flex flex-col gap-2">
        {['kontra-4/kontra-subfinder', 'kontra-9/kontra-httpx', 'kontra-2/kontra-crawler'].map((id) => (
          <div
            key={id}
            className="flex items-center justify-between gap-3 rounded-md border border-border bg-card px-3 py-2"
          >
            <span className="font-mono text-xs">{id}/0</span>
            <TileMenu scope={`fleet:${id}/0`} items={items} triggerLabel="Tile actions" />
          </div>
        ))}
      </div>
    </Frame>
  );
}
