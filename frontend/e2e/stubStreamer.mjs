/**
 * A stub Dashboard streamer — the fixture behind Playwright project `contract` (ADR 0020).
 *
 * WRITTEN BY HAND, ON PURPOSE, and it is HALF the suite. This file implements RFC 6455 and ADR 0020's
 * framing INDEPENDENTLY of `backend/src/panels/`, which is what makes "a browser can read what
 * the wire specifies" a claim about the contract rather than about one author's code: if the two sides
 * drift, project `contract` fails.
 *
 * What it cannot do is fail on a bug in the real streamer's hand-rolled `Sec-WebSocket-Accept`, frame
 * encoding, masking, fragmentation, close handling or 126/127 length cases — nothing here executes
 * that code. That is project `real` (`e2e/streamer.ts`), which boots `PanelServer` itself with the
 * same specs. CONTRACT.md amendment 14 records why both exist; a disagreement between them is the most
 * valuable output this suite can produce.
 *
 * It serves the four routes with the shapes CONTRACT.md pins, mints single-use tickets, fails closed
 * with 503 when its token is switched off, and pushes one snapshot frame per interval:
 * `[1 byte idLen][id][\x1b[H\x1b[2J + screen]`.
 *
 * It has no SSH, no Pulumi and no Temporal.
 */

import { createHash, randomBytes } from 'node:crypto';
import { createServer } from 'node:http';

const PORT = Number(process.env.STUB_PANEL_PORT ?? 8190);
const TOKEN = process.env.STUB_PANEL_TOKEN ?? 'stub-panel-token';
const ORIGIN = process.env.STUB_PANEL_ORIGIN ?? '*';
/** How often a subscribed Terminal is repainted. Fast, so a spec does not wait 3 s per assertion. */
const SNAPSHOT_MS = Number(process.env.STUB_SNAPSHOT_MS ?? 150);
/** The var name the fail-closed reply names, matching what `auth.ts` would say. */
const TOKEN_VAR = process.env.STUB_PANEL_TOKEN_VAR ?? 'KONTRA_PANEL_TOKEN';

/** Two Machines, same as `fakeFleet.ts`: one with its session, one without. Kept in step so ONE spec
 * file can drive either streamer — the ids, the windows and the screens have to line up. */
const MACHINE = 'kf-crawl-01';
const MACHINE_WITHOUT_SESSION = 'kf-crawl-02';
const SESSION = 'kontra-webcrawl';
const WINDOWS = ['actor', 'handler'];
/** Must match `SCREEN_MARKER` in `fakeFleet.ts`. Duplicated rather than imported: this file is
 * deliberately independent of the TypeScript side, and a spec asserting on it would catch a drift. */
const SCREEN_MARKER = 'KONTRA-E2E-SCREEN';

/**
 * Fixture state. `/__stub/*` is a control channel and exists ONLY here: the real streamer has no such
 * route, `readonly.test.ts` is what guarantees it never grows one, and project `real` drives the same
 * specs by reaching into its in-process seams instead.
 *
 * `POST /__stub/session?state=absent` also pushes a `state` message to every open socket, so the
 * "no session — converge" tile is exercised on the LIVE path a real `tmux kill-session` would take
 * rather than only at page load.
 */
let sessionState = process.env.STUB_SESSION ?? 'present';
let label = 'SCREEN-ONE';
let tokenOn = true;
let paints = 0;
let converges = [];
/** Sockets to notify when the fixture's state changes. */
const live = new Set();

function healthFor(machine) {
  const state = machine === MACHINE_WITHOUT_SESSION ? 'absent' : sessionState;
  const health = {
    reachable: 'ok',
    session: state,
    // Slice 3 measures these; the stub says what the real streamer says today.
    poller: 'unknown',
    loads: 'unknown',
  };
  if (state !== 'present') {
    health.detail = `${machine} is up but has no session ${SESSION} — converge to create it`;
  }
  return health;
}

function terminals() {
  const out = [];
  for (const machine of [MACHINE, MACHINE_WITHOUT_SESSION]) {
    for (const window of WINDOWS) {
      out.push({
        id: `fleet:${machine}/${SESSION}/${window}`,
        machine,
        host: machine === MACHINE ? '10.124.0.9' : '10.124.0.10',
        publicIp: machine === MACHINE ? '203.0.113.9' : '203.0.113.10',
        tag: 'crawl',
        fleet: 'fleet-apex-119',
        actor: 'webcrawl',
        version: '0.2.0',
        window,
        health: healthFor(machine),
      });
    }
  }
  // Sorted by id, like `terminalList()`: the Dashboard pins the first one.
  return out.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
}

/** One window's screen, in the same shape `fakeFleet.ts` produces — including the SGR sequences a
 * `capture-pane -e` carries, so a spec can prove xterm PARSED the bytes. */
function screenFor(id, paint) {
  const machine = id.split(':')[1]?.split('/')[0] ?? MACHINE;
  const window = id.split('/').pop() ?? 'actor';
  const clock = String(paint % 60).padStart(2, '0');
  return [
    `\x1b[1;32m●\x1b[0m kontra-${window}.service on ${machine}`,
    `Aug 11 22:04:${clock} ${machine} kontra-${window}[1421]: fetched page/${paint}`,
    `Aug 11 22:04:${clock} ${machine} kontra-${window}[1421]: \x1b[33mwarn\x1b[0m retrying once`,
    `${SCREEN_MARKER} ${label} paint=${paint}`,
  ].join('\n');
}

const tickets = new Set();

function bearerOk(req) {
  return req.headers.authorization === `Bearer ${TOKEN}`;
}

/** The fail-closed reply, worded as `checkBearer` words it: an unset token means this streamer serves
 * NOTHING, which is a different problem from a caller's token being wrong (401). */
function disabledBody() {
  return { error: `disabled: set one of ${TOKEN_VAR} to enable this endpoint` };
}

function send(res, code, body) {
  const payload = JSON.stringify(body);
  res.writeHead(code, {
    'content-type': 'application/json',
    'content-length': Buffer.byteLength(payload),
    'access-control-allow-origin': ORIGIN,
    'access-control-allow-headers': 'authorization, content-type',
    'access-control-allow-methods': 'GET, POST, OPTIONS',
    vary: 'Origin',
    'cache-control': 'no-store',
  });
  res.end(payload);
}

// --- RFC 6455, server side, minimal ---------------------------------------------------------

const WS_GUID = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11';

function frame(opcode, payload) {
  const len = payload.length;
  let header;
  if (len < 126) {
    header = Buffer.alloc(2);
    header[1] = len;
  } else if (len < 65536) {
    header = Buffer.alloc(4);
    header[1] = 126;
    header.writeUInt16BE(len, 2);
  } else {
    header = Buffer.alloc(10);
    header[1] = 127;
    header.writeUInt32BE(0, 2);
    header.writeUInt32BE(len, 6);
  }
  header[0] = 0x80 | opcode;
  return Buffer.concat([header, payload]);
}

/** Read one masked client frame out of `buf`; returns [frame, rest] or null. */
function readFrame(buf) {
  if (buf.length < 2) return null;
  const opcode = buf[0] & 0x0f;
  let len = buf[1] & 0x7f;
  let offset = 2;
  if (len === 126) {
    if (buf.length < 4) return null;
    len = buf.readUInt16BE(2);
    offset = 4;
  } else if (len === 127) {
    if (buf.length < 10) return null;
    len = buf.readUInt32BE(6);
    offset = 10;
  }
  const masked = (buf[1] & 0x80) !== 0;
  const maskLen = masked ? 4 : 0;
  if (buf.length < offset + maskLen + len) return null;
  const mask = masked ? buf.subarray(offset, offset + 4) : null;
  offset += maskLen;
  const payload = Buffer.allocUnsafe(len);
  for (let i = 0; i < len; i += 1) {
    payload[i] = mask ? buf[offset + i] ^ mask[i & 3] : buf[offset + i];
  }
  return [{ opcode, payload }, buf.subarray(offset + len)];
}

/** ADR 0020's tagged binary framing. */
function tagged(id, payload) {
  const idBytes = Buffer.from(id, 'utf8');
  return Buffer.concat([Buffer.from([idBytes.length]), idBytes, payload]);
}

const server = createServer((req, res) => {
  const url = new URL(req.url ?? '/', 'http://stub.invalid');
  if (req.method === 'OPTIONS') {
    res.writeHead(204, {
      'access-control-allow-origin': ORIGIN,
      'access-control-allow-headers': 'authorization, content-type',
      'access-control-allow-methods': 'GET, POST, OPTIONS',
    });
    res.end();
    return;
  }
  // Every panel route serves nothing while the token is off, health included — that is how
  // `kontra doctor` can say "disabled" rather than "DOWN".
  if (!tokenOn && url.pathname.startsWith('/api/panels/')) return send(res, 503, disabledBody());

  if (url.pathname === '/api/panels/health') {
    const list = terminals();
    send(res, 200, { ok: true, terminals: list.length, live: live.size, machines: 2 });
    return;
  }
  if (url.pathname === '/api/panels/terminals') {
    if (!bearerOk(req)) return send(res, 401, { error: 'unauthorized' });
    return send(res, 200, { terminals: terminals() });
  }
  if (url.pathname === '/api/panels/ticket' && req.method === 'POST') {
    if (!bearerOk(req)) return send(res, 401, { error: 'unauthorized' });
    req.resume();
    const ticket = randomBytes(24).toString('base64url');
    tickets.add(ticket);
    return send(res, 200, { ticket, expiresAt: Date.now() + 30_000 });
  }

  // --- the fixture control channel -----------------------------------------------------------
  if (url.pathname === '/__stub/session' && req.method === 'POST') {
    sessionState = url.searchParams.get('state') ?? 'present';
    req.resume();
    return send(res, 200, { session: sessionState, notified: announce() });
  }
  if (url.pathname === '/__stub/label' && req.method === 'POST') {
    label = url.searchParams.get('value') ?? 'SCREEN-ONE';
    req.resume();
    return send(res, 200, { label });
  }
  if (url.pathname === '/__stub/token' && req.method === 'POST') {
    tokenOn = url.searchParams.get('state') !== 'off';
    req.resume();
    return send(res, 200, { tokenOn });
  }
  if (url.pathname === '/__stub/stats') {
    return send(res, 200, { ticketsOutstanding: tickets.size, paints, converges });
  }
  if (url.pathname === '/__stub/reset' && req.method === 'POST') {
    sessionState = 'present';
    label = 'SCREEN-ONE';
    tokenOn = true;
    paints = 0;
    converges = [];
    tickets.clear();
    req.resume();
    return send(res, 200, { ok: true });
  }
  send(res, 404, { error: 'not found' });
});

/** Announce current health to every open socket, one `state` per Terminal — what the real streamer's
 * `announceHealth()` does after a probe round. Returns how many sockets were told. */
function announce() {
  for (const socket of live) {
    for (const t of terminals()) {
      socket.write(
        frame(0x1, Buffer.from(JSON.stringify({ t: 'state', id: t.id, mode: 'snapshot', health: t.health })))
      );
    }
  }
  return live.size;
}

server.on('upgrade', (req, socket) => {
  socket.on('error', () => undefined);
  const url = new URL(req.url ?? '/', 'http://stub.invalid');
  const ticket = url.searchParams.get('ticket');
  if (!tokenOn) {
    // Fail closed on the socket too: a ticket minted before the token went away is worth nothing.
    socket.write('HTTP/1.1 503 Service Unavailable\r\nConnection: close\r\n\r\n');
    socket.destroy();
    return;
  }
  if (url.pathname !== '/api/panels/ws' || !ticket || !tickets.delete(ticket)) {
    socket.write('HTTP/1.1 401 Unauthorized\r\nConnection: close\r\n\r\n');
    socket.destroy();
    return;
  }
  const key = req.headers['sec-websocket-key'];
  const accept = createHash('sha1')
    .update(key + WS_GUID)
    .digest('base64');
  socket.write(
    'HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n' +
      `Sec-WebSocket-Accept: ${accept}\r\n\r\n`
  );

  live.add(socket);
  const list = terminals();
  socket.write(frame(0x1, Buffer.from(JSON.stringify({ t: 'hello', terminals: list.length }))));

  const subs = new Map();
  const timer = setInterval(() => {
    for (const [id] of subs) {
      paints += 1;
      // A full screen, preceded by home+clear so a repaint replaces the tile rather than appending.
      const screen = screenFor(id, paints);
      socket.write(frame(0x2, tagged(id, Buffer.from(`\x1b[H\x1b[2J${screen}`, 'utf8'))));
    }
  }, SNAPSHOT_MS);

  let buf = Buffer.alloc(0);
  socket.on('data', (chunk) => {
    buf = Buffer.concat([buf, chunk]);
    for (;;) {
      const next = readFrame(buf);
      if (!next) return;
      const [f, rest] = next;
      buf = rest;
      if (f.opcode === 0x8) {
        live.delete(socket);
        clearInterval(timer);
        socket.destroy();
        return;
      }
      if (f.opcode !== 0x1) continue;
      let msg;
      try {
        msg = JSON.parse(f.payload.toString('utf8'));
      } catch {
        continue;
      }
      const reply = (m) => socket.write(frame(0x1, Buffer.from(JSON.stringify(m))));
      if (msg.t === 'ping') reply({ t: 'pong' });
      else if (msg.t === 'subscribe') {
        const found = list.find((t) => t.id === msg.id);
        if (!found) reply({ t: 'error', id: msg.id, message: 'no such Terminal' });
        else {
          subs.set(msg.id, { cols: msg.cols, rows: msg.rows });
          reply({ t: 'state', id: msg.id, mode: 'snapshot', health: found.health });
        }
      } else if (msg.t === 'unsubscribe') subs.delete(msg.id);
      else if (msg.t === 'focus' || msg.t === 'blur') {
        reply({ t: 'error', id: msg.id, message: 'live attach lands in slice 2 (ADR 0020)' });
      } else if (msg.t === 'converge') {
        // WHICH Machine is the client's choice; WHAT a converge contains never is. Recorded so a spec
        // can assert the converge reached the streamer, and for the Machine the id names only.
        const machine = String(msg.id).split(':')[1]?.split('/')[0];
        const known = terminals().some((t) => t.machine === machine);
        if (!known) reply({ t: 'error', id: msg.id, message: 'no such Machine' });
        else {
          converges.push(machine);
          reply({ t: 'state', id: msg.id, mode: 'snapshot', health: healthFor(machine) });
        }
      }
    }
  });
  socket.on('close', () => {
    live.delete(socket);
    clearInterval(timer);
  });
});

server.on('error', (err) => {
  process.stderr.write(`[stub] ${String(err)}\n`);
  process.exit(1);
});
server.listen(PORT, '127.0.0.1', () => {
  process.stdout.write(`[stub] panels stub on http://127.0.0.1:${PORT} session=${sessionState}\n`);
});
