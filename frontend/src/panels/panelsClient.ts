/**
 * The Dashboard's transport, in the browser (ADR 0020).
 *
 * Everything here is pure or a single `fetch`/`WebSocket` call, deliberately: the streamer's URL,
 * the ticket exchange, the frame decoding and the health wording all need to be testable without a
 * DOM, a fleet, or a running orchestrator.
 *
 * TWO ORIGINS, ON PURPOSE. The SPA is served by `orchestrator-api` on 8088; tickets and the socket
 * come from the streamer's own port (8090), under a narrow CORS allow-list. That keeps panel bytes
 * off the event loop serving the CLI's DuckDB queries, at the cost of the streamer's URL having to
 * be injected rather than inferred — hence {@link resolvePanelBase} and `VITE_KONTRA_PANEL_BASE`, never a
 * hardcoded host.
 *
 * THE TOKEN THIS PAGE MAY HOLD is `KONTRA_PANEL_TOKEN` (as `VITE_KONTRA_PANEL_TOKEN`), which mints tickets
 * and nothing else. `KONTRA_STATE_TOKEN` also authorises `POST /api/infra/stacks/:fqn/:op`, and a
 * credential a browser holds must not be able to spend money. The ticket, not the token, is what
 * appears in the WebSocket URL: it is single-use and lives ~30 seconds, because a URL ends up in
 * access logs, `Referer` headers and proxy buffers.
 */

/** The streamer's default port, published from the infra container. */
export const PANEL_PORT = 8090;

export interface TerminalHealth {
  reachable: 'ok' | 'fail' | 'unknown';
  session: 'present' | 'absent' | 'no-tmux' | 'unknown';
  /**
   * Is anything still RUNNING in the pane — a different question from whether the session exists.
   *
   * A Worker can finish while its session stays perfectly present: `cli/tmux.go` holds the window
   * open on purpose so the exit status stays readable, so from every other angle a finished Worker
   * looks exactly like a running one. OPTIONAL because a streamer older than this field sends health
   * without it, and the honest rendering of that is `unknown`, never `ok`.
   */
  process?: 'running' | 'exited' | 'unknown';
  poller: 'live' | 'none' | 'unknown';
  loads: 'ok' | 'failing' | 'unknown';
  detail?: string;
}

export interface Terminal {
  id: string;
  machine: string;
  host: string;
  publicIp: string;
  tag: string;
  fleet: string;
  actor: string;
  version: string;
  window: string;
  /** `pane_current_command` — what tmux says is in the pane's foreground. Optional: an older
   *  streamer does not send it, and a status line must draw either way. */
  command?: string;
  /** The PANE's own geometry, from `list-panes`. NOT the browser tile's measurement — since
   *  `window-size manual`, the two no longer track each other and showing one as the other is a lie
   *  about the operator's session. */
  paneCols?: number;
  paneRows?: number;
  /** The exit status the pane reported for the command that WAS running in it, or ''. */
  exitStatus?: string;
  health: TerminalHealth;
  lastSnapshotAt?: number;
  /**
   * THE MACHINE'S OWN NUMBERS, when a **Warden** is reporting them (ADR 0037).
   *
   * They arrive on THIS payload rather than through a run's event log, and that is the decision
   * rather than a convenience: a terminal frame and a CPU reading through Temporal history is the
   * shape this repo measured at 86% of a workflow's events, for a value stale a second later. So it
   * rides the thing that is re-fetched and overwritten.
   *
   * Optional at every level — no Warden, an older Warden, a platform with no /proc, or one reading
   * that failed. An ABSENT field is unmeasured and must never be drawn as 0: a Machine nobody
   * measured is not a Machine that is idle.
   */
  telemetry?: MachineTelemetry;
}

/** See {@link Terminal.telemetry}. Absent means unmeasured; 0 is a real reading. */
export interface MachineTelemetry {
  /** Not-idle fraction of the last interval, across all cores. 0..1. */
  cpu?: number;
  /** used/total, from MemAvailable rather than MemFree. 0..1. */
  memory?: number;
  /** The kernel's one-minute run-queue average. */
  load1?: number;
}

export type ServerMessage =
  | { t: 'hello'; terminals: number }
  | { t: 'state'; id: string; mode: 'snapshot' | 'live' | 'error'; health: TerminalHealth }
  | { t: 'error'; id?: string; message: string }
  | { t: 'elided'; id: string; bytes: number }
  | { t: 'pong' };

export type ClientMessage =
  | { t: 'subscribe'; id: string; cols: number; rows: number }
  | { t: 'unsubscribe'; id: string }
  | { t: 'converge'; id: string }
  | { t: 'ping' };

/**
 * Where the streamer is.
 *
 * `VITE_KONTRA_PANEL_BASE` wins; otherwise the page's own host on the panel port, which is right for the
 * local-first compose topology and wrong to hardcode. `localhost` is never assumed: a Controller
 * reached over its VPC address must not have its Dashboard point at the operator's laptop.
 */
export function resolvePanelBase(
  env: { VITE_KONTRA_PANEL_BASE?: string | undefined },
  origin?: { protocol: string; hostname: string }
): string {
  const configured = env.VITE_KONTRA_PANEL_BASE?.trim();
  if (configured) return configured.replace(/\/+$/, '');
  const protocol = origin?.protocol === 'https:' ? 'https:' : 'http:';
  const hostname = origin?.hostname || 'localhost';
  return `${protocol}//${hostname}:${PANEL_PORT}`;
}

/** `http(s)://host:8090` -> `ws(s)://host:8090/api/panels/ws?ticket=…`. */
export function panelSocketUrl(base: string, ticket: string): string {
  const scheme = base.startsWith('https:') ? 'wss:' : 'ws:';
  const hostAndPath = base.replace(/^https?:/, '');
  return `${scheme}${hostAndPath}/api/panels/ws?ticket=${encodeURIComponent(ticket)}`;
}

export function panelBase(): string {
  const env = import.meta.env as unknown as { VITE_KONTRA_PANEL_BASE?: string };
  return resolvePanelBase(
    env,
    typeof window === 'undefined' ? undefined : window.location
  );
}

/**
 * The two reads below go SAME ORIGIN, to orchestrator-api, and carry no credential.
 *
 * This page used to hold `VITE_KONTRA_PANEL_TOKEN`, which Vite can only inject at BUILD time — so
 * the token was baked into the bundle and into the image, readable by anyone who could fetch a JS
 * file, and unrotatable without a rebuild. orchestrator-api now holds it and forwards both reads to
 * the streamer. Only the WebSocket still goes to the streamer's own origin, carrying the minted
 * ticket, which is single-use and short-lived by construction.
 *
 * What this does NOT fix: the API has no authentication, so anyone who can reach it can still mint.
 * That is a property of the whole surface (`auth.ts` records it), not of the Dashboard.
 */
const API = '/api/panels';

export interface Ticket {
  ticket: string;
  expiresAt: number;
}

async function detail(res: Response): Promise<string> {
  try {
    return (await res.text()).slice(0, 400);
  } catch {
    return '';
  }
}

/**
 * Mint a ticket. Cross-origin, so it needs the streamer to have this origin in
 * `KONTRA_PANEL_ORIGIN` — a 503 here means the streamer has no token configured and is serving
 * nothing, which is a different problem from an unreachable one and gets different words.
 */
export async function fetchTicket(): Promise<Ticket> {
  const res = await fetch(`${API}/ticket`, { method: 'POST' });
  if (!res.ok) {
    // 503 means orchestrator-api has no KONTRA_PANEL_TOKEN and is fail-closed; 401 means the token
    // it holds is not the streamer's; 502 means the forked child is down while its container is up.
    // The bodies say which, and they are passed through unchanged rather than collapsed here.
    throw new Error(`could not mint a Dashboard ticket: ${res.status} ${await detail(res)}`);
  }
  return (await res.json()) as Ticket;
}

export async function fetchTerminals(): Promise<Terminal[]> {
  const res = await fetch(`${API}/terminals`);
  if (!res.ok) {
    throw new Error(`could not list Terminals: ${res.status} ${await detail(res)}`);
  }
  return ((await res.json()) as { terminals: Terminal[] }).terminals;
}

/** `[1 byte idLen][id][payload]` — the server's `encodeTagged`, inverted. */
export function decodeTagged(buf: ArrayBuffer): { id: string; payload: Uint8Array } {
  const bytes = new Uint8Array(buf);
  const idLen = bytes[0] ?? 0;
  if (!idLen || bytes.length < 1 + idLen) throw new Error('malformed Terminal frame');
  const id = new TextDecoder().decode(bytes.subarray(1, 1 + idLen));
  return { id, payload: bytes.subarray(1 + idLen) };
}

/** The five signals, plus the one human sentence that explains a failing one. */
export type HealthSignal = 'reachable' | 'session' | 'process' | 'poller' | 'loads' | 'detail';

export interface HealthEntry {
  signal: HealthSignal;
  text: string;
  /**
   * The CHIP's words: `<signal>: <state>` and nothing after it.
   *
   * A second field rather than a shorter `text`, because the two have different jobs and the long
   * one is still owed to somebody. `text` is the sentence — it carries the clause that says what to
   * DO — and it goes to the drawer, to `healthLines`, and to the chip's own tooltip. `label` is what
   * fits on a tile beside four others. Merging them is how a chip row ends up 91 characters wide and
   * wraps a lone chip onto a second line, which is the layout bug this split fixes.
   */
  label: string;
  /**
   * FALSE WHEN THIS AXIS CANNOT SAY ANYTHING ABOUT A PANE IN THIS MODE. See {@link signalApplies} —
   * NOT APPLICABLE is a third thing beside `ok` and `unknown`, and rendering it as either is a lie
   * in one direction or noise in the other.
   */
  applies: boolean;
  /** `true` good, `false` bad, `null` NOT MEASURED. Three states, never two — a boolean here is how
   * "unknown" silently becomes "ok" in a renderer. */
  ok: boolean | null;
}

/**
 * WHICH OF THE FIVE AXES CARRY INFORMATION FOR A PANE IN THIS MODE — and it is not five everywhere.
 *
 * `TerminalHealth` has one shape for all three execution modes, and two of its signals are answered
 * by machinery that only exists on the fleet:
 *
 *   `reachable`  is whether the PROBE'S TRANSPORT reached the node, and only `fleet` dials one.
 *                `docker.ts` runs `docker exec` and `local.ts` runs `sh -c`, both on the streamer's
 *                own host — so `ssh: reachable` on a local pane is not a health fact, it is the page
 *                having loaded. `transport.ts:reachWord` already knows this and says "the local tmux
 *                server"; the chip was the only place still calling every mode's transport `ssh`.
 *
 *   `loads`      is the actor host's failure ratio, and it exists only where vmagent puts it there.
 *                `infra/programs/machine.ts` installs and enables `kontra-vmagent.service` on FLEET
 *                Machines and nowhere else — no compose service, no `kontra serve`, and no worker
 *                image scrapes :9110 — so nothing has ever remote-written a series for a local or a
 *                docker Worker. "unknown (not measured yet)" there is a promise that is never kept.
 *
 * The other three apply in every mode: a session exists or does not, a pane's process is running or
 * over, and Temporal can be asked about any Worker's queue from anywhere.
 *
 * DEFAULTS TO APPLICABLE. An unrecognised mode — a streamer from a later build — gets all five, so a
 * new mode's tiles are noisy rather than silently short a signal.
 */
export function signalApplies(signal: HealthSignal, mode: string): boolean {
  if (mode !== 'local' && mode !== 'docker') return true;
  return signal !== 'reachable' && signal !== 'loads';
}

/**
 * Why an axis does not apply, in one sentence — the thing a "n/a" marker owes its reader.
 *
 * ADR 0020 lets a surface omit an inapplicable axis but never lets a signal become uninspectable.
 * This is what the marker's tooltip and the drawer both print, so the two cannot drift.
 */
export function inapplicableReason(signal: HealthSignal, mode: string): string | undefined {
  if (signalApplies(signal, mode)) return undefined;
  if (signal === 'reachable') {
    return mode === 'docker'
      ? 'ssh: not applicable — a docker pane is probed with `docker exec` on the streamer’s own host, so there is no hop to be reachable over'
      : 'ssh: not applicable — a local pane is probed with `sh -c` on the streamer’s own host, so there is no hop to be reachable over';
  }
  return (
    'loads: not applicable — the actor host’s failure ratio comes from vmagent, which only the fleet ' +
    `Machine program installs, so nothing has ever scraped a ${mode} Worker`
  );
}

/**
 * One entry per signal, and `unknown` is never rendered as healthy.
 *
 * The rule comes from `heartbeat.ts`: "unknown" and "zero" have to stay distinguishable. The case
 * it exists for is the round-3 incident — 81 of 82 resource loads failing on one Machine while the
 * run reported `completed` — so the four signals are never collapsed into one light.
 *
 * Returned as structured entries rather than strings so a tile can carry a per-signal test hook
 * (`chip-<signal>`) and slice 3's chips can colour from `ok` without re-deriving any of this.
 *
 * `mode` DEFAULTS TO `fleet`, which is the mode where every axis applies — so a caller that does not
 * know the mode gets all five entries and drops nothing. The sentences themselves are unchanged by
 * it except `reachable`'s transport word, because that word was wrong for two of the three modes.
 */
export function healthEntries(health: TerminalHealth, mode = 'fleet'): HealthEntry[] {
  // What actually carried the probe. `transport.ts:reachWord` is the server's version of this, and
  // the chip used to say `ssh` in all three modes regardless of which one ran.
  const reach = mode === 'docker' ? 'exec' : mode === 'local' ? 'shell' : 'ssh';
  const entries: HealthEntry[] = [
    {
      signal: 'reachable',
      applies: signalApplies('reachable', mode),
      ok: health.reachable === 'ok' ? true : health.reachable === 'fail' ? false : null,
      label:
        health.reachable === 'ok'
          ? `${reach}: reachable`
          : health.reachable === 'fail'
            ? `${reach}: UNREACHABLE`
            : `${reach}: unknown`,
      text:
        health.reachable === 'ok'
          ? `${reach}: reachable`
          : health.reachable === 'fail'
            ? `${reach}: UNREACHABLE`
            : `${reach}: unknown (not probed yet)`,
    },
    {
      signal: 'session',
      applies: signalApplies('session', mode),
      ok: health.session === 'present' ? true : health.session === 'unknown' ? null : false,
      label:
        health.session === 'present'
          ? 'session: present'
          : health.session === 'absent'
            ? 'session: ABSENT'
            : health.session === 'no-tmux'
              ? 'session: no tmux'
              : 'session: unknown',
      text:
        health.session === 'present'
          ? 'session: present'
          : health.session === 'absent'
            ? 'session: ABSENT — converge to create it'
            : health.session === 'no-tmux'
              ? 'session: no tmux on the Machine — converge installs it'
              : 'session: unknown (not probed yet)',
    },
    {
      /**
       * THE FIFTH SIGNAL, and the one a present session hides.
       *
       * `exited` is a REAL failure state — it colours red like any other — because a Worker that
       * finished while its wall tile kept painting its last screen is precisely the thing this
       * surface is for. `unknown` is common and honest: tmux reports the hold shell for a running
       * Worker and a finished one alike, so the streamer refuses to guess from the command name.
       */
      signal: 'process',
      applies: signalApplies('process', mode),
      ok: health.process === 'running' ? true : health.process === 'exited' ? false : null,
      label:
        health.process === 'running'
          ? 'process: running'
          : health.process === 'exited'
            ? 'process: EXITED'
            : 'process: unknown',
      text:
        health.process === 'running'
          ? 'process: running'
          : health.process === 'exited'
            ? 'process: EXITED — the screen is its last output'
            : 'process: unknown (tmux reports the hold shell either way)',
    },
    // Slice 3 measures these two. Until then they say so, rather than looking fine.
    {
      signal: 'poller',
      applies: signalApplies('poller', mode),
      ok: health.poller === 'live' ? true : health.poller === 'none' ? false : null,
      label:
        health.poller === 'live'
          ? 'poller: live'
          : health.poller === 'none'
            ? 'poller: NONE'
            : 'poller: unknown',
      text:
        health.poller === 'live'
          ? 'poller: live'
          : health.poller === 'none'
            ? 'poller: NONE — registered but nothing polling'
            : 'poller: unknown (not measured yet)',
    },
    {
      signal: 'loads',
      applies: signalApplies('loads', mode),
      ok: health.loads === 'ok' ? true : health.loads === 'failing' ? false : null,
      label:
        health.loads === 'ok'
          ? 'loads: ok'
          : health.loads === 'failing'
            ? 'loads: FAILING'
            : 'loads: unknown',
      text:
        health.loads === 'ok'
          ? 'loads: ok'
          : health.loads === 'failing'
            ? 'loads: FAILING'
            : // NOT "not measured yet". On an installation with no metrics backend — this one, since
              // `docker-compose.yml` dropped VictoriaMetrics — that phrasing promises a reading that
              // never arrives. The honest sentence names what is missing instead of implying a wait.
              'loads: unknown — no metrics backend has answered for this node',
    },
  ];
  if (health.detail) {
    entries.push({ signal: 'detail', text: health.detail, label: health.detail, applies: true, ok: false });
  }
  return entries;
}

/** The same thing as plain sentences. */
export function healthLines(health: TerminalHealth): string[] {
  return healthEntries(health).map((e) => e.text);
}

/** The axes that roll up into "serving". `process` is not one — see {@link isHealthy}. */
const ROLLUP_SIGNALS: readonly HealthSignal[] = ['reachable', 'session', 'poller', 'loads'];

/**
 * True only when every signal that CAN say something about a pane in this mode has been measured AND
 * is good. `unknown` is not healthy.
 *
 * MODE-AWARE FOR THE REASON THE CHIPS ARE (`signalApplies`, and this calls it rather than restating
 * it). `loads` comes from vmagent, which only `infra/programs/machine.ts` installs, and `reachable`
 * is whether a TRANSPORT was crossed, which only `fleet` crosses. So `loads === 'ok'` was a
 * condition a `local` or `docker` node could never satisfy on any installation — and this predicate
 * is what `ActorsPage` and `ActorCard` count "serving" with, so a perfectly healthy local actor was
 * reported as **0 serving**, forever, with no way to tell it from an actor nothing was polling. That
 * is the same defect the chip row fixed one surface earlier: an axis that cannot apply to a mode,
 * treated as a failure.
 *
 * OMISSION IS ONE-DIRECTIONAL HERE TOO, which is what keeps it from being the collapse ADR 0020
 * forbids — and it is the same one-directionality `HealthChips.chipIsShown` enforces, expressed
 * against the same entries. An axis is skipped only when it did not apply AND had nothing to say
 * (`ok === null`); a reading of `false` — `fail`, `failing`, `absent`, `no-tmux`, `none` — is
 * disqualifying in every mode, applicable or not, and `unknown` on an axis that COULD have been
 * measured is still not healthy. The only thing this can turn from false to true is a node whose
 * sole objection was an axis nothing was ever going to answer.
 *
 * DERIVED FROM `healthEntries`, not from a second reading of the same fields, so the number on the
 * Actors page and the chips on the tile cannot come apart.
 *
 * `mode` DEFAULTS TO `fleet` — the mode where every axis applies — so a caller that does not know the
 * mode gets the strictest reading and this can never accidentally loosen a count.
 *
 * `process` IS DELIBERATELY NOT IN THIS ROLLUP, and leaving it out is the honest choice rather than
 * the convenient one. It is `unknown` for a perfectly healthy local Worker by construction — tmux
 * reports the hold shell whether the Worker is running or finished — so folding it in would make
 * `ActorsPage`'s "serving" count read 0 on a fleet that is serving fine, which is the same class of
 * lie in the other direction. The signal is shown per tile, where "unknown" can carry its reason;
 * it is never rolled up into a number.
 */
export function isHealthy(health: TerminalHealth, mode = 'fleet'): boolean {
  return healthReading(health, mode) === true;
}

/**
 * THE SAME ROLLUP, TRI-STATE — `true` measured and fine, `false` something is broken, `null` NOBODY
 * LOOKED. Feed it to `HealthChips.chipState` and the three become `ok | bad | unknown`.
 *
 * {@link isHealthy} is this, narrowed to a boolean, and it is narrowed the only way a count can be:
 * "not healthy". That is right for a NUMBER — a tile you cannot vouch for is not one you count as
 * serving — and wrong for a LABEL, because it renders "we never measured this pane" in the same
 * words as "this pane's handler is down", and the operator's next action differs completely. ADR
 * 0020's rule is that the two must stay apart; the chips have kept them apart per signal since
 * slice 3, and this is what lets a surface with room for one reading do the same.
 *
 * SEVERITY DECIDES, in the chips' own order: a single `false` on any axis outranks every unknown,
 * and an unknown outranks the greens. So a pane with one dead handler and three unmeasured axes is
 * `false`, never `null` — "we cannot see" must not swallow "it is broken".
 *
 * The axes, the mode-awareness and the one-directional omission are {@link isHealthy}'s, unchanged:
 * this is where they now live, and that function is a thin call so the two cannot come apart.
 */
export function healthReading(health: TerminalHealth, mode = 'fleet'): boolean | null {
  const entries = healthEntries(health, mode).filter((entry) =>
    ROLLUP_SIGNALS.includes(entry.signal)
  );
  if (entries.some((entry) => entry.ok === false)) return false;
  if (entries.every((entry) => entry.ok === true || (entry.ok === null && !entry.applies))) {
    return true;
  }
  return null;
}
