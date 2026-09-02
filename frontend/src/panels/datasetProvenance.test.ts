/**
 * The provenance panel, rendered.
 *
 * `renderToStaticMarkup` rather than a DOM, for the reason `HealthChips.test.ts` records:
 * `vite.config.ts` runs vitest with `environment: 'node'` and an `src/**\/*.test.ts` include, and
 * there is no jsdom or testing-library in `frontend/package.json` — a file this slice may
 * not add a dependency to. Every assertion below is about text and attributes, all of which are
 * in the markup.
 *
 * The two Datasets that must render without error are the ones that actually exist: `lame`, whose
 * 1,246 rows all carry the legacy `('w','0')` pair, and a Dataset whose Batches recorded nothing.
 */

import { describe, expect, it } from 'vitest';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';

import DatasetProvenance from './DatasetProvenance';
import type { DatasetProvenance as Provenance } from '../datasets/provenance';

function measured(
  groups: Array<[string | null, string | null, number, (string | null)?]>
): Provenance {
  return {
    name: 'lame',
    kind: 'output',
    rows: groups.reduce((n, g) => n + g[2], 0),
    groups: groups.map(([machine, version, rows, run]) => ({
      machine,
      version,
      run: run === undefined ? 'r1' : run,
      rows,
    })),
    carriesProvenance: true,
    carriesRun: true,
    measuredAt: MEASURED_AT,
  };
}

/** The moment the one statement answered — 2026-08-16T02:15:07Z. */
const MEASURED_AT = Date.UTC(2026, 7, 16, 2, 15, 7);

function render(
  provenance: Provenance | null,
  error?: string | null,
  scoped?: { scopedRun: string | null; onScopeRun?: (run: string | null) => void }
): string {
  return renderToStaticMarkup(
    createElement(DatasetProvenance, { provenance, error, ...scoped })
  );
}

/** One row of the panel, read back out of the markup by its test hook. */
function row(html: string, testid: string): { state: string; value: string; rows: string; text: string } {
  const m = new RegExp(`<li[^>]*data-testid="${testid}"[^>]*>(.*?)</li>`, 's').exec(html);
  if (!m) throw new Error(`no ${testid} in:\n${html}`);
  const [tag] = /<li[^>]*>/.exec(m[0]) ?? [''];
  return {
    state: /data-state="([^"]*)"/.exec(tag)?.[1] ?? '',
    value: /data-value="([^"]*)"/.exec(tag)?.[1] ?? '',
    rows: /data-rows="([^"]*)"/.exec(tag)?.[1] ?? '',
    text: (m[1] ?? '').replace(/<[^>]*>/g, ''),
  };
}

function text(html: string, testid: string): string {
  const m = new RegExp(`data-testid="${testid}"[^>]*>(.*?)<`, 's').exec(html);
  return (m?.[1] ?? '').replace(/<[^>]*>/g, '');
}

describe('a fleet run, read back', () => {
  const html = render(
    measured([
      ['kf-dns-01', '0.1.0', 600],
      ['kf-dns-02', '0.1.0', 400],
      ['kf-dns-03', '0.1.0', 246],
    ])
  );

  it('names each Machine and what it contributed', () => {
    // The sentence the run status cannot form. Three Machines produced rows; the fourth is
    // absent, because a Dataset knows who wrote it and never who was asked.
    expect(text(html, 'provenance-machines-headline')).toBe('3 Machines');
    expect(row(html, 'provenance-machine-recorded-kf-dns-01')).toMatchObject({
      state: 'recorded',
      value: 'kf-dns-01',
      rows: '600',
    });
    expect(row(html, 'provenance-machine-recorded-kf-dns-03').rows).toBe('246');
    expect(html).not.toContain('kf-dns-04');
  });

  it('gives every row its own hook — never one id matching a dozen elements', () => {
    const ids = [...html.matchAll(/data-testid="(provenance-(?:machine|version)-[^"]*)"/g)].map(
      (m) => m[1]
    );
    expect(ids).toHaveLength(new Set(ids).size);
  });

  it('says what it counted, so two correct numbers are never confusable', () => {
    // The header above the panel counts the dispatch the operator opened; this counts every RUN
    // that wrote the name, because that is what the editor below reads. `SELECT count(*) FROM
    // lame` = 1,246 and the listing showing 623 is the PRD's third gap — both correct, neither
    // labelled. And as of when, because a Dataset a Run is still appending to grows.
    expect(text(html, 'provenance-scope')).toBe(
      `1,246 rows · every run · as of ${new Date(MEASURED_AT).toLocaleTimeString()}`
    );
  });

  it('says one Actor version when there is one, and draws no distribution over it', () => {
    expect(text(html, 'provenance-versions-headline')).toBe('1 Actor version');
    expect(row(html, 'provenance-version-recorded-0.1.0').rows).toBe('1246');
    // A single bucket gets no bar: a lone bar at 100% claims a distribution that does not exist.
    // Three bars in the whole panel — the three Machines — and none for the one version.
    expect(html.match(/data-share=/g)).toHaveLength(3);
  });
});

describe('a Dataset written entirely before provenance travelled with the Batch', () => {
  // `lame` as it stands: 1,246 rows, every one `('w','0')`.
  const html = render(measured([['w', '0', 1246]]));

  it('renders without error and reads as a placeholder, not as a Machine', () => {
    expect(text(html, 'provenance-machines-headline')).toBe('no Machine recorded');
    const w = row(html, 'provenance-machine-legacy-w');
    expect(w.state).toBe('legacy');
    expect(w.rows).toBe('1246');
    // The value is shown — the rows really do carry it — but never as a host: it is labelled a
    // placeholder, struck through, and its own colour.
    expect(w.text).toContain('w (placeholder)');
    expect(html).toContain('line-through');
  });

  it('is not relabelled as unrecorded, and not merged with anything', () => {
    // "Nothing wrote this" and "the old placeholder was written here" are different facts.
    expect(html).not.toContain('data-testid="provenance-machine-unrecorded"');
    expect(text(html, 'provenance-versions-headline')).toBe('no Actor version recorded');
    expect(row(html, 'provenance-version-legacy-0').state).toBe('legacy');
  });
});

describe('a Dataset nothing recorded a Machine for', () => {
  const html = render(measured([[null, null, 42]]));

  it('renders as *not recorded*, visually distinct from every value', () => {
    expect(text(html, 'provenance-machines-headline')).toBe('no Machine recorded');
    const gap = row(html, 'provenance-machine-unrecorded');
    expect(gap.state).toBe('unrecorded');
    expect(gap.value).toBe(''); // there is no value — that is the point
    expect(gap.rows).toBe('42');
    expect(gap.text).toContain('not recorded');
    expect(html).toContain('border-dashed');
  });

  it('never invents a Machine out of the absence of one', () => {
    // The Run IS recorded for these rows — `run_id` was never the substituted column — and the
    // Machine is not. One dimension answering must not lend its answer to the other.
    expect(html).not.toMatch(/data-testid="provenance-machine-recorded-/);
    expect(text(html, 'provenance-machines-headline')).not.toMatch(/\d/);
    expect(text(html, 'provenance-runs-headline')).toBe('1 Run');
  });
});

describe('all three in one Dataset', () => {
  const html = render(
    measured([
      ['kf-dns-01', '0.1.0', 10],
      ['w', '0', 5],
      [null, null, 2],
    ])
  );

  it('draws three separate rows, one per state', () => {
    expect(row(html, 'provenance-machine-recorded-kf-dns-01').state).toBe('recorded');
    expect(row(html, 'provenance-machine-legacy-w').state).toBe('legacy');
    expect(row(html, 'provenance-machine-unrecorded').state).toBe('unrecorded');
    // One Machine — the measured hostname. The other two name none.
    expect(text(html, 'provenance-machines-headline')).toBe('1 Machine');
  });

  it('measures every share against the same total the server counted', () => {
    const shares = [...html.matchAll(/data-share="([^"]*)"/g)].map((m) => Number(m[1]));
    // Two dimensions, three buckets each, all summing to 1 — no share is drawn from a second
    // query against a Dataset that may still be growing.
    expect(shares).toHaveLength(6);
    expect(shares.slice(0, 3).reduce((a, b) => a + b, 0)).toBeCloseTo(1, 3);
  });
});

/**
 * Two Runs, one Dataset name — `lame`'s measured shape, and the reason every count is labelled.
 */
describe('a Dataset two Runs wrote', () => {
  const TWO_RUNS = measured([
    ['w', '0', 623, 'nightly-2026-08-14'],
    ['w', '0', 623, 'nightly-2026-08-15'],
  ]);
  const html = render(TWO_RUNS);

  it('names both Runs and what each contributed, so 623 and 1,246 are one screen apart', () => {
    expect(text(html, 'provenance-runs-headline')).toBe('2 Runs');
    expect(row(html, 'provenance-run-recorded-nightly-2026-08-14').rows).toBe('623');
    expect(row(html, 'provenance-run-recorded-nightly-2026-08-15').rows).toBe('623');
    expect(text(html, 'provenance-scope')).toContain('1,246 rows · every run');
  });

  it('counts a Run whose rows carry the dead placeholder as a Run all the same', () => {
    // No Machine was recorded for any of these rows, and both Runs still were: `run_id` was
    // never the substituted column, so striking these through would delete a measured fact.
    expect(row(html, 'provenance-run-recorded-nightly-2026-08-14').state).toBe('recorded');
    expect(text(html, 'provenance-machines-headline')).toBe('no Machine recorded');
  });

  it('gives every row its own hook — never one id matching a dozen elements', () => {
    const ids = [...html.matchAll(/data-testid="(provenance-(?:machine|version|run)-[^"]*)"/g)].map(
      (m) => m[1]
    );
    expect(ids).toHaveLength(new Set(ids).size);
  });

  it('states the scope of a Dataset ONE Run wrote rather than omitting it', () => {
    // "Unlabelled because it happens to be unambiguous today" is how this bug arrived. The
    // second Run of the same workflow is what makes yesterday's obvious number confusing.
    const one = render(measured([['kf-dns-01', '0.1.0', 623, 'only-run']]));
    expect(text(one, 'provenance-runs-headline')).toBe('1 Run');
    expect(row(one, 'provenance-run-recorded-only-run').rows).toBe('623');
    expect(text(one, 'provenance-scope')).toContain('623 rows · every run');
  });
});

describe('scoping the console to one Run', () => {
  const TWO_RUNS = measured([
    ['kf-dns-01', '0.1.0', 623, 'r1'],
    ['kf-dns-01', '0.1.0', 623, 'r2'],
  ]);

  it('says "623 of 1,246 rows · this run" — both numbers, one statement', () => {
    const html = render(TWO_RUNS, null, { scopedRun: 'r1', onScopeRun: () => {} });
    const scope = /<span[^>]*data-testid="provenance-run-scope"[^>]*>/.exec(html)?.[0] ?? '';
    expect(html).toContain('623 of 1,246 rows · this run');
    expect(scope).toContain('data-run="r1"');
    expect(scope).toContain('data-rows="623"');
    // The share is drawn because ONE scan produced both numbers. It is not a division of the
    // panel's total by a separately-issued count.
    expect(scope).toContain('data-share="0.5000"');
  });

  it('marks the scoped Run in the list, and offers the way back to every Run', () => {
    const html = render(TWO_RUNS, null, { scopedRun: 'r1', onScopeRun: () => {} });
    expect(row(html, 'provenance-run-recorded-r1').text).toContain('623');
    expect(html).toContain('data-scoped-run="r1"');
    expect(html).toContain('aria-pressed="true"');
    expect(html).toContain('data-testid="provenance-clear-scope"');
  });

  it('still names every Run when nothing can be scoped — a name is not a link', () => {
    // Read-only: the panel is the same panel, and a Run that wrote rows is still nameable.
    const html = render(TWO_RUNS);
    expect(text(html, 'provenance-runs-headline')).toBe('2 Runs');
    expect(html).not.toContain('aria-pressed');
    expect(html).not.toContain('data-testid="provenance-run-scope"');
  });

  it('offers no scope for a Run nothing recorded — a gap is not an address', () => {
    const html = render(measured([[null, null, 5, null]]), null, {
      scopedRun: null,
      onScopeRun: () => {},
    });
    expect(row(html, 'provenance-run-unrecorded').state).toBe('unrecorded');
    expect(html).not.toContain('aria-pressed');
  });
});

describe('the panel never renders an empty grid in place of an answer', () => {
  it('says it is reading while the request is in flight', () => {
    expect(render(null)).toContain('data-state="reading"');
  });

  it('shows the server’s own sentence when the read failed', () => {
    const html = render(null, 'provenance: 502 could not read dataset provenance');
    expect(html).toContain('data-state="error"');
    expect(html).toContain('could not read dataset provenance');
  });

  it('tells an operator-loaded list apart from a Dataset whose Machines were lost', () => {
    const html = render({
      name: 'scope_paid',
      kind: 'standalone',
      rows: 0,
      groups: [],
      carriesProvenance: false,
      carriesRun: false,
      measuredAt: MEASURED_AT,
    });
    expect(html).toContain('data-state="none"');
    expect(html).toContain('an operator-loaded list has none of those columns');
    expect(html).not.toContain('not recorded');
  });

  it('answers about the dimension a table carries and stays silent about the one it lacks', () => {
    // `run_id` and the Machine/version pair are gated separately, because they arrived
    // separately. A table with one of them is answerable about that one — which is not the same
    // as a table with neither.
    const html = render({
      name: 'seeded',
      kind: 'standalone',
      rows: 3,
      groups: [{ machine: null, version: null, run: 'r1', rows: 3 }],
      carriesProvenance: false,
      carriesRun: true,
      measuredAt: MEASURED_AT,
    });
    expect(html).toContain('data-state="measured"');
    expect(text(html, 'provenance-runs-headline')).toBe('1 Run');
    expect(html).not.toContain('data-testid="provenance-machines"');
    expect(html).not.toContain('data-testid="provenance-versions"');
  });

  it('says an empty Dataset is empty rather than showing nothing at all', () => {
    const html = render(measured([]));
    expect(html).toContain('data-state="empty"');
    expect(html).toContain('No rows yet');
  });
});
