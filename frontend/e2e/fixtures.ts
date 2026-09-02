/**
 * Fixtures: one streamer per worker — stub or real, decided by the project — and one instrumented page
 * per test (ADR 0020, CONTRACT.md amendment 14).
 *
 * The instrumentation is the point. Three of this suite's claims are invisible in the DOM —
 * that the socket reached OPEN through the hand-rolled 101, that the geometry the page reported to
 * the streamer was not 0×0, and that nothing the page sends could ever carry bytes to a session — so
 * the fixture records every WebSocket frame in both directions and every uncaught page error.
 *
 * `pageErrors` is checked by {@link DashboardView.open} rather than at teardown: a view that throws
 * on mount is the failure this whole suite exists for, and it must be reported as itself instead of
 * as a locator that timed out.
 */

import { test as base, expect, type Locator, type Page } from '@playwright/test';
import { realControl, stubControl, type E2EOptions, type StreamerControl } from './control';
import { PANEL_PORT } from './env';
import { FIRST_TERMINAL } from './fakeFleet';
import { startFakeStreamer } from './streamer';
import { startStubStreamer } from './stubProcess';

/**
 * The one uncaught exception this suite tolerates, and why.
 *
 * `main.tsx` renders under `React.StrictMode`, so in dev every effect runs mount → cleanup → mount.
 * `DashboardPage`'s cleanup calls `term.dispose()`, and xterm 5.5 already has a frame queued: the
 * disposed instance's `Viewport.syncScrollArea` then reads `_renderService.dimensions` on a service
 * that is gone.
 *
 * MEASURED: it is dev-only. The same page from a production `vite build`, served by `vite preview`
 * and driven the same way, mounts with zero page errors — StrictMode's double invoke is what creates
 * the disposed instance. It is still worth fixing (a real unmount races the same way, which slice 4's
 * grid will do every time a tile leaves the wall), and the terminal that replaces it works: every
 * spec below paints through this.
 *
 * The pattern is narrow on purpose: any OTHER page exception still fails the suite.
 */
export const KNOWN_MOUNT_RACE = /reading 'dimensions'[\s\S]*syncScrollArea/;

/**
 * The one socket error this suite tolerates, and why.
 *
 * `React.StrictMode` runs the connect effect mount → cleanup → mount, and the cleanup calls `close()` on
 * a socket whose 101 has not completed yet. Chromium reports that as
 * "WebSocket is closed before the connection is established." — the page abandoning its own first
 * attempt, not a streamer that refused one. It appears only when the first page load is slow enough for
 * the cleanup to land mid-handshake (a cold vite dependency cache does it), which is exactly the kind of
 * timing an assertion must not depend on.
 *
 * A real failure still fails: a refused connection, a 401/403 on the upgrade, or an abnormal close all
 * carry different text, and the `hello` frame assertion proves a socket DID reach open.
 */
export const ABANDONED_SOCKET = /closed before the connection is established/;

/** One `{t:…}` message the page sent. Typed loosely on purpose: the suite asserts on what a real
 * browser actually put on the wire, including a message type the contract does not have. */
export interface SentMessage {
  t?: unknown;
  id?: unknown;
  cols?: unknown;
  rows?: unknown;
}

export interface DashboardView {
  readonly page: Page;
  /** WS URLs the page opened — the ticket travels here, and the token must not. */
  readonly socketUrls: string[];
  readonly socketErrors: string[];
  /** Socket errors minus {@link ABANDONED_SOCKET}. This is the list that must be empty. */
  unexpectedSocketErrors(): string[];
  /** Text frames the page sent, parsed. */
  readonly sent: SentMessage[];
  /** Text frames the page received (control JSON). */
  readonly received: string[];
  /** Binary frames the page received: `[1 byte idLen][id][payload]`. */
  readonly binary: Buffer[];
  readonly pageErrors: string[];
  /** Uncaught page exceptions minus {@link KNOWN_MOUNT_RACE}. This is the list that must be empty. */
  unexpectedPageErrors(): string[];
  /** Load the SPA and switch to the Dashboard. */
  open(): Promise<void>;
  tile(id?: string): Locator;
  /**
   * The terminal's rendered rows, inside the tile.
   *
   * Assertions about what a Machine printed belong here and not on the tile: the tile also carries
   * chrome (a size badge, a "Go live" control), and counting lines on the wrapper measured that chrome
   * as terminal output — which read as "the tile is appending screens" when it was not.
   */
  screen(): Locator;
  chip(signal: string): Locator;
  /**
   * The detail drawer, opened — where every health signal is readable in full.
   *
   * IT IS NOW THE ONLY PLACE ALL FOUR ARE. The tile's chip row was removed (it spent a row of every
   * Worker's output restating what a healthy pane always is), which ADR 0020 permits only because the
   * signals remain independently inspectable somewhere. This is that somewhere, so a spec asserting
   * the never-collapse rule asserts it here.
   */
  health(id?: string): Promise<Locator>;
  /** The tile's foot status line — the one health signal that is still ON the wall. */
  statusLine(id?: string): Locator;
  emptyTile(id?: string): Locator;
  /** The `subscribe` messages the page sent, in order. */
  subscribes(): SentMessage[];
  /** The payload of a received binary frame, with the id tag stripped. */
  payload(index: number): Buffer;
}

function instrument(page: Page): DashboardView {
  const socketUrls: string[] = [];
  const socketErrors: string[] = [];
  const sent: SentMessage[] = [];
  const received: string[] = [];
  const binary: Buffer[] = [];
  const pageErrors: string[] = [];

  // The stack, not just the message: "Cannot read properties of undefined" from inside an addon is
  // unidentifiable without it, and this listener is the whole point of the suite.
  page.on('pageerror', (err) => pageErrors.push(err.stack ?? String(err)));
  page.on('websocket', (ws) => {
    // The dev server has a socket of its own (`ws://…:5199/?token=…`, vite's HMR channel). Recording
    // it would make "the page opened the streamer socket" pass on the wrong socket, and would put
    // HMR frames in `sent`.
    if (!ws.url().includes('/api/panels/ws')) return;
    socketUrls.push(ws.url());
    ws.on('socketerror', (err) => socketErrors.push(String(err)));
    ws.on('framesent', ({ payload }) => {
      // A BINARY frame from the page would be a byte path towards a session, which this design does
      // not have. Recorded as its own type rather than dropped: "nothing the page sent can carry
      // bytes" is only an assertion if the bytes would show up here.
      if (typeof payload !== 'string') {
        sent.push({ t: `<binary ${payload.length} bytes>` });
        return;
      }
      try {
        sent.push(JSON.parse(payload) as SentMessage);
      } catch {
        sent.push({ t: `<not JSON: ${payload.slice(0, 40)}>` });
      }
    });
    ws.on('framereceived', ({ payload }) => {
      if (typeof payload === 'string') received.push(payload);
      else binary.push(payload);
    });
  });

  const unexpectedPageErrors = (): string[] =>
    pageErrors.filter((err) => !KNOWN_MOUNT_RACE.test(err));
  const unexpectedSocketErrors = (): string[] =>
    socketErrors.filter((err) => !ABANDONED_SOCKET.test(err));

  return {
    page,
    socketUrls,
    socketErrors,
    sent,
    received,
    binary,
    pageErrors,
    unexpectedPageErrors,
    unexpectedSocketErrors,
    async open(): Promise<void> {
      await page.goto('/');
      // `view` is zustand state, not a route, so the nav entry is the only way in — which makes
      // this also the test that the wall is reachable at all.
      //
      // BY TEST ID, NOT BY ACCESSIBLE NAME, and that is a correction. This read
      // `getByRole('button', { name: 'Dashboard' })` — the name the surface had before it was
      // renamed to Monitor — and the whole hermetic suite had been timing out at 90 s per test ever
      // since, silently, because nothing runs it on the way to a green typecheck. A rail item's
      // LABEL is a wording decision and will change again; its id is the contract.
      //
      // RETRIED, because one click is not reliable here. Every surface is `React.lazy`, so the
      // first paint is a nav beside a Suspense fallback: the button is present, hit-testable and
      // enabled — everything Playwright waits for — a measurable moment before React has committed
      // its handler. A click in that window is swallowed and the run then fails 25 seconds later
      // pointing at the wall, which is not where the problem is. Observed once in ~35 runs.
      await expect(async () => {
        await page.getByTestId('nav-monitor').click();
        await expect(page.getByRole('heading', { name: 'Monitor' })).toBeVisible({ timeout: 2000 });
      }).toPass({ timeout: 20_000 });
      // A view that throws on mount is the failure this suite exists for, so it is reported as
      // itself rather than as a locator that timed out later.
      expect(unexpectedPageErrors(), 'the Dashboard threw on mount').toEqual([]);
    },
    tile(id = FIRST_TERMINAL): Locator {
      return page.getByTestId(`terminal-${id}`);
    },
    screen(): Locator {
      // xterm's DOM renderer puts one element per row under `.xterm-rows` — which is also why the
      // renderer is DOM and not WebGL: browsers cap WebGL contexts around 16 and a wall would
      // silently lose them.
      return page.locator('.xterm-rows').first();
    },
    chip(signal: string): Locator {
      // `HealthChips.tsx` suffixes the Terminal id once a wall has more than one tile
      // (`chip-poller-fleet:…`), so match the prefix. The single-tile page has exactly one of each.
      //
      // NOTE: the tile no longer draws this row. Kept because `HealthChips` is still rendered
      // elsewhere and a spec may legitimately reach for it there — but a Dashboard spec asserting
      // health should use `health()` below, which is where the tile's signals went.
      return page.locator(`[data-testid^="chip-${signal}"]`).first();
    },
    async health(id = FIRST_TERMINAL): Promise<Locator> {
      // Through the menu, the way an operator reaches it — so the spec also proves the route to the
      // drawer still exists, not merely that the drawer renders when handed props.
      //
      // RETRIED, because the wall repaints on every snapshot frame and a repaint can detach the open
      // menu between resolving the item and clicking it (`element was detached from the DOM`).
      // Playwright's own auto-retry loops on that until the test times out, since re-attaching does
      // not reopen the menu. Re-opening is the fix; the postcondition is the drawer, not the click.
      const drawer = page.getByTestId(`detail-${id}`);
      const item = page.getByTestId(`tile-menu-drawer-${id}`);
      for (let attempt = 0; attempt < 10; attempt += 1) {
        // Checked FIRST, and every time round: a click can land and open the drawer while the
        // `waitFor` that would have observed it is timing out against a stale handle, and re-clicking
        // the trigger from behind an open drawer is how the loop used to exhaust itself.
        if (await drawer.isVisible()) return drawer;
        try {
          if (!(await item.isVisible())) {
            await page.getByTestId(`tile-menu-${id}`).click({ timeout: 4_000 });
          }
          await item.click({ timeout: 4_000 });
        } catch {
          /* a repaint took the menu with it — open it again */
        }
        try {
          await drawer.waitFor({ state: 'visible', timeout: 3_000 });
          return drawer;
        } catch {
          /* not open yet */
        }
      }
      throw new Error(`could not open the detail drawer for ${id} — the menu never stayed open`);
    },
    statusLine(id = FIRST_TERMINAL): Locator {
      return page.getByTestId(`tile-status-${id}`);
    },
    emptyTile(id = FIRST_TERMINAL): Locator {
      return page.getByTestId(`tile-empty-${id}`);
    },
    subscribes(): SentMessage[] {
      return sent.filter((m) => m.t === 'subscribe');
    },
    payload(index: number): Buffer {
      const frame = binary[index];
      if (!frame) throw new Error(`no binary frame at ${index} (have ${binary.length})`);
      const idLen = frame[0] ?? 0;
      return frame.subarray(1 + idLen);
    },
  };
}

export const test = base.extend<
  { view: DashboardView },
  E2EOptions & { control: StreamerControl }
>({
  // Set per project in `playwright.config.ts`. Worker-scoped, because the streamer it selects is.
  streamerKind: ['real', { option: true, scope: 'worker' }],

  control: [
    async ({ streamerKind }, use) => {
      if (streamerKind === 'contract') {
        // A child process: independent of this one, and of the TypeScript it is the counterpart to.
        const stub = await startStubStreamer(PANEL_PORT);
        await use(stubControl(stub.base, () => stub.logs));
        await stub.close();
        return;
      }
      // The shipping streamer, in this process, so the specs can reach its seams.
      const streamer = await startFakeStreamer();
      await use(realControl(streamer));
      await streamer.close();
    },
    { scope: 'worker' },
  ],

  view: async ({ page, control }, use) => {
    // One streamer serves the whole worker, so each test starts from a known fleet: token back,
    // sessions present, screens at SCREEN-ONE.
    await control.reset();
    await use(instrument(page));
    // …and leaves one behind. Without this, a run whose LAST test switched the token off leaves the
    // stub fail-closed, and the next run's `webServer` readiness check reads as a broken fixture.
    await control.reset();
  },
});

export { expect } from '@playwright/test';
