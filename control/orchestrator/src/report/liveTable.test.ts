/** The default report draws a LIVE table from the run's own Datasets, mid-run. */
import { describe, expect, it } from 'vitest';
import { DEFAULT_TEMPLATE, defaultContext } from './defaultTemplate';
import { buildContext } from './context';
import { renderReport, type EngineDeps } from './engine';
import type { RefResolution } from './codeTag';

const deps: EngineDeps = {
  async resolveRef(): Promise<RefResolution> {
    return { bytes: Buffer.from('bytes'), fullBytes: 5 };
  },
  redactLatin1: (t) => t,
  redactText: (t) => t,
};
const render = (tpl: string, c: Record<string, unknown>) =>
  renderReport(tpl, c as never, deps).then((r) => r.markdown);

const base = {
  runId: 'r-1', startedAt: 1_000, closedAt: 0, status: 'running',
  type: 'redditscan', version: 1, now: 2_000,
  identity: { workflow: 'redditscan', version: '0.1.0' },
};

/** Composed exactly as `sweep.ts` composes it for a default-template render: the context, plus the
 *  `default` root. Building only half of it was how the first version of this test failed. */
function ctx(datasets: Record<string, unknown>, progress?: unknown) {
  const c = buildContext({ ...base, input: {}, datasets, ...(progress ? { progress } : {}) } as never);
  return { ...c, default: defaultContext(c.run as never, c.result) } as unknown as Record<string, unknown>;
}

describe('a running report shows a table of what has landed so far', () => {
  const live = {
    reddit_scan: {
      rows: 3, batches: 2, last_commit_at: '2026-10-09T18:00:00Z',
      head: [
        { type: 'post', subreddit: 'r/webscraping', score: 212 },
        { type: 'post', subreddit: 'r/legaladvice', score: 64 },
      ],
      tail: [],
    },
  };

  it('renders the dataset as a real markdown table while the run is RUNNING', async () => {
    const out = await render(DEFAULT_TEMPLATE, ctx(live, {
      units_done: 2, units_total: 5, isolated: 0, phase: 'harvest', updated_at: '',
    }));
    // ESCAPED, and that is correct: Liquid's output is Markdown, so `_` is escaped to `\_` and
    // renders back as an underscore. Asserting the raw name would be asserting a bug.
    expect(out).toContain('## reddit\\_scan');
    expect(out).toContain('3 rows across 2 batches');
    // the header comes from the derived columns, in first-seen order
    expect(out).toContain('| type | subreddit | score |');
    // …and the rows are the real ones
    expect(out).toContain('| post | r/webscraping | 212 |');
    expect(out).toContain('| post | r/legaladvice | 64 |');
    // progress too, since that is the other thing that only exists mid-run
    expect(out).toContain('harvest — 2 of 5 units committed');
  });

  it('GROWS as batches commit — which is the whole claim', async () => {
    const first = await render(DEFAULT_TEMPLATE, ctx({
      reddit_scan: { rows: 1, batches: 1, last_commit_at: 'x', head: [live.reddit_scan.head[0]], tail: [] },
    }));
    const later = await render(DEFAULT_TEMPLATE, ctx(live));
    expect(first).toContain('1 row across 1 batch');
    expect(later).toContain('3 rows across 2 batches');
    expect(later.length).toBeGreaterThan(first.length);
    expect(first).not.toContain('r/legaladvice');
    expect(later).toContain('r/legaladvice');
  });

  it('says PUSHED, not committed, for rows that have not been published', async () => {
    // The in-flight summary is what makes a report move before the first commit: a Method's pushes
    // are durable blobs at push time but only reach the lake when the batch is published after the
    // Method returns. So `batches: 0` with rows is a real, correct state — and "last commit" over
    // it is a sentence a reader cannot tell is wrong.
    const out = await render(DEFAULT_TEMPLATE, ctx({
      'in flight': { rows: 5, batches: 0, last_commit_at: '2026-10-09T18:30:00Z',
                     head: [{ target: 'a.example', ok: true }], tail: [] },
    }));
    // The timestamp is asserted only as "present", not literally: Liquid's output is Markdown, so
    // a date arrives escaped (2026\\-10\\-09). Asserting the raw form would assert a bug — the same
    // escaping that made `reddit_scan` render as `reddit\\_scan`.
    expect(out).toContain('5 rows pushed so far, not yet published, newest at ');
    expect(out).not.toContain('last commit');
    expect(out).toContain('| target | ok |');   // still a real table
  });

  it('says COMMITTED once a batch has published', async () => {
    const out = await render(DEFAULT_TEMPLATE, ctx({
      reddit_scan: { rows: 5, batches: 2, last_commit_at: 'T', head: [], tail: [] },
    }));
    expect(out).toContain('5 rows across 2 batches, last commit T');
  });

  it('counts elapsed time while the run is OPEN, not 0', async () => {
    // MEASURED on the live install: `duration_s` read 0 for a whole 52-second run, because the
    // old expression keyed on `closedAt > startedAt` and `closedAt` is 0 until a run ends. A live
    // report whose clock never moves is the most visible way to look broken.
    const c = buildContext({ ...base, input: {}, datasets: {} } as never);
    expect(c.run.status).toBe('running');
    expect(c.run.duration_s).toBe(1); // now 2_000 - startedAt 1_000
  });

  it('switches to the CLOSED duration once the run ends, so a stored render is reproducible', async () => {
    const c = buildContext({
      ...base, status: 'completed', closedAt: 6_000, now: 999_999, input: {}, datasets: {},
    } as never);
    // from closedAt, NOT from the render clock — or every re-render would produce a new document
    expect(c.run.duration_s).toBe(5);
  });

  it('says nothing at all when the run has no datasets — no empty table', async () => {
    const out = await render(DEFAULT_TEMPLATE, ctx({}));
    expect(out).not.toContain('|---|---|---|');
    expect(out).toContain('# redditscan');
  });
});
