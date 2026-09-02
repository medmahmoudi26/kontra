/**
 * The streamer — HTTP + one multiplexed WebSocket per browser tab (ADR 0020).
 *
 * This module is the whole Dashboard read path, and it is deliberately assembled from injected
 * seams: discovery, the health probe, the snapshot exec and the converge are all interfaces, so
 * every path here is provable without a fleet. No Machine in this environment can be SSHed to, and
 * a design that could only be tested against one would have shipped untested.
 *
 * WHAT CANNOT HAPPEN HERE, and why each is structural rather than a review comment:
 *
 *  - **Nothing writes to a session.** There is no route, no message type and no code path that
 *    puts bytes on a channel to a Machine. ADR 0020's finding (3): `tmux attach -r` executed both
 *    `run-shell` and `send-keys` as root and reported no error, so read-only is a property of this
 *    file's message table, not of tmux.
 *  - **No floating promises.** Finding (6) measured an unhandled rejection failing an in-flight
 *    `pulumi up` from elsewhere in the same process. That is why this runs in a forked child, and
 *    it is still not licence to drop a rejection: every timer body is a try/catch/finally, and
 *    every socket has an `'error'` listener.
 *  - **The browser holds `KONTRA_PANEL_TOKEN` and nothing wider.** `KONTRA_STATE_TOKEN` also
 *    authorises `POST /api/infra/stacks/:fqn/:op`; a credential in a page must not be able to spend
 *    money. With no panel token configured, every route 503s and serves nothing — `auth.ts`'s
 *    fail-closed behaviour, reused rather than reimplemented.
 *  - **A stalled tab cannot become this process's memory profile.** Per-Terminal ring, per-Terminal
 *    second-by-second budget, and a socket whose kernel-side queue is backed up is elided rather
 *    than buffered.
 */

import * as http from 'node:http';
import type * as net from 'node:net';
import { checkBearer } from '../auth';
import { CLEAR_HOME, paneExitFromScreen } from './tmux';
import { clampDimension, type AttachSink, type Attacher, type LiveAttach } from './attach';
import { GEOMETRY } from './converge';
import { nodeKey, terminalId, tryParseTerminalId } from './ids';
import type { MachineTarget } from './discovery';
import { modeOf } from './transport';
import { joinDetails, probeTarget, type ProbeResult } from './probe';
import { measureFleetHealth, terminalsWithHealth, type FleetHealthProbe } from './health';
import { FrameBudget, Ring } from './stream';
import { TicketBook } from './tickets';
import {
  binaryFrame,
  closeFrame,
  encodeTagged,
  FrameDecoder,
  handshakeResponse,
  OPCODE,
  textFrame,
  WsProtocolError,
} from './ws';
import { UNKNOWN_HEALTH, type ClientMessage, type ServerMessage, type Terminal } from './types';
import {
  loadsFromReport,
  machineTelemetry,
  probeFromReport,
  targetFromReport,
  type WardenReport,
  type WardenReports,
} from './warden';

/** The token that mints tickets. One var, deliberately: see the header. */
export const PANEL_TOKEN_VARS = ['KONTRA_PANEL_TOKEN'] as const;

export const DEFAULT_PORT = 8090;
export const DEFAULT_SNAPSHOT_MS = 3000;
/** How often the inventory and the health probes are refreshed. Slower than snapshots: a Machine
 * appearing is a minute-scale event, and each round is one SSH per Machine. */
export const DEFAULT_DISCOVER_MS = 30_000;
export const DEFAULT_ORIGIN = 'http://localhost:8088';

/** Kernel-side queue depth at which a client is considered stalled and its frames are elided
 * rather than buffered. One screen is tens of KiB, so this is several seconds of grace. */
export const STALL_BYTES = 1024 * 1024;

/** The largest Warden report this server will read. See `readReport` for why there is a cap at all. */
export const REPORT_MAX_BYTES = 1024 * 1024;

/** Protocol-level keepalive, and how long silence is tolerated before the socket is dropped. */
const PING_MS = 30_000;
const IDLE_MS = 120_000;

/** How long `close()` waits for connections to drain before it stops caring. See `drain()`: an
 * upgraded socket that died is never accounted for again, so this bound is what keeps a SIGTERM from
 * hanging. */
export const CLOSE_DRAIN_MS = 500;

export interface PanelDeps {
  /** Machines from the Fleet inventory — Pulumi stack outputs, read as a library call. */
  discover(): Promise<MachineTarget[]>;
  /** One `list-panes` per Machine: reachable, session present, real window list. */
  probe(machine: MachineTarget): Promise<ProbeResult>;
  /** ONE exec per Machine covering every window asked for. */
  snapshot(machine: MachineTarget, windows: string[]): Promise<Map<string, string>>;
  /** Start the session converge. Absent when this process has no Temporal client, in which case
   * a `converge` message is answered with an error instead of pretending to have worked. */
  converge?(machine: MachineTarget): Promise<{ workflowId: string }>;
  /** The live PTY attach a `focus` promotes to. Absent when this streamer cannot open one, in which
   * case a `focus` is refused with words rather than leaving a tile looking hung — the same rule
   * `converge` follows above. */
  attach?: Attacher;
  /** The `poller` and `loads` signals (slice 3). Absent means both report `unknown` with a sentence
   * saying so — never `ok`, which is the whole point: a chip derived from a counter nothing
   * increments is the same lie as a Run reporting `completed` while it lost an eighth of a sweep. */
  fleetHealth?: FleetHealthProbe;
  /**
   * Panes and telemetry a **Warden** dialled in (ADR 0037). Absent means every fleet Machine is
   * probed over SSH exactly as before — which is what a Fleet placed before the Warden, and every
   * `docker` or `local` node, permanently is.
   *
   * NOT A TRANSPORT. It holds no connection and dials nothing; the Machines POST to
   * `/api/panels/report` on this server, so the arrow is outbound from the Machine and NAT, a
   * private VPC and a customer's firewall all keep working.
   */
  reports?: WardenReports;
  now?(): number;
  log?(line: string, extra?: Record<string, unknown>): void;
}

export interface PanelConfig {
  snapshotMs?: number;
  discoverMs?: number;
  /** CORS allow-list for the ticket POST and the WS `Origin` check. */
  origins?: string[];
  /** Injectable so a test can prove the fail-closed path without unsetting a real env var. */
  tokenVars?: readonly string[];
}

interface Subscription {
  budget: FrameBudget;
  /** The health last announced to this client, so `state` is sent on change and not every tick. */
  announced?: string;
  /** `snapshot` until a `focus` promotes it; `error` when the promotion failed. One tile focused in
   * one tab says nothing about the same Terminal in another, which is why this is per subscription
   * and not per Terminal. */
  mode: 'snapshot' | 'live' | 'error';
  /** The live attach, while there is one. Owning it here is what makes a disconnect able to kill the
   * grouped session it created on the Machine. */
  attach?: LiveAttach;
  /** The geometry the attach was launched with. A PTY's size is fixed at `stty` time, so a re-focus
   * at a different size has to be a new attach — and at the SAME size must not be. */
  cols?: number;
  rows?: number;
}

/**
 * Fold a screen-derived exit status into a Terminal's health.
 *
 * ONE DIRECTION ONLY, and that is the deliberate half. An exit banner on screen upgrades the pane's
 * verdict to `exited`; the ABSENCE of one never downgrades it, because absence is also what a
 * Terminal nobody has subscribed to looks like, and a wall must not oscillate between "exited 143"
 * and "unknown" on the snapshot cadence. A pane whose command genuinely restarted gets its verdict
 * back from the next probe, within one discovery round — and under the hold shell a restart cannot
 * happen without a keystroke, which read-only makes impossible.
 *
 * Exported for the test that pins exactly that asymmetry.
 */
export function applyScreenExit(t: Terminal, exitStatus: string | undefined): void {
  if (!exitStatus) return;
  // The probe already said so, from `@kontra_exit` or `pane_dead`: take the status and leave the
  // sentence alone, or the tile would carry two phrasings of one fact.
  const known = t.health.process === 'exited';
  t.exitStatus = exitStatus;
  const detail = known
    ? t.health.detail
    : joinDetails([
        t.health.detail,
        `this pane printed \`[exited ${exitStatus}]\` and is waiting on a keypress that read-only ` +
          'cannot send — the screen below is its final output, not a live one',
      ]);
  t.health = { ...t.health, process: 'exited', ...(detail === undefined ? {} : { detail }) };
}

/** One browser tab. */
class Client {
  readonly decoder = new FrameDecoder();
  readonly subs = new Map<string, Subscription>();
  lastSeen: number;
  closed = false;

  constructor(
    readonly socket: net.Socket,
    private readonly now: () => number
  ) {
    this.lastSeen = now();
  }

  send(msg: ServerMessage): void {
    this.write(textFrame(JSON.stringify(msg)));
  }

  /** Raw terminal bytes for one Terminal. Returns false when the socket is too far behind, which
   * the caller accounts for as elided rather than queueing. */
  sendBytes(id: string, payload: Buffer): boolean {
    if (this.stalled()) return false;
    this.write(binaryFrame(encodeTagged(id, payload)));
    return true;
  }

  stalled(): boolean {
    return this.socket.writableLength > STALL_BYTES;
  }

  write(frame: Buffer): void {
    if (this.closed || this.socket.destroyed) return;
    // Return value ignored on purpose: `stalled()` is the backpressure signal, and a `drain`
    // handler per frame would be a listener leak on a socket that never drains.
    this.socket.write(frame);
  }

  close(code = 1000, reason = ''): void {
    if (this.closed) return;
    this.closed = true;
    try {
      this.socket.write(closeFrame(code, reason));
    } catch {
      /* already gone */
    }
    this.socket.end();
  }

  touch(): void {
    this.lastSeen = this.now();
  }
}

export class PanelServer {
  readonly http: http.Server;
  readonly tickets: TicketBook;

  private readonly now: () => number;
  private readonly log: (line: string, extra?: Record<string, unknown>) => void;
  private readonly snapshotMs: number;
  private readonly discoverMs: number;
  private readonly origins: string[];
  private readonly tokenVars: readonly string[];

  /**
   * Every node-and-session the wall knows about, keyed by `<mode>:<node>/<session>` (`ids.nodeKey`).
   *
   * NOT by node name, which is what slice 1 did while one Machine held exactly one session. A local
   * host runs `kontra-webcrawl` and `kontra-parse` at the same time, so keying by node would have
   * kept one and silently dropped the other's tiles — and the same is true of a Machine an operator
   * gave a second session by hand. The key is an id with its window removed, so it is derivable from
   * anything a client sends without a second parse.
   */
  private machines = new Map<string, MachineTarget>();
  private terminals = new Map<string, Terminal>();
  private rings = new Map<string, Ring>();
  private clients = new Set<Client>();
  /**
   * The exit status each Terminal's SCREEN is currently showing, keyed by Terminal id.
   *
   * OUTLIVES A DISCOVERY ROUND, deliberately. The probe cannot tell a finished Worker from a running
   * one when both report the hold shell (`tmux.ts:paneProcess`), so for sessions started before
   * `@kontra_exit` existed the only evidence is the banner the shell prints — and that arrives on the
   * SNAPSHOT cadence (3 s), not the discovery one (30 s). Without this map every refresh would blank
   * the verdict and the tile would flicker between "exited 143" and "unknown" ten times a minute.
   */
  private screenExits = new Map<string, string>();

  /**
   * Nodes that are IN the inventory and are NOT on the wall, keyed the same way `machines` is, with
   * one sentence each.
   *
   * A DELIBERATE THIRD STATE, beside "on the wall" and "never heard of". A node that has left has
   * left for a reason an operator needs — its address was reused by another Fleet, or its Warden
   * stopped reporting — and a `subscribe` for the id a Dashboard slot has pinned must answer with
   * that reason rather than the generic "no such Terminal in the Fleet inventory", which is what a
   * Machine that was never provisioned gets and sends somebody to the wrong place entirely.
   */
  private gone = new Map<string, string>();

  private timers: NodeJS.Timeout[] = [];
  private discovering = false;
  private snapshotting = false;
  private stopped = false;

  constructor(
    private readonly deps: PanelDeps,
    config: PanelConfig = {}
  ) {
    this.now = deps.now ?? (() => Date.now());
    this.log =
      deps.log ??
      ((line, extra) => {
        // eslint-disable-next-line no-console
        console.log(`[panels] ${line}${extra ? ` ${JSON.stringify(extra)}` : ''}`);
      });
    this.snapshotMs = config.snapshotMs ?? DEFAULT_SNAPSHOT_MS;
    this.discoverMs = config.discoverMs ?? DEFAULT_DISCOVER_MS;
    this.origins = config.origins ?? [DEFAULT_ORIGIN];
    this.tokenVars = config.tokenVars ?? PANEL_TOKEN_VARS;
    this.tickets = new TicketBook(this.now);

    this.http = http.createServer((req, res) => this.route(req, res));
    this.http.on('upgrade', (req, socket) => this.upgrade(req, socket as net.Socket));
    // An `'error'` with no listener is a process-level throw — the exact shape finding (6)
    // measured taking down an unrelated `pulumi up`.
    this.http.on('error', (err) => this.log('http server error', { err: String(err) }));
    this.http.on('clientError', (_err, socket) => {
      (socket as net.Socket).destroy();
    });
  }

  // --- lifecycle -----------------------------------------------------------------------------

  async listen(port = DEFAULT_PORT, host = '0.0.0.0'): Promise<number> {
    await new Promise<void>((resolve, reject) => {
      const onError = (err: Error): void => reject(err);
      this.http.once('error', onError);
      this.http.listen(port, host, () => {
        this.http.removeListener('error', onError);
        resolve();
      });
    });
    const addr = this.http.address();
    const bound = typeof addr === 'object' && addr ? addr.port : port;

    // Both loops: guarded, self-catching, and never awaited by anything.
    this.refreshSoon();
    this.timers.push(setInterval(() => this.refreshSoon(), this.discoverMs));
    this.timers.push(setInterval(() => this.snapshotSoon(), this.snapshotMs));
    this.timers.push(setInterval(() => this.keepalive(), PING_MS));
    return bound;
  }

  async close(): Promise<void> {
    this.stopped = true;
    for (const t of this.timers) clearInterval(t);
    this.timers = [];
    const clients = [...this.clients];
    for (const c of clients) c.close(1001, 'shutting down');
    this.clients.clear();
    // AWAITED, unlike the per-client path: on SIGTERM this is the last chance to kill the grouped
    // sessions this process created, and a restarted streamer cannot find them again — it does not
    // know the nonces. A leaked one keeps the owner's windows alive after a `kill-session`.
    try {
      await Promise.all(clients.map((c) => this.stopAttaches(c)));
    } catch (err) {
      this.log('cleaning up live attaches on shutdown failed', { err: String(err) });
    }
    // `close()` alone waits for every keep-alive connection to go idle and time out, which on
    // SIGTERM is the difference between the supervisor restarting this child and having to kill it.
    // Every WebSocket has already been sent its close frame above.
    this.http.closeIdleConnections();
    const closed = new Promise<void>((resolve) => this.http.close(() => resolve()));
    this.http.closeAllConnections();
    await this.drain(closed);
  }

  /**
   * Wait for the connections to go, but not forever.
   *
   * MEASURED, on node 22 (slice 2): once a socket has been UPGRADED, a client that destroys it
   * leaves the http server's connection count stuck — `getConnections()` still answers 1 after
   * `closeAllConnections()`, and it never drops, so `close()`'s callback never fires. Awaiting it
   * unbounded means a SIGTERM hangs forever on any streamer where a single browser tab ever went
   * away, which is every real one; the supervisor would then have to SIGKILL, and the sockets it was
   * trying to close politely get reset anyway.
   *
   * `close()` has already released the listening socket synchronously, so nothing after this point
   * is about the port. It is only about draining, and a drain has a deadline.
   */
  private async drain(closed: Promise<void>): Promise<void> {
    let timer: NodeJS.Timeout | undefined;
    const deadline = new Promise<void>((resolve) => {
      timer = setTimeout(resolve, CLOSE_DRAIN_MS);
    });
    try {
      await Promise.race([closed, deadline]);
    } finally {
      if (timer) clearTimeout(timer);
    }
  }

  // --- state ---------------------------------------------------------------------------------

  /** Terminals as the API reports them, sorted so a wall is stable across refreshes. */
  terminalList(): Terminal[] {
    return [...this.terminals.values()].sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
  }

  /** Terminals with at least one subscriber. */
  liveCount(): number {
    const live = new Set<string>();
    for (const c of this.clients) for (const id of c.subs.keys()) live.add(id);
    return live.size;
  }

  /**
   * Distinct NODES, which is what `machines` has always meant on the health route.
   *
   * Counted rather than taken from `this.machines.size` since slice 6: that map is keyed per session,
   * so a host running two local Workers would otherwise report two "machines". One host is one host,
   * whatever it is running.
   */
  nodeCount(): number {
    const nodes = new Set<string>();
    for (const m of this.machines.values()) nodes.add(`${modeOf(m)}:${m.machine}`);
    return nodes.size;
  }

  private tokenConfigured(): boolean {
    return checkBearer(undefined, this.tokenVars)?.code !== 503;
  }

  /**
   * Why an id a client just named is not on the wall.
   *
   * TWO ANSWERS, AND THEY SEND SOMEBODY TO DIFFERENT PLACES. "Never provisioned" is a Dashboard slot
   * pinned to a Machine that has not been created — wait, or run `fleet up`. "Left, and here is why"
   * is a Machine whose address was reused by another Fleet, or whose Warden stopped reporting, and
   * the action is to clean up the stack that still lists it. Collapsing the two into the generic
   * sentence is what let the observed bug read as a temporary blip for as long as it did.
   */
  private whyMissing(ref: Parameters<typeof nodeKey>[0], absent: string): string {
    return this.gone.get(nodeKey(ref)) ?? absent;
  }

  /** The two sentences a node that was NEVER on the wall gets. Kept apart because "there is no such
   * Terminal on this Machine" and "there is no such Machine" are different facts and the second is
   * the one a `converge` is refused for. */
  private static readonly NO_TERMINAL = 'no such Terminal in the Fleet inventory';
  private static readonly NO_MACHINE = 'no such Machine in the Fleet inventory';

  // --- the two loops -------------------------------------------------------------------------

  /** Rediscover the inventory and re-probe every Machine. */
  private refreshSoon(): void {
    if (this.stopped || this.discovering) return;
    this.discovering = true;
    void (async () => {
      try {
        await this.refresh();
      } catch (err) {
        this.log('discovery failed', { err: String(err) });
      } finally {
        this.discovering = false;
      }
    })();
  }

  async refresh(): Promise<void> {
    const discovered = await this.deps.discover();
    const machines = this.withReportingMachines(discovered);
    const nextMachines = new Map<string, MachineTarget>();
    const nextTerminals = new Map<string, Terminal>();

    // Probes run concurrently: twelve Machines at up to 15 s each would otherwise make one slow
    // Machine decide the whole wall's refresh interval.
    const probeAll = Promise.all(
      machines.map(async (m) => {
        // A MACHINE THAT REPORTED IS NOT DIALLED. Its Warden already said what is running there,
        // over a connection the Machine itself opened — which is both cheaper than an SSH round trip
        // and the only evidence an address cannot forge (see `warden.ts`, and `tmux.ts:HOST_MARKER`
        // for the live incident where an address did).
        const report = this.deps.reports?.report(m.machine);
        if (report && modeOf(m) === 'fleet') {
          return { m, probe: probeFromReport(report) };
        }
        try {
          return { m, probe: await this.deps.probe(m) };
        } catch (err) {
          return {
            m,
            probe: {
              reachable: 'unknown',
              session: 'unknown',
              windows: [],
              detail: `probe of ${m.machine} threw: ${String(err)}`,
            } as ProbeResult,
          };
        }
      })
    );

    // Health is measured ONCE for the whole fleet, alongside the probes rather than after them:
    // twelve Machines placed with one actor share one task queue and one metrics query, and a wedged
    // Temporal must cost the `poller` chip, never the refresh interval. `health.ts` bounds each half
    // itself and resolves a timeout to `unknown`.
    const [probes, fleetHealth] = await Promise.all([
      probeAll,
      measureFleetHealth(this.deps.fleetHealth, machines),
    ]);

    const nextGone = new Map<string, string>();
    for (const { m, probe: raw } of probes) {
      // A NODE THAT IS GONE HAS NO TERMINALS, and this is the only place on the wall where a failure
      // produces no tile rather than a tile that says what is wrong. Two ways to reach it, one
      // sentence each, and both are recorded so a `subscribe` or a `converge` for an id that has just
      // left is still answered with the reason rather than "no such Terminal".
      const { probe, gone } = this.attribute(m, raw);
      if (gone !== undefined) {
        nextGone.set(nodeKey({ mode: modeOf(m), machine: m.machine, session: m.session }), gone);
        continue;
      }
      nextMachines.set(nodeKey({ mode: modeOf(m), machine: m.machine, session: m.session }), {
        ...m,
        windows: m.windows,
      });
      for (const t of terminalsWithHealth(m, probe, fleetHealth)) {
        const prev = this.terminals.get(t.id);
        if (prev?.lastSnapshotAt) t.lastSnapshotAt = prev.lastSnapshotAt;
        applyScreenExit(t, this.screenExits.get(t.id));
        this.applyWardenTelemetry(t);
        nextTerminals.set(t.id, t);
      }
    }
    for (const [key, why] of nextGone) {
      if (!this.gone.has(key)) this.log('a node left the wall', { node: key, why });
    }
    this.gone = nextGone;

    this.machines = nextMachines;
    this.terminals = nextTerminals;
    // Rings for Terminals that no longer exist are dropped: a Machine destroyed mid-run must
    // not leave a megabyte per window behind it.
    for (const id of [...this.rings.keys()]) if (!this.terminals.has(id)) this.rings.delete(id);
    for (const id of [...this.screenExits.keys()]) if (!this.terminals.has(id)) this.screenExits.delete(id);
    this.announceHealth();
  }

  /**
   * The inventory, plus every Machine that is reporting and is not in it.
   *
   * ADR 0037's `machines=None` "takes what already exists rather than provisioning", so a
   * self-enrolled **Machine** is in no Pulumi checkpoint at all. A wall built only from
   * `discovery.ts` would be blind to exactly the Machines this program exists to support — and a
   * Machine that dialled out and named itself is better evidence than a checkpoint anyway.
   */
  private withReportingMachines(discovered: MachineTarget[]): MachineTarget[] {
    const reports = this.deps.reports;
    if (!reports) return discovered;
    const known = new Set(discovered.filter((m) => modeOf(m) === 'fleet').map((m) => m.machine));
    const extra: MachineTarget[] = [];
    for (const machine of reports.reporting()) {
      if (known.has(machine)) continue;
      const report = reports.report(machine);
      if (report) extra.push(targetFromReport(report));
    }
    return extra.length ? [...discovered, ...extra] : discovered;
  }

  /**
   * Decide whether this node is still THIS node, and fold in what silence from its Warden means.
   *
   * ═══ SILENCE ALONE IS NOT ENOUGH, AND THE FIRST VERSION OF THIS GOT IT WRONG ═══
   *
   * A report is dialled BY the Machine, so it exists only while the Machine does — which makes
   * silence from a Machine that used to speak real evidence, unlike anything the Pulumi inventory
   * can offer (a checkpoint says a Machine was PROVISIONED and never that it is still there). But it
   * is evidence for TWO different facts and only one of them means the Machine is gone: a Warden can
   * die on a Machine that is perfectly alive, and dropping that Machine off the wall would hide a
   * live Worker at exactly the moment somebody wants to look at it.
   *
   * So silence is CORROBORATED. The Machine falls back to the SSH probe it was skipping, and:
   *
   *   - the probe cannot reach it        → gone. Two independent sources agree it is not there.
   *   - the probe reaches a DIFFERENT box → gone, with the stronger sentence (see `tmux.ts:
   *                                        HOST_MARKER`). This one needs no Warden at all.
   *   - the probe reaches THIS Machine   → NOT gone. The Warden is down, which is its own sentence
   *                                        on the tile and not a reason to take the tile away.
   *
   * A Machine that never enrolled is untouched by all of this: a Fleet placed before the Warden, an
   * appliance install, a `docker` or `local` node.
   */
  private attribute(m: MachineTarget, probe: ProbeResult): { probe: ProbeResult; gone?: string } {
    // The strongest evidence first, and the only one that needs no Warden: the box answered and said
    // it is somebody else. Nothing a Warden could say changes that.
    if (probe.gone !== undefined) return { probe, gone: probe.gone };

    const reports = this.deps.reports;
    if (!reports || modeOf(m) !== 'fleet') return { probe };
    if (!reports.seen(m.machine) || reports.has(m.machine)) return { probe };

    if (probe.reachable !== 'ok') {
      return {
        probe,
        gone:
          `${m.machine} enrolled a Warden, has stopped reporting, and cannot be reached. A report ` +
          'is dialled BY the Machine, so silence from one that used to speak is evidence it is gone ' +
          'rather than merely unreachable, and the probe agrees — converging a session on it would ' +
          'run against whatever now holds its address. The Fleet inventory still lists it; ' +
          '`kontra fleet down` the stack that does.',
      };
    }
    // Reachable, and still itself. The Machine is fine and its Warden is not, which is a finding
    // about the Warden — nothing here is reconciled, no Worker is being judged, and the `loads` chip
    // has fallen back to VictoriaMetrics without saying so.
    return {
      probe: {
        ...probe,
        detail: joinDetails([
          probe.detail,
          `${m.machine} answers but its Warden has stopped reporting — nothing on this Machine is ` +
            'reconciling its Workers or judging their health right now (`systemctl status ' +
            'kontra-warden` on it)',
        ]),
      },
    };
  }

  /**
   * Fold a Machine's own numbers into the Terminal's health.
   *
   * THE `loads` CHIP IS THE ONE THE WARDEN OWNS, and it takes precedence over the VictoriaMetrics
   * reading when a report exists — not because the Warden is trusted more in general, but because
   * `metrics.ts` records at length that its own ratio CANNOT WORK today: `kontra_batches_total` is
   * incremented nowhere, so the expression divides by a permanent zero and that chip can report
   * `failing` or `unknown` and never `ok`. The Warden's verdict is computed from two counters both
   * actor hosts increment, over a window it owns. Where it says nothing (`cannot-tell`), the chip is
   * `unknown` and the existing reading is left exactly as it was.
   *
   * The Machine's own numbers travel as their own field rather than as prose in `detail`, because
   * `types.ts`'s contract for that field is one sentence per FAILING signal and CPU at 4% is not a
   * signal — it is context for the ones that are. A field is also what lets a tile render a number.
   * A reading that was not taken is OMITTED, never zeroed: see `Terminal.telemetry`.
   */
  private applyWardenTelemetry(t: Terminal): void {
    const report = this.deps.reports?.report(t.machine);
    if (!report) return;
    const loads = loadsFromReport(report);
    if (loads !== 'unknown') t.health = { ...t.health, loads };
    const telemetry = machineTelemetry(report);
    if (telemetry) t.telemetry = telemetry;
  }

  /** Take one snapshot round: one exec per Machine that somebody is watching. */
  private snapshotSoon(): void {
    if (this.stopped || this.snapshotting) return;
    this.snapshotting = true;
    void (async () => {
      try {
        await this.snapshotRound();
      } catch (err) {
        this.log('snapshot round failed', { err: String(err) });
      } finally {
        this.snapshotting = false;
      }
    })();
  }

  async snapshotRound(): Promise<void> {
    // Group subscriptions by NODE-AND-SESSION first. Two browsers watching the same Machine, or one
    // browser watching both its windows, cost one exec — acceptance criterion 7. Two SESSIONS on one
    // local host are two execs, correctly: `capture-pane` is per session, so they were never one.
    const wanted = new Map<string, Set<string>>();
    for (const c of this.clients) {
      for (const [id, sub] of c.subs) {
        // A live tile is fed by its own PTY. Painting a snapshot over it would replace a scrolling
        // screen with a three-second-old one, and a Terminal nobody is watching in snapshot mode
        // should not cost an exec at all.
        if (sub.mode === 'live') continue;
        const ref = tryParseTerminalId(id);
        if (!ref) continue;
        const key = nodeKey(ref);
        const bucket = wanted.get(key);
        if (bucket) bucket.add(ref.window);
        else wanted.set(key, new Set([ref.window]));
      }
    }
    if (!wanted.size) return;

    await Promise.all(
      [...wanted].map(async ([key, windows]) => {
        const m = this.machines.get(key);
        if (!m) return;
        try {
          const screens = await this.deps.snapshot(m, [...windows]);
          for (const [window, screen] of screens) {
            this.fanOut(m, window, screen);
          }
        } catch (err) {
          this.log('snapshot failed', { node: key, err: String(err) });
        }
      })
    );
  }

  /** Send one window's screen to every subscriber of that Terminal. */
  private fanOut(m: MachineTarget, window: string, screen: string): void {
    let id: string;
    try {
      // One place formats an id, and it validates. A window name tmux allows but a Terminal id
      // cannot carry must not be silently rewritten into one.
      id = terminalId({ mode: modeOf(m), machine: m.machine, session: m.session, window });
    } catch {
      return;
    }
    const t = this.terminals.get(id);
    // A snapshot is a full SCREEN: the clear-home prefix is what stops a 3-second repaint from
    // appending 50 lines to the tile forever.
    const payload = Buffer.from(CLEAR_HOME + screen, 'utf8');

    let ring = this.rings.get(id);
    if (!ring) {
      ring = new Ring();
      this.rings.set(id, ring);
    }
    // The newest screen replaces the ring rather than growing it — the ring's job is to seed a
    // tile that subscribes late, and for a snapshot stream the newest screen is the whole truth.
    ring.reset(payload);
    if (t) t.lastSnapshotAt = this.now();

    // WHAT THE PANE SAYS ABOUT ITSELF. `cli/internal/tmux/tmux.go`'s hold prints `[exited <n>] press any key…` and
    // then blocks on `read`, so this line on the screen is the pane reporting that its command is
    // over — the only evidence available for a Worker started before `@kontra_exit` existed, which
    // on this host is every Worker currently running. Read here because this is where a screen
    // arrives; announced only when it CHANGES, so a wall of finished Workers costs one message each.
    const exit = paneExitFromScreen(screen);
    const before = this.screenExits.get(id) ?? '';
    if (exit) this.screenExits.set(id, exit);
    else this.screenExits.delete(id);
    if (t && exit !== before) {
      applyScreenExit(t, exit);
      this.announceHealth();
    }

    for (const c of this.clients) {
      const sub = c.subs.get(id);
      if (!sub || sub.mode === 'live') continue;
      this.deliver(c, id, sub, payload);
    }
  }

  /** Push bytes through one subscription's budget, reporting anything dropped. */
  private deliver(c: Client, id: string, sub: Subscription, payload: Buffer): void {
    const admission = sub.budget.offer(payload);
    if (admission.send) {
      if (!c.sendBytes(id, admission.send)) {
        // The socket is backed up. Count it and move on: buffering for a tab that is not reading
        // is how this process would reproduce, on our side, the growth ADR 0020 measured on a
        // Machine.
        const bytes = admission.send.length;
        c.send({ t: 'elided', id, bytes });
        return;
      }
    }
    const elided = sub.budget.takeElided();
    if (elided > 0) c.send({ t: 'elided', id, bytes: elided });
  }

  /** Tell each client about health that changed since it was last told. */
  private announceHealth(): void {
    for (const c of this.clients) {
      for (const [id, sub] of c.subs) {
        const health = this.terminals.get(id)?.health ?? UNKNOWN_HEALTH;
        if (sub.announced === JSON.stringify(health)) continue;
        this.announce(c, id, sub);
      }
    }
  }

  /**
   * One `state` message, and the only place one is built.
   *
   * Mode and health travel together on this wire, so they have to be decided together: a health
   * change on a focused tile must not tell it that it is back on snapshots, and a Terminal that has
   * left the inventory is `error` whatever the subscription thought it was.
   */
  private announce(client: Client, id: string, sub: Subscription): void {
    const t = this.terminals.get(id);
    const health = t?.health ?? UNKNOWN_HEALTH;
    sub.announced = JSON.stringify(health);
    client.send({ t: 'state', id, mode: t ? sub.mode : 'error', health });
  }

  private keepalive(): void {
    const t = this.now();
    for (const c of [...this.clients]) {
      if (t - c.lastSeen > IDLE_MS) {
        this.log('dropping an idle client', { idleMs: t - c.lastSeen });
        this.drop(c, 1001, 'idle');
        continue;
      }
      c.write(Buffer.from([0x89, 0x00])); // an empty ping frame
    }
  }

  // --- HTTP ----------------------------------------------------------------------------------

  private cors(req: http.IncomingMessage, res: http.ServerResponse): void {
    const origin = req.headers.origin;
    if (typeof origin === 'string' && this.origins.includes(origin)) {
      res.setHeader('Access-Control-Allow-Origin', origin);
    }
    // Vary regardless: a cache that served one origin's response to another would defeat the
    // allow-list even when the allow-list is right.
    res.setHeader('Vary', 'Origin');
    res.setHeader('Access-Control-Allow-Headers', 'authorization, content-type');
    res.setHeader('Access-Control-Allow-Methods', 'GET, POST, OPTIONS');
    res.setHeader('Access-Control-Max-Age', '600');
  }

  private json(res: http.ServerResponse, code: number, body: unknown): void {
    const payload = JSON.stringify(body);
    res.writeHead(code, {
      'content-type': 'application/json',
      'content-length': Buffer.byteLength(payload),
      // A ticket must never be cached, and neither must the inventory.
      'cache-control': 'no-store',
    });
    res.end(payload);
  }

  private route(req: http.IncomingMessage, res: http.ServerResponse): void {
    res.on('error', (err) => this.log('response error', { err: String(err) }));
    const url = new URL(req.url ?? '/', 'http://panels.invalid');
    this.cors(req, res);

    if (req.method === 'OPTIONS') {
      res.writeHead(204);
      res.end();
      return;
    }

    switch (`${req.method} ${url.pathname}`) {
      case 'GET /api/panels/health': {
        // Ungated — but a streamer with no token configured serves NOTHING, including this. A 503
        // here is how `kontra doctor` reports "the Dashboard is switched off" rather than "down".
        if (!this.tokenConfigured()) {
          this.json(res, 503, {
            ok: false,
            error: `disabled: set ${PANEL_TOKEN_VARS[0]} to enable the Dashboard`,
          });
          return;
        }
        this.json(res, 200, {
          ok: true,
          terminals: this.terminals.size,
          live: this.liveCount(),
          machines: this.nodeCount(),
        });
        return;
      }
      case 'GET /api/panels/terminals': {
        const fail = checkBearer(req.headers.authorization, this.tokenVars);
        if (fail) return this.json(res, fail.code, fail.body);
        this.json(res, 200, { terminals: this.terminalList() });
        return;
      }
      case 'POST /api/panels/report': {
        // THE INGEST FOR EVERY MACHINE'S PANES AND TELEMETRY (ADR 0037). The arrow is outbound from
        // the Machine, which is the whole reason this is a POST here rather than a pull from there.
        //
        // GATED BY THE SAME TOKEN AS EVERYTHING ELSE, and that is WEAKER than what ADR 0037
        // describes — see `warden.ts`'s header. Until the ingest is also mounted on the mTLS
        // enrolment server, one credential admits reports for the whole Fleet rather than one
        // identity per Machine.
        const fail = checkBearer(req.headers.authorization, this.tokenVars);
        if (fail) return this.json(res, fail.code, fail.body);
        if (!this.deps.reports) {
          // 501 and not 204: a Warden that is reporting into a streamer with nowhere to put it must
          // learn that from the status, or a Fleet reports happily into nothing for a week.
          req.resume();
          this.json(res, 501, { error: 'this streamer does not hold Warden reports' });
          return;
        }
        this.readReport(req, res);
        return;
      }
      case 'POST /api/panels/ticket': {
        const fail = checkBearer(req.headers.authorization, this.tokenVars);
        if (fail) return this.json(res, fail.code, fail.body);
        // The body is ignored entirely: a ticket carries no scope, so there is nothing a caller
        // could ask for that would change what it admits.
        req.resume();
        this.json(res, 200, this.tickets.mint());
        return;
      }
      default:
        this.json(res, 404, { error: 'not found' });
    }
  }

  /**
   * Read one report off the wire.
   *
   * BOUNDED, because this is the one route on this server whose body a stranger writes. A report is
   * a handful of KiB — `cli/warden/panereport.go` caps each frame at a screen — and 1 MiB is two orders of
   * magnitude of headroom over that. The stream is destroyed rather than drained past the cap: a
   * client that keeps sending after being told to stop is not one to keep reading.
   *
   * THE RESPONSE IS 202, NOT 200. The report has been taken and nothing has been decided about it;
   * a Warden's next act does not depend on this answer, and a status implying otherwise would
   * invite one that does.
   */
  private readReport(req: http.IncomingMessage, res: http.ServerResponse): void {
    const chunks: Buffer[] = [];
    let bytes = 0;
    let done = false;
    const finish = (code: number, body: unknown): void => {
      if (done) return;
      done = true;
      this.json(res, code, body);
    };
    req.on('error', (err) => {
      this.log('a Warden report failed mid-body', { err: String(err) });
      finish(400, { error: 'the report did not arrive whole' });
    });
    req.on('data', (chunk: Buffer) => {
      bytes += chunk.length;
      if (bytes > REPORT_MAX_BYTES) {
        finish(413, { error: `a Warden report is at most ${REPORT_MAX_BYTES} bytes` });
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => {
      if (done) return;
      let report: WardenReport;
      try {
        report = JSON.parse(Buffer.concat(chunks).toString('utf8')) as WardenReport;
      } catch {
        finish(400, { error: 'not JSON' });
        return;
      }
      if (!report || typeof report !== 'object' || typeof report.machine !== 'string') {
        finish(400, { error: 'a report names the Machine it is about' });
        return;
      }
      // A REFUSAL IS SAID OUT LOUD. `accept` returns false for a Machine whose name no Terminal id
      // could carry, and a 202 there would leave a Warden reporting cheerfully into a wall its
      // Machine can never appear on — indistinguishable, from the Machine, from a Controller with
      // nobody watching.
      if (!this.deps.reports?.accept(report)) {
        finish(400, {
          error:
            `a Machine reports under a name a Terminal id can carry (kf-<tag>-NN); ` +
            `${JSON.stringify(report.machine)} is not one`,
        });
        return;
      }
      finish(202, { ok: true });
    });
  }

  // --- WebSocket ------------------------------------------------------------------------------

  private upgrade(req: http.IncomingMessage, socket: net.Socket): void {
    socket.on('error', (err) => this.log('socket error', { err: String(err) }));
    const url = new URL(req.url ?? '/', 'http://panels.invalid');
    const bail = (code: number, why: string): void => {
      socket.write(`HTTP/1.1 ${code} ${why}\r\nConnection: close\r\n\r\n`);
      socket.destroy();
    };

    if (url.pathname !== '/api/panels/ws') return bail(404, 'Not Found');
    if (!this.tokenConfigured()) return bail(503, 'Service Unavailable');
    const key = req.headers['sec-websocket-key'];
    if (typeof key !== 'string' || (req.headers['sec-websocket-version'] ?? '13') !== '13') {
      return bail(400, 'Bad Request');
    }
    // Browsers do not apply the same-origin policy to WebSockets, so the Origin check has to be
    // ours. A missing Origin is allowed — that is a non-browser client, and the ticket is what
    // authenticates it.
    const origin = req.headers.origin;
    if (typeof origin === 'string' && !this.origins.includes(origin)) {
      return bail(403, 'Forbidden');
    }
    if (!this.tickets.redeem(url.searchParams.get('ticket'))) {
      return bail(401, 'Unauthorized');
    }

    socket.setNoDelay(true);
    socket.write(handshakeResponse(key));

    const client = new Client(socket, this.now);
    this.clients.add(client);
    socket.on('data', (chunk: Buffer) => this.onData(client, chunk));
    socket.on('close', () => this.drop(client));
    socket.on('end', () => this.drop(client));
    client.send({ t: 'hello', terminals: this.terminals.size });
  }

  private drop(client: Client, code?: number, reason?: string): void {
    if (code !== undefined) client.close(code, reason);
    client.closed = true;
    this.clients.delete(client);
    // The tab is gone; its grouped sessions on the Machines are not, until this runs.
    void this.stopAttaches(client).catch((err) =>
      this.log('cleaning up a dropped client failed', { err: String(err) })
    );
  }

  private onData(client: Client, chunk: Buffer): void {
    client.touch();
    let frames;
    try {
      frames = client.decoder.push(chunk);
    } catch (err) {
      const code = err instanceof WsProtocolError ? err.code : 1002;
      this.log('protocol error', { err: String(err) });
      this.drop(client, code, 'protocol error');
      return;
    }
    for (const frame of frames) {
      switch (frame.opcode) {
        case OPCODE.close:
          this.drop(client, 1000, '');
          return;
        case OPCODE.ping:
          client.write(Buffer.from([0x8a, 0x00]));
          break;
        case OPCODE.pong:
          break;
        case OPCODE.text:
          this.onMessage(client, frame.payload.toString('utf8'));
          break;
        case OPCODE.binary:
          // There is no client→server byte path in this design. A browser sending one is either
          // broken or probing; either way it is not carried anywhere.
          client.send({ t: 'error', message: 'binary frames from a client are not accepted' });
          break;
        default:
          this.drop(client, 1002, 'unexpected opcode');
          return;
      }
    }
  }

  private onMessage(client: Client, raw: string): void {
    let msg: ClientMessage;
    try {
      msg = JSON.parse(raw) as ClientMessage;
    } catch {
      client.send({ t: 'error', message: 'not JSON' });
      return;
    }
    if (!msg || typeof msg !== 'object' || typeof msg.t !== 'string') {
      client.send({ t: 'error', message: 'no message type' });
      return;
    }

    switch (msg.t) {
      case 'ping':
        client.send({ t: 'pong' });
        return;
      case 'subscribe':
        this.onSubscribe(client, msg.id);
        return;
      case 'unsubscribe':
        client.subs.delete(String(msg.id));
        return;
      case 'focus':
        this.onFocus(client, msg.id, msg.cols, msg.rows);
        return;
      case 'blur':
        this.onBlur(client, msg.id);
        return;
      case 'converge':
        this.onConverge(client, msg.id);
        return;
      default: {
        // `msg` is `never` to the compiler here — the runtime is where a browser sends a type the
        // contract does not have, and it gets told so rather than being ignored.
        const t = (msg as { t?: unknown }).t;
        client.send({ t: 'error', message: `unknown message type ${JSON.stringify(t)}` });
      }
    }
  }

  private onSubscribe(client: Client, rawId: unknown): void {
    const id = typeof rawId === 'string' ? rawId : '';
    const ref = tryParseTerminalId(id);
    if (!ref) {
      client.send({ t: 'error', id, message: 'not a Terminal id' });
      return;
    }
    const t = this.terminals.get(id);
    if (!t) {
      // Not an error to be swallowed: a Dashboard slot can name a Machine that is not up yet, and
      // the tile has to say which. A node that LEFT is a different answer from one that was never
      // there — see `gone` — and sends an operator somewhere else entirely.
      client.send({ t: 'error', id, message: this.whyMissing(ref, PanelServer.NO_TERMINAL) });
      return;
    }
    const sub: Subscription = { budget: new FrameBudget(this.now), mode: 'snapshot' };
    client.subs.set(id, sub);
    this.announce(client, id, sub);

    // Seed from the ring so a tile paints immediately instead of at the next cadence tick.
    const ring = this.rings.get(id);
    if (ring && ring.bytes) this.deliver(client, id, sub, ring.concat());
  }

  // --- focus: snapshot -> live ------------------------------------------------------------------

  /**
   * Promote one client's Terminal to a live PTY attach.
   *
   * The tile keeps painting from snapshots until the PTY is actually up, so a focus on a slow
   * Machine is a slower tile rather than a blank one, and `mode:'live'` is only ever claimed once
   * bytes can arrive. A focus on a Machine that cannot be read degrades to `mode:'error'` with a
   * sentence — never a spinner, because a spinner is the one outcome an operator cannot act on.
   *
   * WHAT the client chooses is WHICH Terminal and how big its tile is. It does not choose a command,
   * a host or a session: those come from the inventory, here, exactly as the converge does.
   */
  private onFocus(client: Client, rawId: unknown, rawCols?: unknown, rawRows?: unknown): void {
    const id = typeof rawId === 'string' ? rawId : '';
    const ref = tryParseTerminalId(id);
    if (!ref) {
      client.send({ t: 'error', id, message: 'not a Terminal id' });
      return;
    }
    const t = this.terminals.get(id);
    if (!t) {
      client.send({ t: 'error', id, message: this.whyMissing(ref, PanelServer.NO_TERMINAL) });
      return;
    }
    const m = this.machines.get(nodeKey(ref));
    if (!m) {
      client.send({ t: 'error', id, message: this.whyMissing(ref, PanelServer.NO_MACHINE) });
      return;
    }
    const attacher = this.deps.attach;
    if (!attacher) {
      client.send({
        t: 'error',
        id,
        message: 'this streamer cannot open a live attach; this Terminal stays on snapshots',
      });
      return;
    }

    // A focus implies a subscription: a tile that focuses without having subscribed still has to end
    // up on the wall, and a demote has to have somewhere to land.
    let sub = client.subs.get(id);
    if (!sub) {
      sub = { budget: new FrameBudget(this.now), mode: 'snapshot' };
      client.subs.set(id, sub);
    }
    // The browser reports what addon-fit measured. Bounded rather than refused — a tile that
    // measured nonsense should render at the session's own geometry, not fail to go live.
    const cols = clampDimension('cols', rawCols, GEOMETRY.cols);
    const rows = clampDimension('rows', rawRows, GEOMETRY.rows);

    // An attach this subscription already has, live or still coming up. The `starting` case matters as
    // much as the live one: a promotion takes as long as an SSH round trip, and a tile that asks twice
    // in that window must not leave the first attach orphaned — an orphan is a grouped session on a
    // Machine that nothing will ever kill, because nothing holds its nonce any more.
    const existing = sub.attach;
    if (existing) {
      if (sub.cols === cols && sub.rows === rows) {
        // Already live at this size: re-seed from what the tile was shown rather than tearing down a
        // working PTY and creating a second grouped session on the Machine. A remounted tile (a
        // re-render, a tab coming back) takes this path. Still starting: say so and let it arrive.
        if (sub.mode === 'live') this.deliver(client, id, sub, existing.replay());
        this.announce(client, id, sub);
        return;
      }
      // A PTY's size is set by `stty` before the attach and cannot be changed without a channel to
      // write on — which by design does not exist. So a resize is a new attach, and the old grouped
      // session goes away with the old one.
      sub.attach = undefined;
      this.stopAttach(existing, 'resized');
    }

    // `mine` closes over the attachment this sink belongs to, which is why it is a getter: a resize
    // replaces one attachment while the old one is still shutting down, and the old one's `ended`
    // must not be allowed to clear its replacement.
    let mine: LiveAttach | undefined;
    const attachment = attacher.open(
      {
        // The mode travels with the target, so the attacher's transport is chosen from the
        // INVENTORY and never from anything the client said. What a client picks is which Terminal
        // and how big its tile is.
        target: probeTarget(m),
        session: m.session,
        window: ref.window,
        cols,
        rows,
      },
      this.attachSink(client, id, sub, () => mine)
    );
    mine = attachment;
    sub.attach = attachment;
    sub.cols = cols;
    sub.rows = rows;
    // The mode is logged because the STAKES differ by mode (ADR 0020, finding 2): a viewer that
    // wedges a fleet tmux server costs the view, and one that wedges a local or container tmux
    // server costs the Worker running in its panes. When that happens this line is the record of
    // who attached to what.
    this.log('focus: attaching', { id, mode: modeOf(m), viewer: attachment.viewer, cols, rows });
    // `start()` reports its own failures through the sink and does not reject; the catch is the belt
    // to that braces, because an unhandled rejection in this process is what finding (6) measured
    // failing an unrelated `pulumi up`.
    void attachment.start().catch((err) => {
      this.log('attach start threw', { id, err: String(err) });
      client.send({ t: 'error', id, message: `the live attach failed: ${String(err)}` });
    });
  }

  /** Demote to snapshots, and kill the grouped session the attach created. */
  private onBlur(client: Client, rawId: unknown): void {
    const id = typeof rawId === 'string' ? rawId : '';
    const sub = client.subs.get(id);
    // Idempotent on purpose: a tile that blurs twice, or blurs something it never focused, is a
    // normal thing for a browser to do and not worth an error.
    if (!sub) return;
    const attachment = sub.attach;
    sub.attach = undefined;
    sub.mode = 'snapshot';
    this.announce(client, id, sub);
    if (attachment) this.stopAttach(attachment, 'blurred');
  }

  /** Everything an attachment says, routed to the one client that focused it. */
  private attachSink(
    client: Client,
    id: string,
    sub: Subscription,
    mine: () => LiveAttach | undefined
  ): AttachSink {
    /** Ignore anything from an attachment this subscription has already replaced or dropped — a
     * resize starts a new attach while the old one is still winding down, and its last words are
     * about a tile that has moved on. */
    const current = (): boolean =>
      client.subs.get(id) === sub && !client.closed && sub.attach === mine();
    return {
      bytes: (payload) => {
        if (current()) this.deliver(client, id, sub, payload);
      },
      elided: (bytes) => {
        if (current()) client.send({ t: 'elided', id, bytes });
      },
      live: () => {
        if (!current()) return;
        sub.mode = 'live';
        this.announce(client, id, sub);
      },
      failed: (message) => {
        if (!current()) return;
        sub.mode = 'error';
        sub.attach = undefined;
        // Both: `state` carries the mode a tile renders from, and the message carries the sentence
        // an operator acts on. The wire has no notice type — see CONTRACT.md's slice 1 amendment 6.
        this.announce(client, id, sub);
        client.send({ t: 'error', id, message });
      },
      ended: (message) => {
        if (!current()) return;
        sub.mode = 'snapshot';
        sub.attach = undefined;
        this.announce(client, id, sub);
        client.send({ t: 'error', id, message });
      },
    };
  }

  /** Stop one attachment, without ever letting its cleanup become a floating promise. */
  private stopAttach(attachment: LiveAttach, why: string): void {
    void attachment.stop().catch((err) => {
      this.log('stopping a live attach failed', { why, viewer: attachment.viewer, err: String(err) });
    });
  }

  /**
   * Every attachment one client holds, stopped.
   *
   * This is the leak boundary. A grouped session outlives the client that created it (measured on
   * tmux 3.3a), and while it exists the owner's windows survive a `kill-session` on the owner — so a
   * closed tab that left one behind is a Machine that cannot be quiesced.
   */
  private async stopAttaches(client: Client): Promise<void> {
    const pending: Array<Promise<void>> = [];
    for (const sub of client.subs.values()) {
      const attachment = sub.attach;
      if (!attachment) continue;
      sub.attach = undefined;
      sub.mode = 'snapshot';
      pending.push(
        attachment.stop().catch((err) => {
          this.log('stopping a live attach failed', {
            viewer: attachment.viewer,
            err: String(err),
          });
        })
      );
    }
    await Promise.all(pending);
  }

  private onConverge(client: Client, rawId: unknown): void {
    const id = typeof rawId === 'string' ? rawId : '';
    const ref = tryParseTerminalId(id);
    if (!ref) {
      client.send({ t: 'error', id, message: 'not a Terminal id' });
      return;
    }
    // THE CONVERGE IS FLEET AUTHORITY, AND ONLY FLEET AUTHORITY.
    //
    // Not an omission and not a gap to fill later. On a Machine, converging creates a session whose
    // panes hold `journalctl -fu` — the Worker itself is under systemd, so the converge starts
    // nothing. In `local` mode the panes hold the REAL actor and handler processes (`cli/internal/tmux/tmux.go`), so
    // the same gesture would mean STARTING A WORKER from a browser: money, a Temporal lease, and a
    // credential-free HTTP surface deciding to run code. In `docker` mode nothing creates a session
    // inside a worker container at all. So both are refused with the command that owns session
    // existence for them, which is a sentence an operator can act on.
    if (ref.mode !== 'fleet') {
      client.send({
        t: 'error',
        id,
        message:
          ref.mode === 'local'
            ? 'a local session is owned by `kontra serve --actor <dir> --mode local --tmux`, not by the ' +
              'Dashboard: its panes hold the real actor and handler, so converging it here would be ' +
              'starting a Worker from a browser'
            : 'a worker container carries no tmux session, and the Dashboard does not create one — ' +
              '`kontra scale` (via the MCP tool) owns what runs in a container',
      });
      return;
    }
    const m = this.machines.get(nodeKey(ref));
    if (!m) {
      // THE REMEDY IS WITHDRAWN, NOT OFFERED AND THEN FAILED. A converge for a Machine that left
      // the wall would SSH to whatever now holds its address — see `tmux.ts:HOST_MARKER` — so this
      // answers with the reason it left instead of starting a workflow against a stranger.
      client.send({ t: 'error', id, message: this.whyMissing(ref, PanelServer.NO_MACHINE) });
      return;
    }
    const converge = this.deps.converge;
    if (!converge) {
      client.send({ t: 'error', id, message: 'this streamer has no Temporal client to converge with' });
      return;
    }
    // WHAT is converged is decided here, from the inventory — the client picks WHICH Machine and
    // never what runs on it. Same rule `machine.ts` states for its install script.
    void (async () => {
      try {
        const { workflowId } = await converge(m);
        this.log('converge started', { machine: m.machine, workflowId });
        // The wire has no "notice" message, and a success reported as `{t:'error'}` would be a lie
        // a tile then has to guess about. So the answer to a converge is the `state` that follows
        // it: refresh now rather than waiting out a full discovery interval, and let the workflow's
        // effect appear as health.
        await this.refresh();
      } catch (err) {
        client.send({ t: 'error', id, message: `converge failed: ${String(err)}` });
      }
    })();
  }
}
