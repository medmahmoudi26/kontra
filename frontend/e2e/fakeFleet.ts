/**
 * The SSH/tmux seam, faked — and nothing else is faked (ADR 0020, slice 1's `PanelDeps`).
 *
 * The browser suite boots the REAL `PanelServer`. What it cannot have is a fleet: no Machine in this
 * environment can be SSHed to, so `discover`, `probe`, `snapshot` and `converge` — the four injected
 * seams `PanelDeps` exists for — are answered from memory here. Everything downstream of them is the
 * shipping code: the hand-rolled RFC 6455 handshake, the masked-frame decoder, the `[idLen][id]`
 * tagging, the `\x1b[H\x1b[2J` snapshot prefix, the ticket book, the CORS and Origin checks and the
 * fail-closed 503.
 *
 * WHY THIS AND NOT ONLY THE STUB (CONTRACT.md 14). A real browser against the real streamer is the
 * strictest client slice 1's hand-rolled RFC 6455 will ever meet — Chromium masks every client frame,
 * verifies `Sec-WebSocket-Accept` itself, and closes with a real close frame — and none of that code
 * runs when the specs drive `stubStreamer.mjs`. The stub answers the other question (does the browser
 * read what the WIRE specifies, per an independent implementation), so both projects exist and the
 * specs are shared.
 *
 * Two Machines, on purpose: `kf-crawl-01` has its session, `kf-crawl-02` does not. That is the shape
 * that catches a wall which renders an absent session as a quiet tile.
 */

import type { MachineTarget } from '../../backend/src/panels/discovery';
import type { ProbeResult } from '../../backend/src/panels/probe';
import type { PanelDeps } from '../../backend/src/panels/server';

/** The window every screen carries, exactly once. Counting it in the DOM is how the specs tell a
 * repaint from an append: five appended screens leave five markers. */
export const SCREEN_MARKER = 'KONTRA-E2E-SCREEN';

/** Lines in one synthetic screen. A spec asserts the terminal never grows past this: with the
 * clear-home prefix it stays one screen tall, and without it, it grows by this much every repaint.
 * `stubStreamer.mjs` emits the same four lines. */
export const SCREEN_LINES = 4;

export const MACHINE_WITH_SESSION = 'kf-crawl-01';
export const MACHINE_WITHOUT_SESSION = 'kf-crawl-02';
export const SESSION = 'kontra-webcrawl';

/** `fleet:<machine>/<session>/<window>` — formatted here the way the frontend receives it, so a
 * spec's selector is the id an operator would see. */
export function terminalIdFor(machine: string, window: string): string {
  return `fleet:${machine}/${SESSION}/${window}`;
}

/** The Terminal the Dashboard picks today: `terminalList()` sorts by id, so this is the first. */
export const FIRST_TERMINAL = terminalIdFor(MACHINE_WITH_SESSION, 'actor');

function machineTarget(machine: string, lastOctet: number): MachineTarget {
  return {
    machine,
    host: `10.124.0.${lastOctet}`,
    publicIp: `203.0.113.${lastOctet}`,
    tag: 'crawl',
    fleet: 'fleet-apex-119',
    actor: 'webcrawl',
    version: '0.2.0',
    session: SESSION,
    windows: ['actor', 'handler'],
  };
}

const PRESENT: ProbeResult = { reachable: 'ok', session: 'present', windows: ['actor', 'handler'] };

const ABSENT: ProbeResult = {
  reachable: 'ok',
  session: 'absent',
  windows: [],
  detail: `${MACHINE_WITHOUT_SESSION} is up but has no session ${SESSION} — converge to create it`,
};

/**
 * A Fleet that lives in memory.
 *
 * Mutable on purpose: a spec changes `label` and waits for the tile to follow, or flips a Machine's
 * session to absent and reloads. The suite runs in one worker, so a test owns this object for its
 * duration and {@link reset} puts it back.
 */
export class FakeFleet implements PanelDeps {
  /** What each screen says. A spec sets this to watch a repaint land. */
  label = 'SCREEN-ONE';

  /** Bumped on every screen produced, so two consecutive snapshots always differ. */
  paints = 0;

  /** Every batched exec the streamer asked for — the evidence for "one exec per Machine". */
  readonly execs: Array<{ machine: string; windows: string[] }> = [];

  /** Machines a `converge` reached. The client picks WHICH; the streamer decides what. */
  readonly converges: string[] = [];

  private probes = new Map<string, ProbeResult>();

  constructor() {
    this.reset();
  }

  reset(): void {
    this.label = 'SCREEN-ONE';
    this.paints = 0;
    this.execs.length = 0;
    this.converges.length = 0;
    this.probes = new Map([
      [MACHINE_WITH_SESSION, PRESENT],
      [MACHINE_WITHOUT_SESSION, ABSENT],
    ]);
  }

  /** Make a Machine's session absent (or present again) — the drift a Terminal is meant to detect. */
  setProbe(machine: string, probe: ProbeResult): void {
    this.probes.set(machine, probe);
  }

  /** The same thing in the vocabulary the specs use, so `StreamerControl` reads the same either side. */
  setSession(machine: string, state: 'present' | 'absent'): void {
    this.setProbe(machine, state === 'present' ? PRESENT : FakeFleet.absentProbe(machine));
  }

  /** `probe` for a Machine whose session has gone away, worded the way the real probe words it. */
  static absentProbe(machine: string): ProbeResult {
    return {
      reachable: 'ok',
      session: 'absent',
      windows: [],
      detail: `${machine} is up but has no session ${SESSION} — converge to create it`,
    };
  }

  // --- PanelDeps ------------------------------------------------------------------------------

  async discover(): Promise<MachineTarget[]> {
    return [machineTarget(MACHINE_WITH_SESSION, 9), machineTarget(MACHINE_WITHOUT_SESSION, 10)];
  }

  async probe(machine: MachineTarget): Promise<ProbeResult> {
    return this.probes.get(machine.machine) ?? PRESENT;
  }

  async snapshot(machine: MachineTarget, windows: string[]): Promise<Map<string, string>> {
    this.execs.push({ machine: machine.machine, windows: [...windows].sort() });
    const out = new Map<string, string>();
    for (const window of windows) {
      this.paints += 1;
      out.set(window, this.screen(machine.machine, window, this.paints));
    }
    return out;
  }

  async converge(machine: MachineTarget): Promise<{ workflowId: string }> {
    this.converges.push(machine.machine);
    return { workflowId: `tmux-${machine.machine}` };
  }

  /**
   * One window's screen, shaped like what `capture-pane -p -e` returns: a journal follower's
   * output, with the SGR sequences `-e` preserves. The escapes are here so the spec proves xterm
   * PARSED the bytes rather than that the page printed them — a tile showing `[1;32m` literally is
   * a broken binary path that a string comparison on the wire would not catch.
   */
  private screen(machine: string, window: string, paint: number): string {
    return [
      `\x1b[1;32m●\x1b[0m kontra-${window}.service on ${machine}`,
      `Aug 11 22:04:${String(paint % 60).padStart(2, '0')} ${machine} kontra-${window}[1421]: fetched page/${paint}`,
      `Aug 11 22:04:${String(paint % 60).padStart(2, '0')} ${machine} kontra-${window}[1421]: \x1b[33mwarn\x1b[0m retrying once`,
      `${SCREEN_MARKER} ${this.label} paint=${paint}`,
    ].join('\n');
  }
}
