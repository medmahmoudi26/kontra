/**
 * The `loads` signal — and the measurement that changed what it could honestly be.
 *
 * THE FAILURE THIS IS FOR. In round 3 one Machine failed 81 of 82 resource loads and another 19 of
 * 20 while every peer sat at zero, each silently eating about an eighth of a sweep, and the runs
 * still reported `completed`. The process never dies, so systemd's `Restart=` never fires.
 *
 * WHAT IS ACTUALLY EXPORTED — read, not assumed. `runtime/python/internals/metrics.py` and its
 * byte-for-byte Go peer `runtime/go/engine/metrics.go` serve three counters on :9110,
 * which vmagent's `kontra-actor` scrape job in `machine.ts` collects and remote-writes with
 * `instance` / `actor` / `role` stamped on. But EXPORTED IS NOT THE SAME AS INCREMENTED, and the
 * call sites are where this signal's design was decided:
 *
 *   kontra_isolated_units_total   INCREMENTED ON BOTH HOSTS.
 *                                 python `internals/engine.py:273`, go `internal/engine/engine.go:774`.
 *                                 "NON-ZERO MEANS THE RUN LOST WORK" — its own HELP text.
 *   kontra_resource_reloads_total INCREMENTED ON THE GO HOST ONLY (`engine.go:469`).
 *                                 `count_reload()` in metrics.py has NO CALLER anywhere in the repo,
 *                                 so on a python actor host this counter is a permanent 0 — and the
 *                                 crawler that caused round 3 is a python actor, which is the reason
 *                                 metrics.py's own header gives for existing.
 *   kontra_batches_total          INCREMENTED NOWHERE. `countBatch()` (go) and `count_batch()`
 *                                 (python) are both defined, both exercised only by
 *                                 `metrics_test.go`, and called by no production code path.
 *
 * SO THE RATIO IN THE DASHBOARD CANNOT WORK, AND THE CHIP MUST NOT PRETEND OTHERWISE.
 * `infra/observability/dashboards/kontra-fleet.json`'s panel "Sick-worker signature — reload ratio
 * per instance" plots `rate(reloads) / clamp_min(rate(batches), 0.001)`. With the denominator pinned
 * at zero that expression is not a ratio: on a go host it is `1000 x reloads`, which clamps to the
 * panel's `max: 1` and shows red for a single ordinary reload (`maxUnitReloads = 2` makes reloads
 * routine); on a python host it is a constant 0, which shows green forever. The 81/82 and 19/20
 * figures in that panel's description are real, but they were measured under the Ansible/Alloy
 * instrumentation this code replaced — they are not evidence that this expression works now.
 *
 * WHAT THIS FILE THEREFORE DOES. `loads` is `failing` only from evidence that is genuinely
 * incremented today, and `unknown` — never `ok` — whenever the reading is the permanently-zero
 * denominator:
 *
 *   failing   isolated units in the window, on either host: the run lost work, verifiably wired.
 *   failing   the reload ratio at or over the threshold, IF a real denominator exists.
 *   unknown   no denominator (`kontra_batches_total` flat at 0) — with a sentence naming the
 *             missing call site, so the chip explains its own blindness instead of hiding it.
 *   unknown   query error, non-200, absent series, NaN.
 *
 * Consequence, stated plainly rather than buried: until one line is added to each actor host, this
 * chip reports `failing` or `unknown` and never `ok`. That is the correct behaviour — a green chip
 * derived from a counter nothing increments would be worse than no chip, because it is the exact
 * "reported completed while losing an eighth of a sweep" shape one layer up.
 */

import type { TerminalHealth } from './types';

/** In-compose. `victoriametrics:8428` serves `/api/v1/write` for the fleet's push and
 * `/api/v1/query` for this. */
export const DEFAULT_METRICS_URL = 'http://victoriametrics:8428';

/** Short on purpose: this runs inside the discovery round, and a wedged metrics backend must cost
 * the `loads` chip, never the wall. */
export const DEFAULT_METRICS_TIMEOUT_MS = 2000;

/**
 * The ratio threshold, from the dashboard panel's own red step (`{ "color": "red", "value": 0.5 }`).
 *
 * Wide on purpose and the width is measured: the sick Machines ran above 0.9 and every healthy peer
 * sat at 0, so anything in between is noise. The **Warden** draws its own line at 0.8 before it
 * restarts a Worker (`shared/conformance/workerhealth.json`, inherited from the retired watchdog's
 * threshold) — a chip that only informs can afford to speak earlier than one that reboots
 * something.
 */
export const FAILING_RATIO = 0.5;

/** Any isolated unit at all is a failure: the HELP text is "NON-ZERO MEANS THE RUN LOST WORK", and
 * the dashboard's stat panel turns red at 1. The epsilon is because `increase()` extrapolates, so a
 * single increment can come back as 0.8 or 1.2 rather than exactly 1. */
export const ISOLATED_EPSILON = 0.01;

/** The rate window, matching the dashboard's `[5m]`. Longer than vmagent's 30 s scrape by enough
 * that one missed scrape does not move the answer. */
export const RATE_WINDOW = '5m';

/** The one counter incremented by BOTH actor hosts. This is what makes `failing` reachable today. */
export const ISOLATED_QUERY = `sum by (instance) (increase(kontra_isolated_units_total[${RATE_WINDOW}]))`;
/** Go host only — see the header. */
export const RELOAD_RATE_QUERY = `sum by (instance) (rate(kontra_resource_reloads_total[${RATE_WINDOW}]))`;
/** Incremented nowhere today. Queried anyway, because the day someone wires `count_batch()` this
 * signal starts working with no change here — and because a zero read from it is what the `unknown`
 * sentence cites. */
export const BATCH_RATE_QUERY = `sum by (instance) (rate(kontra_batches_total[${RATE_WINDOW}]))`;

/**
 * The dashboard's single expression, kept verbatim as the provenance of the two rate queries above
 * and pinned by a test. NOT used to query: see the header on why its denominator does not exist.
 */
export const DASHBOARD_RATIO_QUERY =
  'sum by (instance) (rate(kontra_resource_reloads_total[5m])) / ' +
  'clamp_min(sum by (instance) (rate(kontra_batches_total[5m])), 0.001)';

export interface MetricSample {
  labels: Record<string, string>;
  value: number;
}

/** The VictoriaMetrics seam — an interface so tests never dial anything. Throws on failure; the
 * caller turns a throw into `unknown`. */
export interface MetricsQuerier {
  query(promql: string): Promise<MetricSample[]>;
}

/** One instance's three readings. */
export interface LoadReading {
  /** Units permanently dropped in the window. Extrapolated by `increase()`. */
  isolated: number;
  /** Resource reloads per second. Always 0 on a python actor host. */
  reloads: number;
  /** Batches per second. Always 0 today — nothing increments the counter. */
  batches: number;
  /** reloads/batches, or `null` when there is no denominator to divide by. `null` is the value that
   * keeps a missing denominator from becoming a zero ratio, which would read as healthy. */
  ratio: number | null;
}

export interface LoadRates {
  byInstance: Map<string, LoadReading>;
  /** Set when the queries could not be answered. Its presence makes every Machine `unknown`. */
  error?: string;
}

/**
 * reloads/batches, or `null` when the denominator is absent.
 *
 * Deliberately NOT the dashboard's `clamp_min(batches, 0.001)`. That clamp turns "we have no
 * denominator" into "the denominator is a thousandth", which manufactures a ratio of 1000x the
 * numerator out of no evidence — fine as a visual hint on a graph an operator is interpreting, wrong
 * as the input to a tri-state chip that has an honest third value available.
 */
export function ratioOf(reloads: number, batches: number): number | null {
  if (!Number.isFinite(reloads) || !Number.isFinite(batches)) return null;
  if (batches <= 0) return null;
  return reloads / batches;
}

/**
 * All three vectors for the whole fleet — THREE queries per discovery round, not three per Machine.
 *
 * Never throws: a `loads` chip able to take down the discovery loop would be worse than no chip.
 *
 * An instance is present in the result if ANY vector mentions it, and `kontra_isolated_units_total`
 * is the reliable one: `metrics.py` emits it explicitly at zero rather than omitting it, "so `rate()`
 * over nothing does not render identically to no data". Presence therefore means this Machine's
 * vmagent is reporting; absence means it is not, which is `unknown`.
 */
export async function loadRates(q: MetricsQuerier): Promise<LoadRates> {
  let isolated: MetricSample[];
  let reloads: MetricSample[];
  let batches: MetricSample[];
  try {
    [isolated, reloads, batches] = await Promise.all([
      q.query(ISOLATED_QUERY),
      q.query(RELOAD_RATE_QUERY),
      q.query(BATCH_RATE_QUERY),
    ]);
  } catch (err) {
    return { byInstance: new Map(), error: (err as Error)?.message || String(err) };
  }

  const index = (samples: MetricSample[]): Map<string, number> => {
    const m = new Map<string, number>();
    for (const s of samples) {
      const instance = s.labels.instance;
      if (instance) m.set(instance, s.value);
    }
    return m;
  };
  const iso = index(isolated);
  const rel = index(reloads);
  const bat = index(batches);

  const byInstance = new Map<string, LoadReading>();
  for (const instance of new Set([...iso.keys(), ...rel.keys(), ...bat.keys()])) {
    const r = rel.get(instance) ?? 0;
    const b = bat.get(instance) ?? 0;
    byInstance.set(instance, {
      isolated: iso.get(instance) ?? 0,
      reloads: r,
      batches: b,
      ratio: ratioOf(r, b),
    });
  }
  return { byInstance };
}

export interface LoadsVerdict {
  loads: TerminalHealth['loads'];
  detail?: string;
}

/**
 * The addresses one Machine might be labelled by. A structural subset of `MachineTarget`, so this
 * file does not depend on discovery.
 */
export interface LoadTarget {
  machine: string;
  host?: string;
  publicIp?: string;
}

/**
 * Which `instance` label belongs to this Machine.
 *
 * THREE KEYS, BECAUSE THE LABEL IS NOT PROVEN TO CARRY ONE FORM. `machine.ts` passes
 * `-remoteWrite.label=instance=%H`, so the hostname — and `programs/fleet.ts` names droplets
 * `kf-<role>-NN` explicitly, DigitalOcean setting a droplet's hostname from its name, so that should
 * be the Machine name. But the dashboard panel this signal descends from describes real round-3
 * instances as "kf-m9 … and 10.124.0.21", one a hostname and one an ADDRESS, so the fleet has
 * demonstrably reported both forms. No Machine exists in this environment to settle which this
 * config produces.
 *
 * All three are tried and none of them is a loopback: if the remote-write label ever fails to
 * override the scrape target's own `instance` (`127.0.0.1:9110`, identical on every Machine), no
 * lookup matches, every chip says `unknown`, and the wall says "we cannot attribute this" rather
 * than merging the whole fleet into one series. That is the failure mode to have.
 */
export function instanceKeyFor(target: LoadTarget, rates: LoadRates): string | undefined {
  for (const key of [target.machine, target.host, target.publicIp]) {
    if (key && rates.byInstance.has(key)) return key;
  }
  return undefined;
}

/** Percent for a sentence a human reads. */
function pct(ratio: number): string {
  return `${Math.round(ratio * 100)}%`;
}

/** Per minute, because "3.2 reloads/min" is a rate an operator can hold in their head and
 * "0.053/s" is not. */
function perMin(rate: number): string {
  const v = rate * 60;
  return v >= 10 ? v.toFixed(0) : v.toFixed(1);
}

/**
 * One Machine's verdict.
 *
 * The branch order is the contract, and the last two branches are the point of this whole file:
 * a reading that cannot distinguish a healthy Worker from an uninstrumented one is `unknown`.
 */
export function loadsFor(target: LoadTarget | string, rates: LoadRates): LoadsVerdict {
  const t: LoadTarget = typeof target === 'string' ? { machine: target } : target;
  const machine = t.machine;

  if (rates.error !== undefined) {
    return {
      loads: 'unknown',
      detail: `could not ask VictoriaMetrics about ${machine}'s load health: ${rates.error}`,
    };
  }

  const key = instanceKeyFor(t, rates);
  const r = key === undefined ? undefined : rates.byInstance.get(key);
  if (!r) {
    return {
      loads: 'unknown',
      detail:
        `no kontra_isolated_units_total series for instance="${machine}" — its vmagent has not ` +
        `reported, or the actor host's metrics listener on :9110 is off`,
    };
  }

  // Work lost. The only one of the three counters both actor hosts increment, so this is the
  // branch that actually catches a sick Worker today.
  if (r.isolated > ISOLATED_EPSILON) {
    return {
      loads: 'failing',
      detail:
        `${machine} permanently dropped ${Math.round(r.isolated)} unit(s) in the last ${RATE_WINDOW} ` +
        `(kontra_isolated_units_total) — the run lost work, and it will still report completed`,
    };
  }

  if (r.ratio !== null && Number.isFinite(r.ratio)) {
    if (r.ratio >= FAILING_RATIO) {
      return {
        loads: 'failing',
        detail:
          `${machine} is reloading its actor resource on ${pct(r.ratio)} of batches ` +
          `(${perMin(r.reloads)}/min against ${perMin(r.batches)} batches/min over ${RATE_WINDOW}) — ` +
          `this is the round-3 signature, and a restart cleared it both times`,
      };
    }
    // The only path to `ok`: a real denominator, a ratio under the threshold, and no lost units.
    return { loads: 'ok' };
  }

  // No denominator. Reached whenever `kontra_batches_total` is flat, which is ALWAYS today — the
  // sentence names the missing call site rather than leaving an operator to wonder.
  if (r.reloads > 0) {
    return {
      loads: 'unknown',
      detail:
        `${machine} reports ${perMin(r.reloads)} resource reloads/min but kontra_batches_total is 0, ` +
        `so there is no denominator for the ratio — nothing in either actor host calls ` +
        `count_batch()/countBatch(), and one reload is routine (maxUnitReloads=2)`,
    };
  }
  return {
    loads: 'unknown',
    detail:
      `${machine} reports no isolated units, but kontra_batches_total is 0 and nothing increments ` +
      `it, so a healthy Worker and an uninstrumented one read identically here — this is not ok`,
  };
}

// --- the real querier ---------------------------------------------------------------------------

export interface VictoriaMetricsOptions {
  baseUrl?: string;
  timeoutMs?: number;
}

/** VictoriaMetrics' instant-query response, as much of it as this file reads. */
interface RawQueryResponse {
  status?: string;
  error?: string;
  errorType?: string;
  data?: {
    resultType?: string;
    result?: Array<{ metric?: Record<string, string>; value?: [number, string] }> | null;
  } | null;
}

/** Pure, so the wire shape is pinned by a test rather than by a running VictoriaMetrics. */
export function parseInstantQuery(body: RawQueryResponse): MetricSample[] {
  if (body.status === 'error') {
    throw new Error(body.error ?? body.errorType ?? 'query failed');
  }
  const out: MetricSample[] = [];
  for (const row of body.data?.result ?? []) {
    const raw = row.value?.[1];
    if (raw === undefined) continue;
    const value = Number(raw);
    // VictoriaMetrics renders a missing sample as the string "NaN". Dropping it here is what keeps
    // it from arriving as a numeric zero, which would read as a clean Worker.
    if (!Number.isFinite(value)) continue;
    out.push({ labels: row.metric ?? {}, value });
  }
  return out;
}

/**
 * The real querier. Configurable because the endpoint is a compose hostname in production and a
 * localhost port on an operator's laptop, and timed out because this call sits inside the discovery
 * round.
 */
export function victoriaMetricsQuerier(options?: VictoriaMetricsOptions): MetricsQuerier {
  const baseUrl = (
    options?.baseUrl ??
    process.env.KONTRA_PANEL_METRICS_URL ??
    DEFAULT_METRICS_URL
  ).replace(/\/+$/, '');
  const timeoutMs =
    options?.timeoutMs ?? intEnv('KONTRA_PANEL_METRICS_MS', DEFAULT_METRICS_TIMEOUT_MS);

  return {
    async query(promql: string): Promise<MetricSample[]> {
      const url = `${baseUrl}/api/v1/query?query=${encodeURIComponent(promql)}`;
      const res = await fetch(url, {
        signal: AbortSignal.timeout(timeoutMs),
        headers: { accept: 'application/json' },
      });
      if (!res.ok) {
        // The body is read for the message: VictoriaMetrics puts the PromQL parse error in there,
        // and "400" alone would send an operator to the wrong file.
        const text = await res.text().catch(() => '');
        throw new Error(`${res.status} ${res.statusText}${text ? `: ${text.slice(0, 200)}` : ''}`);
      }
      return parseInstantQuery((await res.json()) as RawQueryResponse);
    },
  };
}

function intEnv(name: string, fallback: number): number {
  const raw = process.env[name];
  if (!raw) return fallback;
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : fallback;
}
