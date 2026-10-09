/**
 * A template that prints `run.error` whole takes the whole report down — and the report is the
 * only surface that carries a run's failure message.
 *
 * ── WHY THIS IS A GENERAL TEST AND NOT A REDDITSCAN ONE ────────────────────────────────────────
 *
 * It started as "render redditscan's report.md", reading the file from a sibling CHECKOUT by
 * absolute path. That passes on the machine it was written on and fails in CI, where only this
 * repository exists — a test whose green is a property of one developer's disk. The thing worth
 * pinning is not one workflow's template, it is the ENGINE CONTRACT every template is written
 * against: `run.error` is an object, printing it is a render error, and a report that fails to
 * render is how a failed run becomes a silent one.
 *
 * MEASURED: `redditscan-1791567587` failed with a precise message naming what the operator had
 * pasted. `/api/runs/:id` exposes no error field, `/io` exposes none, and the report — which could
 * have shown it — came back `status: error`, "cannot print an object directly, line:54". Three
 * surfaces, three silences.
 */
import { describe, expect, it } from 'vitest';

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

function failed() {
  return buildContext({
    runId: 'redditscan-1', status: 'failed', startedAt: 1_000, closedAt: 2_000, now: 3_000,
    type: 'RedditScan', version: 1, identity: { workflow: 'redditscan', version: '0.1.0' },
    input: {},
    error: { type: 'NoInput', message: 'no Reddit thread URLs in the request' },
    datasets: {},
  } as never) as never;
}

describe('run.error in a template', () => {
  it('REFUSES to print whole — which is what made a failed run silent', async () => {
    await expect(renderReport('{{ run.error }}', failed(), deps)).rejects.toThrow(
      /cannot print an object directly/
    );
  });

  it('renders by field, which is how a template must say what went wrong', async () => {
    const out = await renderReport(
      '{% if run.error %}**{{ run.error.type }}** — {{ run.error.message }}{% endif %}',
      failed(),
      deps
    );
    expect(out.markdown).toContain('NoInput');
    expect(out.markdown).toContain('no Reddit thread URLs in the request');
  });
});
