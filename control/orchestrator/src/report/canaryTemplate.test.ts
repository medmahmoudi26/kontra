import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { describe, expect, it } from 'vitest';

import type { RefResolution } from './codeTag';
import { renderReport, type EngineDeps } from './engine';
import { countNodes, parseMarkdown } from './render';

/**
 * THE CANARY'S `report.md`, RENDERED IN EVERY SHAPE A CANARY RUN CAN END IN.
 *
 * The canary is the first run anybody does on a fresh install, so its report is the first report
 * anybody reads. A template that errors renders a stored error version instead of a document — the
 * run still completes, nothing goes red, and the first impression is a stack trace. `kontra workflow
 * serve` lints the template's SYNTAX and context roots; it does not check field names under
 * `result`, and it cannot see what a table looks like once rendered. This does both, against
 * contexts shaped exactly like what `workflow.py::_result` returns (the field names are held to the
 * dataclass by `tests/test_canary_report.py`).
 *
 * The e2e that runs the real thing on a fresh install is `e2e/canary-report.spec.ts`; this is the
 * part of it that does not need a cluster.
 */

const TEMPLATE = readFileSync(
  resolve(__dirname, '../../../../workspaces/default/workflows/canary/report.md'),
  'utf8'
);

const deps: EngineDeps = {
  async resolveRef(): Promise<RefResolution> {
    throw new Error('the canary report carries no claim-checked bytes');
  },
  redactLatin1: (t) => t,
  redactText: (t) => t,
};

const RUN_ID = 'canary-1791506965';

function ctx(
  status: string,
  result: Record<string, unknown> | null,
  error: unknown = null,
  live: { progress?: unknown; datasets?: Record<string, unknown> } = {}
) {
  return {
    run: {
      id: RUN_ID,
      workflow_id: RUN_ID,
      status,
      started_at: '2026-10-09T10:00:00Z',
      ended_at: status === 'running' ? '' : '2026-10-09T10:00:55Z',
      duration_s: status === 'running' ? 31 : 55,
      error,
      progress: live.progress ?? null,
    },
    workflow: { name: 'canary', version: '1.1.0', workspace: 'default' },
    input: {},
    result,
    report: { rendered_at: '2026-10-09T10:00:56Z', template_hash: 'x', version: 1 },
    // Always an object, as `buildContext` makes it; `in flight` only while rows have been pushed.
    datasets: live.datasets ?? {},
  };
}

const pushed = (n: number) =>
  Array.from({ length: n }, (_, i) => ({
    target: 'alpha',
    step: i + 1,
    phase: ['resolve', 'connect', 'handshake', 'probe', 'settle'][i % 5],
    latency_ms: 8 + i * 2.5,
    ok: true,
    worker: '808b7f0da909',
  }));

function result(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    summary:
      'All 2 targets swept: 10 of 10 records landed in canary_signals, on 1 docker machine that is now destroyed.',
    complete: true,
    records: 10,
    expected: 10,
    steps: 5,
    every: 2.0,
    provider: 'docker',
    machines: 1,
    sessions: 1,
    dataset: 'canary_signals',
    query: `select target, step, phase, latency_ms, worker\nfrom canary_signals\nwhere run_id = '${RUN_ID}'\norder by target, step`,
    run: RUN_ID,
    targets: [
      { target: 'alpha', records: 5, outcome: 'swept', error: null },
      { target: 'beta', records: 5, outcome: 'swept', error: null },
    ],
    voided: null,
    ...over,
  };
}

async function render(c: Record<string, unknown>) {
  const out = await renderReport(TEMPLATE, c, deps);
  return { ...out, tree: parseMarkdown(out.markdown) };
}

describe('the canary report while the run is LIVE (ADR 0062)', () => {
  it('before any target reaches a Worker: the clock, and that the Fleet is still coming up', async () => {
    const { markdown, blocks } = await render(ctx('running', null));
    expect(markdown).toContain('# canary · running');
    expect(markdown).toContain('Running for 31s');
    expect(markdown).toContain('the Fleet is still coming up');
    // Not the ended branch: an open run is not a run that "returned nothing".
    expect(markdown).not.toContain('This run ended');
    expect(blocks).toHaveLength(0);
  });

  it('mid-sweep: targets swept so far, the record count, and the newest records as a table', async () => {
    const { markdown, tree } = await render(
      ctx('running', null, null, {
        progress: { units_done: 1, units_total: 2, isolated: 0, phase: 'one batch running', updated_at: '' },
        datasets: {
          'in flight': { rows: 7, batches: 0, last_commit_at: '', head: pushed(5), tail: pushed(5), columns: [] },
        },
      })
    );
    expect(markdown).toContain('**1 of 2** target(s) swept so far.');
    expect(markdown).toContain('## Records so far: 7');
    expect(markdown).toMatch(/\| alpha \| 3 \| handshake \| 13 \| 808b7f0da909 \|/);
    expect(countNodes(tree, 'table')).toBe(1);
    expect(countNodes(tree, 'tableRow')).toBe(1 + 5);
  });

  it('says how many were dropped while it is still going', async () => {
    const { markdown } = await render(
      ctx('running', null, null, {
        progress: { units_done: 2, units_total: 2, isolated: 1, phase: 'one batch running', updated_at: '' },
      })
    );
    expect(markdown).toContain('**2 of 2** target(s) swept so far, 1 dropped.');
  });
});

describe('the canary report', () => {
  it('a complete run: the summary, one fleet row, one row per target, and the query', async () => {
    const { markdown, blocks, tree } = await render(ctx('completed', result()));

    expect(markdown).toContain('# canary · completed');
    // The summary is a value, so its underscore arrives escaped; it has no parentheses to escape
    // because `_result` writes real plurals.
    expect(markdown).toContain('All 2 targets swept: 10 of 10 records landed in canary\\_signals');
    // Two tables, and the target table has a header plus one row per target. A row that broke
    // across lines (an unescaped newline, a stray `|`) would change this count.
    expect(countNodes(tree, 'table')).toBe(2);
    expect(countNodes(tree, 'tableRow')).toBe(2 + 3);
    expect(markdown).toMatch(/\| alpha \| 5 \| swept \|/);
    expect(markdown).toMatch(/\| beta \| 5 \| swept \|/);
    expect(markdown).toContain('| 10 of 10 |');
    // `canary_signals` is escaped as a value, so it reaches Markdown as `canary\_signals` and
    // reads as the plain name once rendered — which is why it is not inside backticks.
    expect(markdown).toContain('**canary\\_signals**');
    expect(markdown).not.toContain('Why the sweep was voided');

    expect(blocks).toHaveLength(1);
    expect(blocks[0]!.lang).toBe('sql');
  });

  it('a dropped target: named in the table with its own error, and the run still reads as a result', async () => {
    const { markdown } = await render(
      ctx(
        'completed',
        result({
          complete: false,
          records: 5,
          summary: '1 of 2 targets dropped: 5 of 10 records landed in canary_signals. The rest of the sweep carried on.',
          targets: [
            { target: 'alpha', records: 5, outcome: 'swept', error: null },
            { target: 'beta', records: 0, outcome: 'dropped', error: 'canary: refusing beta because fail_on names it' },
          ],
        })
      )
    );
    expect(markdown).toMatch(/\| beta \| 0 \| dropped: canary: refusing beta because fail\\_on names it \|/);
    expect(markdown).toContain('| 5 of 10 |');
  });

  it('drops that could not be named: no target is called swept, and the unknown count is blank, not 0', async () => {
    const { markdown, tree } = await render(
      ctx(
        'completed',
        result({
          complete: false,
          records: 5,
          summary: '1 of 2 targets dropped: 5 of 10 records landed in canary_signals. The rest of the sweep carried on.',
          targets: [
            { target: 'alpha', records: null, outcome: 'unknown', error: null },
            { target: 'beta', records: null, outcome: 'unknown', error: null },
          ],
        })
      )
    );
    expect(markdown).toMatch(/\| alpha \|  \| unknown \|/);
    expect(countNodes(tree, 'tableRow')).toBe(2 + 3);
  });

  it('a voided sweep: every target says so, and the reason is shown verbatim in a block', async () => {
    const reason = "ActivityError('activity task failed')";
    const { markdown, blocks } = await render(
      ctx(
        'completed',
        result({
          complete: false,
          records: 0,
          voided: reason,
          summary: 'The sweep was voided before it reported back, so 0 of 10 records are accounted for. The Fleet was still released.',
          targets: [
            { target: 'alpha', records: 0, outcome: 'voided', error: null },
            { target: 'beta', records: 0, outcome: 'voided', error: null },
          ],
        })
      )
    );
    expect(markdown).toContain('## Why the sweep was voided');
    expect(markdown).toMatch(/\| alpha \| 0 \| voided \|/);
    expect(blocks.map((b) => b.lang)).toEqual(['text', 'sql']);
  });

  it('a run that did not complete: no result, the status, and the error', async () => {
    const { markdown, blocks } = await render(
      ctx('failed', null, { type: 'ApplicationError', message: 'the Fleet never became ready' })
    );
    expect(markdown).toContain('# canary · failed');
    expect(markdown).toContain('This run ended **failed**');
    expect(markdown).not.toContain('## Targets');
    expect(blocks).toHaveLength(1);
    expect(blocks[0]!.lang).toBe('text');
  });

  it('a run that did not complete and carries no error still renders', async () => {
    const { markdown, blocks } = await render(ctx('cancelled', null));
    expect(markdown).toContain('This run ended **cancelled**');
    expect(blocks).toHaveLength(0);
  });
});
