/**
 * THE RUN-END SWEEP, end to end: a stubbed Temporal, the real store on in-memory SQLite, and the REAL
 * engine rendering into the real parser.
 *
 * Nothing is mocked between the template and the snapshot, because the things most likely to be wrong
 * live in the seams — a context field a template names and the builder does not set, a status spelled
 * `canceled` on one side and `cancelled` on the other, a snapshot that stores as `undefined`.
 *
 * Acceptance tests 11 (failed run), 13 (idempotent render), 14 (render error isolation) and 15 (no
 * template) live here.
 */

import { beforeEach, describe, expect, it } from 'vitest';

import { templateHash } from './sweep';
import { contextForRun, isReportable, sweepFinishedRuns, type SweepDeps, type SweepRun } from './sweep';
import { DEFAULT_TEMPLATE } from './defaultTemplate';
import { ReportStore } from './store';

const RUN: SweepRun = {
  runId: 'enrich-1791234567',
  status: 'completed',
  startedAt: 1_791_234_567_000,
  closedAt: 1_791_235_028_000,
  type: 'Enrich',
};

const TEMPLATE = `# {{ workflow.name }} over {{ input.catalog }}

{% if result %}
{{ result.summary }}

| products | missing |
|---|---|
| {{ result.products }} | {{ result.missing }} |
{% else %}
Run ended **{{ run.status }}** after {{ run.duration_s }}s: {{ run.error.message }}
{% endif %}
`;

let store: ReportStore;

function deps(over: Partial<SweepDeps> = {}): SweepDeps {
  return {
    store,
    list: async () => [RUN],
    io: async () => ({
      input: { catalog: 'catalog-eu' },
      output: { summary: 'all good', products: 12480, missing: 214 },
    }),
    close: async () => undefined,
    identity: async () => ({ workflow: 'enrich', version: '0.4.1' }),
    now: () => 1_791_235_030_000,
    // In-process, so the test asserts the render rather than the worker-entry resolution. The worker
    // path is exercised only by a built image — see renderHost's header.
    render: undefined,
    ...over,
  };
}

async function snapshotOf(runId: string, version?: number): Promise<Record<string, unknown>> {
  const v = await store.version(runId, version);
  return JSON.parse(v!.snapshotJson!) as Record<string, unknown>;
}

beforeEach(async () => {
  store = new ReportStore({ url: ':memory:' });
  await store.ensureSchema();
});

describe('isReportable', () => {
  it('takes a closed run and leaves an open one', () => {
    expect(isReportable({ status: 'completed', closedAt: 1 })).toBe(true);
    expect(isReportable({ status: 'failed', closedAt: 1 })).toBe(true);
    expect(isReportable({ status: 'running', closedAt: 0 })).toBe(false);
    expect(isReportable({ status: 'pending', closedAt: 0 })).toBe(false);
    // A closed-at of zero with a terminal status is not a thing Temporal produces, and if it did, the
    // report would have no end instant to state. Excluded by the same predicate.
    expect(isReportable({ status: 'completed', closedAt: 0 })).toBe(false);
  });
});

describe('a pinned template', () => {
  beforeEach(async () => {
    await store.pinTemplate({
      runId: RUN.runId,
      templateHash: templateHash(TEMPLATE),
      templateText: TEMPLATE,
      source: 'workspace',
      workspace: 'demo',
    });
  });

  it('renders it, and the snapshot holds the run\'s own values', async () => {
    const out = await sweepFinishedRuns(deps());
    expect(out).toMatchObject({ scanned: 1, closed: 1, rendered: 1, present: 0, noTemplate: 0, errored: 0 });
    const snapshot = JSON.stringify(await snapshotOf(RUN.runId));
    expect(snapshot).toContain('enrich over catalog-eu');
    expect(snapshot).toContain('all good');
    expect(snapshot).toContain('12480');
  });

  it('ACCEPTANCE 13: a second pass renders nothing and stores no second version', async () => {
    await sweepFinishedRuns(deps());
    const second = await sweepFinishedRuns(deps());
    expect(second).toMatchObject({ rendered: 0, present: 1 });
    expect(await store.versions(RUN.runId)).toHaveLength(1);
  });

  it('records the template hash it rendered through, so a re-render can reproduce it', async () => {
    await sweepFinishedRuns(deps());
    const list = await store.versions(RUN.runId);
    expect(list[0]!.templateHash).toBe(templateHash(TEMPLATE));
    expect(list[0]!.renderedBy).toBe('sweep');
  });

  it('ACCEPTANCE 14: a template error stores an error version and leaves the run alone', async () => {
    await store.pinTemplate({
      runId: RUN.runId,
      templateHash: 'sha256:bad',
      templateText: '{{ results.nope }}',
      source: 'workspace',
    });
    const out = await sweepFinishedRuns(deps());
    expect(out).toMatchObject({ rendered: 0, errored: 1, failed: 0 });
    const v = await store.version(RUN.runId);
    expect(v?.status).toBe('error');
    expect(v?.errorText).toMatch(/undefined variable/i);
    expect('snapshotJson' in (v as object)).toBe(false);
  });

  it('does not retry a failed render on the next pass, because the key is the same', async () => {
    await store.pinTemplate({
      runId: RUN.runId,
      templateHash: 'sha256:bad',
      templateText: '{{ results.nope }}',
      source: 'workspace',
    });
    await sweepFinishedRuns(deps());
    const second = await sweepFinishedRuns(deps());
    expect(second).toMatchObject({ errored: 0, present: 1 });
    expect(await store.versions(RUN.runId)).toHaveLength(1);
  });
});

describe('ACCEPTANCE 11: a run that did not complete', () => {
  beforeEach(async () => {
    await store.pinTemplate({
      runId: RUN.runId,
      templateHash: templateHash(TEMPLATE),
      templateText: TEMPLATE,
      source: 'workspace',
    });
  });

  it('renders the else branch, with result null and the error message set', async () => {
    const out = await sweepFinishedRuns(
      deps({
        list: async () => [{ ...RUN, status: 'failed' }],
        io: async () => ({ input: { catalog: 'catalog-eu' } }),
        close: async () => ({ type: 'failed', message: 'the feed did not answer' }),
      })
    );
    expect(out.rendered).toBe(1);
    const snapshot = JSON.stringify(await snapshotOf(RUN.runId));
    expect(snapshot).toContain('the feed did not answer');
    expect(snapshot).toContain('failed');
    // The completed branch's content must be absent — `result` was null, so `{% if result %}` took else.
    expect(snapshot).not.toContain('all good');
  });

  it('spells a cancelled run the way the contract does, not the way Temporal does', async () => {
    await sweepFinishedRuns(
      deps({
        list: async () => [{ ...RUN, status: 'canceled' }],
        // `input.catalog` is present because the template's heading names it and `strictVariables`
        // makes an absent field a render error — which is the behaviour acceptance test 6 asks for,
        // and which caught this fixture being wrong rather than the code.
        io: async () => ({ input: { catalog: 'catalog-eu' } }),
        close: async () => ({ type: 'canceled', message: '' }),
      })
    );
    const snapshot = JSON.stringify(await snapshotOf(RUN.runId));
    expect(snapshot, 'a template matching on "cancelled" would never fire').toContain('cancelled');
  });

  it('discards a return value a non-completed run somehow carries', async () => {
    // Temporal's close event has no result for a failure, but a caller could pass one. `result` is
    // null for anything but `completed`, so a template cannot show output from a run that failed.
    await sweepFinishedRuns(
      deps({
        list: async () => [{ ...RUN, status: 'failed' }],
        io: async () => ({ input: { catalog: 'catalog-eu' }, output: { summary: 'should not appear' } }),
        close: async () => ({ type: 'failed', message: 'boom' }),
      })
    );
    expect(JSON.stringify(await snapshotOf(RUN.runId))).not.toContain('should not appear');
  });
});

describe('ACCEPTANCE 15: no template', () => {
  it('renders the default report and says in the snapshot why', async () => {
    const out = await sweepFinishedRuns(deps());
    expect(out).toMatchObject({ rendered: 1, noTemplate: 1 });
    const snapshot = await snapshotOf(RUN.runId);
    const text = JSON.stringify(snapshot);
    expect(text).toContain('enrich');
    expect(text, 'the result was not rendered as tables').toContain('12480');
    expect((snapshot.warnings as string[])[0]).toMatch(/No report.md was pinned/);
  });

  it('names the default template by its own digest, so the version is reproducible', async () => {
    await sweepFinishedRuns(deps());
    expect((await store.versions(RUN.runId))[0]!.templateHash).toMatch(/^default@[0-9a-f]{12}$/);
  });

  it('is still idempotent', async () => {
    await sweepFinishedRuns(deps());
    expect(await sweepFinishedRuns(deps())).toMatchObject({ rendered: 0, present: 1 });
  });
});

describe('the counters, which are what an operator reads', () => {
  it('names the runs whose metadata went away rather than only counting them', async () => {
    const out = await sweepFinishedRuns(deps({ io: async () => undefined }));
    expect(out).toMatchObject({ closed: 1, gone: 1, rendered: 0 });
    expect(out.goneIds).toEqual([RUN.runId]);
  });

  it('counts a failure per run and keeps going', async () => {
    const out = await sweepFinishedRuns(
      deps({
        list: async () => [RUN, { ...RUN, runId: 'other-1' }],
        io: async (runId) => {
          if (runId === RUN.runId) throw new Error('temporal said no');
          return { input: {}, output: { n: 1 } };
        },
      })
    );
    expect(out).toMatchObject({ closed: 2, failed: 1, rendered: 1 });
  });

  it('reports a full page as possibly a prefix of what exists', async () => {
    const out = await sweepFinishedRuns(deps({ listLimit: 1 }));
    expect(out.capped).toBe(true);
  });

  it('does not call a page full when it is short', async () => {
    expect((await sweepFinishedRuns(deps({ listLimit: 200 }))).capped).toBe(false);
  });

  it('skips an open run without counting it closed', async () => {
    const out = await sweepFinishedRuns(deps({ list: async () => [{ ...RUN, status: 'running', closedAt: 0 }] }));
    expect(out).toMatchObject({ scanned: 1, closed: 0, rendered: 0 });
  });
});

describe('the render host seam', () => {
  it('stores the error a failing render reports, rather than throwing out of the pass', async () => {
    const out = await sweepFinishedRuns(
      deps({ render: async () => ({ ok: false, error: 'the render thread exited with code 1' }) })
    );
    expect(out).toMatchObject({ errored: 1, failed: 0 });
    expect((await store.version(RUN.runId))?.errorText).toContain('exited with code 1');
  });

  it('writes the unredacted bytes a render hands back, keyed to the version it stored', async () => {
    const out = await sweepFinishedRuns(
      deps({
        render: async () => ({
          ok: true,
          snapshot: { v: 1, root: { type: 'root', children: [] }, blocks: {} },
          bytes: 40,
          secrets: [{ blockId: 'b1', rawB64: Buffer.from('Authorization: Bearer abc').toString('base64') }],
        }),
      })
    );
    expect(out.rendered).toBe(1);
    const secret = await store.secret(RUN.runId, 1, 'b1');
    expect(Buffer.from(secret!, 'base64').toString()).toContain('Bearer abc');
  });
});

describe('a run pinned to the DEFAULT template renders the default, not an empty document', () => {
  // `pinTemplate` stores `templateText: ''` for `source: 'default'` on purpose — the text ships with
  // the orchestrator — and `contextForRun` read `pinned` as a boolean, so `'' ?? DEFAULT_TEMPLATE`
  // was `''`. That is every run started without a `report.md`, which is most of them. Found by the
  // live-report session; both assertions fail against the code before the fix.
  const RUN = 'enrich-1791234567';

  async function pinnedDefault(): Promise<Awaited<ReturnType<typeof contextForRun>>> {
    await store.pinTemplate({
      runId: RUN,
      templateHash: 'default@abc',
      templateText: '',
      source: 'default',
    });
    return contextForRun(
      { runId: RUN, status: 'completed', startedAt: 1, closedAt: 2 },
      { input: {}, result: { n: 1 } },
      { store, now: () => 3 }
    );
  }

  it('hands the renderer the default template rather than the empty string', async () => {
    const built = await pinnedDefault();
    expect(built.template).toBe(DEFAULT_TEMPLATE);
    expect(built.template).not.toBe('');
  });

  it('still supplies `default`, which the default template loops over', async () => {
    // Fixing only the text would trade the empty document for an UndefinedVariableError on
    // `default.tables` — the context arm has to key on the source too, which is why it is one change.
    const built = await pinnedDefault();
    expect(built.context.default).toBeDefined();
  });

  it('does not move the render key, which is what makes the fix safe for every stored version', async () => {
    const built = await pinnedDefault();
    expect(built.templateHash).toBe('default@abc');
  });
});
