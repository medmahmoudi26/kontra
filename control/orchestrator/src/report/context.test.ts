/**
 * ADR 0062's additions to §2.4's contract, and the one guarantee that makes them safe to deploy.
 *
 * The expensive mistake this file exists to prevent: `renderKey` hashes `run` whole, so a new field
 * under it moves the key for every run that already has a stored version — `versionByKey` misses, and
 * the convergence pass re-renders the entire retention window on deploy. The projection in
 * `renderKey` is what stops that, and "a run without progress hashes as it did before" is the
 * assertion that proves it rather than assuming it.
 */

import { describe, expect, it } from 'vitest';

import {
  buildContext,
  DATASET_HEAD_MAX,
  DATASET_TAIL_MAX,
  renderKey,
  type BuildContextInput,
  type DatasetSummary,
} from './context';

const HASH = 'default@abc123';

/** A closed, completed run — the shape every stored version was keyed from before ADR 0062. */
function closed(over: Partial<BuildContextInput> = {}): BuildContextInput {
  return {
    runId: 'r_8f3c1a',
    status: 'completed',
    startedAt: 1_700_000_000_000,
    closedAt: 1_700_000_461_300,
    type: 'Enrich',
    identity: { workflow: 'enrich', version: '1.0.0' },
    input: { catalog: 'catalog-eu' },
    output: { summary: '12,480 products enriched' },
    version: 1,
    now: 1_700_000_500_000,
    ...over,
  };
}

/** An open run: `closedAt` 0 is what "still running" looks like to `isoOf`. */
function open(over: Partial<BuildContextInput> = {}): BuildContextInput {
  return closed({ status: 'running', closedAt: 0, output: undefined, ...over });
}

const PROGRESS = {
  units_done: 4_120,
  units_total: 12_480,
  isolated: 3,
  phase: 'enriching',
  updated_at: '2026-10-08T01:00:00.000Z',
};

function summary(over: Partial<DatasetSummary> = {}): DatasetSummary {
  return { rows: 0, batches: 0, last_commit_at: '', head: [], tail: [], ...over };
}

describe('renderKey does not move for a run that predates ADR 0062', () => {
  /**
   * THE ONE THAT MATTERS. If this fails, deploying ADR 0062 re-renders every closed run in the
   * retention window, and the version dropdown of every finished report grows a duplicate.
   */
  it('is byte-identical whether or not `run.progress` is present', () => {
    const without = renderKey(HASH, buildContext(closed()));
    const with_ = renderKey(HASH, buildContext(closed({ progress: PROGRESS })));
    expect(with_).toBe(without);
  });

  it('is byte-identical whether or not `datasets` carries anything', () => {
    const without = renderKey(HASH, buildContext(closed()));
    const with_ = renderKey(
      HASH,
      buildContext(closed({ datasets: { products: summary({ rows: 12_480, batches: 26 }) } }))
    );
    expect(with_).toBe(without);
  });

  /** A projection that excluded too much would make the key stop distinguishing runs at all. */
  it('still moves when something inside `run` that is NOT progress changes', () => {
    const a = renderKey(HASH, buildContext(closed()));
    const b = renderKey(HASH, buildContext(closed({ runId: 'r_other' })));
    expect(b).not.toBe(a);
  });

  it('still moves when the template hash changes, which is what heals the back catalogue', () => {
    const ctx = buildContext(closed());
    expect(renderKey('default@newer', ctx)).not.toBe(renderKey(HASH, ctx));
  });
});

describe('`result` has three cases and only the middle one is new', () => {
  it('is the return value once the run completed', () => {
    expect(buildContext(closed()).result).toEqual({ summary: '12,480 products enriched' });
  });

  /**
   * ADR 0062's change: the workflow's `report` query answers while the run is open. This is why
   * `run.status` rather than `result` is the completion test from here on.
   */
  it('is the workflow’s `report` query while the run is OPEN', () => {
    const ctx = buildContext(open({ partial: { summary: '4,120 so far', products: 4_120 } }));
    expect(ctx.result).toEqual({ summary: '4,120 so far', products: 4_120 });
  });

  it('is null for an open run whose workflow defines no handler', () => {
    expect(buildContext(open()).result).toBeNull();
  });

  /**
   * §2.4's LOAD-BEARING CASE, UNCHANGED. A failed, cancelled, terminated or timed-out run has no
   * return value, and `null` with `lenientIf` is what makes the `{% else %}` branch reachable. A
   * partial must never leak into a terminal non-completion — the run did not produce it.
   */
  it.each(['failed', 'cancelled', 'terminated', 'timed_out'])(
    'is null for a %s run even when a partial was captured',
    (status) => {
      const ctx = buildContext(
        closed({ status, output: undefined, partial: { summary: 'should not appear' } })
      );
      expect(ctx.result).toBeNull();
    }
  );
});

describe('the new values are null-or-empty rather than absent', () => {
  /** `strictVariables` makes an absent name an error, so a falsy value has to be a value. */
  it('renders `run.progress` as null when there is none, not undefined', () => {
    expect(buildContext(closed()).run.progress).toBeNull();
  });

  it('renders `datasets` as an object when the run wrote none', () => {
    expect(buildContext(closed()).datasets).toEqual({});
  });

  it('carries progress through unchanged when there is some', () => {
    expect(buildContext(open({ progress: PROGRESS })).run.progress).toEqual(PROGRESS);
  });
});

describe('a Dataset summary is bounded by the context, not by the template', () => {
  const rows = (n: number): unknown[] => Array.from({ length: n }, (_, i) => ({ i }));

  it('clamps `head` so no template can ask for more', () => {
    const ctx = buildContext(open({ datasets: { products: summary({ head: rows(500) }) } }));
    expect(ctx.datasets.products!.head).toHaveLength(DATASET_HEAD_MAX);
  });

  it('clamps `tail` so a report does not get slower as a Dataset grows', () => {
    const ctx = buildContext(open({ datasets: { products: summary({ tail: rows(500) }) } }));
    expect(ctx.datasets.products!.tail).toHaveLength(DATASET_TAIL_MAX);
  });

  it('leaves a summary already within bounds alone', () => {
    const ctx = buildContext(open({ datasets: { products: summary({ head: rows(3), tail: rows(7) }) } }));
    expect(ctx.datasets.products!.head).toHaveLength(3);
    expect(ctx.datasets.products!.tail).toHaveLength(7);
  });

  it('keeps the counts, which are the part a template usually prints', () => {
    const ctx = buildContext(
      open({
        datasets: {
          products: summary({ rows: 12_480, batches: 26, last_commit_at: '2026-10-08T01:00:00.000Z' }),
        },
      })
    );
    expect(ctx.datasets.products).toMatchObject({
      rows: 12_480,
      batches: 26,
      last_commit_at: '2026-10-08T01:00:00.000Z',
    });
  });
});
