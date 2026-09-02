/**
 * `pnpm test:e2e` — allocate this run's ports, then hand over to Playwright (ADR 0020).
 *
 * WHY A WRAPPER AND NOT THE CONFIG. Ports have to be (a) identical in every process of one run and
 * (b) different between concurrent runs. A Playwright config satisfies neither on its own: it is
 * re-evaluated in the main process, in a config-loader, and in every worker, so anything allocated at
 * config scope differs per evaluation. Measured, with the allocation in `playwright.config.ts`: NINETEEN
 * `test-results/run-<port>` directories for one run, the dev server baked one port, the workers bound
 * others, and every spec failed against a streamer nobody was running. Publishing from the config
 * through `process.env` is only half a fix — whichever process evaluates first wins, and a mutation made
 * inside a config-loader dies with it.
 *
 * The environment a run STARTS with is the one channel every process in it inherits. So this allocates
 * once, exports, and `exec`s. `KONTRA_E2E_PANEL_PORT` / `KONTRA_E2E_WEB_PORT` already set (a human
 * pinning them, or serve mode) are left alone.
 *
 * Running `npx playwright test` directly still works — `e2e/env.ts` falls back to the fixed pair
 * (8190/5199) — it just gives up the isolation, so two direct runs can collide.
 */

import { spawn } from 'node:child_process';
import { createServer } from 'node:net';

const SERVING = !!process.env.KONTRA_E2E_SERVE;

/** One free ephemeral port. Bound and released; the streamer retries `EADDRINUSE` for the gap. */
function freePort() {
  return new Promise((resolve, reject) => {
    const server = createServer();
    server.on('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      const port = typeof address === 'object' && address ? address.port : 0;
      // Held until both are chosen (see below), so the OS cannot hand out the same port twice.
      resolve({ port, release: () => new Promise((done) => server.close(() => done())) });
    });
  });
}

async function allocate() {
  const held = [await freePort(), await freePort()];
  const ports = held.map((h) => h.port);
  for (const h of held) await h.release();
  return ports;
}

if (!SERVING && !(process.env.KONTRA_E2E_PANEL_PORT && process.env.KONTRA_E2E_WEB_PORT)) {
  const [panel, web] = await allocate();
  process.env.KONTRA_E2E_PANEL_PORT ??= String(panel);
  process.env.KONTRA_E2E_WEB_PORT ??= String(web);
}

// `pnpm test:e2e -- --project=real` hands us a bare `--` first; Playwright treats it as a filter and
// silently runs everything, which is worse than an error.
const args = process.argv.slice(2).filter((arg, i, all) => !(arg === '--' && i === 0 && all.length > 1));

const child = spawn('playwright', ['test', ...args], {
  stdio: 'inherit',
  env: process.env,
  // Resolved through the package's own bin directory by pnpm/npm, which is how this file is invoked.
  shell: false,
});
child.on('error', (err) => {
  process.stderr.write(`e2e: could not start playwright: ${String(err)}\n`);
  process.exit(1);
});
child.on('exit', (code, signal) => process.exit(signal ? 1 : (code ?? 1)));
