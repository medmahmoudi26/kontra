import { HealthChips, TileHeader, TileWall, WidgetView } from '@kontra/frontend';

function Frame({ children }: { children: React.ReactNode }) {
  return (
    <div className="dark">
      <div className="bg-background text-foreground rounded-lg p-4">{children}</div>
    </div>
  );
}

const noop = () => {};

/**
 * The Monitor hands the wall a definite height — it is a flex sibling of the sidebar in a
 * full-height page — so a card has to say so too, or the canvas, which is `flex-1`, has nothing to
 * be one of. One row unit is 132 px plus the 12 px gap, and that 132 was MEASURED against the
 * chrome a tile carries: two banner lines and the health chips leave about five rows of terminal.
 */
function Wall({ rows, children }: { rows: number; children: React.ReactNode }) {
  return (
    <div className="flex flex-col" style={{ height: `${rows * 144 - 12}px` }}>
      {children}
    </div>
  );
}

/**
 * The wall reads its rectangles from `useWall`, which owns `localStorage`. A card cannot mount a
 * hook, so this is the same API as a static document: `sync` is a no-op, which is exactly what it
 * is in the product whenever the inventory has not changed.
 */
function wallOf(tiles: Array<{ id: string; x: number; y: number; w: number; h: number }>) {
  return {
    tiles,
    warning: null,
    dismissWarning: noop,
    sync: noop,
    move: noop,
    resize: noop,
    compact: noop,
    reset: noop,
  } as never;
}

/** `<mode>:<node>/<session>/<window>` — slice 6's id grammar. The mode and the session exist only
 *  inside the id, which is why the tile parses it rather than reading a field. */
function refOf(id: string) {
  const [mode = '?', rest = ''] = id.split(':');
  const [node = '?', session = '?', window = '?'] = rest.split('/');
  return { mode, node, session, window, wellFormed: true };
}

interface Fixture {
  id: string;
  machine: string;
  host: string;
  publicIp: string;
  actor: string;
  version: string;
  window: string;
  health: Record<string, string>;
  /** What `addon-fit` measures for the rectangle this Terminal is drawn in. */
  cols: number;
  rows: number;
  live: boolean;
  scrolledBack: number;
  held: boolean;
  elided: number;
  widget: { title: string; seq: string; body: string } | null;
}

const OK = { reachable: 'ok', session: 'present', poller: 'live', loads: 'ok' };
const NO_POLLER = {
  reachable: 'ok',
  session: 'present',
  poller: 'none',
  loads: 'failing',
  detail:
    'No poller on task queue kontra-crawler for 4m12s · 81 of 82 resource loads failed on 10.124.0.11',
};
function fixture(over: Partial<Fixture> & { id: string }): Fixture {
  const ref = refOf(over.id);
  return {
    machine: ref.node,
    host: ref.node,
    publicIp: '164.92.71.18',
    actor: 'subfinder',
    version: '0.3.1',
    window: '0',
    health: OK,
    cols: 54,
    rows: 13,
    live: false,
    scrolledBack: 0,
    held: false,
    elided: 0,
    widget: null,
    ...over,
  };
}

const SUBFINDER = fixture({
  id: 'fleet:kontra-4/kontra-subfinder/0',
  publicIp: '164.92.71.18',
  widget: {
    title: 'subfinder — batch 41 of 119',
    seq: '41',
    body: [
      '**37,412** subdomains from **119** apexes.',
      '',
      '| source | found |',
      '| --- | --- |',
      '| crtsh | 21,884 |',
      '| wayback | 6,118 |',
      '',
      'Two apexes returned `SERVFAIL` and were isolated, not dropped.',
    ].join('\n'),
  },
});

const HTTPX = fixture({
  id: 'fleet:kontra-9/kontra-httpx/0',
  publicIp: '164.92.71.22',
  actor: 'httpx',
  version: '1.4.2',
  cols: 56,
  rows: 5,
  live: true,
  elided: 524_288,
});

const CRAWLER = fixture({
  id: 'fleet:kontra-11/kontra-crawler/0',
  publicIp: '164.92.71.31',
  actor: 'crawler',
  version: '0.9.0',
  health: NO_POLLER,
  cols: 112,
  rows: 5,
  scrolledBack: 340,
  held: true,
});

const LOCAL = fixture({
  id: 'local:laptop/kontra-crawler/0',
  host: 'laptop',
  publicIp: '',
  actor: 'crawler',
  version: '0.9.0',
  cols: 56,
  rows: 5,
});

const ARRANGED = [SUBFINDER, HTTPX, CRAWLER, LOCAL];

/**
 * The wall as the operator dragged it: the crawler put next to the handler it feeds, the two
 * sweepers side by side above them. Rectangles, not slots — a saved query could never express
 * "these two, together".
 */
const ARRANGED_TILES = [
  { id: SUBFINDER.id, x: 0, y: 0, w: 6, h: 2 },
  { id: HTTPX.id, x: 6, y: 0, w: 6, h: 1 },
  { id: LOCAL.id, x: 6, y: 1, w: 6, h: 1 },
  { id: CRAWLER.id, x: 0, y: 2, w: 12, h: 1 },
];

/** The same failure on two Machines, on two rectangles — this is the one wall where the Machines
 *  are alike on purpose, because what a tile can afford to SAY is a property of its size. */
function failing(node: string) {
  return {
    reachable: 'ok',
    session: 'present',
    poller: 'none',
    loads: 'failing',
    detail: `No poller on task queue kontra-crawler for 4m12s · 81 of 82 resource loads failed on ${node}`,
  };
}

const DENSITY = [
  fixture({
    id: 'fleet:kontra-12/kontra-crawler/0',
    publicIp: '164.92.71.44',
    actor: 'crawler',
    version: '0.9.0',
    health: failing('10.124.0.12'),
    cols: 112,
    rows: 5,
  }),
  fixture({
    id: 'fleet:kontra-13/kontra-crawler/0',
    publicIp: '164.92.71.57',
    actor: 'crawler',
    version: '0.9.0',
    health: failing('10.124.0.13'),
    cols: 112,
    rows: 19,
  }),
];

/** Three rows — 420 px — is exactly where a tile can afford both detail sentences, which is also
 *  the size an operator drags one to when they mean to read it. Two rows is still DENSE. */
const DENSITY_TILES = [
  { id: DENSITY[0].id, x: 0, y: 0, w: 12, h: 1 },
  { id: DENSITY[1].id, x: 0, y: 1, w: 12, h: 3 },
];

/**
 * One tile's contents, which the WALL never knows: `renderTile` is what lets the banner be the
 * grab point without this file knowing what a tile's chrome looks like.
 *
 * In the product this is `TerminalTile`, which owns an xterm and a WebSocket subscription. Nothing
 * connects inside a captured card, so the screens are the real thing they are before a frame
 * arrives — the terminal ground — and one pane is announcing a document, which is a Terminal state
 * that needs no socket at all. Everything above the screen is the shipped chrome: `TileHeader` and
 * `HealthChips`, wired exactly as `TerminalTile` wires them.
 */
function tileOf(inventory: Fixture[]) {
  const byId = new Map(inventory.map((t) => [t.id, t]));
  return function renderTile(
    terminal: { id: string },
    context: { compact: boolean; dense: boolean; dragHandle: (e: never) => void }
  ) {
    const t = byId.get(terminal.id);
    if (!t) return null;
    const { compact, dense, dragHandle } = context;
    return (
      <>
        <TileHeader
          id={t.id}
          ref_={refOf(t.id) as never}
          host={t.host}
          ip={t.publicIp}
          actor={t.actor}
          version={t.version}
          cols={t.cols}
          rows={t.rows}
          live={t.live}
          scrolledBack={t.scrolledBack}
          held={t.held}
          elided={t.elided}
          compact={compact}
          onGoLive={noop}
          onStopLive={noop}
          onCopy={noop}
          onConverge={noop}
          onOpenDrawer={noop}
          onJumpToTail={noop}
          onDragHandle={dragHandle as never}
        />
        <HealthChips
          health={t.health as never}
          terminalId={t.id}
          dense={dense}
          className={`shrink-0 px-2 pt-1 ${
            compact ? 'max-h-[19px] overflow-hidden [&>ul]:flex-nowrap' : ''
          }`}
        />
        {t.widget ? (
          <div className="m-1 mt-0 flex min-h-0 flex-1 flex-col overflow-hidden rounded">
            <WidgetView
              widget={{ kind: 'markdown', ...t.widget } as never}
              terminalId={t.id}
              fallbackTitle={`${t.machine} · ${t.window}`}
              className="min-h-0 flex-1"
            />
          </div>
        ) : (
          <div
            className="m-1 mt-0 min-h-0 flex-1 overflow-hidden rounded"
            style={{ background: '#0b0b0e' }}
          />
        )}
      </>
    );
  };
}

/**
 * The wall an operator arranged, and the four readings it keeps apart at a glance.
 *
 * A TILE IS A RECTANGLE YOU DRAG BY ITS BANNER AND RESIZE BY ITS CORNER — the corner is drawn by
 * the wall because it belongs to the rectangle, and the banner is drawn by the tile because it
 * belongs to the Terminal. A drag moves `left`/`top` and never a `transform`: `addon-fit`'s
 * measured cols/rows is what the streamer puts into `stty` before `tmux attach`, and a scaled
 * container makes that measurement a lie about the remote pane's shape.
 *
 * The tall tile is announcing a document rather than a screen; the one beside it holds a real PTY
 * attach and has bytes the streamer's cap dropped; under that is a `local` pane, whose tmux server
 * holds the actual Worker rather than a journal; and the tile dragged the full width of the wall is
 * scrolled back with its repaints HELD, on a queue nothing is polling.
 */
export function Arranged() {
  return (
    <Frame>
      <Wall rows={3}>
        <TileWall
          wall={wallOf(ARRANGED_TILES)}
          inventory={ARRANGED as never}
          renderTile={tileOf(ARRANGED) as never}
        />
      </Wall>
    </Frame>
  );
}

/**
 * WHAT A TILE SAYS IS A FUNCTION OF ITS RECTANGLE, and the WALL is what measures it — `DENSE_PX`
 * lives in this file, not in the tile, because only the wall knows how big it made one.
 *
 * Two Machines failing the same way, one row tall and three rows tall. The short one is DENSE, so
 * its health detail is clipped to a single line: two sentences on a `WALL_ROW_PX` tile were 34 px
 * of about 150 px of terminal, spent on prose that repeats verbatim on every tile of the same
 * Machine. Clipped VISUALLY only — both sentences stay in the DOM and move to the container's
 * `title`, so an operator hovering, the drawer and a spec all still get the whole reading. The tall
 * one is the size an operator drags a tile to when they actually mean to read it: both sentences,
 * and room left over for the output that made them true.
 *
 * The four chips are never dropped at any density. They are the tri-state, and `unknown` reading as
 * `ok` is the whole thing they exist to prevent.
 */
export function Density() {
  return (
    <Frame>
      <Wall rows={4}>
        <TileWall
          wall={wallOf(DENSITY_TILES)}
          inventory={DENSITY as never}
          renderTile={tileOf(DENSITY) as never}
        />
      </Wall>
    </Frame>
  );
}

/**
 * The sidebar sent an operator here. The revealed tile takes a ring that arrives fast and fades
 * slowly — a REVEAL, not a selection: there is no per-tile selected state on this wall, and a
 * permanent highlight would imply one.
 */
export function Revealed() {
  return (
    <Frame>
      <Wall rows={3}>
        <TileWall
          wall={wallOf(ARRANGED_TILES)}
          inventory={ARRANGED as never}
          renderTile={tileOf(ARRANGED) as never}
          revealed={CRAWLER.id}
        />
      </Wall>
    </Frame>
  );
}

/**
 * A filter is a VIEW, NOT AN EDIT, and that is the whole reason `hidden` is a separate prop rather
 * than a shorter inventory. The wall syncs its saved document to the inventory it is handed, so
 * passing a filtered list would DELETE every hidden tile's rectangle — the operator's arrangement,
 * thrown away by typing in a search box. The document still holds all four; only two are drawn,
 * and they are re-settled so a filter matching two Machines does not leave holes where the others
 * were.
 */
export function FilteredView() {
  return (
    <Frame>
      <Wall rows={2}>
        <TileWall
          wall={wallOf(ARRANGED_TILES)}
          inventory={ARRANGED as never}
          renderTile={tileOf(ARRANGED) as never}
          hidden={new Set([HTTPX.id, CRAWLER.id])}
        />
      </Wall>
    </Frame>
  );
}

/**
 * A filter that matches nothing is NOT an empty wall, and this is the sentence that keeps the two
 * apart. Saying `WallEmpty`'s words here would tell an operator their Fleet is gone when they have
 * only mistyped a hostname — so this one counts what is hidden and offers the way back.
 */
export function FilterMatchesNothing() {
  return (
    <Frame>
      <Wall rows={1}>
        <TileWall
          wall={wallOf(ARRANGED_TILES)}
          inventory={ARRANGED as never}
          renderTile={tileOf(ARRANGED) as never}
          hidden={new Set(ARRANGED.map((t) => t.id))}
        />
      </Wall>
    </Frame>
  );
}
