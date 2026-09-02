/**
 * What a **Warden** says about its **Machine**, and what the Monitor makes of it (ADR 0037).
 *
 * ═══ WHY THE PANE-SNAPSHOT PATH AND NOT WORKFLOW HISTORY ═══
 *
 * ADR 0037: "Telemetry does not ride workflow history. CPU, memory, load ratios and tmux frames go
 * over the existing pane-snapshot path. A 200×50 terminal frame through Temporal history is the
 * shape this repo already measured as 86% of a workflow's events, for a value that is stale a second
 * later."
 *
 * So this module has no Temporal client, no workflow, no signal and no activity. It is a MAP with an
 * expiry, filled by a POST that a Machine dials outbound, and read by the same `refresh()` that
 * already builds Terminals. `warden.test.ts` sweeps this file and `server.ts` for a Temporal import
 * with a positive control, because "we did not do X" is the assertion most likely to be vacuous.
 *
 * ═══ A REPORT IS EVIDENCE OF EXISTENCE, WHICH IS WHAT THE INVENTORY IS NOT ═══
 *
 * `discovery.ts` reads Pulumi's checkpoint, which says a **Machine** was PROVISIONED. That is not
 * the same claim as "it is there now", and the difference has been visible on the wall: a Fleet
 * whose Machines were destroyed outside Pulumi — or whose `fleet down` never ran — leaves a
 * checkpoint full of Machines that do not exist, and the streamer then dials their addresses.
 * See `tmux.ts:HOST_MARKER` for what it found when it did.
 *
 * A Warden's report is the other kind of evidence: it is DIALLED BY THE MACHINE, so it exists only
 * while the Machine does, and it carries the Warden's own identity rather than an address anybody
 * can inherit. Two consequences, and both are the point:
 *
 *   - a Machine that reports is not probed by address at all, so a recycled address cannot
 *     impersonate it; and
 *   - a Machine that HAS reported and then goes quiet is gone, not merely unreachable, and its
 *     Terminals leave the wall rather than sitting there offering a converge.
 *
 * A Machine that has NEVER reported keeps the SSH path unchanged. That is not a transitional
 * hedge — a Fleet whose Machines predate the Warden, an appliance install with no enrolment, and a
 * `docker`/`local` node are all permanent cases with no Warden in them.
 *
 * ═══ WHAT AUTHENTICATES A REPORT TODAY, STATED PLAINLY ═══
 *
 * The ingest is gated by `KONTRA_PANEL_TOKEN`, exactly like every other route on this server, and
 * that is WEAKER than what ADR 0037 describes: it says the Controller "derives it from the key the
 * handshake proved this Machine holds", so that "a Warden cannot ask for another Warden's assignment
 * even by trying". That derivation lives on the mTLS enrolment server (`cli/warden/warden_ca.go`), and
 * until the ingest is mounted there too, anything holding the panel token can file a report naming
 * any Machine. The panel token is not a public credential — `routes/panels.ts` keeps it out of the
 * browser bundle deliberately — but it is one credential for a whole Fleet rather than one identity
 * per Machine. Recorded here rather than left to be discovered.
 */

import type { MachineTarget } from './discovery';
import { sessionNameFor } from './discovery';
import { SAFE } from './ids';
import type { ProbeResult, PaneFacts } from './probe';
import type { MachineTelemetry } from './types';

/**
 * How long a report is believed.
 *
 * A Warden posts every 3 s (`cli/warden/panereport.go:reportInterval`), so this is forty missed reports.
 * Wide on purpose: the cost of being late is a stale pane for two minutes, and the cost of being
 * early is a Machine that drops off the wall every time its Controller has a slow minute — and the
 * whole reason this path exists rather than a workflow's is that it is allowed to lose a message.
 */
export const REPORT_TTL_MS = 120_000;

/**
 * How many Machines one streamer will hold reports for.
 *
 * A cap rather than a trust, because this map is filled by a POST. The bound is generous against any
 * real Fleet (the largest run this repo has done is ten Machines) and small enough that a flood
 * cannot become the streamer's memory profile — which is the same rule `server.ts` applies to a
 * stalled browser tab.
 *
 * IT IS A COUNT OF MACHINES AND NOT OF WORKERS, which used to be the same number and is not since
 * packing (ADR 0037): a Machine holds several Workers now, and one Warden per Machine still sends
 * one report. So this bound did not have to move when the Worker count stopped being bounded by it.
 */
export const MAX_REPORTING_MACHINES = 512;

/** One window's screen, as a Warden rendered it. */
export interface WardenPane {
  window: string;
  cols: number;
  rows: number;
  frame: string;
}

/** A `workerVerdict` from `cli/warden/sickworker.go`, on the wire. */
export interface WardenHealth {
  verdict: 'sick' | 'healthy' | 'cannot-tell' | string;
  reason: string;
  detail?: string;
  ratio: number;
  loads: number;
  failures: number;
  spanSeconds: number;
}

export interface WardenWorker {
  id: string;
  name: string;
  version: string;
  whole: boolean;
  halves: string[];
  health: WardenHealth;
  panes: WardenPane[];
}

/**
 * The Machine's own numbers.
 *
 * EVERY FIELD IS OPTIONAL AND ABSENT MEANS UNMEASURED. `types.ts` states the rule: "`unknown` is a
 * value, never a shrug." A Machine whose /proc could not be read is not a Machine that is idle, and
 * a tile drawing 0% for it would be the friendliest possible lie.
 */
export interface WardenTelemetry {
  /** Fraction of the interval not idle, across all cores. 0..1. */
  cpu?: number;
  /** used/total, from MemAvailable. 0..1. */
  memory?: number;
  /** The kernel's one-minute run-queue average. */
  load1?: number;
}

export interface WardenReport {
  warden: string;
  machine: string;
  driver: string;
  /** The MACHINE's clock. Never used for freshness — see `accept`. */
  at: string;
  telemetry: WardenTelemetry;
  workers: WardenWorker[];
}

/** A report plus what the Controller knows about it that the Machine could not. */
interface Held {
  report: WardenReport;
  /** THE CONTROLLER'S clock, at arrival. */
  receivedAt: number;
}

/**
 * Every Machine's latest report.
 *
 * ONE REPORT PER MACHINE, REPLACED RATHER THAN APPENDED. A pane is the current screen; a history of
 * screens is a log, and this path exists precisely because a Machine's screen is not worth keeping.
 */
export class WardenReports {
  private held = new Map<string, Held>();

  /**
   * Every Machine that has EVER reported to this streamer.
   *
   * The question `has()` cannot answer, and the stale rule turns on it: a Machine that never had a
   * Warden keeps the SSH path, and a Machine that HAD one and went quiet is gone. Those two look
   * identical to a map with a TTL, and treating them the same would either empty the wall of every
   * pre-Warden Fleet or keep a destroyed Machine on it forever.
   *
   * It is NOT pruned with the reports, and that is the point: forgetting a Machine had a Warden is
   * exactly how a gone Machine would quietly become an unreachable one again. It is bounded by the
   * same cap as the reports, and it dies with the process — a restarted streamer legitimately knows
   * nothing, and reverts to the SSH path until the next report arrives.
   */
  private everSeen = new Set<string>();

  constructor(private readonly now: () => number = () => Date.now()) {}

  /**
   * Take one report.
   *
   * FRESHNESS IS THE CONTROLLER'S CLOCK, NOT THE MACHINE'S. A Machine whose clock is an hour behind
   * would otherwise have every report arrive already expired, and one an hour ahead would have its
   * last report believed for an hour after it died — which is exactly the stale pane this module is
   * here to end, arriving through the field meant to prevent it. The Machine's `at` is kept and
   * shown, so a skew reads as a skew.
   *
   * Returns whether the report was TAKEN, so the route can answer a refusal rather than a 202. A
   * Warden told its report was accepted, whose Machine then never appears on the wall, has no way to
   * tell that from a Controller with no Monitor open.
   */
  accept(report: WardenReport): boolean {
    if (!report || typeof report.machine !== 'string' || !report.machine.trim()) return false;
    const machine = report.machine.trim();
    // THE NAME HAS TO BE ONE A TERMINAL ID CAN CARRY, and this is the boundary where that stops
    // being assumed. `ids.ts:SAFE.machine` is `kf-<tag>-NN`, and a name outside it reaches
    // `terminalId`, throws, and is silently skipped one layer down — so a Machine reporting under
    // any other name would be accepted, held, counted, and invisible, which is indistinguishable
    // from a Machine whose reports never arrived. `discovery.ts` applies the same rule to the
    // inventory for the same reason and says so: "a name that does not match `kf-<role>-NN` cannot
    // be a Terminal id, and quietly rewriting it would put a value we never validated into a remote
    // command."
    if (!SAFE.machine.test(machine)) return false;
    if (!this.held.has(machine) && this.held.size >= MAX_REPORTING_MACHINES) {
      // A NEW Machine is refused at the cap; an existing one's report is always taken. Refusing the
      // update instead would freeze the wall at whatever it held when the cap was hit, which reads
      // as a Fleet that stopped changing rather than as a Controller that stopped listening.
      return false;
    }
    this.held.set(machine, { report, receivedAt: this.now() });
    this.everSeen.add(machine);
    return true;
  }

  /** Reports still inside their TTL, oldest evicted as a side effect. */
  private fresh(): Map<string, Held> {
    const cutoff = this.now() - REPORT_TTL_MS;
    for (const [machine, held] of this.held) {
      if (held.receivedAt < cutoff) this.held.delete(machine);
    }
    return this.held;
  }

  /** Has this Machine reported recently enough to be believed? */
  has(machine: string): boolean {
    return this.fresh().has(machine);
  }

  /** Has this Machine reported at ANY point in this streamer's life? See {@link everSeen}. */
  seen(machine: string): boolean {
    return this.everSeen.has(machine);
  }

  report(machine: string): WardenReport | undefined {
    return this.fresh().get(machine)?.report;
  }

  receivedAt(machine: string): number | undefined {
    return this.fresh().get(machine)?.receivedAt;
  }

  /** Machines currently reporting. Sorted, so a wall is stable across refreshes. */
  reporting(): string[] {
    return [...this.fresh().keys()].sort();
  }

  size(): number {
    return this.fresh().size;
  }
}

/**
 * Turn one Warden's report into the probe result the existing pipeline already understands.
 *
 * THE POINT OF DOING IT THIS WAY. `terminalsWithHealth(machine, probe, fleetHealth)` builds every
 * Terminal on the wall, and it does not care where its `ProbeResult` came from. So a Warden-sourced
 * fleet pane is the same object as an SSH-sourced one — same id, same geometry fields, same health
 * block, same tile — and the Monitor renders it with no change at all. That is what "over the
 * EXISTING pane-snapshot path" means in practice, and it is the reason this returns a `ProbeResult`
 * rather than inventing a second shape for the frontend to learn.
 */
export function probeFromReport(report: WardenReport): ProbeResult {
  const windows: string[] = [];
  const panes = new Map<string, PaneFacts>();
  for (const worker of report.workers ?? []) {
    for (const p of worker.panes ?? []) {
      if (!p || typeof p.window !== 'string' || !p.window) continue;
      if (!windows.includes(p.window)) windows.push(p.window);
      panes.set(p.window, {
        // WHAT IS IN THE PANE is the Worker's half, named. Under the tmux path this is
        // `pane_current_command`, which `types.ts` documents as honest-but-uninterpretable because
        // the hold shell owns the foreground. A Warden has no such indirection: it knows the half is
        // running because `list()` reported it.
        command: `${worker.name}@${worker.version} ${p.window}`,
        cols: p.cols || 0,
        rows: p.rows || 0,
        process: worker.whole || worker.halves?.includes(p.window) ? 'running' : 'exited',
        exitStatus: '',
        ...(worker.whole
          ? {}
          : {
              detail:
                `only its ${(worker.halves ?? []).join('+') || 'no'} half is running, and the pair is ` +
                'the unit — the Warden stops a half-dead Worker and starts it again on its next turn',
            }),
      });
    }
  }
  const detail = healthDetail(report);
  return {
    // A report ARRIVED, which is a stronger statement than an SSH round trip succeeding: the Machine
    // dialled out, so it is up, it has network, and its Warden is running.
    reachable: 'ok',
    session: windows.length ? 'present' : 'absent',
    windows,
    ...(windows.length ? { panes } : {}),
    ...(detail === undefined ? {} : { detail }),
  };
}

/**
 * The Machine's sentences: every Worker's health that is not plainly fine.
 *
 * `cannot-tell` IS INCLUDED. It is the value this whole path exists to keep honest — a health check
 * that silently degrades to always-pass is worse than no check — and a chip that says `unknown` with
 * no reason is one an operator learns to ignore.
 *
 * TELEMETRY IS DELIBERATELY NOT IN HERE. `types.ts` gives `detail` a precise contract — "one human
 * sentence per failing signal, never a collapsed summary" — and CPU at 4% is not a signal, it is
 * context for the ones that are. It travels as its own field on the Terminal, which is also what
 * lets a tile render it as a number rather than parse it out of prose.
 */
function healthDetail(report: WardenReport): string | undefined {
  const parts: string[] = [];
  for (const worker of report.workers ?? []) {
    const h = worker.health;
    if (!h || h.verdict === 'healthy') continue;
    parts.push(h.detail ? `${worker.id}: ${h.detail}` : `${worker.id}: ${h.verdict} (${h.reason})`);
  }
  return parts.length ? parts.join(' · ') : undefined;
}

/**
 * The Machine's numbers, as the wire carries them — or nothing at all when none were measured.
 *
 * A FIELD THAT WAS NOT MEASURED IS OMITTED, never rendered as 0, at every level including this one:
 * a report whose /proc could not be read produces no `telemetry` on the Terminal rather than a
 * `{cpu: 0}`. `types.ts`: "`unknown` is a value, never a shrug."
 */
export function machineTelemetry(report: WardenReport): MachineTelemetry | undefined {
  const t = report.telemetry;
  if (!t) return undefined;
  const out: MachineTelemetry = {};
  if (typeof t.cpu === 'number' && Number.isFinite(t.cpu)) out.cpu = t.cpu;
  if (typeof t.memory === 'number' && Number.isFinite(t.memory)) out.memory = t.memory;
  if (typeof t.load1 === 'number' && Number.isFinite(t.load1)) out.load1 = t.load1;
  return Object.keys(out).length ? out : undefined;
}

/**
 * CPU, memory and load as one sentence, for a surface that has room for prose and not for a row of
 * numbers — the CLI's `panels list`, and a tooltip.
 *
 * A FIELD THAT WAS NOT MEASURED IS OMITTED, never rendered as 0. See `WardenTelemetry`.
 */
export function telemetrySentence(t: WardenTelemetry | undefined): string | undefined {
  if (!t) return undefined;
  const bits: string[] = [];
  if (typeof t.cpu === 'number' && Number.isFinite(t.cpu)) bits.push(`cpu ${Math.round(t.cpu * 100)}%`);
  if (typeof t.memory === 'number' && Number.isFinite(t.memory)) {
    bits.push(`memory ${Math.round(t.memory * 100)}%`);
  }
  if (typeof t.load1 === 'number' && Number.isFinite(t.load1)) bits.push(`load ${t.load1.toFixed(2)}`);
  return bits.length ? bits.join(', ') : undefined;
}

/**
 * The `loads` verdict a Warden's report carries, translated to the chip's three values.
 *
 * THE MAPPING IS DELIBERATELY LOSSY IN ONE DIRECTION ONLY. `sick` and `healthy` become `failing` and
 * `ok`; every one of the Warden's six `cannot tell` reasons becomes `unknown`, which is the chip's
 * own third value and carries the Warden's sentence with it (see `healthDetail`). What must never
 * happen is the other direction — a `cannot tell` arriving as `ok` — which is the exact failure
 * `metrics.ts` refuses at length for the VictoriaMetrics path.
 */
export function loadsFromReport(report: WardenReport): 'ok' | 'failing' | 'unknown' {
  let sawHealthy = false;
  for (const worker of report.workers ?? []) {
    const verdict = worker.health?.verdict;
    if (verdict === 'sick') return 'failing';
    if (verdict === 'healthy') sawHealthy = true;
  }
  return sawHealthy ? 'ok' : 'unknown';
}

/**
 * The `poller` verdict a report can support, which is LESS than it looks.
 *
 * A Warden knows both halves are running; it does not know that Temporal has a poller on the queue.
 * Those are different facts — `serve.go`'s "half-dead worker" is precisely a process that is up and
 * not polling — so this reports `unknown` for anything short of a whole Worker and leaves the real
 * answer to `pollers.ts`, which asks Temporal. Claiming `live` from a process being up would be the
 * same lie one layer down that the `loads` chip refuses one layer up.
 */
export function pollerFromReport(report: WardenReport): 'none' | 'unknown' {
  const workers = report.workers ?? [];
  if (workers.length === 0) return 'none';
  return 'unknown';
}

/**
 * A `MachineTarget` for a Machine that is reporting but is not in the Fleet inventory.
 *
 * THIS IS THE BYOC PATH ARRIVING. ADR 0037's `machines=None` "takes what already exists rather than
 * provisioning", and a self-enrolled Machine was never in a Pulumi checkpoint at all — so a Monitor
 * that only listed what `discovery.ts` found would be blind to exactly the Machines this program is
 * being built for. A Machine that dialled out and named itself is better evidence than a checkpoint.
 */
export function targetFromReport(report: WardenReport): MachineTarget {
  const worker = (report.workers ?? [])[0];
  const actor = worker?.name ?? '';
  const version = worker?.version ?? '';
  return {
    mode: 'fleet',
    machine: report.machine,
    host: '',
    publicIp: '',
    tag: '',
    // A self-enrolled Machine belongs to no stack. `warden` rather than '' so the sidebar groups
    // them under something an operator can read, and so a blank never reaches an id.
    fleet: 'warden',
    actor,
    version,
    session: sessionNameFor(actor, version),
    windows: (worker?.panes ?? []).map((p) => p.window),
  };
}
