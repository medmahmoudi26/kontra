import { PaneThroughput, TileHeader } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="rounded-lg bg-background p-4 text-foreground">{children}</div>
    </div>
  );
}

/**
 * THE CARD'S CLOCK, COMPRESSED — the one thing here that is not the product's.
 *
 * The component owns its own sampler: it drains the byte counter once a second, and below two
 * samples it deliberately draws NOTHING. A static card therefore captures a component that has
 * been alive for a few hundred milliseconds, which is a permanently empty 46px slot — true, and
 * useless as a picture of what this is.
 *
 * So the 1s sampler runs fast enough to fill one window (48 samples ≈ a minute and a half of
 * output) and is then stopped, freezing the card on a full chart. THE NUMBERS ARE UNTOUCHED:
 * every point below is a second of bytes a tile really receives. Only the wait is skipped.
 */
const g = globalThis as unknown as {
  setInterval: (fn: () => void, ms?: number) => number;
  clearInterval: (id: number) => void;
};
const realSetInterval = g.setInterval.bind(globalThis);
const realClearInterval = g.clearInterval.bind(globalThis);
g.setInterval = (fn: () => void, ms?: number): number => {
  if (ms !== 1000) return realSetInterval(fn, ms);
  let ticks = 0;
  const id = realSetInterval(() => {
    fn();
    if ((ticks += 1) >= 48) realClearInterval(id);
  }, 6);
  return id;
};

/**
 * A stand-in for the byte path in `TerminalTile`: each frame adds its length to a counter, and
 * this component drains that counter once a second and zeroes it. So the series is BYTES THAT
 * ARRIVED, never a synthesised curve — the counter here hands back the next second of a real
 * shape (a journal tail: a steady trickle of lines, with a spike when a resolver batch lands),
 * and swallows the component's reset exactly the way a live tile's byte path overwrites it.
 */
function bytePath(perSecond: readonly number[]) {
  let i = 0;
  return {
    get current() {
      return perSecond[i++ % perSecond.length] ?? 0;
    },
    set current(_zeroed: number) {
      /* the component resets after each drain; the next frame refills it */
    },
  } as never;
}

/**
 * One window's worth — 48 seconds of bytes, which is exactly what the rolling series keeps.
 * subfinder tailing its journal: a couple of KB a second of found hosts, with a spike each time
 * a resolver batch lands and the pane prints a screen at once.
 */
const BUSY = [
  704, 960, 1_216, 1_840, 2_240, 1_664, 18_432, 6_144, 2_048, 1_216, 960, 1_408,
  2_240, 3_072, 1_664, 1_216, 22_016, 8_192, 3_072, 1_920, 1_408, 1_024, 896, 1_216,
  1_664, 2_240, 12_288, 4_480, 1_920, 1_408, 1_024, 768, 1_216, 1_664, 2_048, 1_408,
  960, 704, 1_216, 9_216, 3_584, 1_664, 1_024, 832, 1_216, 1_536, 960, 768,
];
/** probe printing one status line per target — steady, small, never bursty. */
const STEADY = [
  512, 640, 448, 1_024, 576, 384, 704, 512, 608, 448, 736, 512,
  576, 640, 448, 512, 704, 832, 576, 448, 512, 640, 384, 576,
  704, 512, 448, 640, 576, 384, 512, 704, 448, 576, 640, 512,
  384, 448, 576, 704, 512, 640, 448, 512, 576, 384, 640, 512,
];
/** A pane that printed nothing for a minute and a half. A real reading, and it draws FLAT ALONG
 *  THE BOTTOM — which is why fewer than two samples must draw nothing at all: a flat line is a
 *  measured zero, and "nobody has been watching yet" is not one. */
const SILENT = new Array(48).fill(0) as number[];

const noop = () => {};

function ref_(node: string, session: string) {
  return { mode: 'fleet', node, session, window: '0', wellFormed: true } as const;
}

/**
 * Where it lives: the right end of a tile banner's first row, between the hostname and the
 * live/snapshot control. `counter` is a REF rather than a prop because a prop that changed on
 * every frame would re-render this banner at the frame rate of the busiest Machine on the wall
 * — only the seven-pixel svg re-renders, once a second.
 */
export function InTheTileBanner() {
  return (
    <Frame>
      <TileHeader
        id="fleet:kontra-6/kontra-subfinder/0"
        ref_={ref_('kontra-6', 'kontra-subfinder')}
        host="kontra-subfinder-6"
        ip="10.124.0.6"
        actor="subfinder"
        version="0.3.1"
        cols={120}
        rows={32}
        live
        scrolledBack={0}
        elided={0}
        compact={false}
        bytes={bytePath(BUSY)}
        onGoLive={noop}
        onStopLive={noop}
        onCopy={noop}
        onConverge={noop}
        onOpenDrawer={noop}
      />
    </Frame>
  );
}

/**
 * Three panes on one wall, which is how the chart is actually read: not for a number, but to
 * tell a Machine that is working from one that is quiet WITHOUT reading three journals. Each
 * line is normalised to its own maximum, so the shapes are comparable and the heights are not
 * — the bottom pane printed nothing at all this minute.
 */
export function AcrossTheWall() {
  const rows = [
    { id: 'fleet:kontra-6/kontra-subfinder/0', node: 'kontra-6', host: 'kontra-subfinder-6', ip: '10.124.0.6', actor: 'subfinder', version: '0.3.1', session: 'kontra-subfinder', series: BUSY },
    { id: 'fleet:kontra-4/kontra-probe/0', node: 'kontra-4', host: 'kontra-probe-4', ip: '10.124.0.4', actor: 'probe', version: '0.2.0', session: 'kontra-probe', series: STEADY },
    { id: 'fleet:kontra-7/kontra-probe/0', node: 'kontra-7', host: 'kontra-probe-7', ip: '10.124.0.7', actor: 'probe', version: '0.2.0', session: 'kontra-probe', series: SILENT },
  ];
  return (
    <Frame>
      <div className="flex flex-col gap-2">
        {rows.map((r) => (
          <div key={r.id} className="overflow-hidden rounded-md border border-border">
            <TileHeader
              id={r.id}
              ref_={ref_(r.node, r.session)}
              host={r.host}
              ip={r.ip}
              actor={r.actor}
              version={r.version}
              cols={120}
              rows={32}
              live={r.series === BUSY}
              scrolledBack={0}
              elided={0}
              compact={false}
              bytes={bytePath(r.series)}
              onGoLive={noop}
              onStopLive={noop}
              onCopy={noop}
              onConverge={noop}
              onOpenDrawer={noop}
            />
          </div>
        ))}
      </div>
    </Frame>
  );
}

/**
 * The chart on its own, out of the banner, at the size it is really drawn: 46 × 13 px, muted,
 * one per pane. Nothing is labelled and no axis is implied — the sparkline says SHAPE, and the
 * identity beside it says whose.
 */
export function OnItsOwn() {
  const rows = [
    { id: 'kontra-6', label: 'subfinder@0.3.1 · 10.124.0.6', series: BUSY },
    { id: 'kontra-4', label: 'probe@0.2.0 · 10.124.0.4', series: STEADY },
    { id: 'kontra-7', label: 'probe@0.2.0 · 10.124.0.7', series: SILENT },
  ];
  return (
    <Frame>
      <div className="flex flex-col gap-2">
        {rows.map((r) => (
          <div
            key={r.id}
            className="flex items-center gap-2 rounded-md border border-border bg-card px-2 py-1.5"
          >
            <code className="truncate text-[11px]">{r.label}</code>
            <span className="ml-auto flex shrink-0 items-center">
              <PaneThroughput counter={bytePath(r.series)} id={r.id} />
            </span>
          </div>
        ))}
      </div>
    </Frame>
  );
}

/**
 * One banner that has the chart, and the two ways it goes away — which are not the same thing.
 *
 * TOP — measured: 46px of sparkline between the hostname and the live control.
 *
 * MIDDLE — a tile too narrow to spare those 46px. `compact` drops the chart outright: a chart
 * nobody can read is not worth the hostname it truncates.
 *
 * BOTTOM — a tile with NO byte path at all (`bytes` is optional; a local `kontra actor serve
 * … --tmux` pane is drawn from snapshots nothing counts). Nothing was measured, so nothing is
 * drawn — and never a flat line, because a flat line is a MEASUREMENT: it says this pane
 * printed zero bytes, which is a different sentence from "we are not counting this one".
 */
export function WhenThereIsNoChart() {
  return (
    <Frame>
      <div className="flex flex-col gap-2">
        <div className="overflow-hidden rounded-md border border-border">
          <TileHeader
            id="fleet:kontra-6/kontra-subfinder/0"
            ref_={ref_('kontra-6', 'kontra-subfinder')}
            host="kontra-subfinder-6"
            ip="10.124.0.6"
            actor="subfinder"
            version="0.3.1"
            cols={120}
            rows={32}
            live={false}
            scrolledBack={0}
            elided={0}
            compact={false}
            bytes={bytePath(BUSY)}
            onGoLive={noop}
            onStopLive={noop}
            onCopy={noop}
            onConverge={noop}
            onOpenDrawer={noop}
          />
        </div>
        <div className="overflow-hidden rounded-md border border-border" style={{ width: 210 }}>
          <TileHeader
            id="fleet:kontra-6/kontra-subfinder/0"
            ref_={ref_('kontra-6', 'kontra-subfinder')}
            host="kontra-subfinder-6"
            ip="10.124.0.6"
            actor="subfinder"
            version="0.3.1"
            cols={80}
            rows={24}
            live={false}
            scrolledBack={0}
            elided={0}
            compact
            bytes={bytePath(BUSY)}
            onGoLive={noop}
            onStopLive={noop}
            onCopy={noop}
            onConverge={noop}
            onOpenDrawer={noop}
          />
        </div>
        <div className="overflow-hidden rounded-md border border-border">
          <TileHeader
            id="local:host/kontra-serve-probe/0"
            ref_={{ mode: 'local', node: 'host', session: 'kontra-serve-probe', window: '0', wellFormed: true }}
            host="controller"
            ip=""
            actor="probe"
            version="0.2.0"
            cols={120}
            rows={32}
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
        </div>
      </div>
    </Frame>
  );
}
