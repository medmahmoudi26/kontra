/**
 * The stub streamer as a child process, started and stopped by the fixture (CONTRACT.md 14).
 *
 * WHY NOT A `webServer`. The two projects need the SPA pointed at two different streamers, and
 * `VITE_KONTRA_PANEL_BASE` is baked in when the dev server starts — so two streamers meant two dev
 * servers, and two concurrent vite instances in one project directory FIGHT OVER `node_modules/.vite`:
 * measured, the second one served a blank page and the `contract` project failed with an empty DOM.
 *
 * So the two streamers take turns on ONE port instead, each owned by its project's worker, behind ONE
 * dev server. That also makes them interchangeable at the same address, which is the honest test: the
 * page cannot tell which one it is talking to.
 */

import { spawn, type ChildProcess } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { PANEL_TOKEN, PANEL_TOKEN_VAR, SNAPSHOT_MS, panelBaseFor } from './env';

export interface StubProcess {
  readonly base: string;
  /** The stub's own stdout/stderr, so a red spec can show what the fixture said. */
  readonly logs: string[];
  close(): Promise<void>;
}

const SCRIPT = fileURLToPath(new URL('./stubStreamer.mjs', import.meta.url));

export async function startStubStreamer(port: number): Promise<StubProcess> {
  const base = panelBaseFor(port);
  const logs: string[] = [];
  const child: ChildProcess = spawn(process.execPath, [SCRIPT], {
    env: {
      ...process.env,
      STUB_PANEL_PORT: String(port),
      STUB_PANEL_TOKEN: PANEL_TOKEN,
      STUB_PANEL_TOKEN_VAR: PANEL_TOKEN_VAR,
      STUB_SNAPSHOT_MS: String(SNAPSHOT_MS),
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const collect = (chunk: Buffer): void => {
    for (const line of chunk.toString().split('\n')) if (line.trim()) logs.push(line.trim());
  };
  child.stdout?.on('data', collect);
  child.stderr?.on('data', collect);
  // A child that dies (a port already held, a syntax error) must fail the fixture, not time out
  // silently against a control channel nobody is listening on.
  child.on('error', (err) => logs.push(`spawn failed: ${String(err)}`));

  const deadline = Date.now() + 15_000;
  for (;;) {
    if (child.exitCode !== null) {
      throw new Error(`the stub exited with ${child.exitCode}: ${logs.join(' | ')}`);
    }
    try {
      // `/__stub/stats` and not `/api/panels/health`: the health route fails closed with the token
      // off, and a readiness probe must not read a fixture's deliberate 503 as a dead server.
      const res = await fetch(`${base}/__stub/stats`);
      if (res.ok) break;
    } catch {
      /* not up yet */
    }
    if (Date.now() > deadline) throw new Error(`the stub never answered: ${logs.join(' | ')}`);
    await new Promise((r) => setTimeout(r, 50));
  }

  return {
    base,
    logs,
    async close(): Promise<void> {
      if (child.exitCode !== null) return;
      // Awaited, not fired and forgotten: the next project binds this same port, and a half-dead
      // child would make that fail as EADDRINUSE somewhere unrelated.
      const exited = new Promise<void>((resolve) => child.once('exit', () => resolve()));
      child.kill('SIGTERM');
      await Promise.race([exited, new Promise((r) => setTimeout(r, 5_000)).then(() => child.kill('SIGKILL'))]);
      await exited;
    },
  };
}
