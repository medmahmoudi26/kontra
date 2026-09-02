/**
 * One control seam, two streamers (CONTRACT.md amendment 14).
 *
 * The suite runs the SAME specs against the hand-written stub (project `contract`) and against a
 * booted `PanelServer` (project `real`). That is only possible if "flip this session to absent" and
 * "switch the token off" are expressed once, in terms a spec can say, and implemented twice:
 *
 *   contract  →  HTTP calls to the stub's `/__stub/*` fixture channel
 *   real      →  in-process mutation of the faked `PanelDeps` seam, then `server.refresh()`
 *
 * The point of the pair is disagreement. If a spec passes against one and fails against the other,
 * either the stub has drifted from the wire the contract pins, or the real streamer has a bug in code
 * the stub never executes — the hand-rolled `Sec-WebSocket-Accept`, the frame encoder, masking,
 * fragmentation, close frames, the 126/127 length cases. Both answers are worth a report.
 *
 * Everything here is the FIXTURE's vocabulary, not the product's: the real streamer has no control
 * route, and `readonly.test.ts` is what keeps it that way.
 */

import { MACHINE_WITH_SESSION } from './fakeFleet';
import type { FakeStreamer } from './streamer';

export type StreamerKind = 'contract' | 'real';

/** Custom Playwright options — `playwright.config.ts` sets this per project. The run's PORTS are not
 * here: they come from the environment `e2e/run.mjs` established, which every process shares. */
export interface E2EOptions {
  streamerKind: StreamerKind;
}

export interface StreamerStats {
  /** Tickets minted and not yet redeemed. A page that connected leaves this at 0. */
  ticketsOutstanding: number;
  /** Screens produced since the last reset. */
  paints: number;
  /** Machines a `converge` reached, in order. */
  converges: string[];
}

export interface StreamerControl {
  readonly kind: StreamerKind;
  readonly base: string;
  /** Back to the known fleet: session present, screens at `SCREEN-ONE`, token on, counters cleared. */
  reset(): Promise<void>;
  /** What each screen says, so a spec can watch a repaint land. */
  setLabel(label: string): Promise<void>;
  /**
   * The session on the Machine the Dashboard is showing, announced on the LIVE path — the streamer
   * pushes `{t:'state'}` to open sockets, exactly as a probe round after a real `tmux kill-session`
   * would.
   */
  setSession(state: 'present' | 'absent'): Promise<void>;
  /** Unset the token: `auth.ts` fails closed, every panel route 503s and serves nothing. */
  disableToken(): Promise<void>;
  stats(): Promise<StreamerStats>;
  /** Lines the streamer logged — `PanelDeps.log` for the real one, stdout/stderr for the stub. */
  logs(): string[];
}

/** Project `real`: the shipping `PanelServer`, reached through the seams it was built to accept. */
export function realControl(streamer: FakeStreamer): StreamerControl {
  return {
    kind: 'real',
    base: streamer.base,
    reset: () => streamer.reset(),
    async setLabel(label: string): Promise<void> {
      streamer.fleet.label = label;
    },
    async setSession(state: 'present' | 'absent'): Promise<void> {
      streamer.fleet.setSession(MACHINE_WITH_SESSION, state);
      // The read path IS the drift detector: one probe round, then `announceHealth()` tells every
      // subscribed tab. No reload, no polling in the page.
      await streamer.server.refresh();
    },
    async disableToken(): Promise<void> {
      streamer.disableToken();
    },
    async stats(): Promise<StreamerStats> {
      return {
        ticketsOutstanding: streamer.server.tickets.outstanding,
        paints: streamer.fleet.paints,
        converges: [...streamer.fleet.converges],
      };
    },
    logs: () => streamer.logs,
  };
}

/** Project `contract`: the independently written stub, driven over its `/__stub/*` channel. */
export function stubControl(base: string, logs: () => string[]): StreamerControl {
  const control = async (path: string, method = 'POST'): Promise<unknown> => {
    const res = await fetch(`${base}${path}`, { method });
    if (!res.ok) throw new Error(`stub control ${method} ${path} answered ${res.status}`);
    return res.json();
  };
  return {
    kind: 'contract',
    base,
    async reset(): Promise<void> {
      await control('/__stub/reset');
    },
    async setLabel(label: string): Promise<void> {
      await control(`/__stub/label?value=${encodeURIComponent(label)}`);
    },
    async setSession(state: 'present' | 'absent'): Promise<void> {
      await control(`/__stub/session?state=${state}`);
    },
    async disableToken(): Promise<void> {
      await control('/__stub/token?state=off');
    },
    async stats(): Promise<StreamerStats> {
      return (await control('/__stub/stats', 'GET')) as StreamerStats;
    },
    // Its stdout and stderr, captured by whoever spawned it.
    logs,
  };
}
