/**
 * The Monitor, WIRED — the socket, the subscription set, and what a reconnect does to it.
 *
 * THIS IS THE SURFACE THE POLLING AND RESUBSCRIBE BUGS LIVE ON, and until the suite had a DOM none
 * of them was reachable. The page's own header records three that were found in a browser instead:
 * a `subscribed` set that belonged to the page rather than to the socket, so under StrictMode the
 * surviving socket subscribed nothing and one tile in four stayed blank; an early return on an
 * unchanged measurement, which left a remounted tile permanently unsubscribed; and a hidden pane
 * that kept costing a `capture-pane` every three seconds because nothing called `sync`. Every one
 * of those is a call the page did or did not make on a socket — invisible to a pure module and to
 * `renderToStaticMarkup` alike.
 *
 * THE SOCKET IS A FAKE, DRIVEN BY THE TEST. `FakeSocket` records what the page sends and lets a
 * test decide when it opens, what it delivers and when it drops — which is the only way to assert
 * "each wanted Terminal subscribed exactly once on the new socket, no loss and no duplication".
 *
 * THE TILE IS A STUB, AND IT IS A SEAM RATHER THAN A CONVENIENCE. `TerminalTile` owns an xterm,
 * which reaches for a canvas jsdom does not implement — but more to the point, what the page needs
 * from a tile is a MEASUREMENT and four callbacks. The stub reports a size on mount exactly as the
 * real tile's `addon-fit` does, which is what puts an id into `wanted` at all.
 */

import { createElement, useEffect } from 'react';
import { act, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Terminal, TerminalHealth } from './panelsClient';

/* ── the seam ──────────────────────────────────────────────────────────────────────────────── */

const fetchTerminals = vi.fn();
const fetchTicket = vi.fn();

vi.mock('./panelsClient', async (actual) => ({
  ...(await actual<typeof import('./panelsClient')>()),
  fetchTerminals: (...a: unknown[]) => fetchTerminals(...a),
  fetchTicket: (...a: unknown[]) => fetchTicket(...a),
  panelBase: () => 'http://localhost:8090',
}));

/**
 * What a tile is, to this page: something that measures itself and offers four controls.
 *
 * IT REPORTS NOTHING AT 0×0, which is `TerminalTile.measureNow`'s own rule verbatim (`if (!term.cols
 * || !term.rows) return null`) and is the half of the zero-size guard that actually holds. The page's
 * `wantedSubscriptions` asks only whether an id has been MEASURED, so a stub that reported `0, 0`
 * would be a tile the real one cannot be — and the test built on it would fail against a page that
 * is correct. A tile with no entry here reports 90×15; one entered as 0×0 reports nothing at all.
 */
const MEASURE = new Map<string, { cols: number; rows: number }>();

vi.mock('./TerminalTile', () => ({
  default: ({
    terminal,
    mode,
    onMeasure,
    onFocus,
    onBlur,
    onHide,
    onZoom,
    zoomed,
  }: {
    terminal: Terminal;
    mode: string;
    onMeasure(id: string, cols: number, rows: number): void;
    onFocus(id: string, cols: number, rows: number): void;
    onBlur(id: string): void;
    onHide?(id: string): void;
    onZoom?(id: string): void;
    zoomed?: boolean;
  }) => {
    const size = MEASURE.get(terminal.id) ?? { cols: 90, rows: 15 };
    useEffect(() => {
      if (size.cols && size.rows) onMeasure(terminal.id, size.cols, size.rows);
    }, [onMeasure, size.cols, size.rows, terminal.id]);
    return createElement(
      'div',
      { 'data-testid': `stub-tile-${terminal.id}`, 'data-mode': mode, 'data-zoomed': zoomed ? 'true' : undefined },
      createElement('button', {
        'data-testid': `go-live-${terminal.id}`,
        onClick: () => onFocus(terminal.id, size.cols, size.rows),
      }),
      createElement('button', {
        'data-testid': `go-snapshot-${terminal.id}`,
        onClick: () => onBlur(terminal.id),
      }),
      createElement('button', {
        'data-testid': `hide-${terminal.id}`,
        onClick: () => onHide?.(terminal.id),
      }),
      createElement('button', {
        'data-testid': `zoom-toggle-${terminal.id}`,
        onClick: () => onZoom?.(terminal.id),
      })
    );
  },
}));

/* ── the socket ────────────────────────────────────────────────────────────────────────────── */

type Sent = Record<string, unknown>;

class FakeSocket {
  static readonly OPEN = 1;
  static readonly CLOSED = 3;
  static instances: FakeSocket[] = [];

  readyState = 0;
  binaryType = '';
  onopen: (() => void) | null = null;
  onmessage: ((e: MessageEvent<unknown>) => void) | null = null;
  onerror: (() => void) | null = null;
  onclose: (() => void) | null = null;
  readonly sent: Sent[] = [];
  closed = false;

  constructor(readonly url: string) {
    FakeSocket.instances.push(this);
  }

  send(raw: string): void {
    this.sent.push(JSON.parse(raw) as Sent);
  }

  close(): void {
    this.closed = true;
    this.readyState = FakeSocket.CLOSED;
  }

  /** The streamer accepted the ticket. */
  open(): void {
    this.readyState = FakeSocket.OPEN;
    this.onopen?.();
  }

  deliver(msg: unknown): void {
    this.onmessage?.({ data: JSON.stringify(msg) } as MessageEvent<unknown>);
  }

  drop(): void {
    this.readyState = FakeSocket.CLOSED;
    this.onclose?.();
  }

  /** What it was asked to do, in order — the assertion surface for every test here. */
  ops(kind: string): string[] {
    return this.sent.filter((m) => m.t === kind).map((m) => String(m.id));
  }
}

const { useAppStore } = await import('../state/store');
const DashboardPage = (await import('./DashboardPage')).default;
const { LIVE_BUDGET, INVENTORY_REFRESH_MS } = await import('./DashboardPage');

/* ── fixtures ──────────────────────────────────────────────────────────────────────────────── */

const HEALTHY: TerminalHealth = {
  reachable: 'ok',
  session: 'present',
  poller: 'live',
  loads: 'ok',
};

function terminal(id: string, over: Partial<Terminal> = {}): Terminal {
  return {
    id,
    machine: id.split(':')[0] ?? id,
    host: '10.124.0.3',
    publicIp: '',
    tag: '',
    fleet: 'recon',
    actor: 'probe',
    version: '0.1.0',
    window: '0',
    health: HEALTHY,
    ...over,
  };
}

const WALL = [terminal('n1:probe:0'), terminal('n2:probe:0'), terminal('n3:probe:0')];

function live(): FakeSocket {
  const socket = FakeSocket.instances.at(-1);
  if (!socket) throw new Error('no socket was opened');
  return socket;
}

async function mount(): Promise<void> {
  render(createElement(DashboardPage));
  await settle();
}

async function settle(): Promise<void> {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

/** Mount, let the ticket land, and open the socket the page created. */
async function connected(): Promise<FakeSocket> {
  await mount();
  const socket = live();
  await act(async () => {
    socket.open();
  });
  return socket;
}

const initial = useAppStore.getState();

beforeEach(() => {
  vi.useFakeTimers();
  localStorage.clear();
  MEASURE.clear();
  FakeSocket.instances = [];
  useAppStore.setState(initial, true);
  vi.clearAllMocks();
  vi.stubGlobal('WebSocket', FakeSocket);
  fetchTerminals.mockResolvedValue(WALL);
  fetchTicket.mockResolvedValue({ ticket: 'tkt-1', expiresAt: Date.now() + 60_000 });
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

/* ── connecting ────────────────────────────────────────────────────────────────────────────── */

describe('coming up', () => {
  it('reads the inventory, mints a ticket, and opens ONE socket carrying it', async () => {
    await mount();
    expect(fetchTerminals).toHaveBeenCalledTimes(1);
    expect(fetchTicket).toHaveBeenCalledTimes(1);
    expect(FakeSocket.instances.length).toBe(1);
    expect(live().url).toContain('tkt-1');
  });

  it('subscribes every measured tile exactly once when the socket opens', async () => {
    const socket = await connected();
    expect(socket.ops('subscribe').sort()).toEqual(['n1:probe:0', 'n2:probe:0', 'n3:probe:0']);
    expect(socket.sent.filter((m) => m.t === 'subscribe')[0]).toMatchObject({ cols: 90, rows: 15 });
  });

  it('does not subscribe a tile that has not laid out', async () => {
    // A subscribe for an unmeasured tile would ask a Machine for a zero-sized screen. The rule is
    // split across two files — the tile reports nothing until it has real cols and rows, and
    // `wantedSubscriptions` wants only what has reported — and only a mounted tile joins them up.
    MEASURE.set('n2:probe:0', { cols: 0, rows: 0 });
    const socket = await connected();
    expect(socket.ops('subscribe')).not.toContain('n2:probe:0');
    expect(socket.ops('subscribe')).toContain('n1:probe:0');
  });

  it('says so, and opens nothing, when there are no Machines at all', async () => {
    fetchTerminals.mockResolvedValue([]);
    await mount();
    expect(screen.getByTestId('dashboard-error').textContent).toContain('No Terminals');
    expect(FakeSocket.instances.length).toBe(0);
  });

  it('keeps its error page and does NOT reconnect when the ticket mint fails', async () => {
    // "The Dashboard is switched off", not "a dropped socket" — the 503 fail-closed path relies on
    // no socket being opened at all.
    fetchTicket.mockRejectedValue(new Error('503 KONTRA_PANEL_TOKEN is not set'));
    await mount();
    expect(screen.getByTestId('dashboard-error').textContent).toContain('KONTRA_PANEL_TOKEN');
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(FakeSocket.instances.length).toBe(0);
  });

  it('publishes the inventory and the live count to the chrome', async () => {
    await connected();
    expect(useAppStore.getState().wall).toMatchObject({ panes: 3, live: 0 });
    expect(useAppStore.getState().panes.map((t) => t.id)).toEqual(WALL.map((t) => t.id));
  });

  it('gives up its live claim on unmount, because the sessions really are gone', async () => {
    const view = render(createElement(DashboardPage));
    await settle();
    await act(async () => {
      live().open();
    });
    view.unmount();
    expect(useAppStore.getState().wall.live).toBe(0);
    expect(live().closed).toBe(true);
  });
});

/* ── the inventory's own clock ─────────────────────────────────────────────────────────────── */

describe('the inventory poll', () => {
  it('re-reads on its own cadence and subscribes what arrived', async () => {
    const socket = await connected();
    fetchTerminals.mockResolvedValue([...WALL, terminal('n4:probe:0')]);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(INVENTORY_REFRESH_MS);
    });
    await settle();
    expect(socket.ops('subscribe')).toContain('n4:probe:0');
    // …and nothing already held is asked for twice. This is `resubscribePlan`'s whole contract,
    // asserted through the socket rather than against the function.
    expect(socket.ops('subscribe').filter((id) => id === 'n1:probe:0').length).toBe(1);
  });

  it('unsubscribes a Terminal that left the Fleet', async () => {
    const socket = await connected();
    fetchTerminals.mockResolvedValue([WALL[0]!, WALL[1]!]);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(INVENTORY_REFRESH_MS);
    });
    await settle();
    expect(socket.ops('unsubscribe')).toEqual(['n3:probe:0']);
  });

  it('treats a failed refresh as a notice, never as a blanked wall', async () => {
    await connected();
    fetchTerminals.mockRejectedValue(new Error('streamer unreachable'));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(INVENTORY_REFRESH_MS);
    });
    await settle();
    expect(screen.getByTestId('notices').textContent).toContain('could not refresh');
    expect(screen.queryByTestId('dashboard-error')).toBeNull();
    expect(screen.getByTestId('stub-tile-n1:probe:0')).toBeTruthy();
  });

  it('stops polling once the page is gone', async () => {
    const view = render(createElement(DashboardPage));
    await settle();
    view.unmount();
    const reads = fetchTerminals.mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(INVENTORY_REFRESH_MS * 3);
    });
    expect(fetchTerminals.mock.calls.length).toBe(reads);
  });
});

/* ── the reconnect ─────────────────────────────────────────────────────────────────────────── */

describe('a dropped socket', () => {
  it('says so, backs off, and comes back on its own', async () => {
    const first = await connected();
    await act(async () => {
      first.drop();
    });
    expect(screen.getByTestId('dashboard-reconnecting').textContent).toContain('reconnecting');
    // Nothing is live once the socket is gone: the streamer kills every grouped session with it.
    expect(useAppStore.getState().wall.live).toBe(0);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    await settle();
    expect(FakeSocket.instances.length).toBe(2);
  });

  it('resubscribes each wanted Terminal EXACTLY ONCE on the fresh socket', async () => {
    const first = await connected();
    await act(async () => {
      first.drop();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    await settle();
    const second = live();
    expect(second).not.toBe(first);
    await act(async () => {
      second.open();
    });
    // No loss and no duplication — the set travels with the socket, so a fresh one starts empty.
    expect(second.ops('subscribe').sort()).toEqual(['n1:probe:0', 'n2:probe:0', 'n3:probe:0']);
  });

  it('clears the banner once the new socket is open', async () => {
    const first = await connected();
    await act(async () => {
      first.drop();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    await settle();
    await act(async () => {
      live().open();
    });
    expect(screen.queryByTestId('dashboard-reconnecting')).toBeNull();
  });

  it('schedules ONE reconnect for an error immediately followed by a close', async () => {
    const first = await connected();
    await act(async () => {
      first.onerror?.();
      first.drop();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    await settle();
    expect(FakeSocket.instances.length).toBe(2);
  });

  it('does not resurrect a socket for a page that has gone', async () => {
    const view = render(createElement(DashboardPage));
    await settle();
    const first = live();
    await act(async () => {
      first.open();
    });
    view.unmount();
    await act(async () => {
      first.drop();
      await vi.advanceTimersByTimeAsync(30_000);
    });
    expect(FakeSocket.instances.length).toBe(1);
  });

  it('ignores a superseded socket closing', async () => {
    const first = await connected();
    fetchTerminals.mockResolvedValue([...WALL, terminal('n4:probe:0')]);
    await act(async () => {
      first.drop();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    await settle();
    await act(async () => {
      live().open();
    });
    expect(screen.queryByTestId('dashboard-reconnecting')).toBeNull();
    await act(async () => {
      // The old socket, arriving late. It is not `connRef.current`, so it must change nothing.
      first.onclose?.();
    });
    expect(screen.queryByTestId('dashboard-reconnecting')).toBeNull();
  });
});

/* ── the live budget ───────────────────────────────────────────────────────────────────────── */

describe('going live', () => {
  const many = ['n1', 'n2', 'n3', 'n4', 'n5'].map((n) => terminal(`${n}:probe:0`));

  it('asks the streamer to focus the Terminal, at the size the tile measured', async () => {
    const socket = await connected();
    await act(async () => {
      screen.getByTestId('go-live-n1:probe:0').click();
    });
    expect(socket.sent.find((m) => m.t === 'focus')).toMatchObject({
      id: 'n1:probe:0',
      cols: 90,
      rows: 15,
    });
  });

  it('demotes the oldest promotion past the budget, and says which', async () => {
    fetchTerminals.mockResolvedValue(many);
    const socket = await connected();
    for (const t of many) {
      await act(async () => {
        screen.getByTestId(`go-live-${t.id}`).click();
      });
    }
    expect(socket.ops('focus').length).toBe(LIVE_BUDGET + 1);
    expect(socket.ops('blur')).toEqual(['n1:probe:0']);
    expect(screen.getByTestId('notices').textContent).toContain('back to snapshots');
  });

  it('refuses to promote a tile that has not measured a size', async () => {
    MEASURE.set('n2:probe:0', { cols: 0, rows: 0 });
    const socket = await connected();
    await act(async () => {
      screen.getByTestId('go-live-n2:probe:0').click();
    });
    expect(socket.ops('focus')).toEqual([]);
    expect(screen.getByTestId('notices').textContent).toContain('has not measured a size yet');
  });

  it('follows the STREAMER’s answer rather than its own optimism', async () => {
    const socket = await connected();
    await act(async () => {
      screen.getByTestId('go-live-n1:probe:0').click();
    });
    expect(useAppStore.getState().wall.live).toBe(1);
    await act(async () => {
      socket.deliver({ t: 'state', id: 'n1:probe:0', mode: 'snapshot', health: HEALTHY });
    });
    expect(useAppStore.getState().wall.live).toBe(0);
  });

  it('treats an error carrying an id as "that Terminal is not live"', async () => {
    const socket = await connected();
    await act(async () => {
      screen.getByTestId('go-live-n1:probe:0').click();
    });
    await act(async () => {
      socket.deliver({ t: 'error', id: 'n1:probe:0', message: 'attach refused' });
    });
    expect(useAppStore.getState().wall.live).toBe(0);
    expect(screen.getByTestId('notices').textContent).toContain('attach refused');
  });

  it('leaves the live set alone for an error about the socket rather than a Terminal', async () => {
    const socket = await connected();
    await act(async () => {
      screen.getByTestId('go-live-n1:probe:0').click();
    });
    await act(async () => {
      socket.deliver({ t: 'error', message: 'the byte budget is low' });
    });
    expect(useAppStore.getState().wall.live).toBe(1);
  });

  it('records elided bytes rather than swallowing them', async () => {
    const socket = await connected();
    await act(async () => {
      socket.deliver({ t: 'elided', id: 'n1:probe:0', bytes: 4096 });
    });
    // The drawer and the status bar read it; the assertion here is that the message was accepted at
    // all, which nothing could reach before.
    expect(screen.queryByTestId('dashboard-error')).toBeNull();
  });
});

/* ── hiding, which is the one that costs the Fleet less ────────────────────────────────────── */

describe('hiding a pane', () => {
  it('unsubscribes it, so the streamer stops taking its capture-pane', async () => {
    const socket = await connected();
    await act(async () => {
      screen.getByTestId('hide-n2:probe:0').click();
    });
    expect(socket.ops('unsubscribe')).toEqual(['n2:probe:0']);
    expect(screen.queryByTestId('stub-tile-n2:probe:0')).toBeNull();
    // Hidden must not look like gone: the count, the name and the way back stay on the page.
    expect(screen.getByTestId('unhide-n2:probe:0')).toBeTruthy();
    expect(screen.getByTestId('hidden-panes').textContent).toContain('1 pane hidden');
  });

  it('gives up a live attach with it', async () => {
    const socket = await connected();
    await act(async () => {
      screen.getByTestId('go-live-n2:probe:0').click();
    });
    await act(async () => {
      screen.getByTestId('hide-n2:probe:0').click();
    });
    expect(socket.ops('blur')).toEqual(['n2:probe:0']);
    expect(screen.getByTestId('notices').textContent).toContain('you hid this pane');
  });

  it('re-subscribes once the restored tile has measured itself again', async () => {
    const socket = await connected();
    await act(async () => {
      screen.getByTestId('hide-n2:probe:0').click();
    });
    await act(async () => {
      screen.getByTestId('unhide-n2:probe:0').click();
    });
    await settle();
    expect(socket.ops('subscribe').filter((id) => id === 'n2:probe:0').length).toBe(2);
  });

  it('survives a reload, because a wall nobody can curate twice is curated once', async () => {
    await connected();
    await act(async () => {
      screen.getByTestId('hide-n2:probe:0').click();
    });
    expect(localStorage.getItem('kontra-monitor-hidden')).toContain('n2:probe:0');
  });
});

/* ── zoom, which was `chrome/zoom.ts` ──────────────────────────────────────────────────────── */

describe('zooming one pane', () => {
  /*
   * THESE ASSERTIONS CAME OFF `chrome/zoom.ts`, whose whole body was `current === id ? null : id`
   * under twenty-five lines of rationale. The rationale is the page's — it is about what a zoom is
   * on a wall of read-only Terminals — and with a DOM the toggle can be pinned where it happens:
   * through the control, against the overlay it produces. `zoom.test.ts` asserted the return value
   * of a ternary; this asserts that clicking the control zooms, that clicking it again restores,
   * that a second pane MOVES the zoom rather than adding one, and that the zoomed tile leaves the
   * wall — the last of which the pure function could not express at all, and which is the one that
   * matters: two tiles for one id fight over the frame writer and one receives nothing.
   */

  it('draws the pane over the wall and takes its wall tile away', async () => {
    await connected();
    await act(async () => {
      screen.getByTestId('zoom-toggle-n2:probe:0').click();
    });
    expect(screen.getByTestId('zoom-n2:probe:0')).toBeTruthy();
    // Exactly one tile for the id — the overlay's. The wall keeps the other two.
    expect(screen.getAllByTestId('stub-tile-n2:probe:0').length).toBe(1);
    expect(screen.getByTestId('stub-tile-n2:probe:0').dataset.zoomed).toBe('true');
    expect(screen.getByTestId('stub-tile-n1:probe:0')).toBeTruthy();
  });

  it('restores the wall when the same pane is clicked again', async () => {
    await connected();
    await act(async () => {
      screen.getByTestId('zoom-toggle-n2:probe:0').click();
    });
    await act(async () => {
      screen.getByTestId('zoom-toggle-n2:probe:0').click();
    });
    expect(screen.queryByTestId('zoom-n2:probe:0')).toBeNull();
    expect(screen.getByTestId('stub-tile-n2:probe:0').dataset.zoomed).toBeUndefined();
  });

  it('MOVES the zoom to another pane rather than showing two', async () => {
    await connected();
    await act(async () => {
      screen.getByTestId('zoom-toggle-n2:probe:0').click();
    });
    await act(async () => {
      screen.getByTestId('zoom-toggle-n3:probe:0').click();
    });
    expect(screen.queryByTestId('zoom-n2:probe:0')).toBeNull();
    expect(screen.getByTestId('zoom-n3:probe:0')).toBeTruthy();
  });

  it('puts NOTHING on the socket — a zoom cannot break read-only', async () => {
    const socket = await connected();
    const before = socket.sent.length;
    await act(async () => {
      screen.getByTestId('zoom-toggle-n2:probe:0').click();
    });
    expect(socket.sent.length).toBe(before);
  });

  it('offers no HIDE control on the overlay, which has no bar to be restored from', async () => {
    // The zoomed tile is handed `onZoom` and deliberately not `onHide` — hiding the one pane on
    // screen would leave an operator looking at nothing, with the way back drawn behind it.
    await connected();
    await act(async () => {
      screen.getByTestId('zoom-toggle-n2:probe:0').click();
    });
    const socket = live();
    const before = socket.sent.length;
    await act(async () => {
      screen.getByTestId('hide-n2:probe:0').click();
    });
    expect(screen.getByTestId('zoom-n2:probe:0')).toBeTruthy();
    expect(socket.sent.length).toBe(before);
  });

  it('restores the wall when the zoomed Terminal leaves the Fleet', async () => {
    await connected();
    await act(async () => {
      screen.getByTestId('zoom-toggle-n3:probe:0').click();
    });
    fetchTerminals.mockResolvedValue([WALL[0]!, WALL[1]!]);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(INVENTORY_REFRESH_MS);
    });
    await settle();
    expect(screen.queryByTestId('zoom-n3:probe:0')).toBeNull();
  });
});
