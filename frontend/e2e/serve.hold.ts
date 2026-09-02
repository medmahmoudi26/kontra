/**
 * `pnpm e2e:serve` — the Dashboard, held open for a human to look at (ADR 0020).
 *
 * Same streamer and same fake fleet the specs drive, minus the assertions: Playwright starts the SPA's
 * dev server, this file boots the real `PanelServer` behind the faked SSH/tmux seam, and then holds
 * both until Ctrl-C. It is a `test` only because that is what already knows how to start the dev
 * server with the right `VITE_` values; nothing here asserts anything.
 *
 * It is NOT collected by an ordinary run: the config's `testMatch` is `**\/*.spec.ts` unless
 * `KONTRA_E2E_SERVE=1` is set, which is what `pnpm test:e2e:serve` does.
 */

import { test } from '@playwright/test';
import { PANEL_BASE, PANEL_TOKEN_VAR, WEB_BASE } from './env';
import { FIRST_TERMINAL } from './fakeFleet';
import { serveFakeStreamer } from './streamer';

test('holds the Dashboard open until Ctrl-C', async () => {
  test.setTimeout(0);
  // eslint-disable-next-line no-console
  console.log(
    [
      '',
      `  Dashboard:  ${WEB_BASE}  → click "Dashboard" in the toolbar`,
      `  streamer:   ${PANEL_BASE}  (real PanelServer, faked SSH/tmux seam, token var ${PANEL_TOKEN_VAR})`,
      `  Terminal:   ${FIRST_TERMINAL}`,
      '',
      '  Every screen is synthetic: no Machine is SSHed to, nothing is captured from tmux.',
      '  Ctrl-C to stop both.',
      '',
    ].join('\n')
  );
  await serveFakeStreamer();
});
