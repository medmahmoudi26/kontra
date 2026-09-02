/**
 * The real streamer, in the test process (ADR 0020).
 *
 * `PanelServer` is constructed here exactly as `backend/src/panels/server.test.ts` constructs
 * it — injected seams, injected `tokenVars` — and then a real Chromium talks to it over a real
 * socket. Nothing about the wire is re-implemented: `handshakeResponse`, `FrameDecoder`,
 * `encodeTagged`, `TicketBook`, `checkBearer` and the CORS/Origin checks are the shipping ones.
 *
 * It runs IN the Playwright worker rather than as a `webServer` child for one reason: the specs need
 * to reach in. "Two snapshots, different content, assert the second replaced the first" and "no token
 * configured ⇒ the UI shows the fail-closed 503" are both statements about the streamer's state
 * mid-test, and a child process would need a control channel — that is, a second wire to get wrong.
 * {@link serveFakeStreamer} is the same object with a `webServer`-shaped lifetime for a human.
 */

import { PanelServer } from '../../backend/src/panels/server';
import { FakeFleet } from './fakeFleet';
import { HOST, PANEL_ORIGINS, PANEL_PORT, PANEL_TOKEN, PANEL_TOKEN_VAR, SNAPSHOT_MS } from './env';

export interface FakeStreamer {
  readonly server: PanelServer;
  readonly fleet: FakeFleet;
  readonly port: number;
  readonly base: string;
  /** Lines the streamer logged. Read on failure: "snapshot failed" here explains a blank tile. */
  readonly logs: string[];
  /** Put the token back and re-probe. Called between tests. */
  reset(): Promise<void>;
  /** Unset the token var, which is how `auth.ts` fails closed: every route 503s and serves nothing. */
  disableToken(): void;
  close(): Promise<void>;
}

export interface FakeStreamerOptions {
  port?: number;
  /** CORS + WS `Origin` allow-list. Comes from the project's `use` options during a run. */
  origins?: string[];
  /** A discovery cadence long enough that only the specs' explicit `refresh()` calls change health. */
  discoverMs?: number;
}

/**
 * Bind, retrying while the port is still held.
 *
 * The two streamers TAKE TURNS on one port (see `env.ts`), so this worker may start while the previous
 * project's stub child is still releasing it. Without the retry that surfaces as EADDRINUSE and every
 * spec in the project fails at once, which reads like a broken streamer rather than a slow `close()`.
 */
async function listenWithRetry(server: PanelServer, port: number): Promise<number> {
  const deadline = Date.now() + 15_000;
  for (;;) {
    try {
      return await server.listen(port, HOST);
    } catch (err) {
      const code = (err as NodeJS.ErrnoException).code;
      if (code !== 'EADDRINUSE' || Date.now() > deadline) throw err;
      await new Promise((r) => setTimeout(r, 100));
    }
  }
}

export async function startFakeStreamer(opts: FakeStreamerOptions = {}): Promise<FakeStreamer> {
  const fleet = new FakeFleet();
  const logs: string[] = [];
  process.env[PANEL_TOKEN_VAR] = PANEL_TOKEN;

  const server = new PanelServer(
    {
      // Delegated rather than spread: `FakeFleet`'s methods live on its prototype, and a spread
      // would hand the streamer an object with none of them.
      discover: () => fleet.discover(),
      probe: (m) => fleet.probe(m),
      snapshot: (m, windows) => fleet.snapshot(m, windows),
      converge: (m) => fleet.converge(m),
      // `log` is how the streamer reports a failed probe or a dropped client. Captured, not printed:
      // it makes a red spec explain itself instead of just timing out.
      log: (line, extra) => logs.push(`${line}${extra ? ` ${JSON.stringify(extra)}` : ''}`),
    },
    {
      snapshotMs: SNAPSHOT_MS,
      discoverMs: opts.discoverMs ?? 600_000,
      origins: opts.origins ?? PANEL_ORIGINS,
      tokenVars: [PANEL_TOKEN_VAR],
    }
  );

  server.http.on('error', (err) => logs.push(`http: ${String(err)}`));

  const port = await listenWithRetry(server, opts.port ?? PANEL_PORT);
  // `listen()` kicks off the first discovery round; await one so the inventory is populated before
  // the browser asks for it, rather than racing the page's first fetch.
  await server.refresh();

  return {
    server,
    fleet,
    port,
    base: `http://${HOST}:${port}`,
    logs,
    async reset(): Promise<void> {
      process.env[PANEL_TOKEN_VAR] = PANEL_TOKEN;
      fleet.reset();
      logs.length = 0;
      await server.refresh();
    },
    disableToken(): void {
      delete process.env[PANEL_TOKEN_VAR];
    },
    async close(): Promise<void> {
      await server.close();
      delete process.env[PANEL_TOKEN_VAR];
    },
  };
}

/**
 * Boot the streamer and hold it until the process is signalled — what `pnpm e2e:serve` runs so a
 * human can open the Dashboard and look at it. Prints what it is, because a wall of synthetic
 * journal lines should never be mistaken for a fleet.
 */
export async function serveFakeStreamer(): Promise<void> {
  const streamer = await startFakeStreamer({ discoverMs: 30_000 });
  const lines = [
    `[e2e] streamer on ${streamer.base} (the REAL PanelServer, faked SSH/tmux seam)`,
    `[e2e] token var ${PANEL_TOKEN_VAR}, origins ${PANEL_ORIGINS.join(', ')}`,
    `[e2e] Terminals: ${streamer.server
      .terminalList()
      .map((t) => t.id)
      .join(' ')}`,
    '[e2e] NOT a fleet: no Machine is SSHed to and every screen below is synthetic.',
  ];
  process.stdout.write(`${lines.join('\n')}\n`);
  await new Promise<void>((resolve) => {
    for (const signal of ['SIGINT', 'SIGTERM'] as const) {
      process.once(signal, () => {
        void streamer.close().then(resolve, resolve);
      });
    }
  });
}
