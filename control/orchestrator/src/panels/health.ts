/**
 * The two health signals slice 1 left as `'unknown'` (ADR 0020, slice 3).
 *
 * `probe.ts` answers the two questions one SSH round trip can answer — did we reach the Machine,
 * does the session exist. This file answers the other two, each from a different system:
 *
 *   poller   Temporal `DescribeTaskQueue` on the actor's shared queue   -> `pollers.ts`
 *   loads    the sick-worker reload ratio from VictoriaMetrics          -> `metrics.ts`
 *
 * FOUR SIGNALS, NEVER COLLAPSED, and `unknown` never rendered as healthy. The case is the round-3
 * incident — 81 of 82 resource loads failing on one Machine while the run reported `completed` —
 * where every other view of that Worker looked fine. Each signal here fails independently, in its
 * own words, and a signal nobody could measure says so.
 *
 * THE DISCOVERY LOOP MUST NOT BE ABLE TO STALL ON EITHER. Both halves are bounded independently and
 * run concurrently, so a wedged Temporal costs the `poller` chip and a wedged VictoriaMetrics costs
 * the `loads` chip, and neither costs the wall. A timeout is `unknown` — the only value that is true
 * when the answer never arrived.
 *
 * The fleet is measured ONCE PER ROUND, not once per Machine: twelve Machines placed with one actor
 * share one task queue and one metrics query, so this is two Temporal calls and two HTTP calls for
 * the whole wall regardless of its size.
 */

import { hostname as osHostname } from 'node:os';

import type { MachineTarget } from './discovery';
import { joinDetails, terminalsForMachine, type ProbeResult } from './probe';
import { modeOf } from './transport';
import type { Terminal, TerminalHealth } from './types';
import {
  describeFleetQueues,
  pollerFor,
  queueForMachine,
  temporalQueueDescriber,
  type QueueDescriber,
  type QueueState,
} from './pollers';
import {
  loadRates,
  loadsFor,
  victoriaMetricsQuerier,
  type LoadRates,
  type MetricsQuerier,
} from './metrics';

/**
 * What the box these panes live on calls ITSELF.
 *
 * IT IS `os.hostname()` AGAIN, and the variable that used to override it is gone (issue 18).
 *
 * The history is worth keeping because the failure was invisible. A Worker started by
 * `kontra serve` identifies to Temporal with the host's name (`main-droplet`); this comparison
 * asks whether anything is polling a LOCAL pane's queue; and inside a container `os.hostname()`
 * returns the container id (`242b5de62fa4`). So every local pane on the wall reported
 * `poller: NONE — its kontra-handler.service is probably down`, in red, about a handler that was
 * up and polling. `kontra infra up` was made to inject `KONTRA_HOST_NAME` from the host, which
 * fixed it for the one deployment where the streamer is a container and the Workers are not.
 *
 * That deployment is the local development topology ADR 0031 retired. This code runs in ONE of
 * two places now: inside `kontra up`, a host process beside the host's own `kontra serve` — where
 * `os.hostname()` is exactly the right answer — or inside the compose controller's
 * `orchestrator-infra`, which streams FLEET panes, and a fleet pane's health never consults the
 * host's own name (see `hostIsMachine`: the third argument only decides a `localhost` node).
 *
 * `hostIsMachine` itself is untouched and was never wrong. It was being handed the wrong name,
 * and its unit test passed the name in as a parameter — which proves the rule and says nothing
 * about what production passes. Only running it on the live wall found that, which is why the
 * sentence survives the variable.
 */
function selfHostName(): string {
  return osHostname();
}

/** Temporal's half. Bounded separately from the metrics half so one slow system cannot hide the
 * other's answer. */
export const DEFAULT_POLLER_TIMEOUT_MS = 3000;

/** What one Machine's two extra signals came out as. */
export interface MachineHealth {
  poller: TerminalHealth['poller'];
  loads: TerminalHealth['loads'];
  /** The failing signals' sentences, joined. See `mergeDetail` on why one field holds several. */
  detail?: string;
}

/** One round's measurement of the whole fleet, as a lookup. */
export interface FleetHealth {
  for(machine: MachineTarget): MachineHealth;
}

/** The seam `server.ts` holds. Optional there: a streamer with no probe reports both signals as
 * `unknown`, which is exactly what it does today and is honest rather than degraded. */
export interface FleetHealthProbe {
  measure(machines: readonly MachineTarget[]): Promise<FleetHealth>;
  close?(): Promise<void>;
}

/** Nothing measured. Not a fallback to healthy — `unknown` is a visible state with its own words. */
export const UNKNOWN_FLEET_HEALTH: FleetHealth = {
  for: () => ({ poller: 'unknown', loads: 'unknown' }),
};

/**
 * Compose one round from the two systems' answers.
 *
 * Exported and pure so a test can pin the composition — which failing signal contributes which
 * sentence, and in what order — without a Temporal or a VictoriaMetrics anywhere near it.
 */
export function composeFleetHealth(queues: Map<string, QueueState>, rates: LoadRates): FleetHealth {
  return {
    for(machine: MachineTarget): MachineHealth {
      const queue = queueForMachine(machine);
      const p =
        queue === undefined && modeOf(machine) !== 'fleet' && machine.actor !== ''
          ? unnamedQueue(machine)
          : // The controller's own hostname, because a `local` node calls itself `localhost` while
            // its Worker identifies by the host's name. Resolved HERE and not in `pollers.ts`: that
            // module is in the browser bundle too, and a `node:os` import there fails the SPA build.
            // See `selfHostName` for why it is not simply `os.hostname()`.
            pollerFor(
              machine.machine,
              queue === undefined ? undefined : queues.get(queue),
              selfHostName()
            );
      // The three addresses vmagent's `instance` label might carry — see `instanceKeyFor`.
      const l = loadsFor(
        { machine: machine.machine, host: machine.host, publicIp: machine.publicIp },
        rates
      );
      const health: MachineHealth = { poller: p.poller, loads: l.loads };
      const detail = joinDetails([p.detail, loadsDetail(machine, l)]);
      if (detail !== undefined) health.detail = detail;
      return health;
    },
  };
}

/**
 * The `loads` SENTENCE, which a node outside the fleet does not get — while its VERDICT always rides.
 *
 * "could not ask VictoriaMetrics about localhost's load health: fetch failed" is not a finding about
 * localhost. It is a restatement of the mode: `infra/programs/machine.ts` installs and enables
 * `kontra-vmagent.service` on FLEET Machines and nowhere else — no compose service, no `kontra
 * serve`, and no worker image scrapes an actor host's :9110 — so nothing has ever remote-written a
 * series for a local or a docker Worker, and nothing ever will until something installs vmagent
 * there. Printing that on every local tile of every node is the noise that trains an operator to
 * stop reading the health row, and the Dashboard now says the same thing once, as `n/a`, with the
 * reason on hover.
 *
 * THE VERDICT IS NEVER SUPPRESSED, only the prose, and the difference is the whole safety argument:
 * `loadsFor` still runs and `failing` still rides the wire, so if a series ever does exist for one of
 * these nodes — someone ran vmagent by hand — the round-3 shape is still caught. What is dropped is
 * one sentence, in the one case where it can only ever say "there was nothing to ask".
 */
function loadsDetail(m: MachineTarget, l: { loads: TerminalHealth['loads']; detail?: string }): string | undefined {
  if (modeOf(m) === 'fleet' || l.loads !== 'unknown') return l.detail;
  return undefined;
}

/**
 * The `poller` verdict for a node whose actor is known but whose VERSION is not.
 *
 * `pollerFor(machine, undefined)` says "the stack carries no actor placement", which is true of a
 * fleet Machine nobody deployed to and false of a local session — `kontra-webcrawl` names the actor
 * and says nothing about its version, and the shared queue is `<actor>-<version>`. Describing
 * `<actor>-shared` instead would be a DIFFERENT queue: nothing polls it, so a perfectly healthy local
 * Worker would light up `poller: none`, which is the loudest false alarm this file can produce.
 * `unknown` with the reason is the honest answer, and `kontra workers list` is where the real one is.
 */
function unnamedQueue(m: MachineTarget): { poller: TerminalHealth['poller']; detail: string } {
  return {
    poller: 'unknown',
    detail:
      `a ${modeOf(m)} session names its actor (${m.actor}) but not its version, and the shared queue ` +
      `is <actor>-<version> — so Temporal cannot be asked about this Worker from here; ` +
      '`kontra workers list` can',
  };
}

/**
 * Several sentences in the one `detail` field the wire has.
 *
 * The contract gives `TerminalHealth` a single optional `detail`, and requires "one human sentence
 * per failing signal — never collapse signals". Both hold, because the signals themselves stay
 * separate in their own tri-state fields and it is only their PROSE that shares a field: a chip is
 * still coloured from `poller` or `loads`, never from this string. The separator is ' · ' because
 * `HealthChips.tsx` splits on it to put each sentence back beside the signal that produced it.
 *
 * DEFINED IN `probe.ts` SINCE THE PANE SIGNAL LANDED, and re-exported here because this is where it
 * is documented and where every test looks for it. `probe.ts` now joins the pane's own sentence onto
 * the Machine's, and it cannot import this file — `health.ts` imports `probe.ts`, and a cycle
 * between them would be resolved differently by vitest and by the built bundle.
 */
export { DETAIL_SEPARATOR, joinDetails } from './probe';

/**
 * Fold this file's sentences into a `ProbeResult`, so the existing `terminalsForMachine` needs no
 * change to carry them.
 *
 * SSH's sentence comes first on purpose. When a Machine is unreachable, "nothing is polling" and
 * "no metrics series" are both consequences of that one fact, and an operator reading the tile
 * should meet the cause before its two symptoms.
 */
export function mergeDetail(probe: ProbeResult, health: MachineHealth): ProbeResult {
  const detail = joinDetails([probe.detail, health.detail]);
  if (detail === undefined) return probe;
  return { ...probe, detail };
}

/**
 * THE INTEGRATION ENTRY POINT. One call replaces `terminalsForMachine(m, probe)` in
 * `server.ts:refresh()` and carries all four signals.
 */
export function terminalsWithHealth(
  m: MachineTarget,
  probe: ProbeResult,
  fleet: FleetHealth
): Terminal[] {
  const health = fleet.for(m);
  return terminalsForMachine(m, mergeDetail(probe, health), health.poller, health.loads);
}

/**
 * THE OTHER HALF OF THE ENTRY POINT. Measure the fleet, or report all-unknown when this streamer has
 * no probe — so the caller never needs a null check and never has a branch where an unmeasured
 * signal could default to something else.
 */
export async function measureFleetHealth(
  probe: FleetHealthProbe | undefined,
  machines: readonly MachineTarget[]
): Promise<FleetHealth> {
  if (!probe) return UNKNOWN_FLEET_HEALTH;
  try {
    return await probe.measure(machines);
  } catch {
    // A probe that throws is a wall of `unknown` chips, never a wall of missing tiles. The message
    // is dropped here rather than logged because `fleetHealthProbe` already turns every real failure
    // into a per-signal sentence; reaching this line at all is a bug in that, and the chips will say
    // `unknown` either way.
    return UNKNOWN_FLEET_HEALTH;
  }
}

export interface FleetHealthProbeDeps {
  /** Absent ⇒ `poller` stays `unknown` for every Machine, with a sentence saying so. */
  describer?: QueueDescriber;
  /** Absent ⇒ `loads` stays `unknown` for every Machine, with a sentence saying so. */
  metrics?: MetricsQuerier;
  pollerTimeoutMs?: number;
  metricsTimeoutMs?: number;
  log?(line: string, extra?: Record<string, unknown>): void;
}

/**
 * The probe, over injected seams. Tests construct this with fakes and never dial anything.
 */
export function fleetHealthProbe(deps: FleetHealthProbeDeps): FleetHealthProbe {
  const pollerTimeoutMs = deps.pollerTimeoutMs ?? DEFAULT_POLLER_TIMEOUT_MS;

  return {
    async measure(machines: readonly MachineTarget[]): Promise<FleetHealth> {
      // Concurrent and separately bounded: the two answers are independent, and serialising them
      // would make the slower system decide how long the round takes.
      const [queues, rates] = await Promise.all([
        deps.describer
          ? withTimeout(
              describeFleetQueues(deps.describer, machines),
              pollerTimeoutMs,
              `Temporal did not answer within ${pollerTimeoutMs}ms`
            ).catch((err: unknown) => queuesAllUnknown(machines, String((err as Error)?.message ?? err)))
          : Promise.resolve(new Map<string, QueueState>()),
        deps.metrics
          ? withTimeout(
              loadRates(deps.metrics),
              deps.metricsTimeoutMs ?? pollerTimeoutMs,
              `VictoriaMetrics did not answer within ${deps.metricsTimeoutMs ?? pollerTimeoutMs}ms`
            ).catch((err: unknown) => ({
              byInstance: new Map(),
              error: String((err as Error)?.message ?? err),
            }))
          : Promise.resolve<LoadRates>({
              byInstance: new Map(),
              error: 'no metrics endpoint configured for this streamer',
            }),
      ]);
      return composeFleetHealth(queues, rates);
    },
    async close(): Promise<void> {
      if (deps.describer) await deps.describer.close().catch(() => undefined);
    },
  };
}

/** Every queue this round needed, marked unknown with one reason — what a describe TIMEOUT means,
 * as opposed to a describe error, which `describeQueue` already reports per queue. */
function queuesAllUnknown(
  machines: readonly MachineTarget[],
  error: string
): Map<string, QueueState> {
  const out = new Map<string, QueueState>();
  for (const m of machines) {
    const queue = queueForMachine(m);
    if (queue !== undefined) out.set(queue, { queue, identities: [], workers: [], lastPoll: 0, error });
  }
  return out;
}

/**
 * Bound a promise.
 *
 * The timer is cleared on both paths: a `setTimeout` left pending here would hold the event loop
 * open for the length of every round, and the streamer is a long-lived forked child whose exit is
 * how the supervisor notices it died.
 */
export async function withTimeout<T>(p: Promise<T>, ms: number, message: string): Promise<T> {
  let timer: NodeJS.Timeout | undefined;
  try {
    return await Promise.race([
      p,
      new Promise<never>((_resolve, reject) => {
        timer = setTimeout(() => reject(new Error(message)), ms);
      }),
    ]);
  } finally {
    if (timer) clearTimeout(timer);
  }
}

/**
 * The real probe: a lazily-connected Temporal describer and a VictoriaMetrics querier.
 *
 * Both endpoints are configurable and both fail to `unknown`, so this is safe to wire
 * unconditionally — a streamer running with no Temporal and no metrics backend shows eight chips of
 * `unknown` with eight sentences explaining why, which is the state it is actually in.
 */
export function realFleetHealthProbe(options?: {
  temporalAddress?: string;
  namespace?: string;
  metricsUrl?: string;
  log?(line: string, extra?: Record<string, unknown>): void;
}): FleetHealthProbe {
  return fleetHealthProbe({
    describer: temporalQueueDescriber({
      address: options?.temporalAddress,
      namespace: options?.namespace,
    }),
    metrics: victoriaMetricsQuerier({ baseUrl: options?.metricsUrl }),
    pollerTimeoutMs: intEnv('KONTRA_PANEL_POLLER_MS', DEFAULT_POLLER_TIMEOUT_MS),
    log: options?.log,
  });
}

function intEnv(name: string, fallback: number): number {
  const raw = process.env[name];
  if (!raw) return fallback;
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : fallback;
}
