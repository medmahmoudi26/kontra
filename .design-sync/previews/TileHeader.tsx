import { TileHeader } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="bg-background text-foreground rounded-lg p-4">{children}</div>
    </div>
  );
}

/**
 * A tile is a rectangle with a border. The banner is the top of it, so it is drawn against the
 * frame it actually lives in rather than floating on the page — `bg-muted` reads as a banner only
 * when there is a `bg-card` tile under it.
 */
function Tile({ children }: { children: React.ReactNode }) {
  return <div className="overflow-hidden rounded border border-border bg-card">{children}</div>;
}

const fleetRef = {
  mode: 'fleet',
  node: 'kontra-4',
  session: 'kontra-subfinder',
  window: '0',
  wellFormed: true,
} as const;

const noop = () => {};

/**
 * The three names, on a Machine that is answering.
 *
 * `kontra-4` is what a person calls it, `164.92.71.18` is what `ssh` takes, and
 * `kontra-subfinder:0` is what `tmux attach -t` takes — and only the address is unambiguous when
 * two Fleets share a naming scheme. A snapshot tile at the tail: measured, not live.
 */
export function Snapshot() {
  return (
    <Frame>
      <Tile>
        <TileHeader
          id="fleet:kontra-4/kontra-subfinder/0"
          ref_={fleetRef}
          host="kontra-4"
          ip="164.92.71.18"
          actor="subfinder"
          version="0.3.1"
          cols={164}
          rows={43}
          live={false}
          scrolledBack={0}
          elided={0}
          compact={false}
          onGoLive={noop}
          onStopLive={noop}
          onCopy={noop}
          onConverge={noop}
          onOpenDrawer={noop}
        />
      </Tile>
    </Frame>
  );
}

/**
 * Attached to a live PTY — an sshd session, a PTY and a per-viewer tmux session on this node — with
 * bytes the streamer's cap dropped. The elided badge is shown and never swallowed: a tile that
 * silently skips output is a tile that lies about what a Worker printed.
 */
export function Live() {
  return (
    <Frame>
      <Tile>
        <TileHeader
          id="fleet:kontra-4/kontra-subfinder/0"
          ref_={fleetRef}
          host="kontra-4"
          ip="164.92.71.18"
          actor="subfinder"
          version="0.3.1"
          cols={164}
          rows={43}
          live
          scrolledBack={0}
          elided={524_288}
          compact={false}
          onGoLive={noop}
          onStopLive={noop}
          onCopy={noop}
          onConverge={noop}
          onOpenDrawer={noop}
        />
      </Tile>
    </Frame>
  );
}

/**
 * `local` mode raises the stakes, and it is the one tile with no address to copy.
 *
 * A local pane holds the actual actor and handler processes, so crashing that tmux server costs a
 * running Worker and not just the view — hence the amber mode badge, which a `fleet` tile does not
 * get. The address line says `local` rather than sitting blank, because an empty cell reads as a
 * field that failed to load; the session is then the only thing telling two Workers on this host
 * apart.
 */
export function LocalStakes() {
  return (
    <Frame>
      <Tile>
        <TileHeader
          id="local:laptop/kontra-crawler/0"
          ref_={{
            mode: 'local',
            node: 'laptop',
            session: 'kontra-crawler',
            window: '0',
            wellFormed: true,
          }}
          host="laptop"
          ip=""
          actor="crawler"
          version="0.9.0"
          cols={96}
          rows={24}
          live
          scrolledBack={0}
          elided={0}
          compact={false}
          onGoLive={noop}
          onStopLive={noop}
          onCopy={noop}
          onConverge={noop}
          onOpenDrawer={noop}
        />
      </Tile>
    </Frame>
  );
}

/**
 * Scrolled back, and scrolled back WITH THE REPAINTS HELD — the distinction the amber badge exists
 * for.
 *
 * Both tiles are showing old output while their stream keeps arriving, which on a wall of
 * near-identical journals is indistinguishable from a Machine that stopped printing. The held one
 * is showing it deliberately, so the screen under the reader is not replaced, and it takes the same
 * amber every other "older than now" state on this surface uses. Either badge is also the way back
 * to the tail.
 */
export function ScrolledBackAndHeld() {
  return (
    <Frame>
      <div className="flex flex-col gap-2">
        <Tile>
          <TileHeader
            id="fleet:kontra-4/kontra-subfinder/0"
            ref_={fleetRef}
            host="kontra-4"
            ip="164.92.71.18"
            actor="subfinder"
            version="0.3.1"
            cols={164}
            rows={43}
            live
            scrolledBack={1_200}
            elided={0}
            compact={false}
            onGoLive={noop}
            onStopLive={noop}
            onCopy={noop}
            onConverge={noop}
            onOpenDrawer={noop}
            onJumpToTail={noop}
          />
        </Tile>
        <Tile>
          <TileHeader
            id="fleet:kontra-9/kontra-httpx/0"
            ref_={{
              mode: 'fleet',
              node: 'kontra-9',
              session: 'kontra-httpx',
              window: '0',
              wellFormed: true,
            }}
            host="kontra-9"
            ip="164.92.71.22"
            actor="httpx"
            version="1.4.2"
            cols={120}
            rows={32}
            live={false}
            scrolledBack={340}
            held
            elided={1_835_008}
            compact={false}
            onGoLive={noop}
            onStopLive={noop}
            onCopy={noop}
            onConverge={noop}
            onOpenDrawer={noop}
            onJumpToTail={noop}
          />
        </Tile>
      </div>
    </Frame>
  );
}

/**
 * Two absences that are not blanks, on a `docker` pane.
 *
 * `0×0` is drawn as `—`: a tile that has not measured itself has not subscribed, and a `0×0` badge
 * would read as a measurement rather than as its absence. The actor is likewise dropped rather
 * than printed empty — this container holds no placement yet. What stays is the mode badge, because
 * a worker container's pane may hold the real process, so losing this tmux server can cost a
 * running Worker.
 */
export function NotMeasuredNoPlacement() {
  return (
    <Frame>
      <Tile>
        <TileHeader
          id="docker:kontra-worker-2/kontra-worker/0"
          ref_={{
            mode: 'docker',
            node: 'kontra-worker-2',
            session: 'kontra-worker',
            window: '0',
            wellFormed: true,
          }}
          host="kontra-worker-2"
          ip="10.124.0.9"
          actor=""
          version=""
          cols={0}
          rows={0}
          live={false}
          scrolledBack={0}
          elided={0}
          compact={false}
          onGoLive={noop}
          onStopLive={noop}
          onCopy={noop}
          onOpenDrawer={noop}
        />
      </Tile>
    </Frame>
  );
}

/**
 * Compact is the four-column wall: the actor, the geometry and the mode badge go, the identity and
 * the controls stay. Anything reachable only by widening a tile is a control an operator cannot
 * find — so `live`/`snapshot` and the menu survive at every width, and the second tile keeps its
 * elided badge even though there is barely room for it.
 */
export function Compact() {
  return (
    <Frame>
      <div className="flex flex-col gap-2" style={{ width: '300px' }}>
        <Tile>
          <TileHeader
            id="fleet:kontra-4/kontra-subfinder/0"
            ref_={fleetRef}
            host="kontra-4"
            ip="164.92.71.18"
            actor="subfinder"
            version="0.3.1"
            cols={64}
            rows={18}
            live
            scrolledBack={0}
            elided={0}
            compact
            onGoLive={noop}
            onStopLive={noop}
            onCopy={noop}
            onConverge={noop}
            onOpenDrawer={noop}
          />
        </Tile>
        <Tile>
          <TileHeader
            id="local:laptop/kontra-crawler/0"
            ref_={{
              mode: 'local',
              node: 'laptop',
              session: 'kontra-crawler',
              window: '0',
              wellFormed: true,
            }}
            host="laptop"
            ip=""
            actor="crawler"
            version="0.9.0"
            cols={0}
            rows={0}
            live={false}
            scrolledBack={0}
            elided={262_144}
            compact
            onGoLive={noop}
            onStopLive={noop}
            onCopy={noop}
            onConverge={noop}
            onOpenDrawer={noop}
          />
        </Tile>
      </div>
    </Frame>
  );
}
