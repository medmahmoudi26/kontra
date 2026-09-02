/**
 * The streamer with Wardens reporting into it, over a real socket.
 *
 * A SEPARATE FILE FROM `server.test.ts` ON PURPOSE. That file's harness gives every Machine an SSH
 * probe that always answers "session present", which is exactly the fixture this slice's rules must
 * be able to override: a Warden's report has to be able to say something the probe does not, and a
 * gone Machine has to leave the wall while the probe is still cheerfully reporting it. Sharing the
 * harness would have meant weakening it for everybody.
 *
 * ═══ THE CONTROLS COME FIRST ═══
 *
 * Every assertion below that a pane LEFT is preceded, in the same test, by the assertion that it was
 * there. This program has found five vacuous guards; "the wall is empty" is the easiest sixth to
 * write, because it passes against a streamer that never had a wall.
 */

import { afterEach, describe, expect, it } from 'vitest';

import { PanelServer, type PanelDeps } from './server';
import type { MachineTarget } from './discovery';
import type { ProbeResult } from './probe';
import { HOST_MARKER } from './tmux';
import { interpretProbe } from './probe';
import { WardenReports, REPORT_TTL_MS, type WardenReport } from './warden';

const TOKEN = 'panel-token-for-tests';
const TOKEN_VAR = 'KONTRA_PANEL_TOKEN_TEST';

const open: PanelServer[] = [];
afterEach(async () => {
  for (const s of open.splice(0)) await s.close();
  delete process.env[TOKEN_VAR];
});

function machine(n: string): MachineTarget {
  return {
    mode: 'fleet',
    machine: n,
    host: '10.124.0.5',
    publicIp: '203.0.113.9',
    tag: 'nscheck',
    fleet: 'nscheck-0.1.0',
    actor: 'nscheck',
    version: '0.1.0',
    session: 'nscheck-0_1_0',
    windows: ['actor', 'handler'],
  };
}

function report(over: Partial<WardenReport> = {}): WardenReport {
  return {
    warden: 'w-abc',
    machine: 'kf-nscheck-01',
    driver: 'podman',
    at: '2026-08-30T12:00:00Z',
    telemetry: { cpu: 0.42, memory: 0.31, load1: 1.25 },
    workers: [
      {
        id: 'nscheck@0.1.0',
        name: 'nscheck',
        version: '0.1.0',
        whole: true,
        halves: ['actor', 'handler'],
        health: { verdict: 'healthy', reason: 'ratio', ratio: 0.02, loads: 82, failures: 2, spanSeconds: 300 },
        panes: [
          { window: 'actor', cols: 120, rows: 40, frame: 'actor is up and polling' },
          { window: 'handler', cols: 120, rows: 40, frame: 'handler is up and polling' },
        ],
      },
    ],
    ...over,
  };
}

interface Harness {
  server: PanelServer;
  port: number;
  reports: WardenReports;
  now: { ms: number };
  /** Every Machine the SSH probe was asked about. The `reports` path must not use it. */
  probed: string[];
}

async function boot(over: Partial<PanelDeps> = {}, machines: MachineTarget[] = [machine('kf-nscheck-01')]): Promise<Harness> {
  process.env[TOKEN_VAR] = TOKEN;
  const now = { ms: 1_000_000 };
  const reports = new WardenReports(() => now.ms);
  const probed: string[] = [];
  const deps: PanelDeps = {
    discover: async () => machines,
    probe: async (m) => {
      probed.push(m.machine);
      return { reachable: 'ok', session: 'present', windows: ['actor', 'handler'] } as ProbeResult;
    },
    snapshot: async () => new Map(),
    reports,
    now: () => now.ms,
    log: () => undefined,
    ...over,
  };
  const server = new PanelServer(deps, {
    snapshotMs: 60_000,
    discoverMs: 60_000,
    origins: ['http://localhost:8088'],
    tokenVars: [TOKEN_VAR],
  });
  const port = await server.listen(0, '127.0.0.1');
  open.push(server);
  await server.refresh();
  return { server, port, reports, now, probed };
}

async function post(port: number, body: unknown, token = TOKEN): Promise<{ status: number; body: string }> {
  const res = await fetch(`http://127.0.0.1:${port}/api/panels/report`, {
    method: 'POST',
    headers: { authorization: `Bearer ${token}`, 'content-type': 'application/json' },
    body: typeof body === 'string' ? body : JSON.stringify(body),
  });
  return { status: res.status, body: await res.text() };
}

describe('the ingest', () => {
  it('takes a report the Machine dialled in, and refuses one with no credential', async () => {
    const h = await boot();
    // THE CONTROL: unauthenticated is refused, or "it accepted the report" says nothing about the
    // gate. The panel token is the whole authority here today — see `warden.ts`'s header on what
    // that is weaker than.
    expect((await post(h.port, report(), 'wrong')).status).toBe(401);
    expect(h.reports.size()).toBe(0);

    const ok = await post(h.port, report());
    expect(ok.status).toBe(202);
    expect(h.reports.has('kf-nscheck-01')).toBe(true);
  });

  it('refuses a body that is not a report, without taking anything from it', async () => {
    const h = await boot();
    expect((await post(h.port, 'not json at all')).status).toBe(400);
    expect((await post(h.port, { warden: 'w', workers: [] })).status).toBe(400); // no machine
    expect(h.reports.size()).toBe(0);
  });

  it('caps the body, because this is the one route a stranger writes', async () => {
    const h = await boot();
    const huge = report();
    huge.workers[0]!.panes[0]!.frame = 'x'.repeat(2 * 1024 * 1024);
    const res = await post(h.port, huge);
    expect(res.status).toBe(413);
    expect(h.reports.size()).toBe(0);
  });

  it('tells a Warden reporting into a streamer that holds nothing, rather than silently accepting', async () => {
    // A 204 here is how a Fleet reports happily into nothing for a week.
    const h = await boot({ reports: undefined });
    expect((await post(h.port, report())).status).toBe(501);
  });
});

describe('a reporting Machine’s panes come from its Warden, not from its address', () => {
  it('puts the report’s panes on the wall and stops probing the Machine', async () => {
    const h = await boot();
    // THE CONTROL: with no report, the Machine is probed over SSH exactly as before.
    expect(h.probed).toContain('kf-nscheck-01');
    expect(h.server.terminalList().map((t) => t.id)).toEqual([
      'fleet:kf-nscheck-01/nscheck-0_1_0/actor',
      'fleet:kf-nscheck-01/nscheck-0_1_0/handler',
    ]);

    h.probed.length = 0;
    await post(h.port, report());
    await h.server.refresh();

    // …and now it is not dialled at all. A report is dialled BY the Machine, so it is both cheaper
    // than an SSH round trip and the only evidence an address cannot forge.
    expect(h.probed).toEqual([]);
    const terminals = h.server.terminalList();
    expect(terminals.map((t) => t.id)).toEqual([
      'fleet:kf-nscheck-01/nscheck-0_1_0/actor',
      'fleet:kf-nscheck-01/nscheck-0_1_0/handler',
    ]);
    expect(terminals[0]!.paneCols).toBe(120);
    expect(terminals[0]!.paneRows).toBe(40);
    expect(terminals[0]!.health.session).toBe('present');
    expect(terminals[0]!.health.reachable).toBe('ok');
  });

  it('carries a sick Worker onto the loads chip, and a cannot-tell as unknown', async () => {
    const h = await boot();
    const sick = report();
    sick.workers[0]!.health = {
      verdict: 'sick',
      reason: 'ratio',
      detail: '81 of its 82 resource loads failed in 5m0s (99%) — the process is alive',
      ratio: 0.988,
      loads: 82,
      failures: 81,
      spanSeconds: 300,
    };
    await post(h.port, sick);
    await h.server.refresh();
    const t = h.server.terminalList()[0]!;
    expect(t.health.loads).toBe('failing');
    expect(t.health.detail).toContain('81 of its 82 resource loads failed');

    // …and a Warden that cannot tell leaves the chip alone rather than turning it green. The
    // one-way rule `metrics.ts` argues for at length.
    const blind = report();
    blind.workers[0]!.health = {
      verdict: 'cannot-tell',
      reason: 'scrape',
      detail: 'could not read its counters',
      ratio: 0,
      loads: 0,
      failures: 0,
      spanSeconds: 0,
    };
    await post(h.port, blind);
    await h.server.refresh();
    expect(h.server.terminalList()[0]!.health.loads).not.toBe('ok');
  });

  it('puts a self-enrolled Machine on the wall although no checkpoint has ever named it', async () => {
    // ADR 0037's `machines=None`: capacity that already exists, provisioned by nobody here.
    const h = await boot({}, []);
    expect(h.server.terminalList()).toEqual([]);

    // The name still has to be one a Terminal id can carry (`ids.ts:SAFE.machine`), which the
    // ingest now enforces rather than leaving to be silently dropped at `terminalId`.
    await post(h.port, report({ machine: 'kf-byoc-01' }));
    await h.server.refresh();
    expect(h.server.terminalList().map((t) => t.id)).toEqual([
      'fleet:kf-byoc-01/nscheck-0_1_0/actor',
      'fleet:kf-byoc-01/nscheck-0_1_0/handler',
    ]);

    // …and a report under a name no id could carry is refused AT THE INGEST, not accepted and then
    // rendered as nothing — which is indistinguishable from a Machine whose reports never arrived.
    expect((await post(h.port, report({ machine: 'byoc-01' }))).status).toBe(400);
  });
});

describe('a destroyed Machine leaves the wall instead of offering a converge', () => {
  it('drops the panes when a Warden goes quiet AND the Machine cannot be reached', async () => {
    // SILENCE IS CORROBORATED, NEVER TAKEN ALONE. The first version of this rule dropped a Machine
    // the moment its Warden stopped reporting, which would hide a live Worker whose Warden had
    // merely crashed — see `server.ts:attribute`. So the Machine falls back to the SSH probe it was
    // skipping, and two independent sources have to agree before a tile leaves the wall.
    let reachable = true;
    const h = await boot({
      probe: async () =>
        (reachable
          ? { reachable: 'ok', session: 'present', windows: ['actor', 'handler'] }
          : { reachable: 'fail', session: 'unknown', windows: [], detail: 'ssh: connect: timed out' }) as ProbeResult,
    });
    await post(h.port, report());
    await h.server.refresh();
    // THE CONTROL: it is on the wall first, or "it disappeared" is a fact about an empty streamer.
    expect(h.server.terminalList()).toHaveLength(2);

    // The Warden goes quiet and the Machine still answers: it stays, and the tile says the Warden
    // is down. Nothing on that Machine is reconciling or judging right now, which is a finding.
    h.now.ms += REPORT_TTL_MS + 1;
    await h.server.refresh();
    expect(h.server.terminalList()).toHaveLength(2);
    expect(h.server.terminalList()[0]!.health.detail).toContain('its Warden has stopped reporting');

    // …and now the Machine stops answering too. Two sources agree, and the tile goes.
    reachable = false;
    await h.server.refresh();
    expect(h.server.terminalList()).toEqual([]);
  });

  it('answers a converge for it with why it left, and never starts one', async () => {
    const converges: string[] = [];
    const h = await boot({
      converge: async (m) => {
        converges.push(m.machine);
        return { workflowId: `tmux-${m.machine}` };
      },
      // Unreachable, so the Warden's silence is corroborated — see the test above.
      probe: async () =>
        ({ reachable: 'fail', session: 'unknown', windows: [], detail: 'ssh: connect: timed out' }) as ProbeResult,
    });
    await post(h.port, report());
    await h.server.refresh();
    h.now.ms += REPORT_TTL_MS + 1;
    await h.server.refresh();

    const client = await connect(h.port);
    client.send({ t: 'converge', id: 'fleet:kf-nscheck-01/nscheck-0_1_0/actor' });
    const err = await client.nextError();
    expect(err).toContain('stopped reporting');
    expect(err).toContain('gone rather than merely');
    expect(err).toContain('the probe agrees');
    // A converge against a Machine that left would SSH to whatever now holds its address.
    expect(converges).toEqual([]);
    client.close();
  });

  it('leaves a Machine that NEVER had a Warden exactly as it was', async () => {
    // The asymmetry is the rule. Silence from a Machine that never enrolled says nothing — a Fleet
    // placed before the Warden, an appliance install, a `docker` node — so the SSH path is
    // untouched for it.
    const h = await boot();
    h.now.ms += REPORT_TTL_MS * 10;
    await h.server.refresh();
    expect(h.server.terminalList()).toHaveLength(2);
  });

  it('drops the panes of a Machine whose address now answers as somebody else', async () => {
    // The observed bug, through the whole stack: a recycled VPC address plus a session named
    // `<actor>-<version>` put one Machine's live screen on the wall under a destroyed Machine's
    // name. No Warden involved — this is the SSH path's own identity check.
    const panes = 'nscheck-0_1_0 actor %1 120x40 0 c:zsh x: \nnscheck-0_1_0 handler %2 120x40 0 c:zsh x: \n';
    let stdout = `${HOST_MARKER} kf-nscheck-01\n${panes}`;
    const h = await boot({
      probe: async (m) => interpretProbe(m, 0, stdout, ''),
    });
    expect(h.server.terminalList()).toHaveLength(2);

    stdout = `${HOST_MARKER} kf-nscheck-07\n${panes}`;
    await h.server.refresh();
    expect(h.server.terminalList()).toEqual([]);

    const client = await connect(h.port);
    client.send({ t: 'subscribe', id: 'fleet:kf-nscheck-01/nscheck-0_1_0/actor' });
    const err = await client.nextError();
    expect(err).toContain('kf-nscheck-07');
    expect(err).not.toBe('no such Terminal in the Fleet inventory');
    client.close();
  });
});

// --- a minimal WS client, because this file's assertions are about two messages ------------------

interface TestSocket {
  send(msg: unknown): void;
  nextError(): Promise<string>;
  close(): void;
}

async function connect(port: number): Promise<TestSocket> {
  const ticket = await fetch(`http://127.0.0.1:${port}/api/panels/ticket`, {
    method: 'POST',
    headers: { authorization: `Bearer ${TOKEN}` },
  }).then((r) => r.json() as Promise<{ ticket: string }>);

  const { newClientKey, encodeMaskedFrame, FrameDecoder, OPCODE } = await import('./ws');
  const net = await import('node:net');
  const socket = net.connect(port, '127.0.0.1');
  // `requireMask: false` — this is reading the SERVER's frames, which RFC 6455 leaves unmasked.
  const decoder = new FrameDecoder(1_000_000, false);
  const errors: string[] = [];
  let waiting: ((s: string) => void) | undefined;

  const onFrames = (buf: Buffer): void => {
    for (const frame of decoder.push(buf)) {
      if (frame.opcode !== OPCODE.text) continue;
      const msg = JSON.parse(frame.payload.toString('utf8')) as { t: string; message?: string };
      if (msg.t === 'error' && msg.message) {
        errors.push(msg.message);
        waiting?.(msg.message);
        waiting = undefined;
      }
    }
  };

  // THE HANDSHAKE RESPONSE IS AWAITED BEFORE ANYTHING IS SENT, and it is not politeness. Node's
  // `http.Server` 'upgrade' event carries the first bytes of the upgraded stream in its `head`
  // argument, and `server.ts` does not read it — correctly, since a browser never speaks before the
  // response arrives. A test client that wrote a frame into the same TCP segment as its request
  // would have that frame silently swallowed, which reads as a server that never answered.
  await new Promise<void>((resolve, reject) => {
    let headers = '';
    socket.on('error', reject);
    const onData = (chunk: Buffer): void => {
      headers += chunk.toString('latin1');
      const end = headers.indexOf('\r\n\r\n');
      if (end < 0) return;
      socket.removeListener('data', onData);
      const rest = Buffer.from(headers.slice(end + 4), 'latin1');
      socket.on('data', onFrames);
      if (rest.length) onFrames(rest);
      resolve();
    };
    socket.on('data', onData);
    socket.on('connect', () => {
      socket.write(
        `GET /api/panels/ws?ticket=${encodeURIComponent(ticket.ticket)} HTTP/1.1\r\n` +
          `Host: 127.0.0.1:${port}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n` +
          `Sec-WebSocket-Key: ${newClientKey()}\r\nSec-WebSocket-Version: 13\r\n\r\n`
      );
    });
  });

  return {
    send: (msg) => socket.write(encodeMaskedFrame(OPCODE.text, Buffer.from(JSON.stringify(msg)))),
    nextError: () =>
      new Promise<string>((resolve, reject) => {
        const first = errors.shift();
        if (first !== undefined) return resolve(first);
        const timer = setTimeout(() => reject(new Error('no error message arrived')), 3000);
        waiting = (s) => {
          clearTimeout(timer);
          resolve(s);
        };
      }),
    close: () => socket.destroy(),
  };
}
