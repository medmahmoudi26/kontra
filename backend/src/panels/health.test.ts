import { describe, expect, it } from 'vitest';
import type { MachineTarget } from './discovery';
import type { ProbeResult } from './probe';
import {
  composeFleetHealth,
  DETAIL_SEPARATOR,
  fleetHealthProbe,
  joinDetails,
  measureFleetHealth,
  mergeDetail,
  terminalsWithHealth,
  UNKNOWN_FLEET_HEALTH,
  withTimeout,
} from './health';
import {
  BATCH_RATE_QUERY,
  ISOLATED_QUERY,
  RELOAD_RATE_QUERY,
  type LoadRates,
  type MetricSample,
  type MetricsQuerier,
} from './metrics';
import type { PollerInfo, QueueDescriber, QueueState, TaskQueueType } from './pollers';

const MACHINE: MachineTarget = {
  machine: 'kf-crawl-01',
  host: '10.124.0.9',
  publicIp: '203.0.113.9',
  tag: 'crawl',
  fleet: 'run-apex-119',
  actor: 'webcrawl',
  version: '0.2.0',
  session: 'kontra-webcrawl',
  windows: ['actor', 'handler'],
};

const PRESENT: ProbeResult = { reachable: 'ok', session: 'present', windows: ['actor', 'handler'] };

function livePoller(machine: string): PollerInfo {
  return { identity: `42@${machine}@webcrawl-0.2.0`, lastAccess: 1_700_000_000_000 };
}

function fakeDescriber(byQueue: Record<string, PollerInfo[] | Error>): QueueDescriber {
  return {
    async pollers(queue: string, _type: TaskQueueType) {
      const a = byQueue[queue] ?? [];
      if (a instanceof Error) throw a;
      return a;
    },
    async close() {},
  };
}

function fakeMetrics(byQuery: Record<string, MetricSample[] | Error>): MetricsQuerier {
  return {
    async query(promql) {
      const a = byQuery[promql] ?? [];
      if (a instanceof Error) throw a;
      return a;
    },
  };
}

const rateSample = (instance: string, value: number): MetricSample => ({
  labels: { instance, actor: 'webcrawl', tag: 'crawl' },
  value,
});

const okQueue = (machine: string): Map<string, QueueState> =>
  new Map([
    [
      'webcrawl-0.2.0',
      {
        queue: 'webcrawl-0.2.0',
        identities: [livePoller(machine).identity],
        workers: [{ identity: livePoller(machine).identity, lastPoll: 1 }],
        lastPoll: 1,
      },
    ],
  ]);

/** A Machine whose loads are genuinely OK: units counted (so there is a denominator), a ratio of
 * zero, and nothing isolated. Note the batch rate — `metrics.ts` documents that nothing increments
 * `kontra_batches_total` today, so this is the shape of a FIXED fleet, not the current one. */
const okRates = (machine: string): LoadRates => ({
  byInstance: new Map([[machine, { isolated: 0, reloads: 0, batches: 0.3, ratio: 0 }]]),
});

/** The reading every Machine really gives today: no isolated units and no denominator. */
const todaysRates = (machine: string): LoadRates => ({
  byInstance: new Map([[machine, { isolated: 0, reloads: 0, batches: 0, ratio: null }]]),
});

describe('composeFleetHealth', () => {
  it('fills the two signals slice 1 left unknown', () => {
    const fleet = composeFleetHealth(okQueue('kf-crawl-01'), okRates('kf-crawl-01'));
    expect(fleet.for(MACHINE)).toEqual({ poller: 'live', loads: 'ok' });
  });

  it('keeps the two signals independent — one failing does not touch the other', () => {
    const fleet = composeFleetHealth(okQueue('kf-crawl-01'), {
      byInstance: new Map([
        ['kf-crawl-01', { isolated: 0, reloads: 0.27, batches: 0.273, ratio: 0.98 }],
      ]),
    });
    const h = fleet.for(MACHINE);
    expect(h.poller).toBe('live');
    expect(h.loads).toBe('failing');
    expect(h.detail).toContain('98%');
  });

  it('reports today’s real fleet reading as unknown, with the reason, not as ok', () => {
    // The measurement that shaped this slice: nothing increments `kontra_batches_total`, so a
    // Machine with a live poller and a present session still cannot have its loads called healthy.
    const fleet = composeFleetHealth(okQueue('kf-crawl-01'), todaysRates('kf-crawl-01'));
    const h = fleet.for(MACHINE);
    expect(h.poller).toBe('live');
    expect(h.loads).toBe('unknown');
    expect(h.loads).not.toBe('ok');
    expect(h.detail).toContain('kontra_batches_total is 0');
  });

  it('reports isolated units as failing — the one branch that works on both actor hosts', () => {
    const fleet = composeFleetHealth(okQueue('kf-crawl-01'), {
      byInstance: new Map([['kf-crawl-01', { isolated: 7, reloads: 0, batches: 0, ratio: null }]]),
    });
    const h = fleet.for(MACHINE);
    expect(h.loads).toBe('failing');
    expect(h.detail).toContain('permanently dropped 7 unit(s)');
  });

  it('carries a sentence per failing signal, both of them', () => {
    const fleet = composeFleetHealth(
      new Map([['webcrawl-0.2.0', { queue: 'webcrawl-0.2.0', identities: [], lastPoll: 0 }]]),
      { byInstance: new Map(), error: 'connect ECONNREFUSED' }
    );
    const h = fleet.for(MACHINE);
    expect(h.poller).toBe('none');
    expect(h.loads).toBe('unknown');
    // Four signals are never collapsed: two failures produce two sentences, and the separator is
    // what lets HealthChips put each back beside the signal that produced it.
    expect(h.detail?.split(DETAIL_SEPARATOR)).toHaveLength(2);
    expect(h.detail).toContain('nothing is polling');
    expect(h.detail).toContain('VictoriaMetrics');
  });

  it('reports unknown for a Machine with no placement, and does not invent a queue', () => {
    const fleet = composeFleetHealth(new Map(), { byInstance: new Map() });
    const h = fleet.for({ ...MACHINE, actor: '', version: '' });
    expect(h.poller).toBe('unknown');
    expect(h.loads).toBe('unknown');
  });

  /**
   * "VictoriaMetrics did not answer about localhost" is not a finding about localhost.
   *
   * `infra/programs/machine.ts` installs `kontra-vmagent.service` on FLEET Machines and nowhere
   * else, so no series has ever existed for a local or a docker Worker. That sentence printed on
   * every non-fleet tile, forever, beside a chip promising a reading that never arrives.
   */
  it('does not tell a local or docker node that a metrics backend it never had is missing', () => {
    const rates: LoadRates = { byInstance: new Map(), error: 'fetch failed' };
    const fleet = composeFleetHealth(okQueue('localhost'), rates);
    const local = fleet.for({ ...MACHINE, mode: 'local', machine: 'localhost', host: 'localhost' });
    expect(local.loads).toBe('unknown');
    expect(local.detail ?? '').not.toContain('VictoriaMetrics');

    const docker = fleet.for({ ...MACHINE, mode: 'docker', machine: 'w1', host: 'w1' });
    expect(docker.detail ?? '').not.toContain('VictoriaMetrics');

    // THE FLEET STILL GETS IT. There a missing answer IS a finding: vmagent is supposed to be
    // running on that Machine, so silence from it is something to go and look at.
    const machine = fleet.for(MACHINE);
    expect(machine.loads).toBe('unknown');
    expect(machine.detail).toContain('VictoriaMetrics');
  });

  it('never suppresses a FAILING loads verdict, whatever the mode', () => {
    // The safety argument for the rule above: only the sentence is conditional, never the verdict.
    // If a series does exist for a local node — somebody ran vmagent by hand — the round-3 shape is
    // still caught and still speaks.
    const fleet = composeFleetHealth(okQueue('localhost'), {
      byInstance: new Map([['localhost', { isolated: 7, reloads: 0, batches: 0, ratio: null }]]),
    });
    const local = fleet.for({ ...MACHINE, mode: 'local', machine: 'localhost', host: 'localhost' });
    expect(local.loads).toBe('failing');
    expect(local.detail).toContain('permanently dropped 7 unit(s)');
  });
});

describe('detail composition', () => {
  it('joins only the sentences that exist', () => {
    expect(joinDetails([undefined, undefined])).toBeUndefined();
    expect(joinDetails(['a', undefined])).toBe('a');
    expect(joinDetails([undefined, 'b'])).toBe('b');
    expect(joinDetails(['a', '', 'b'])).toBe(`a${DETAIL_SEPARATOR}b`);
  });

  it('puts the SSH sentence before its two symptoms', () => {
    // An unreachable Machine has no pollers and no metrics BECAUSE it is unreachable. The cause
    // reads first or the tile blames the wrong system.
    const probe: ProbeResult = {
      reachable: 'fail',
      session: 'unknown',
      windows: [],
      detail: 'ssh to kf-crawl-01 failed: timeout',
    };
    const merged = mergeDetail(probe, {
      poller: 'none',
      loads: 'unknown',
      detail: 'nothing is polling',
    });
    expect(merged.detail?.startsWith('ssh to kf-crawl-01 failed')).toBe(true);
  });

  it('leaves a clean probe object untouched when there is nothing to add', () => {
    const merged = mergeDetail(PRESENT, { poller: 'live', loads: 'ok' });
    expect(merged).toBe(PRESENT);
  });
});

describe('terminalsWithHealth — the integration entry point', () => {
  it('puts all five signals on every window of the Machine', () => {
    const fleet = composeFleetHealth(okQueue('kf-crawl-01'), okRates('kf-crawl-01'));
    const terminals = terminalsWithHealth(MACHINE, PRESENT, fleet);

    expect(terminals.map((t) => t.window)).toEqual(['actor', 'handler']);
    for (const t of terminals) {
      expect(t.health).toEqual({
        reachable: 'ok',
        session: 'present',
        // The fifth. `unknown` here because this probe result carries no pane facts — which is the
        // honest value for a signal nothing measured, and the same rule the other four follow.
        process: 'unknown',
        poller: 'live',
        loads: 'ok',
      });
    }
  });

  it('keeps unknown out of ok when nothing has been measured', () => {
    const terminals = terminalsWithHealth(MACHINE, PRESENT, UNKNOWN_FLEET_HEALTH);
    for (const t of terminals) {
      expect(t.health.poller).toBe('unknown');
      expect(t.health.loads).toBe('unknown');
      // The two signals the probe DID measure are untouched by the two it did not.
      expect(t.health.reachable).toBe('ok');
      expect(t.health.session).toBe('present');
    }
  });

  it('renders the round-3 Machine as failing loads WHILE its session is present', () => {
    // Acceptance criterion 6, exactly: the watchdog chip is red while the pane still scrolls.
    const fleet = composeFleetHealth(okQueue('kf-crawl-01'), {
      byInstance: new Map([
        ['kf-crawl-01', { isolated: 0, reloads: 0.27, batches: 0.273, ratio: 0.98 }],
      ]),
    });
    const [actor] = terminalsWithHealth(MACHINE, PRESENT, fleet);
    expect(actor?.health.session).toBe('present');
    expect(actor?.health.reachable).toBe('ok');
    expect(actor?.health.loads).toBe('failing');
    expect(actor?.health.detail).toContain('round-3 signature');
  });
});

describe('measureFleetHealth', () => {
  it('is all-unknown when the streamer has no health probe', async () => {
    const fleet = await measureFleetHealth(undefined, [MACHINE]);
    expect(fleet.for(MACHINE)).toEqual({ poller: 'unknown', loads: 'unknown' });
  });

  it('is all-unknown, not a throw, when the probe itself throws', async () => {
    const fleet = await measureFleetHealth(
      {
        measure() {
          throw new Error('boom');
        },
      },
      [MACHINE]
    );
    expect(fleet.for(MACHINE).poller).toBe('unknown');
  });
});

describe('fleetHealthProbe', () => {
  it('measures both systems and never dials anything in a test', async () => {
    const probe = fleetHealthProbe({
      describer: fakeDescriber({ 'webcrawl-0.2.0': [livePoller('kf-crawl-01')] }),
      metrics: fakeMetrics({
        [ISOLATED_QUERY]: [rateSample('kf-crawl-01', 0)],
        [RELOAD_RATE_QUERY]: [rateSample('kf-crawl-01', 0)],
        [BATCH_RATE_QUERY]: [rateSample('kf-crawl-01', 0.3)],
      }),
    });
    const fleet = await probe.measure([MACHINE]);
    expect(fleet.for(MACHINE)).toEqual({ poller: 'live', loads: 'ok' });
  });

  it('reports unknown for the half that is not configured, and measures the other', async () => {
    const pollerOnly = fleetHealthProbe({
      describer: fakeDescriber({ 'webcrawl-0.2.0': [livePoller('kf-crawl-01')] }),
    });
    const h = await (await pollerOnly.measure([MACHINE])).for(MACHINE);
    expect(h.poller).toBe('live');
    expect(h.loads).toBe('unknown');
    expect(h.detail).toContain('no metrics endpoint configured');
  });

  it('does not let one wedged system delay or hide the other', async () => {
    // The constraint that matters most: this runs inside the discovery round. A Temporal that never
    // answers must cost the `poller` chip and nothing else.
    const probe = fleetHealthProbe({
      describer: {
        pollers: () => new Promise<PollerInfo[]>(() => {}), // never settles
        close: async () => {},
      },
      metrics: fakeMetrics({
        [ISOLATED_QUERY]: [rateSample('kf-crawl-01', 0)],
        [RELOAD_RATE_QUERY]: [rateSample('kf-crawl-01', 0)],
        [BATCH_RATE_QUERY]: [rateSample('kf-crawl-01', 0.3)],
      }),
      pollerTimeoutMs: 10,
    });

    const started = Date.now();
    const h = (await probe.measure([MACHINE])).for(MACHINE);
    expect(Date.now() - started).toBeLessThan(2000);
    expect(h.poller).toBe('unknown');
    expect(h.detail).toContain('did not answer within 10ms');
    // The half that answered is still a real measurement.
    expect(h.loads).toBe('ok');
  });

  it('turns a Temporal timeout into unknown, never none', async () => {
    const probe = fleetHealthProbe({
      describer: { pollers: () => new Promise<PollerInfo[]>(() => {}), close: async () => {} },
      pollerTimeoutMs: 5,
    });
    const h = (await probe.measure([MACHINE])).for(MACHINE);
    expect(h.poller).toBe('unknown');
    expect(h.poller).not.toBe('none');
  });

  it('turns a metrics timeout into unknown, never ok', async () => {
    const probe = fleetHealthProbe({
      metrics: { query: () => new Promise<MetricSample[]>(() => {}) },
      metricsTimeoutMs: 5,
    });
    const h = (await probe.measure([MACHINE])).for(MACHINE);
    expect(h.loads).toBe('unknown');
    expect(h.loads).not.toBe('ok');
  });
});

describe('withTimeout', () => {
  it('resolves through and clears its timer', async () => {
    await expect(withTimeout(Promise.resolve(7), 1000, 'late')).resolves.toBe(7);
  });

  it('rejects with its own message when the promise never settles', async () => {
    await expect(withTimeout(new Promise(() => {}), 5, 'late')).rejects.toThrow('late');
  });

  it('propagates the underlying rejection rather than masking it as a timeout', async () => {
    await expect(withTimeout(Promise.reject(new Error('real')), 1000, 'late')).rejects.toThrow(
      'real'
    );
  });
});
