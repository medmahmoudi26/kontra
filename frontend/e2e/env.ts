/**
 * The browser suite's fixed facts: host, token, cadence, and the ports SERVE mode uses (ADR 0020).
 *
 * PORTS FOR A TEST RUN ARE NOT HERE. They are allocated once in `playwright.config.ts` and handed to
 * the workers as Playwright `use` options (`panelPort`, `panelOrigins`) — the sanctioned channel, and
 * the only one that holds. This module used to allocate them and publish them through `process.env`,
 * which LOOKED fine on a single run and was measurably wrong under two: a worker re-imports this module,
 * did not always see the parent's mutation, allocated a port of its own, and then the page pointed at a
 * streamer nobody was running (an empty terminal, ten seconds, no explanation). Three distinct port
 * directories for two concurrent runs is what that looks like from outside.
 *
 * Serve mode keeps FIXED ports on purpose: a human needs a URL that does not move, and only one person
 * is looking at a time.
 */

/** 127.0.0.1, never `localhost`: on this machine `localhost` resolves to `::1` first, and a streamer
 * bound to IPv4 with a page loaded from `[::1]` fails the Origin check for reasons that look nothing
 * like the cause. */
export const HOST = '127.0.0.1';

/**
 * This run's ports.
 *
 * `e2e/run.mjs` allocates a free pair and exports them BEFORE Playwright starts, so every process in a
 * run — config loader, main, each worker, the dev-server child — reads the same two numbers here. The
 * fallback pair is what serve mode uses and what a bare `npx playwright test` gets: fixed, away from
 * 8090/5173 so a compose stack or a `pnpm dev` does not collide, and shared, so two bare runs would.
 */
export const PANEL_PORT = Number(process.env.KONTRA_E2E_PANEL_PORT ?? 8190);
export const WEB_PORT = Number(process.env.KONTRA_E2E_WEB_PORT ?? 5199);

export const PANEL_BASE = `http://${HOST}:${PANEL_PORT}`;
export const WEB_BASE = `http://${HOST}:${WEB_PORT}`;

/** The WS `Origin` allow-list for this run's dev server. */
export const PANEL_ORIGINS = originsFor(WEB_PORT);

export function panelBaseFor(port: number): string {
  return `http://${HOST}:${port}`;
}

export function webBaseFor(port: number): string {
  return `http://${HOST}:${port}`;
}

/** The WS `Origin` allow-list for a given dev server. Browsers do not apply the same-origin policy to
 * WebSockets, so this check is ours; both spellings are listed so opening the page by either name works
 * when a human drives it by hand. */
export function originsFor(webPort: number): string[] {
  return [webBaseFor(webPort), `http://localhost:${webPort}`];
}

/** What the page holds, and what both streamers mint tickets against. */
export const PANEL_TOKEN = 'e2e-panel-token-not-a-real-secret';

/** Injected as `PanelServer`'s `tokenVars`, so a real `KONTRA_PANEL_TOKEN` in this shell can neither
 * leak into a run nor make the fail-closed spec pass. The stub names the same var in its 503. */
export const PANEL_TOKEN_VAR = 'KONTRA_PANEL_TOKEN_E2E';

/** Fast enough that a spec does not wait out the 3-second production cadence per assertion. */
export const SNAPSHOT_MS = 150;
