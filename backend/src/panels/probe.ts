/**
 * The read path IS the drift detector (ADR 0020).
 *
 * There is deliberately no polling reconciler for sessions. One `list-panes -a` per Machine, on the
 * discovery cadence, answers both questions a tile needs before it can honestly draw anything:
 * did SSH reach the Machine, and does the session exist on it. A Terminal that cannot attach
 * reports "no session" and offers the converge — which costs nothing when nobody is looking, and
 * duplicates neither systemd nor the stack converge.
 *
 * Three failures, three tiles, three actions — never one grey light:
 *
 *   reachable: 'fail'      the Machine is gone, rebooting, or the key is wrong
 *   session: 'no-tmux'     the Machine has no tmux; the converge installs it
 *   session: 'absent'      tmux is there and the session is not; the converge creates it
 *
 * `poller` and `loads` stay `'unknown'` here. They are slice 3's Temporal and VictoriaMetrics
 * joins, and `unknown` is the honest value for a signal nothing has measured — never `ok`.
 */

import { terminalId } from './ids';
import type { MachineTarget } from './discovery';
import { sshAddress } from './discovery';
import type { SshRunner } from './ssh';
import {
  LIST_PANES_COMMAND,
  looksLikeHostKeyChanged,
  looksLikeNoTmux,
  paneProcess,
  parseListPanes,
  parseProbeHost,
  type PaneProcess,
  type PaneRow,
} from './tmux';
import { modeOf, reachWord, type ExecTarget } from './transport';
import type { Terminal, TerminalHealth } from './types';

/**
 * What the probe saw in ONE window's pane, beyond its existence.
 *
 * PER WINDOW, not per Machine, because this is the half of a tile's truth that differs between the
 * `actor` window and the `handler` window beside it — one can be finished while the other runs, and
 * a Machine-level answer would have to pick one of them to be wrong about.
 */
export interface PaneFacts {
  /** `pane_current_command` — what tmux says is in the pane's foreground. */
  command: string;
  /** The pane's OWN geometry, which since `window-size manual` is pinned and no longer follows a
   *  viewer's tile. Not the browser's measurement of the tile — those are two different numbers and
   *  a tile that shows one labelled as the other is the lie this pass exists to end. */
  cols: number;
  rows: number;
  process: PaneProcess;
  /** '' unless the pane reported one. */
  exitStatus: string;
  /** The pane's own sentence, when it has one — folded into `health.detail`. */
  detail?: string;
}

export interface ProbeResult {
  reachable: TerminalHealth['reachable'];
  session: TerminalHealth['session'];
  /** Windows really present in the Machine's session, in tmux's order. */
  windows: string[];
  /** Per window, what its pane is doing. Absent for a window the probe never saw. */
  panes?: Map<string, PaneFacts>;
  /** One sentence per failing signal, for `health.detail`. */
  detail?: string;
  /**
   * WHY THIS NODE HAS NO TERMINALS AT ALL, when that is the honest answer.
   *
   * Set only when what answered is NOT the node this probe was for — see {@link HOST_MARKER}. It is
   * a separate field rather than a health value because the consequence is different in kind: every
   * other failure produces a tile that says what is wrong, and this one must produce NO TILE, since
   * the alternative is a tile that draws a live screen belonging to somebody else. `server.ts` reads
   * it, drops the node's Terminals, and answers a `subscribe` or a `converge` for one of them with
   * this sentence.
   *
   * It deliberately does not become a `session` value. `frontend/…/TerminalTile.tsx:sessionGone`
   * offers "Converge session" for every `session` that is neither `present` nor `unknown`, so a new
   * enum member would have appeared as the exact remedy this is here to withdraw.
   */
  gone?: string;
}

/**
 * The target for one node's exec: the address in every mode, and the mode itself, which is what the
 * runner dispatches on.
 *
 * THE ONE PLACE A MachineTarget BECOMES AN ExecTarget. It used to be two — the streamer's live
 * attach built the same literal by hand — which is how a field added for the fleet transport
 * (`publicIp`, which keys its known-hosts file) would have reached the probe and not the attach,
 * and the tiles would have disagreed about a Machine's identity depending on which one asked.
 */
export function probeTarget(m: MachineTarget): ExecTarget {
  return { mode: modeOf(m), machine: m.machine, host: sshAddress(m), publicIp: m.publicIp };
}

/**
 * Probe one node. Never throws: a probe that throws is a tile that disappears.
 *
 * `run` is whichever transport the target's mode names — `ssh`, `docker exec`, or a local shell — and
 * this function does not know which. That is the extraction's point: one probe, three modes, no
 * second implementation of "what does `list-panes` mean".
 */
export async function probeMachine(ssh: SshRunner, m: MachineTarget): Promise<ProbeResult> {
  let res;
  try {
    res = await ssh.run(probeTarget(m), LIST_PANES_COMMAND, { timeoutMs: 15_000 });
  } catch (err) {
    return {
      reachable: 'fail',
      session: 'unknown',
      windows: [],
      detail: `${reachWord(modeOf(m), m.machine)} failed: ${(err as Error).message}`,
    };
  }
  return interpretProbe(m, res.code, res.stdout, res.stderr, res.timedOut);
}

/**
 * What an operator should DO about a session that is not there, per mode.
 *
 * The three answers are genuinely different, and printing the fleet's answer everywhere would send
 * someone to a converge that cannot exist:
 *
 *   fleet    converge it — the Fleet owns session existence, as a Temporal workflow
 *   local    `kontra serve --tmux` owns it, and converging would mean STARTING A WORKER from a browser
 *   docker   nothing does, yet: `kontra scale` creates no session inside a worker container
 */
export function sessionAdvice(m: MachineTarget): string {
  switch (modeOf(m)) {
    case 'local':
      return `start one with \`kontra serve --actor <dir> --mode local --tmux\``;
    case 'docker':
      return `\`kontra scale\` does not create a tmux session inside a worker container, so there is nothing to attach to yet`;
    default:
      return 'converge to create it';
  }
}

/**
 * Turn one `list-panes` exit into health. Pure — this is where the three failures are separated,
 * so the separation is testable without a Machine.
 */
export function interpretProbe(
  m: MachineTarget,
  code: number,
  stdout: string,
  stderr: string,
  timedOut = false
): ProbeResult {
  const mode = modeOf(m);
  const reach = reachWord(mode, m.machine);
  if (timedOut) {
    return {
      reachable: 'fail',
      session: 'unknown',
      windows: [],
      detail: `${reach} timed out — the ${nodeWord(m)} is unreachable or wedged`,
    };
  }
  if (looksLikeNoTmux(code, stderr)) {
    // Reachable: the command ran and the shell answered. It is tmux that is missing.
    return {
      reachable: 'ok',
      session: 'no-tmux',
      windows: [],
      detail:
        mode === 'fleet'
          ? `${m.machine} has no tmux — converge the session to install it`
          : `${m.machine} has no tmux (${mode === 'local' ? 'apt-get install tmux' : 'the worker image does not carry it'})`,
    };
  }
  if (looksLikeHostKeyChanged(code, stderr)) {
    // The Machine answered; we refused it. Distinguished from every other exit-255 because the
    // action is different — waiting does not fix a key that will not change back.
    return {
      reachable: 'fail',
      session: 'unknown',
      windows: [],
      detail:
        `${m.machine} answered with a host key we did not expect at ${m.host} — ` +
        'the address was reused by an earlier Machine. Nothing is wrong with the Machine; the ' +
        'streamer keeps its learned keys per Machine, so this clears when it next discovers this one.',
    };
  }
  if (code !== 0) {
    // `list-panes` on a Machine with a running server but no sessions exits non-zero with exactly
    // this message. That is "reachable, no session", not a failure.
    if (/no server running|error connecting to/i.test(stderr)) {
      return {
        reachable: 'ok',
        session: 'absent',
        windows: [],
        detail:
          mode === 'fleet'
            ? `${m.machine} has no tmux server running — converge to create the session`
            : `${m.machine} has no tmux server running — ${sessionAdvice(m)}`,
      };
    }
    return {
      reachable: 'fail',
      session: 'unknown',
      windows: [],
      detail: `${reach} exited ${code}: ${stderr.trim().split('\n')[0] ?? ''}`,
    };
  }

  // THE BOX ANSWERED; IS IT THE ONE WE ASKED? Checked before any pane is read, because every pane
  // below belongs to whatever actually answered. See `tmux.ts:HOST_MARKER` for the live incident:
  // a recycled VPC address plus a session name derived from `<actor>-<version>` put one Machine's
  // screen on the wall under a destroyed Machine's name, with every field consistent.
  const gone = misattributed(m, parseProbeHost(stdout));
  if (gone) {
    return { reachable: 'fail', session: 'unknown', windows: [], detail: gone, gone };
  }

  const panes = parseListPanes(stdout);
  const windows: string[] = [];
  const facts = new Map<string, PaneFacts>();
  for (const p of panes) {
    if (p.session !== m.session) continue;
    if (!windows.includes(p.window)) windows.push(p.window);
    const seen = facts.get(p.window);
    // ONE PANE SPEAKS FOR THE WINDOW, and which one is a decision rather than an accident: kontra
    // creates exactly one pane per window, so this only arises for a window an operator SPLIT — and
    // there the running half is the answer an operator wants, not whichever half tmux listed first.
    if (!seen || (seen.process !== 'running' && verdictOf(p).process === 'running')) {
      facts.set(p.window, factsFor(p));
    }
  }
  if (!windows.length) {
    return {
      reachable: 'ok',
      session: 'absent',
      windows: [],
      detail: `${m.machine} is up but has no session ${m.session} — ${sessionAdvice(m)}`,
    };
  }
  return { reachable: 'ok', session: 'present', windows, panes: facts };
}

/**
 * Did the box that answered say it is somebody else?
 *
 * FLEET ONLY, and that is a decision rather than a convenience. A `fleet` node is addressed by an IP
 * that a provider RECYCLES and named `kf-<tag>-NN`, which DigitalOcean makes the droplet's hostname
 * — so the two are comparable and the comparison is the whole point. A `docker` node is addressed by
 * its container name and reports the container ID as its hostname; a `local` node is `localhost`
 * while the box calls itself something else (`health.ts:selfHostName` documents the same asymmetry
 * and the live incident it caused). Comparing either would take every local and container tile off
 * the wall, which is precisely the failure this function exists to prevent, aimed the other way.
 *
 * SILENCE IS NOT A MISMATCH. An older streamer's command printed no marker, and `docker exec` into a
 * scratch-based image has no `hostname` at all. Both are unchecked, not wrong.
 *
 * The comparison is on the first LABEL of the name, case-insensitively: a Machine's hostname is
 * `kf-nscheck-01` and a box may or may not append a domain, and neither of those is a difference
 * between two Machines.
 */
function misattributed(m: MachineTarget, reported: string | undefined): string | undefined {
  if (modeOf(m) !== 'fleet' || !reported) return undefined;
  const label = (s: string): string => (s.split('.')[0] ?? s).trim().toLowerCase();
  if (label(reported) === label(m.machine)) return undefined;
  return (
    `${sshAddress(m)} answered, and it says it is ${reported} — not ${m.machine}. This Machine is ` +
    'gone and its address has been reused by another Fleet, so what is on that box belongs to ' +
    'somebody else; nothing here will show it, and a converge would run against the wrong Machine. ' +
    '`kontra fleet down` the stack that still lists it.'
  );
}

/**
 * Several sentences in the one `detail` field the wire has, joined.
 *
 * `health.ts` documents this and re-exports it; it LIVES here because the pane's sentence is joined
 * onto the Machine's in this file, and `health.ts` already imports this one. The separator is ' · '
 * because `HealthChips.tsx` splits on it to put each sentence back beside the signal that produced
 * it — the signals themselves never merge, only their prose shares a field.
 */
export const DETAIL_SEPARATOR = ' · ';

export function joinDetails(parts: readonly (string | undefined)[]): string | undefined {
  const kept = parts.filter((p): p is string => typeof p === 'string' && p.trim() !== '');
  return kept.length === 0 ? undefined : kept.join(DETAIL_SEPARATOR);
}

/** One pane's verdict, memo-free — {@link paneProcess} is pure and cheap (a set lookup and two
 * field reads), and caching it would be the kind of state that later disagrees with itself. */
function verdictOf(row: PaneRow): ReturnType<typeof paneProcess> {
  return paneProcess(row);
}

function factsFor(row: PaneRow): PaneFacts {
  const verdict = verdictOf(row);
  const facts: PaneFacts = {
    command: row.command,
    cols: row.cols,
    rows: row.rows,
    process: verdict.process,
    exitStatus: verdict.exitStatus,
  };
  if (verdict.detail !== undefined) facts.detail = verdict.detail;
  return facts;
}

/** What the node IS, for a sentence about it. A fleet node is a Machine; the other two are not, and
 * calling a container "the Machine" is how an operator goes looking for a droplet that never
 * existed. */
function nodeWord(m: MachineTarget): string {
  switch (modeOf(m)) {
    case 'docker':
      return 'container';
    case 'local':
      return 'local tmux server';
    default:
      return 'Machine';
  }
}

/**
 * The Terminals for one Machine.
 *
 * The union of what the converge WOULD create and what the probe FOUND, in that order: a Machine
 * with no session still shows its `actor` and `handler` tiles (that is the converge affordance),
 * and an operator's hand-made window shows up beside them rather than being invisible because it
 * was not in our defaults.
 */
export function terminalsForMachine(
  m: MachineTarget,
  probe: ProbeResult,
  poller: TerminalHealth['poller'] = 'unknown',
  loads: TerminalHealth['loads'] = 'unknown'
): Terminal[] {
  const windows = [...m.windows];
  for (const w of probe.windows) if (!windows.includes(w)) windows.push(w);

  const out: Terminal[] = [];
  for (const window of windows) {
    // A window the probe did not see is absent even when its siblings are present — otherwise a
    // killed window renders as a quiet tile, which acceptance criterion 5 forbids.
    const session: TerminalHealth['session'] =
      probe.session === 'present' && !probe.windows.includes(window) ? 'absent' : probe.session;
    const pane = probe.panes?.get(window);
    const health: TerminalHealth = {
      reachable: probe.reachable,
      session,
      // A window with no session behind it has no pane to have a process in, and reporting one
      // would be the fifth signal contradicting the second. `unknown` is the only true value.
      process: session === 'present' ? (pane?.process ?? 'unknown') : 'unknown',
      poller,
      loads,
    };
    // The pane's own sentence rides alongside the Machine's, joined the way `health.ts` joins the
    // rest — the signals stay in their own fields and only their PROSE shares one.
    const detail = joinDetails([
      probe.detail,
      session === 'absent' && probe.session === 'present'
        ? `window ${window} is not in session ${m.session} — ${sessionAdvice(m)}`
        : undefined,
      session === 'present' ? pane?.detail : undefined,
    ]);
    if (detail !== undefined) health.detail = detail;
    let id: string;
    try {
      id = terminalId({ mode: modeOf(m), machine: m.machine, session: m.session, window });
    } catch {
      // A window name tmux allows but we do not (a space, a quote) cannot be a Terminal id, and it
      // must not be silently rewritten into one.
      continue;
    }
    out.push({
      id,
      // The mode, on the wire. A tile has to be able to say whether this pane holds a journal or a
      // running Worker — ADR 0020's finding (2) makes that the difference between a crashed tmux
      // server costing the view and costing the Worker.
      mode: modeOf(m),
      machine: m.machine,
      host: m.host,
      publicIp: m.publicIp,
      tag: m.tag,
      fleet: m.fleet,
      actor: m.actor,
      version: m.version,
      window,
      // WHAT IS IN THE PANE, AND HOW BIG THE PANE IS — the two facts a tile could not previously
      // state, and the reason it could not say anything true about the thing it was drawing. Both
      // come from the `list-panes` probe that already runs; neither is inferred from the browser.
      command: pane?.command ?? '',
      paneCols: pane?.cols ?? 0,
      paneRows: pane?.rows ?? 0,
      exitStatus: pane?.exitStatus ?? '',
      health,
    });
  }
  return out;
}
