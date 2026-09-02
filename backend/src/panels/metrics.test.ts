import { describe, expect, it } from 'vitest';
import {
  BATCH_RATE_QUERY,
  DASHBOARD_RATIO_QUERY,
  DEFAULT_METRICS_URL,
  FAILING_RATIO,
  instanceKeyFor,
  ISOLATED_QUERY,
  loadRates,
  loadsFor,
  parseInstantQuery,
  ratioOf,
  RELOAD_RATE_QUERY,
  type LoadRates,
  type MetricSample,
  type MetricsQuerier,
} from './metrics';

/** The VictoriaMetrics seam, faked. Nothing in this file dials 8428. */
function fakeQuerier(
  answers: Record<string, MetricSample[] | Error>
): MetricsQuerier & { asked: string[] } {
  const asked: string[] = [];
  return {
    asked,
    async query(promql) {
      asked.push(promql);
      const a = answers[promql] ?? [];
      if (a instanceof Error) throw a;
      return a;
    },
  };
}

const sample = (instance: string, value: number): MetricSample => ({
  // The labels vmagent stamps: `-remoteWrite.label=instance=%H,actor=…,role=…` in machine.ts.
  labels: { instance, actor: 'webcrawl', tag: 'crawl' },
  value,
});

/** What the fleet looks like TODAY: isolated units are wired on both hosts, so that series is real;
 * `kontra_batches_total` is incremented by no production code path, so it is flat at 0. */
function todaysFleet(
  over: { isolated?: number; reloads?: number; batches?: number } = {},
  instance = 'kf-crawl-01'
): Record<string, MetricSample[]> {
  return {
    [ISOLATED_QUERY]: [sample(instance, over.isolated ?? 0)],
    [RELOAD_RATE_QUERY]: [sample(instance, over.reloads ?? 0)],
    [BATCH_RATE_QUERY]: [sample(instance, over.batches ?? 0)],
  };
}

describe('the queries name metrics that exist, by the names the actor host exports', () => {
  it('asks for the three counters actorkit serves on :9110', () => {
    expect(ISOLATED_QUERY).toContain('kontra_isolated_units_total');
    expect(RELOAD_RATE_QUERY).toContain('kontra_resource_reloads_total');
    expect(BATCH_RATE_QUERY).toContain('kontra_batches_total');
  });

  it('groups by the label that makes a per-Machine failure attributable', () => {
    // Without `by (instance)` a sick Machine is averaged into its healthy peers, which is the
    // round-3 failure re-created in a query.
    for (const q of [ISOLATED_QUERY, RELOAD_RATE_QUERY, BATCH_RATE_QUERY]) {
      expect(q).toContain('sum by (instance)');
    }
  });

  it('keeps the dashboard expression verbatim as provenance, and does not use it', () => {
    // Copied from infra/observability/dashboards/kontra-fleet.json, panel "Sick-worker signature".
    // Pinned so that if someone fixes the panel, the mismatch shows up here.
    expect(DASHBOARD_RATIO_QUERY).toBe(
      'sum by (instance) (rate(kontra_resource_reloads_total[5m])) / ' +
        'clamp_min(sum by (instance) (rate(kontra_batches_total[5m])), 0.001)'
    );
    expect(DASHBOARD_RATIO_QUERY).toContain(RELOAD_RATE_QUERY);
    expect(DASHBOARD_RATIO_QUERY).toContain(BATCH_RATE_QUERY);
  });

  it('uses the same threshold as that panel red step', () => {
    expect(FAILING_RATIO).toBe(0.5);
  });

  it('defaults to the in-compose endpoint', () => {
    expect(DEFAULT_METRICS_URL).toBe('http://victoriametrics:8428');
  });
});

describe('ratioOf refuses to invent a denominator', () => {
  it('is the plain ratio when batches really were counted', () => {
    // The round-3 numbers, as runtime/go/engine/metrics_test.go pins them: 81 reloads over
    // 82 batches, expressed as per-second rates over a 5-minute window.
    expect(ratioOf(81 / 300, 82 / 300)).toBeCloseTo(81 / 82, 6);
    expect(ratioOf(0, 0.3)).toBe(0);
  });

  it('is null when the denominator is zero — NOT the dashboard clamp', () => {
    // `clamp_min(batches, 0.001)` turns "no denominator" into "a thousandth", manufacturing a ratio
    // of 1000x the numerator from no evidence. Acceptable as a hint on a graph a human reads; wrong
    // as the input to a chip that has an honest third state.
    expect(ratioOf(0.5, 0)).toBeNull();
    expect(ratioOf(0, 0)).toBeNull();
  });

  it('is null for a non-numeric input', () => {
    expect(ratioOf(Number.NaN, 1)).toBeNull();
    expect(ratioOf(1, Number.NaN)).toBeNull();
  });
});

describe('loadRates', () => {
  it('joins the three vectors by instance, in THREE queries for the whole fleet', async () => {
    const q = fakeQuerier({
      [ISOLATED_QUERY]: [sample('kf-crawl-01', 0), sample('kf-crawl-02', 3)],
      [RELOAD_RATE_QUERY]: [sample('kf-crawl-01', 0), sample('kf-crawl-02', 0.27)],
      [BATCH_RATE_QUERY]: [sample('kf-crawl-01', 0.3), sample('kf-crawl-02', 0.273)],
    });

    const rates = await loadRates(q);
    expect(q.asked.length).toBe(3);
    expect(rates.error).toBeUndefined();
    expect(rates.byInstance.get('kf-crawl-01')).toEqual({
      isolated: 0,
      reloads: 0,
      batches: 0.3,
      ratio: 0,
    });
    expect(rates.byInstance.get('kf-crawl-02')?.ratio).toBeCloseTo(0.27 / 0.273, 6);
  });

  it('includes an instance that appears in only one vector', async () => {
    // `kontra_isolated_units_total` is always emitted, explicitly at zero, so its presence is what
    // says "this Machine's vmagent is reporting".
    const rates = await loadRates(fakeQuerier({ [ISOLATED_QUERY]: [sample('kf-crawl-01', 0)] }));
    expect(rates.byInstance.get('kf-crawl-01')).toEqual({
      isolated: 0,
      reloads: 0,
      batches: 0,
      ratio: null,
    });
  });

  it('never throws — a query error becomes an error field, not a rejected round', async () => {
    const rates = await loadRates(
      fakeQuerier({ [ISOLATED_QUERY]: new Error('connect ECONNREFUSED') })
    );
    expect(rates.error).toContain('ECONNREFUSED');
    expect(rates.byInstance.size).toBe(0);
  });

  it('drops a sample with no instance label rather than keying it on empty', async () => {
    const rates = await loadRates(
      fakeQuerier({ [ISOLATED_QUERY]: [{ labels: { actor: 'webcrawl' }, value: 1 }] })
    );
    expect(rates.byInstance.size).toBe(0);
  });
});

describe('loadsFor: what the fleet can actually tell us today', () => {
  it('is FAILING when units were permanently dropped — the one signal both hosts wire', async () => {
    // python internals/engine.py:273 and go internal/engine/engine.go:774 both call this. It is the
    // branch that catches a sick Worker on the runtime that caused round 3.
    const rates = await loadRates(fakeQuerier(todaysFleet({ isolated: 11 })));
    const v = loadsFor('kf-crawl-01', rates);
    expect(v.loads).toBe('failing');
    expect(v.detail).toContain('permanently dropped 11 unit(s)');
    expect(v.detail).toContain('still report completed');
  });

  it('ignores extrapolation dust rather than reporting a phantom drop', () => {
    // `increase()` over a flat-zero counter can return a hair above 0 on some backends.
    const dust: LoadRates = {
      byInstance: new Map([['m', { isolated: 0.001, reloads: 0, batches: 0.3, ratio: 0 }]]),
    };
    expect(loadsFor('m', dust).loads).toBe('ok');
  });

  it('is UNKNOWN, never ok, when the batch denominator is flat at zero', async () => {
    // THE TRAP THIS FILE EXISTS TO AVOID. `count_batch()`/`countBatch()` are called by no production
    // code path in either host, so this is every Machine's real reading right now. A ratio computed
    // from it would be 0 on a python host — green forever, on the exact runtime that lost the work.
    const rates = await loadRates(fakeQuerier(todaysFleet()));
    const v = loadsFor('kf-crawl-01', rates);
    expect(v.loads).toBe('unknown');
    expect(v.loads).not.toBe('ok');
    expect(v.detail).toContain('kontra_batches_total is 0');
    expect(v.detail).toContain('this is not ok');
  });

  it('is UNKNOWN with the reload count named when reloads happen but nothing counts batches', async () => {
    // A go actor host: reloads are real, the denominator is not. One reload is routine
    // (maxUnitReloads=2), so this is not enough to call it failing — but the number belongs in the
    // sentence so an operator can compare it against a peer by eye.
    const rates = await loadRates(fakeQuerier(todaysFleet({ reloads: 0.27 })));
    const v = loadsFor('kf-crawl-01', rates);
    expect(v.loads).toBe('unknown');
    expect(v.detail).toContain('16 resource reloads/min');
    expect(v.detail).toContain('count_batch()/countBatch()');
  });

  it('is FAILING on the ratio once a denominator exists — the day someone wires count_batch', async () => {
    const rates = await loadRates(
      fakeQuerier(todaysFleet({ reloads: 81 / 300, batches: 82 / 300 }))
    );
    const v = loadsFor('kf-crawl-01', rates);
    expect(v.loads).toBe('failing');
    expect(v.detail).toContain('99%');
    expect(v.detail).toContain('round-3 signature');
  });

  it('is FAILING for the 19-of-20 peer too', async () => {
    const rates = await loadRates(fakeQuerier(todaysFleet({ reloads: 19 / 300, batches: 20 / 300 })));
    expect(loadsFor('kf-crawl-01', rates).loads).toBe('failing');
  });

  it('is OK only with a real denominator, a ratio under the threshold, and no lost units', () => {
    const healthy: LoadRates = {
      byInstance: new Map([['m', { isolated: 0, reloads: 0, batches: 0.3, ratio: 0 }]]),
    };
    const v = loadsFor('m', healthy);
    expect(v.loads).toBe('ok');
    // A healthy signal contributes no sentence.
    expect(v.detail).toBeUndefined();
  });

  it('is ok just below the threshold and failing at it', () => {
    const at: LoadRates = {
      byInstance: new Map([['m', { isolated: 0, reloads: 1, batches: 2, ratio: FAILING_RATIO }]]),
    };
    const below: LoadRates = {
      byInstance: new Map([
        ['m', { isolated: 0, reloads: 1, batches: 3, ratio: FAILING_RATIO - 0.01 }],
      ]),
    };
    expect(loadsFor('m', at).loads).toBe('failing');
    expect(loadsFor('m', below).loads).toBe('ok');
  });

  it('reports lost work even when the ratio would have said ok', () => {
    // Isolated units are checked FIRST on purpose: a Worker can drop units without its reload ratio
    // moving, and "the run lost work" outranks "the ratio looks fine".
    const both: LoadRates = {
      byInstance: new Map([['m', { isolated: 4, reloads: 0, batches: 0.3, ratio: 0 }]]),
    };
    expect(loadsFor('m', both).loads).toBe('failing');
  });

  it('is unknown — never ok — when the query failed', () => {
    const v = loadsFor('kf-crawl-01', { byInstance: new Map(), error: 'connect ECONNREFUSED' });
    expect(v.loads).toBe('unknown');
    expect(v.detail).toContain('could not ask VictoriaMetrics');
  });

  it('is unknown — never ok — when the Machine has no series at all', () => {
    // The most dangerous case: a metrics backend that has never received a byte would otherwise
    // paint a whole wall green.
    const v = loadsFor('kf-crawl-99', {
      byInstance: new Map([['kf-crawl-01', { isolated: 0, reloads: 0, batches: 0.3, ratio: 0 }]]),
    });
    expect(v.loads).toBe('unknown');
    expect(v.detail).toContain('no kontra_isolated_units_total series');
    expect(v.detail).toContain('vmagent');
  });
});

describe('which instance label belongs to a Machine', () => {
  const sick = { isolated: 6, reloads: 0, batches: 0, ratio: null };
  const target = { machine: 'kf-crawl-01', host: '10.124.0.9', publicIp: '203.0.113.9' };

  it('prefers the Machine name, which is what instance=%H should produce', () => {
    const rates: LoadRates = { byInstance: new Map([['kf-crawl-01', sick]]) };
    expect(instanceKeyFor(target, rates)).toBe('kf-crawl-01');
    expect(loadsFor(target, rates).loads).toBe('failing');
  });

  it('falls back to the private address, because the fleet has reported that form', () => {
    // kontra-fleet.json describes the two round-3 instances as "kf-m9 … and 10.124.0.21" — one a
    // hostname, one an address. Both forms have really been in this label.
    const rates: LoadRates = { byInstance: new Map([['10.124.0.9', sick]]) };
    expect(instanceKeyFor(target, rates)).toBe('10.124.0.9');
    expect(loadsFor(target, rates).loads).toBe('failing');
  });

  it('falls back to the public address last', () => {
    const rates: LoadRates = { byInstance: new Map([['203.0.113.9', sick]]) };
    expect(instanceKeyFor(target, rates)).toBe('203.0.113.9');
  });

  it('matches NO loopback, so an un-overridden scrape label cannot merge the fleet', () => {
    // If `-remoteWrite.label=instance=%H` ever fails to override the scrape target's own
    // `instance`, every Machine pushes `127.0.0.1:9110` and there is one series for the whole
    // fleet. Attributing that to a Machine would be the green-wall failure; not matching it is
    // `unknown`, which is true.
    const rates: LoadRates = { byInstance: new Map([['127.0.0.1:9110', sick]]) };
    expect(instanceKeyFor(target, rates)).toBeUndefined();
    expect(loadsFor(target, rates).loads).toBe('unknown');
  });

  it('does not attribute one Machine series to another', () => {
    const rates: LoadRates = { byInstance: new Map([['kf-crawl-02', sick]]) };
    expect(instanceKeyFor(target, rates)).toBeUndefined();
    expect(loadsFor(target, rates).loads).toBe('unknown');
  });
});

describe('the /api/v1/query wire shape', () => {
  it('parses an instant vector', () => {
    const samples = parseInstantQuery({
      status: 'success',
      data: {
        resultType: 'vector',
        result: [
          { metric: { instance: 'kf-crawl-01', actor: 'webcrawl' }, value: [1700000000, '0.27'] },
        ],
      },
    });
    expect(samples).toEqual([
      { labels: { instance: 'kf-crawl-01', actor: 'webcrawl' }, value: 0.27 },
    ]);
  });

  it('throws on a PromQL error rather than returning an empty vector', () => {
    // An empty vector means "no series", which is `unknown`; a parse error means the query is wrong,
    // which an operator has to be able to see. Collapsing them would hide a broken query forever
    // behind a chip that says "no data".
    expect(() =>
      parseInstantQuery({ status: 'error', errorType: '422', error: 'unparsed data' })
    ).toThrow(/unparsed data/);
  });

  it('DROPS a NaN sample rather than letting it arrive as a numeric zero', () => {
    // VictoriaMetrics renders a missing sample as the string "NaN". Number("NaN") is NaN, but a
    // careless parser turning it into 0 would report a sick Worker as clean.
    const samples = parseInstantQuery({
      status: 'success',
      data: { result: [{ metric: { instance: 'kf-crawl-01' }, value: [1, 'NaN'] }] },
    });
    expect(samples).toEqual([]);
  });

  it('reads an absent or empty result as no series', () => {
    expect(parseInstantQuery({ status: 'success' })).toEqual([]);
    expect(parseInstantQuery({ status: 'success', data: { result: null } })).toEqual([]);
  });
});
