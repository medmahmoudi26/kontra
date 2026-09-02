import * as net from 'node:net';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { decodeTagged, encodeMaskedFrame, FrameDecoder, newClientKey, OPCODE } from './ws';
import { PanelServer, type PanelDeps } from './server';
import {
  attacher,
  MAX_DIMENSION,
  MIN_COLS,
  MIN_ROWS,
  type PtyEvents,
  type PtyRunner,
} from './attach';
import type { SshResult, SshRunner, SshTarget } from './ssh';
import type { MachineTarget } from './discovery';
import type { ProbeResult } from './probe';
import type { ServerMessage } from './types';

/**
 * The streamer end to end, over a real socket, against faked SSH and Pulumi seams.
 *
 * No fleet exists in this environment, so the fakes ARE the fleet: two Machines from a stack
 * checkpoint, a probe that answers "session present", and a snapshot that counts how many times it
 * was called — which is what proves the "one exec per Machine per interval" claim rather than
 * asserting it in a comment.
 */

const TOKEN = 'panel-token-for-tests';
const TOKEN_VAR = 'KONTRA_PANEL_TOKEN_TEST';
const ORIGIN = 'http://localhost:8088';

function machine(n: string): MachineTarget {
  return {
    machine: n,
    host: `10.124.0.${n.endsWith('01') ? 9 : 10}`,
    publicIp: '203.0.113.9',
    tag: 'crawl',
    fleet: 'run-apex-119',
    actor: 'webcrawl',
    version: '0.2.0',
    session: 'kontra-webcrawl',
    windows: ['actor', 'handler'],
  };
}

/**
 * The two LOWEST seams of the live attach, faked — and only those.
 *
 * The real `Attachment` is what they are given to, so a `focus` on this wire builds the real remote
 * command, does the real backlog read, and issues the real cleanup. Only `ssh` and the PTY are
 * pretend, which is as deep as a test can go where no Machine exists to be SSHed to.
 */
class FakeMachine implements SshRunner {
  readonly execs: string[] = [];
  backlog = 'the last screen before we went live\n';
  reachable = true;
  private held: Promise<void> | undefined;
  private release: (() => void) | undefined;

  /** Hold the backlog read open, so a test can act during the window where an attach is STARTING —
   * on a real Machine that window is an SSH round trip wide. */
  holdBacklog(): void {
    this.held = new Promise<void>((resolve) => {
      this.release = resolve;
    });
  }

  releaseBacklog(): void {
    this.release?.();
  }

  async run(_target: SshTarget, remote: string): Promise<SshResult> {
    this.execs.push(remote);
    if (!this.reachable) {
      return { code: 124, stdout: '', stderr: 'ssh: connect: timed out', timedOut: true };
    }
    if (remote.includes('capture-pane') && this.held) await this.held;
    const stdout = remote.includes('capture-pane') ? this.backlog : '';
    return { code: 0, stdout, stderr: '', timedOut: false };
  }

  kills(): string[] {
    return this.execs.filter((e) => e.includes('kill-session'));
  }

  backlogReads(): number {
    return this.execs.filter((e) => e.includes('capture-pane -p -e -S -2000')).length;
  }
}

interface FakePtyEntry {
  remote: string;
  events: PtyEvents;
  killed: number;
}

class FakePtys implements PtyRunner {
  readonly opened: FakePtyEntry[] = [];

  open(_target: SshTarget, remote: string, events: PtyEvents) {
    const entry: FakePtyEntry = { remote, events, killed: 0 };
    this.opened.push(entry);
    return {
      noWritableInput: true,
      kill: (): void => {
        entry.killed += 1;
      },
    };
  }

  last(): FakePtyEntry | undefined {
    return this.opened[this.opened.length - 1];
  }

  /** The per-viewer session each attach created, read back out of the command it ran. */
  viewers(): string[] {
    return this.opened.map((o) => /-s "(kp-[a-z0-9]+)"/.exec(o.remote)?.[1] ?? '(none)');
  }
}

interface Harness {
  server: PanelServer;
  port: number;
  snapshots: Array<{ machine: string; windows: string[] }>;
  converges: string[];
  probe: ProbeResult;
  machineSsh: FakeMachine;
  ptys: FakePtys;
}

/** Wait on something that is not a client's inbox — a kill that went to a Machine, a snapshot round. */
async function waitFor(check: () => boolean, what: string, ms = 3000): Promise<void> {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (check()) return;
    await new Promise((r) => setTimeout(r, 10));
  }
  throw new Error(`timed out waiting for ${what}`);
}

async function harness(over: Partial<PanelDeps> = {}, snapshotMs = 50): Promise<Harness> {
  const snapshots: Array<{ machine: string; windows: string[] }> = [];
  const converges: string[] = [];
  const machineSsh = new FakeMachine();
  const ptys = new FakePtys();
  const h: Partial<Harness> = {
    snapshots,
    converges,
    machineSsh,
    ptys,
    probe: { reachable: 'ok', session: 'present', windows: ['actor', 'handler'] },
  };
  const deps: PanelDeps = {
    discover: async () => [machine('kf-crawl-01'), machine('kf-crawl-02')],
    probe: async () => h.probe as ProbeResult,
    async snapshot(m, windows) {
      snapshots.push({ machine: m.machine, windows: [...windows].sort() });
      const out = new Map<string, string>();
      for (const w of windows) out.set(w, `screen of ${m.machine}/${w}`);
      return out;
    },
    converge: async (m) => {
      converges.push(m.machine);
      return { workflowId: `tmux-${m.machine}` };
    },
    attach: attacher({ ssh: machineSsh, pty: ptys }),
    log: () => undefined,
    ...over,
  };
  const server = new PanelServer(deps, {
    snapshotMs,
    discoverMs: 60_000,
    origins: [ORIGIN],
    tokenVars: [TOKEN_VAR],
  });
  const port = await server.listen(0, '127.0.0.1');
  // The first discovery round is kicked off by listen(); await one explicitly so tests are not
  // racing it.
  await server.refresh();
  h.server = server;
  h.port = port;
  return h as Harness;
}

let open: PanelServer[] = [];
let sockets: net.Socket[] = [];

beforeEach(() => {
  process.env[TOKEN_VAR] = TOKEN;
});

afterEach(async () => {
  for (const s of sockets) s.destroy();
  sockets = [];
  for (const s of open) await s.close();
  open = [];
  delete process.env[TOKEN_VAR];
});

async function boot(over?: Partial<PanelDeps>, snapshotMs?: number): Promise<Harness> {
  const h = await harness(over, snapshotMs);
  open.push(h.server);
  return h;
}

async function http(
  port: number,
  method: string,
  path: string,
  headers: Record<string, string> = {}
): Promise<{ status: number; body: string; headers: Record<string, string> }> {
  const res = await fetch(`http://127.0.0.1:${port}${path}`, { method, headers });
  const out: Record<string, string> = {};
  res.headers.forEach((v, k) => {
    out[k] = v;
  });
  return { status: res.status, body: await res.text(), headers: out };
}

/** A minimal WebSocket client: the framing is ours, so the test speaks it directly. */
class TestClient {
  // Server frames are unmasked (RFC 6455 §5.1), so the client half of the decoder does not demand a
  // mask — the server half still does.
  private readonly decoder = new FrameDecoder(1_000_000, false);
  readonly text: ServerMessage[] = [];
  readonly binary: Array<{ id: string; payload: Buffer }> = [];
  private handshake = '';

  private constructor(readonly socket: net.Socket) {}

  static async connect(port: number, ticket: string, origin?: string): Promise<TestClient> {
    const socket = net.connect(port, '127.0.0.1');
    sockets.push(socket);
    const client = new TestClient(socket);
    await new Promise<void>((resolve, reject) => {
      socket.on('error', reject);
      socket.on('connect', () => {
        socket.write(
          `GET /api/panels/ws?ticket=${encodeURIComponent(ticket)} HTTP/1.1\r\n` +
            `Host: 127.0.0.1:${port}\r\n` +
            'Upgrade: websocket\r\nConnection: Upgrade\r\n' +
            `Sec-WebSocket-Key: ${newClientKey()}\r\nSec-WebSocket-Version: 13\r\n` +
            (origin ? `Origin: ${origin}\r\n` : '') +
            '\r\n'
        );
      });
      const onData = (chunk: Buffer): void => {
        client.handshake += chunk.toString('latin1');
        const end = client.handshake.indexOf('\r\n\r\n');
        if (end < 0) return;
        socket.removeListener('data', onData);
        const rest = Buffer.from(client.handshake.slice(end + 4), 'latin1');
        client.handshake = client.handshake.slice(0, end);
        socket.on('data', (c: Buffer) => client.onData(c));
        if (rest.length) client.onData(rest);
        resolve();
      };
      socket.on('data', onData);
    });
    return client;
  }

  get status(): number {
    return Number(this.handshake.split(' ')[1] ?? 0);
  }

  private onData(chunk: Buffer): void {
    for (const frame of this.decoder.push(chunk)) {
      if (frame.opcode === OPCODE.text) this.text.push(JSON.parse(frame.payload.toString()) as ServerMessage);
      else if (frame.opcode === OPCODE.binary) this.binary.push(decodeTagged(frame.payload));
    }
  }

  send(msg: unknown): void {
    this.socket.write(encodeMaskedFrame(OPCODE.text, Buffer.from(JSON.stringify(msg))));
  }

  /** Wait until `check` holds, or fail with what actually arrived. */
  async until(check: () => boolean, what: string, ms = 3000): Promise<void> {
    const deadline = Date.now() + ms;
    while (Date.now() < deadline) {
      if (check()) return;
      await new Promise((r) => setTimeout(r, 10));
    }
    throw new Error(
      `timed out waiting for ${what}; text=${JSON.stringify(this.text)} binary=${this.binary.length}`
    );
  }
}

async function ticket(port: number): Promise<string> {
  const res = await http(port, 'POST', '/api/panels/ticket', { authorization: `Bearer ${TOKEN}` });
  expect(res.status).toBe(200);
  return (JSON.parse(res.body) as { ticket: string }).ticket;
}

describe('the HTTP surface', () => {
  it('serves health ungated, with the counts an operator needs', async () => {
    const h = await boot();
    const res = await http(h.port, 'GET', '/api/panels/health');
    expect(res.status).toBe(200);
    expect(JSON.parse(res.body)).toEqual({ ok: true, terminals: 4, live: 0, machines: 2 });
  });

  it('gates the inventory and the ticket on the bearer', async () => {
    const h = await boot();
    for (const path of ['/api/panels/terminals', '/api/panels/ticket']) {
      const method = path.endsWith('ticket') ? 'POST' : 'GET';
      expect((await http(h.port, method, path)).status, `${path} unauthenticated`).toBe(401);
      expect(
        (await http(h.port, method, path, { authorization: 'Bearer wrong' })).status,
        `${path} wrong token`
      ).toBe(401);
      expect(
        (await http(h.port, method, path, { authorization: `Bearer ${TOKEN}` })).status,
        `${path} correct token`
      ).toBe(200);
    }
  });

  it('FAILS CLOSED with no token: every route 503s and serves nothing', async () => {
    const h = await boot();
    delete process.env[TOKEN_VAR];
    for (const [method, path] of [
      ['GET', '/api/panels/health'],
      ['GET', '/api/panels/terminals'],
      ['POST', '/api/panels/ticket'],
    ] as const) {
      const res = await http(h.port, method, path, { authorization: `Bearer ${TOKEN}` });
      expect(res.status, `${method} ${path}`).toBe(503);
      expect(res.body, `${method} ${path}`).toContain('disabled');
      expect(res.body).not.toContain('kf-crawl-01');
    }
    // …including the socket. A ticket minted before the token went away is worth nothing.
    const socket = net.connect(h.port, '127.0.0.1');
    sockets.push(socket);
    const reply = await new Promise<string>((resolve) => {
      socket.on('connect', () =>
        socket.write('GET /api/panels/ws?ticket=whatever HTTP/1.1\r\nUpgrade: websocket\r\n' +
          'Connection: Upgrade\r\nSec-WebSocket-Key: x\r\nSec-WebSocket-Version: 13\r\n\r\n')
      );
      socket.on('data', (c: Buffer) => resolve(c.toString()));
    });
    expect(reply).toContain('503');
  });

  it('answers CORS only for the configured origin', async () => {
    const h = await boot();
    const allowed = await http(h.port, 'POST', '/api/panels/ticket', {
      authorization: `Bearer ${TOKEN}`,
      origin: ORIGIN,
    });
    expect(allowed.headers['access-control-allow-origin']).toBe(ORIGIN);
    const other = await http(h.port, 'POST', '/api/panels/ticket', {
      authorization: `Bearer ${TOKEN}`,
      origin: 'http://evil.example',
    });
    expect(other.headers['access-control-allow-origin']).toBeUndefined();
    expect(other.headers.vary).toBe('Origin');
    // The preflight the browser sends first.
    expect((await http(h.port, 'OPTIONS', '/api/panels/ticket', { origin: ORIGIN })).status).toBe(204);
  });

  it('never caches a ticket', async () => {
    const h = await boot();
    const res = await http(h.port, 'POST', '/api/panels/ticket', {
      authorization: `Bearer ${TOKEN}`,
    });
    expect(res.headers['cache-control']).toBe('no-store');
  });

  /**
   * The exact JSON `kontra panels list` parses.
   *
   * Slice 5's Go struct mirrors CONTRACT.md's `Terminal` field for field, and Go's decoder is
   * case-insensitive on keys but NOT forgiving about `{"terminals": …}` being an object rather than a
   * bare array, nor about an optional field arriving as `""`/`0` instead of being absent — an empty
   * `detail` would print as a blank health sentence, and `lastSnapshotAt: 0` as 1970.
   */
  it('emits exactly the wire shape the CLI parses', async () => {
    const h = await boot(undefined, 40);
    const res = await http(h.port, 'GET', '/api/panels/terminals', {
      authorization: `Bearer ${TOKEN}`,
    });
    expect(res.body.startsWith('{"terminals":[')).toBe(true);
    expect(res.body).toContain('"publicIp"'); // camelCase, not public_ip
    // Absent, not empty: nothing measured `detail` here and no snapshot has been taken.
    expect(res.body).not.toContain('detail');
    expect(res.body).not.toContain('lastSnapshotAt');

    const first = (JSON.parse(res.body) as { terminals: Array<Record<string, unknown>> }).terminals[0];
    expect(Object.keys(first ?? {})).toEqual([
      'id',
      // ADDED by slice 6, never renamed: `mode` is where the Worker runs (`fleet|docker|local`), and
      // slice 5's Go struct decodes every other field exactly as before. It is a separate axis from
      // `{t:'state'}`'s `mode` (`snapshot|live|error`), which rides a different message.
      'mode',
      'machine',
      'host',
      'publicIp',
      'tag',
      'fleet',
      'actor',
      'version',
      'window',
      // ADDED beside `window`, never renamed, for the same reason `mode` was: what is running in the
      // pane, how big the pane is, and what the command exited with are what a tile's status line
      // says — and slice 5's Go struct ignores a field it does not know about.
      'command',
      'paneCols',
      'paneRows',
      'exitStatus',
      'health',
    ]);
    expect(Object.keys((first?.health ?? {}) as Record<string, unknown>)).toEqual([
      'reachable',
      'session',
      // The fifth signal, between the session it is most confused with and the queue signals.
      'process',
      'poller',
      'loads',
    ]);

    // …and once a snapshot HAS happened, lastSnapshotAt is epoch milliseconds.
    const client = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    client.send({ t: 'subscribe', id: 'fleet:kf-crawl-01/kontra-webcrawl/actor' });
    await client.until(() => client.binary.length > 0, 'a snapshot');
    const after = await http(h.port, 'GET', '/api/panels/terminals', {
      authorization: `Bearer ${TOKEN}`,
    });
    const snapped = (
      JSON.parse(after.body) as { terminals: Array<{ id: string; lastSnapshotAt?: number }> }
    ).terminals.find((t) => t.id === 'fleet:kf-crawl-01/kontra-webcrawl/actor');
    expect(typeof snapped?.lastSnapshotAt).toBe('number');
    expect(snapped?.lastSnapshotAt).toBeGreaterThan(1_600_000_000_000);
  });

  it('keeps 401 and 503 distinct — they send an operator to different machines', async () => {
    // 503 means the STREAMER has no token and is fail-closed; 401 means the CALLER's token is wrong.
    // Flattening both into 401 would send someone to edit their own shell for a server-side problem.
    const h = await boot();
    expect(
      (await http(h.port, 'GET', '/api/panels/terminals', { authorization: 'Bearer wrong' })).status
    ).toBe(401);
    delete process.env[TOKEN_VAR];
    const disabled = await http(h.port, 'GET', '/api/panels/terminals', {
      authorization: `Bearer ${TOKEN}`,
    });
    expect(disabled.status).toBe(503);
    expect(disabled.body).toContain(TOKEN_VAR); // the reply names the var that is missing
  });

  it('lists Terminals for a Machine, with unmeasured signals left unknown', async () => {
    const h = await boot();
    const res = await http(h.port, 'GET', '/api/panels/terminals', {
      authorization: `Bearer ${TOKEN}`,
    });
    const { terminals } = JSON.parse(res.body) as { terminals: Array<Record<string, unknown>> };
    expect(terminals.map((t) => t.id)).toEqual([
      'fleet:kf-crawl-01/kontra-webcrawl/actor',
      'fleet:kf-crawl-01/kontra-webcrawl/handler',
      'fleet:kf-crawl-02/kontra-webcrawl/actor',
      'fleet:kf-crawl-02/kontra-webcrawl/handler',
    ]);
    expect(terminals[0]?.health).toEqual({
      reachable: 'ok',
      session: 'present',
      process: 'unknown',
      poller: 'unknown',
      loads: 'unknown',
    });
  });
});

describe('the ticket', () => {
  it('is single-use', async () => {
    const h = await boot();
    const t = await ticket(h.port);
    const first = await TestClient.connect(h.port, t, ORIGIN);
    expect(first.status).toBe(101);
    const second = await TestClient.connect(h.port, t, ORIGIN);
    expect(second.status).toBe(401);
  });

  it('expires', async () => {
    // Time is injected, so this proves the expiry rather than sleeping 30 seconds for it.
    let clock = 1_000_000;
    const server = new PanelServer(
      {
        discover: async () => [],
        probe: async () => ({ reachable: 'ok', session: 'absent', windows: [] }),
        snapshot: async () => new Map(),
        now: () => clock,
        log: () => undefined,
      },
      { origins: [ORIGIN], tokenVars: [TOKEN_VAR] }
    );
    open.push(server);
    const port = await server.listen(0, '127.0.0.1');
    const minted = server.tickets.mint();
    clock += 29_000;
    expect(server.tickets.redeem(minted.ticket)).toBe(true);
    const again = server.tickets.mint();
    clock += 31_000;
    expect(server.tickets.redeem(again.ticket)).toBe(false);
    expect((await TestClient.connect(port, again.ticket, ORIGIN)).status).toBe(401);
  });

  it('refuses an unknown origin on the socket, where the browser will not', async () => {
    const h = await boot();
    const client = await TestClient.connect(h.port, await ticket(h.port), 'http://evil.example');
    expect(client.status).toBe(403);
  });
});

describe('the multiplexed stream', () => {
  it('says hello, then streams a snapshot as [idLen][id][payload] with a clear-screen prefix', async () => {
    const h = await boot();
    const client = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    await client.until(() => client.text.some((m) => m.t === 'hello'), 'hello');
    expect(client.text[0]).toEqual({ t: 'hello', terminals: 4 });

    const id = 'fleet:kf-crawl-01/kontra-webcrawl/actor';
    client.send({ t: 'subscribe', id, cols: 200, rows: 50 });
    await client.until(() => client.binary.length > 0, 'a snapshot frame');

    const state = client.text.find((m) => m.t === 'state');
    expect(state).toMatchObject({ t: 'state', id, mode: 'snapshot' });

    const frame = client.binary[0];
    expect(frame?.id).toBe(id);
    // A snapshot is a full screen: without home+clear a 3-second repaint appends forever.
    expect(frame?.payload.subarray(0, '\x1b[H\x1b[2J'.length).toString()).toBe('\x1b[H\x1b[2J');
    expect(frame?.payload.toString()).toContain('screen of kf-crawl-01/actor');
  });

  /**
   * A DEAD WORKER SAYS SO, from the screen it printed — the half of the answer no probe can give.
   *
   * `pane_current_command` reports kontra's hold shell whether the Worker is running or finished
   * (measured on this host, both directions), so for a session started before `@kontra_exit` existed
   * the ONLY evidence is the banner the hold prints. That arrives on the snapshot cadence rather than
   * the discovery one, which is why the streamer reads it here and remembers it across a refresh.
   */
  it('reads the hold banner off a snapshot and announces the pane as exited', async () => {
    let screen = 'the Worker, printing away\n';
    const h = await boot(
      {
        async snapshot(_m, windows) {
          const out = new Map<string, string>();
          for (const w of windows) out.set(w, screen);
          return out;
        },
      },
      40
    );
    const id = 'fleet:kf-crawl-01/kontra-webcrawl/actor';
    const client = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    client.send({ t: 'subscribe', id });
    await client.until(() => client.binary.length > 0, 'a snapshot');

    // While it is printing, the pane's process is `unknown` — not `running`, which nothing measured,
    // and certainly not `exited`.
    const before = client.text.filter((m) => m.t === 'state').pop();
    expect(before).toMatchObject({ t: 'state', mode: 'snapshot' });
    expect((before as { health: { process: string } }).health.process).toBe('unknown');

    // The Worker exits. `cli/internal/tmux/tmux.go`'s hold keeps the window, so the SESSION stays present and the
    // only thing that changes is what the screen says.
    screen = 'a traceback\n[exited 1] press any key to close this window\n';
    await client.until(
      () => client.text.some((m) => m.t === 'state' && m.health.process === 'exited'),
      'the exited state'
    );
    const after = client.text.filter((m) => m.t === 'state').pop() as {
      health: { session: string; process: string; detail?: string };
    };
    // FOUR FACTS, NOT ONE. The session is still present, and saying otherwise would offer a converge
    // for a session that is right there.
    expect(after.health.session).toBe('present');
    expect(after.health.process).toBe('exited');
    expect(after.health.detail).toContain('exited 1');

    // …and the inventory carries the status, so a tile can print the number rather than a shrug.
    const list = await http(h.port, 'GET', '/api/panels/terminals', {
      authorization: `Bearer ${TOKEN}`,
    });
    const listed = (JSON.parse(list.body) as { terminals: Array<Record<string, unknown>> }).terminals;
    expect(listed.find((t) => t.id === id)?.exitStatus).toBe('1');

    // A DISCOVERY ROUND MUST NOT FORGET IT. The probe cannot see the banner, so a refresh that
    // rebuilt health from the probe alone would flip the tile back to `unknown` every 30 seconds.
    await h.server.refresh();
    const stillListed = JSON.parse(
      (await http(h.port, 'GET', '/api/panels/terminals', { authorization: `Bearer ${TOKEN}` })).body
    ) as { terminals: Array<Record<string, unknown>> };
    const t = stillListed.terminals.find((x) => x.id === id) as { health: { process: string } };
    expect(t.health.process).toBe('exited');
  });

  it('costs ONE exec per Machine per interval, for two browsers and both windows', async () => {
    const h = await boot(undefined, 40);
    const a = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    const b = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    a.send({ t: 'subscribe', id: 'fleet:kf-crawl-01/kontra-webcrawl/actor' });
    b.send({ t: 'subscribe', id: 'fleet:kf-crawl-01/kontra-webcrawl/actor' });
    b.send({ t: 'subscribe', id: 'fleet:kf-crawl-01/kontra-webcrawl/handler' });
    await a.until(() => a.binary.length >= 1, 'a snapshot for client a');
    await b.until(() => b.binary.length >= 2, 'snapshots for client b');

    const rounds = new Map<string, number>();
    for (const s of h.snapshots) rounds.set(s.machine, (rounds.get(s.machine) ?? 0) + 1);
    // Both browsers are fed from the same exec, and the exec covers both windows.
    expect(h.snapshots.every((s) => s.machine === 'kf-crawl-01')).toBe(true);
    expect(h.snapshots.some((s) => s.windows.join(',') === 'actor,handler')).toBe(true);
    // …and nobody is watching kf-crawl-02, so it is never touched.
    expect(rounds.get('kf-crawl-02')).toBeUndefined();
  });

  it('seeds a late subscriber from the ring, immediately', async () => {
    const h = await boot(undefined, 40);
    const first = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    const id = 'fleet:kf-crawl-01/kontra-webcrawl/actor';
    first.send({ t: 'subscribe', id });
    await first.until(() => first.binary.length > 0, 'the first snapshot');

    const late = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    late.send({ t: 'subscribe', id });
    // No waiting for the next cadence tick: the ring is what a tile paints from on open.
    await late.until(() => late.binary.length > 0, 'a seeded frame', 200);
    expect(late.binary[0]?.payload.toString()).toContain('screen of kf-crawl-01/actor');
  });

  it('answers ping, a bad id, and an unknown Terminal without dropping the socket', async () => {
    const h = await boot();
    const client = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    client.send({ t: 'ping' });
    await client.until(() => client.text.some((m) => m.t === 'pong'), 'pong');

    client.send({ t: 'subscribe', id: 'fleet:kf-crawl-01/kontra-webcrawl/$(id)' });
    await client.until(
      () => client.text.some((m) => m.t === 'error' && m.message.includes('not a Terminal id')),
      'a refusal of the hostile id'
    );

    client.send({ t: 'subscribe', id: 'fleet:kf-other-99/kontra-webcrawl/actor' });
    await client.until(
      () => client.text.some((m) => m.t === 'error' && m.message.includes('no such Terminal')),
      'a refusal of the unknown Terminal'
    );

    client.send({ t: 'ping' });
    await client.until(() => client.text.filter((m) => m.t === 'pong').length === 2, 'a second pong');
  });

  it('stops streaming on unsubscribe', async () => {
    const h = await boot(undefined, 30);
    const client = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    const id = 'fleet:kf-crawl-01/kontra-webcrawl/actor';
    client.send({ t: 'subscribe', id });
    await client.until(() => client.binary.length > 0, 'a snapshot');
    client.send({ t: 'unsubscribe', id });
    // TWO SLEEPS THAT LOOK ALIKE AND ARE NOT. The second is the OBSERVATION window — four snapshot
    // intervals in which a still-subscribed client would have been sent something — and it must stay
    // a fixed wait, because there is no condition to poll for a frame that never comes. Polling it
    // would turn "stops streaming" into a test that cannot fail.
    //
    // The first is a QUIESCE: it exists only to let a frame already in flight from before the
    // unsubscribe land, so `seen` is a stable baseline. That one WAS too thin — two intervals on a
    // box that runs at loadavg 15 — and its failure mode is a red that means "the machine was
    // busy". Ten intervals costs a quarter of a second and takes the guesswork out.
    await new Promise((r) => setTimeout(r, 300));
    const seen = client.binary.length;
    await new Promise((r) => setTimeout(r, 120));
    expect(client.binary.length).toBe(seen);
  });

  it('refuses focus when this streamer cannot attach, rather than looking hung', async () => {
    // The same stance `converge` takes with no Temporal client: an absent seam is words, never
    // silence. A tile that silently stayed on snapshots would look like a hung attach.
    const h = await boot({ attach: undefined });
    const client = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    client.send({ t: 'focus', id: 'fleet:kf-crawl-01/kontra-webcrawl/actor', cols: 200, rows: 50 });
    await client.until(
      () => client.text.some((m) => m.t === 'error' && m.message.includes('cannot open a live attach')),
      'the focus refusal'
    );
    expect(client.socket.destroyed).toBe(false);
  });

  it('converges the Machine the id names, and nothing else', async () => {
    const h = await boot();
    const client = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    client.send({ t: 'converge', id: 'fleet:kf-crawl-01/kontra-webcrawl/actor' });
    await client.until(() => h.converges.length > 0, 'the converge');
    expect(h.converges).toEqual(['kf-crawl-01']);

    // A Machine that is not in the inventory cannot be converged by asking for it.
    client.send({ t: 'converge', id: 'fleet:kf-ghost-99/kontra-webcrawl/actor' });
    await client.until(
      () => client.text.some((m) => m.t === 'error' && m.message.includes('no such Machine')),
      'the refusal'
    );
    expect(h.converges).toEqual(['kf-crawl-01']);
  });

  it('drops a client that speaks the protocol wrongly, and keeps serving the others', async () => {
    const h = await boot();
    const good = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    const bad = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    // An unmasked frame: RFC 6455 requires masking from a client.
    bad.socket.write(Buffer.from([0x81, 0x02, 0x7b, 0x7d]));
    // WAIT FOR THE DROP, AND THEN ASSERT IT. This was a fixed 100 ms, and the failure mode was not
    // a red — it was a green that meant nothing: if the server had not got round to destroying the
    // socket inside the window, "and keeps serving the others" was being checked against a client
    // that had never been dropped. Half the test's name went untested on a busy machine.
    await waitFor(() => bad.socket.destroyed, 'the bad client being dropped');
    expect(bad.socket.destroyed).toBe(true);
    good.send({ t: 'ping' });
    await good.until(() => good.text.some((m) => m.t === 'pong'), 'the survivor still answers');
  });

  it('reports health changes as they happen, once each', async () => {
    const h = await boot();
    const client = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    const id = 'fleet:kf-crawl-01/kontra-webcrawl/actor';
    client.send({ t: 'subscribe', id });
    await client.until(() => client.text.some((m) => m.t === 'state'), 'the first state');

    h.probe = {
      reachable: 'ok',
      session: 'absent',
      windows: [],
      detail: 'kf-crawl-01 is up but has no session kontra-webcrawl — converge to create it',
    };
    await h.server.refresh();
    await client.until(
      () =>
        client.text.some(
          (m) => m.t === 'state' && m.id === id && m.health.session === 'absent'
        ),
      'the session going away'
    );
    // Acceptance criterion 5: a killed session is words and an action, never a quiet tile.
    const last = [...client.text].reverse().find((m) => m.t === 'state');
    expect(last && 'health' in last && last.health.detail).toContain('converge');

    // An unchanged probe says nothing more.
    const before = client.text.filter((m) => m.t === 'state').length;
    await h.server.refresh();
    await new Promise((r) => setTimeout(r, 50));
    expect(client.text.filter((m) => m.t === 'state').length).toBe(before);
  });
});

/**
 * Focus and blur, over a real socket, down to the exact remote command.
 *
 * ADR 0020: the wall is snapshots and only a focused Terminal is a live attach. These tests are
 * where that sentence becomes a mechanism — including the two halves an operator pays for if they
 * are wrong: a live tile that is still charged for a snapshot exec, and a grouped session left
 * behind on a Machine.
 */
describe('focus promotes a Terminal to a live attach', () => {
  const ID = 'fleet:kf-crawl-01/kontra-webcrawl/actor';

  async function focused(h: Harness, cols = 120, rows = 40): Promise<TestClient> {
    const c = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    c.send({ t: 'subscribe', id: ID, cols, rows });
    await c.until(() => c.binary.length > 0, 'the first snapshot');
    c.send({ t: 'focus', id: ID, cols, rows });
    await c.until(
      () => c.text.some((m) => m.t === 'state' && m.mode === 'live'),
      'the Terminal going live'
    );
    return c;
  }

  it('seeds from backlog, then streams the PTY, at the size the tile measured', async () => {
    const h = await boot(undefined, 40);
    const c = await focused(h);

    const opened = h.ptys.last();
    // What addon-fit measured reaches `stty`, which is what makes the remote pane the tile's shape.
    expect(opened?.remote).toContain('stty rows 40 cols 120');
    expect(opened?.remote).toContain('attach-session -r');
    // The window is selected in the VIEWER's own session — targeting the owner's would move the
    // current window of whoever is attached on the Machine.
    expect(opened?.remote).toMatch(/select-window -t "kp-[a-z0-9]+:actor"/);
    expect(opened?.remote).toContain('script -q -c');

    // The tile is not blank while it waits for the first line: a focus is seeded with real backlog.
    await c.until(
      () => c.binary.some((f) => f.payload.toString().includes('before we went live')),
      'the backlog seed'
    );
    expect(h.machineSsh.execs.some((e) => e.includes('capture-pane -p -e -S -2000'))).toBe(true);

    opened?.events.onData(Buffer.from('a live line\n', 'utf8'));
    await c.until(
      () => c.binary.some((f) => f.payload.toString().includes('a live line')),
      'a live frame'
    );
  });

  it('stops paying for a snapshot exec while a Terminal is live', async () => {
    // Twelve Machines cost twelve periodic execs; a focused tile must not cost a thirteenth on top
    // of its PTY, and painting a 40 ms snapshot over a scrolling screen would be visible.
    const h = await boot(undefined, 40);
    const c = await focused(h);
    const before = h.snapshots.length;
    await new Promise((r) => setTimeout(r, 200));
    expect(h.snapshots.length).toBe(before);

    c.send({ t: 'blur', id: ID });
    await waitFor(() => h.snapshots.length > before, 'snapshots resuming after a blur');
  });

  it('demotes on blur, and kills the grouped session it created', async () => {
    const h = await boot(undefined, 40);
    const c = await focused(h);
    const viewer = h.ptys.viewers()[0];
    expect(viewer).toMatch(/^kp-[a-z0-9]+$/);

    c.send({ t: 'blur', id: ID });
    await c.until(
      () => c.text.filter((m) => m.t === 'state' && m.mode === 'snapshot').length >= 2,
      'the demote'
    );
    await waitFor(() => h.machineSsh.kills().length === 1, 'the viewer session being killed');
    expect(h.machineSsh.kills()[0]).toContain(viewer as string);
    expect(h.ptys.last()?.killed).toBe(1);
  });

  it('kills the grouped session when the tab simply disappears', async () => {
    // THE LEAK BOUNDARY. Measured on tmux 3.3a: a `kp-*` session outlives the client that created
    // it, and while it exists the owner's windows survive a `kill-session` on the owner — so a
    // closed laptop lid would otherwise leave a Machine that cannot be quiesced.
    const h = await boot(undefined, 40);
    const c = await focused(h);
    const viewer = h.ptys.viewers()[0] as string;
    c.socket.destroy();
    await waitFor(() => h.machineSsh.kills().length === 1, 'the viewer session being killed');
    expect(h.machineSsh.kills()[0]).toContain(viewer);
  });

  it('kills the grouped sessions of every client on shutdown', async () => {
    const h = await boot(undefined, 40);
    await focused(h);
    const viewer = h.ptys.viewers()[0] as string;
    await h.server.close();
    open = open.filter((s) => s !== h.server);
    // Awaited on this path, not fired and forgotten: a restarted streamer cannot find these
    // sessions again, because it does not know the nonces.
    expect(h.machineSsh.kills()[0]).toContain(viewer);
  });

  it('degrades to mode:error with a sentence when the Machine cannot be read, never a spinner', async () => {
    const h = await boot(undefined, 40);
    h.machineSsh.reachable = false;
    const c = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    c.send({ t: 'subscribe', id: ID });
    c.send({ t: 'focus', id: ID, cols: 120, rows: 40 });
    await c.until(
      () => c.text.some((m) => m.t === 'state' && m.mode === 'error'),
      'the tile reporting an error'
    );
    const said = c.text.find((m) => m.t === 'error' && m.message.includes('unreachable'));
    expect(said).toBeTruthy();
    // No PTY was opened toward a Machine we could not read from, so no session can have leaked.
    expect(h.ptys.opened).toHaveLength(0);
    expect(h.machineSsh.kills()).toEqual([]);
  });

  it('gives two browsers on one Machine two independent grouped sessions', async () => {
    // ADR 0020's acceptance criterion 7: one snapshot exec per Machine, but independent live
    // attaches. A shared tmux client would mean one browser's window selection moving the other's.
    const h = await boot(undefined, 40);
    const a = await focused(h);
    const b = await focused(h);
    expect(h.ptys.opened).toHaveLength(2);
    const [first, second] = h.ptys.viewers();
    expect(first).not.toBe(second);

    // …and bytes go only to the tile that asked for them.
    h.ptys.opened[0]?.events.onData(Buffer.from("client a's bytes\n", 'utf8'));
    await a.until(
      () => a.binary.some((f) => f.payload.toString().includes("client a's bytes")),
      'client a receiving its own stream'
    );
    expect(b.binary.some((f) => f.payload.toString().includes("client a's bytes"))).toBe(false);
  });

  it('re-seeds a re-focused tile instead of churning a second session on the Machine', async () => {
    const h = await boot(undefined, 40);
    const c = await focused(h);
    h.ptys.last()?.events.onData(Buffer.from('scrollback\n', 'utf8'));
    await c.until(
      () => c.binary.some((f) => f.payload.toString().includes('scrollback')),
      'the live line'
    );
    const frames = c.binary.length;

    // A remount, a re-render, a tab coming back — the same tile at the same size.
    c.send({ t: 'focus', id: ID, cols: 120, rows: 40 });
    await c.until(() => c.binary.length > frames, 'the replay');
    expect(h.ptys.opened).toHaveLength(1);
    expect(h.machineSsh.kills()).toEqual([]);
    // The replay is a repaint carrying what the tile was shown, so a remount does not go blank.
    const replay = c.binary[c.binary.length - 1]?.payload.toString() ?? '';
    expect(replay.startsWith('\x1b[H\x1b[2J')).toBe(true);
    expect(replay).toContain('scrollback');
  });

  it('does not start a second attach while the first is still coming up', async () => {
    // The window this is about is an SSH round trip wide, and a tile that asks twice inside it (a
    // double click, a resize landing on a mount) would otherwise orphan the first attach — a grouped
    // session on a Machine that nothing will ever kill, because nothing holds its nonce any more.
    const h = await boot(undefined, 40);
    h.machineSsh.holdBacklog();
    const c = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    c.send({ t: 'subscribe', id: ID, cols: 120, rows: 40 });
    c.send({ t: 'focus', id: ID, cols: 120, rows: 40 });
    await waitFor(() => h.machineSsh.backlogReads() === 1, 'the backlog read');
    c.send({ t: 'focus', id: ID, cols: 120, rows: 40 });
    await new Promise((r) => setTimeout(r, 60));

    h.machineSsh.releaseBacklog();
    await c.until(
      () => c.text.some((m) => m.t === 'state' && m.mode === 'live'),
      'the Terminal going live'
    );
    await new Promise((r) => setTimeout(r, 60));
    expect(h.machineSsh.backlogReads()).toBe(1);
    expect(h.ptys.opened).toHaveLength(1);
    expect(h.machineSsh.kills()).toEqual([]);
  });

  it('re-attaches at a new size, because a PTY cannot be resized without a channel to write on', async () => {
    const h = await boot(undefined, 40);
    const c = await focused(h, 120, 40);
    const firstViewer = h.ptys.viewers()[0] as string;
    c.send({ t: 'focus', id: ID, cols: 80, rows: 24 });
    await waitFor(() => h.ptys.opened.length === 2, 'the second attach');
    expect(h.ptys.last()?.remote).toContain('stty rows 24 cols 80');
    // The old grouped session goes away with the old PTY rather than being left behind.
    await waitFor(() => h.machineSsh.kills().length === 1, 'the first viewer session being killed');
    expect(h.machineSsh.kills()[0]).toContain(firstViewer);
  });

  it('bounds a hostile geometry instead of putting it in a command', async () => {
    const h = await boot(undefined, 40);
    const c = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    c.send({ t: 'focus', id: ID, cols: 1e9, rows: -5 });
    await waitFor(() => h.ptys.opened.length === 1, 'the attach');
    // Clamped UP to the floor, not down to 1. `stty` is how a viewer's number reaches the
    // operator's real window, so the floor is the guard — see MIN_COLS/MIN_ROWS.
    expect(h.ptys.last()?.remote).toContain(`stty rows ${MIN_ROWS} cols ${MAX_DIMENSION}`);
  });

  it('never lets a tile that measured itself too small reach stty', async () => {
    // THE REGRESSION. A tile measuring before layout asked for 12x6, the streamer `stty`d it, and
    // tmux's default `window-size latest` resized the operator's real 200x50 worker to 12x5 and
    // left it there — every log line wrapping at twelve characters. Measured on a live box.
    const h = await boot(undefined, 40);
    const c = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    c.send({ t: 'focus', id: ID, cols: 12, rows: 6 });
    await waitFor(() => h.ptys.opened.length === 1, 'the attach');
    const remote = h.ptys.last()?.remote ?? '';
    expect(remote).toContain(`stty rows ${MIN_ROWS} cols ${MIN_COLS}`);
    expect(remote).not.toContain('cols 12');
  });

  it('refuses a focus on an id or a Machine it does not have', async () => {
    const h = await boot(undefined, 40);
    const c = await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    c.send({ t: 'focus', id: 'fleet:kf-crawl-01/kontra-webcrawl/$(id)' });
    await c.until(
      () => c.text.some((m) => m.t === 'error' && m.message.includes('not a Terminal id')),
      'the refusal of a hostile id'
    );
    c.send({ t: 'focus', id: 'fleet:kf-ghost-99/kontra-webcrawl/actor' });
    await c.until(
      () => c.text.some((m) => m.t === 'error' && m.message.includes('no such Terminal')),
      'the refusal of an unknown Terminal'
    );
    expect(h.ptys.opened).toHaveLength(0);
    // A blur of something never focused is a no-op, not an error: browsers do that.
    c.send({ t: 'blur', id: ID });
    c.send({ t: 'ping' });
    await c.until(() => c.text.some((m) => m.t === 'pong'), 'the socket still working');
  });

  it('tells the tile the attach ended, and puts it back on snapshots', async () => {
    const h = await boot(undefined, 40);
    const c = await focused(h);
    h.ptys.last()?.events.onExit(0, null, '');
    await c.until(
      () => c.text.some((m) => m.t === 'error' && m.message.includes('back to snapshots')),
      'the end of the attach'
    );
    await c.until(
      () => c.text.filter((m) => m.t === 'state' && m.mode === 'snapshot').length >= 2,
      'the demote'
    );
    // The session it created is gone even though nobody blurred.
    await waitFor(() => h.machineSsh.kills().length === 1, 'the cleanup');
  });

  it('keeps telling a live tile its health without claiming it is back on snapshots', async () => {
    const h = await boot(undefined, 40);
    const c = await focused(h);
    h.probe = {
      reachable: 'ok',
      session: 'absent',
      windows: [],
      detail: 'kf-crawl-01 is up but has no session kontra-webcrawl — converge to create it',
    };
    await h.server.refresh();
    await c.until(
      () => c.text.some((m) => m.t === 'state' && m.health.session === 'absent'),
      'the health change'
    );
    const last = [...c.text].reverse().find((m) => m.t === 'state');
    // Mode and health travel on one message, so a health change must not silently demote a tile.
    expect(last && 'mode' in last && last.mode).toBe('live');
  });
});

describe('the streamer as a process', () => {
  it('survives a discovery that throws, rather than emptying the wall', async () => {
    // A rejected promise in this process is the failure ADR 0020 measured taking down an unrelated
    // `pulumi up`. It must be caught here, and it must not blank what was already known.
    let fail = false;
    const h = await boot({
      discover: async () => {
        if (fail) throw new Error('checkpoint unreadable');
        return [machine('kf-crawl-01')];
      },
    });
    expect(h.server.terminalList()).toHaveLength(2);
    fail = true;
    await expect(h.server.refresh()).rejects.toThrow('checkpoint unreadable');
    // The refresh threw before replacing anything, so the last known good wall is still there.
    expect(h.server.terminalList()).toHaveLength(2);
    const res = await http(h.port, 'GET', '/api/panels/health');
    expect(res.status).toBe(200);
  });

  it('records a probe that throws as unknown, never as healthy', async () => {
    const h = await boot({
      probe: async () => {
        throw new Error('ssh exploded');
      },
    });
    await h.server.refresh();
    for (const t of h.server.terminalList()) {
      expect(t.health.reachable).toBe('unknown');
      expect(t.health.session).toBe('unknown');
      expect(t.health.detail).toContain('ssh exploded');
    }
  });

  it('closes cleanly with clients attached', async () => {
    const h = await boot();
    await TestClient.connect(h.port, await ticket(h.port), ORIGIN);
    await h.server.close();
    open = open.filter((s) => s !== h.server);
    await expect(http(h.port, 'GET', '/api/panels/health')).rejects.toThrow();
  });
});
