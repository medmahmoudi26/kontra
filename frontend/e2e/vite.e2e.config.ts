/**
 * The project's vite config with a PER-RUN dependency cache, plus the same-origin ticket mint the
 * page now needs (ADR 0020).
 *
 * Two vite dev servers in one project directory share `node_modules/.vite`, and when one re-optimises
 * while the other is serving, the loser hands the browser a broken module graph — measured as a
 * completely blank page. Dynamic ports fixed the listen collision between two concurrent runs; this
 * fixes the other shared mutable thing between them.
 *
 * The cache goes under the OS temp directory rather than the repo, because it is per-run: keeping it in
 * `node_modules/` would leave a directory behind for every run anyone ever does.
 *
 * THE PANEL PROXY EXISTS BECAUSE THE BROWSER NO LONGER HOLDS A TOKEN. The SPA used to read
 * `VITE_KONTRA_PANEL_TOKEN` and call the streamer cross-origin with a bearer; a token that Vite injects
 * is a token baked into the bundle, so that was removed. In production `orchestrator-api` mints the
 * ticket same-origin and adds the bearer server-side. Here vite plays that role: `/api/panels/*` is
 * proxied to the streamer with the header injected, so the page under test exercises the SAME
 * credential-free path it will in production, rather than a shape that only exists in tests.
 *
 * Declared BEFORE the project's own `/api` entry: vite matches proxy keys in insertion order, and
 * `/api` would otherwise swallow `/api/panels` and send it to the orchestrator API, which is not
 * running in a browser test.
 *
 * Everything else — the `@core`/`@contract`/`@` aliases, the tailwind and react plugins — is the
 * project's own config, imported rather than restated, so the page under test is built exactly the way
 * `pnpm dev` builds it.
 */

import * as os from 'node:os';
import * as path from 'node:path';
import base from '../vite.config';

const tag = process.env.KONTRA_E2E_VITE_CACHE ?? 'shared';

/** The streamer under test, and the bearer this proxy adds on the page's behalf. Both come from the
 * Playwright webServer env; the token never reaches the browser. */
const panelTarget = process.env.VITE_KONTRA_PANEL_BASE;
const panelToken = process.env.VITE_KONTRA_PANEL_TOKEN;

const baseServer = (base as { server?: Record<string, unknown> }).server ?? {};
const baseProxy = (baseServer.proxy ?? {}) as Record<string, unknown>;

export default {
  ...base,
  cacheDir: path.join(os.tmpdir(), 'kontra-e2e-vite', tag),
  server: {
    ...baseServer,
    proxy: {
      ...(panelTarget
        ? {
            '/api/panels': {
              target: panelTarget,
              changeOrigin: true,
              ...(panelToken ? { headers: { authorization: `Bearer ${panelToken}` } } : {}),
            },
          }
        : {}),
      ...baseProxy,
    },
  },
};
