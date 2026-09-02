import { StatusBar } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="bg-background text-foreground rounded-lg p-4">{children}</div>
    </div>
  );
}

/** `stats` is a getter, not a value: it is read on every tick so frame-rate values never re-render the wall. */
const stats = (s: Record<string, unknown>) => () => s as never;

/** A wall that is working: every tile painted, live tiles inside budget. */
export function Healthy() {
  return (
    <Frame>
      <StatusBar
        phase="open"
        stats={stats({
          tiles: 12,
          inventory: 12,
          live: 3,
          budget: 4,
          painted: 12,
          oldestFrameAgeMs: 2_400,
          oldestId: 'fleet:kontra-4/kontra-subfinder/0',
          elided: 0,
        })}
      />
    </Frame>
  );
}

/**
 * A stale wall. Past STALE_SNAPSHOT_MS (20s ≈ four missed passes) the age stops reading
 * as normal — and it names WHICH Terminal is oldest, so "something is stale" has an address.
 */
export function StaleSnapshot() {
  return (
    <Frame>
      <StatusBar
        phase="open"
        stats={stats({
          tiles: 12,
          inventory: 14,
          live: 4,
          budget: 4,
          painted: 11,
          oldestFrameAgeMs: 47_000,
          oldestId: 'fleet:kontra-9/kontra-httpx/0',
          elided: 1_048_576,
        })}
      />
    </Frame>
  );
}

/** Nothing has painted yet — shown as `no frame yet` rather than an invented zero. */
export function NoFramesYet() {
  return (
    <Frame>
      <StatusBar
        phase="connecting"
        stats={stats({
          tiles: 8,
          inventory: 8,
          live: 0,
          budget: 4,
          painted: 0,
          oldestFrameAgeMs: null,
          oldestId: null,
          elided: 0,
        })}
      />
    </Frame>
  );
}

/** The socket is gone: the phase word is the whole story. */
export function Disconnected() {
  return (
    <Frame>
      <StatusBar
        phase="disconnected"
        stats={stats({
          tiles: 12,
          inventory: 12,
          live: 0,
          budget: 4,
          painted: 12,
          oldestFrameAgeMs: 130_000,
          oldestId: 'fleet:kontra-2/kontra-crawler/0',
          elided: 262_144,
        })}
      />
    </Frame>
  );
}
