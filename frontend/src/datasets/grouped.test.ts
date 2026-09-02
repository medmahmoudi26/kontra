import { describe, expect, it } from 'vitest';
import { dispatchText, groupDatasets, runText, runTitle, versionText, wholeDataset } from './grouped';
import { listingScope } from './scope';
import { queryCommand } from '../run/dataset';
import type { DatasetInfo } from '../run/api';

/**
 * The listing's grain.
 *
 * THE REPORTED BUG, VERBATIM: "it's showing 3 records of the same dataset. It's just the same
 * dataset really." Three Runs of one workflow each appended 623 rows to `lame`, and `/api/datasets`
 * returns one row per `version=…/dt=…` partition — so the operator saw `lame` three times, each
 * reading `623 rows · sealed`, and nothing said they were one thing.
 */

const AT = 1_786_895_804_639;

function ds(over: Partial<DatasetInfo> & Pick<DatasetInfo, 'name'>): DatasetInfo {
  return {
    kind: 'output',
    version: '0.1.0',
    rows: 623,
    bytes: 38_777,
    updatedAt: AT,
    ...over,
  } as DatasetInfo;
}

/** The response that produced the screenshot, newest dispatch first as the server orders it. */
const LAME: DatasetInfo[] = [
  ds({ name: 'lame', dt: '2026-08-16T15-52-51', updatedAt: 3, state: 'sealed' }),
  ds({ name: 'lame', dt: '2026-08-16T15-20-54', updatedAt: 2, state: 'sealed' }),
  ds({ name: 'lame', dt: '2026-08-15T18-37-25', updatedAt: 1, state: 'sealed' }),
];

describe('three rows of one Dataset become one row', () => {
  it('lists the NAME once', () => {
    const groups = groupDatasets(LAME, AT);
    expect(groups).toHaveLength(1);
    expect(groups[0]!.name).toBe('lame');
    expect(dispatchText(groups[0]!)).toBe('3 dispatches');
  });

  it('adds the dispatches up instead of showing one of them three times', () => {
    const [g] = groupDatasets(LAME, AT);
    expect(g!.total.total.rows).toBe(1869);
    expect(g!.total.total.scope).toBe('dataset');
    expect(g!.bytes).toBe(38_777 * 3);
  });

  it('keeps every dispatch, in the order the server sent them', () => {
    // The fold must not be a summary. A dispatch is still addressable — it is what the write-rate
    // series is keyed on and what a scoped console opens.
    const [g] = groupDatasets(LAME, AT);
    expect(g!.dispatches.map((d) => d.dt)).toEqual([
      '2026-08-16T15-52-51',
      '2026-08-16T15-20-54',
      '2026-08-15T18-37-25',
    ]);
  });

  it('reports the NEWEST dispatch as when the Dataset was written', () => {
    const [g] = groupDatasets(LAME, AT);
    expect(g!.updatedAt).toBe(3);
  });

  it('carries the lifecycle, which is recorded per name', () => {
    const [g] = groupDatasets(LAME, AT);
    expect(wholeDataset(g!).state).toBe('sealed');
  });
});

describe('what a group hands the console', () => {
  it('opens over the whole Dataset, so the header and the provenance panel agree', () => {
    // Before: the header counted the ONE dispatch that was clicked (623) while the provenance panel
    // below it counted the name (1,869). Both correct, unlabelled, and about different rows.
    const whole = wholeDataset(groupDatasets(LAME, AT)[0]!);
    expect(whole.rows).toBe(1869);
    expect(whole.version).toBeUndefined();
    expect(whole.dt).toBeUndefined();
    // Which is what makes the console say "every run" rather than "this dispatch".
    expect(listingScope(whole)).toBe('dataset');
  });

  it('hands over an UNSCOPED CLI command', () => {
    // `--version`/`--dt` prune to one partition. Opening the name and being given a command that
    // reads one dispatch of it is the same mislabelling wearing a different hat.
    const cmd = queryCommand(wholeDataset(groupDatasets(LAME, AT)[0]!));
    expect(cmd).not.toContain('--dt');
    expect(cmd).not.toContain('--version');
    expect(cmd).toContain('kontra dataset query lame');
  });
});

describe('what must not be folded together', () => {
  it('keeps an operator-loaded list separate from an actor output of the same name', () => {
    // They live in different DuckLake schemas. One row spanning both would total across two tables
    // and open only one of them.
    const groups = groupDatasets(
      [
        ds({ name: 'domains', kind: 'standalone', rows: 100, version: undefined, dt: undefined }),
        ds({ name: 'domains', dt: '2026-08-16T15-52-51', rows: 5 }),
      ],
      AT
    );
    expect(groups).toHaveLength(2);
    expect(groups.map((g) => g.kind).sort()).toEqual(['output', 'standalone']);
    expect(groups.find((g) => g.kind === 'standalone')!.total.total.rows).toBe(100);
  });

  it('says "standalone list" rather than "1 dispatch" for a list nobody dispatched', () => {
    const [g] = groupDatasets(
      [ds({ name: 'domains', kind: 'standalone', version: undefined, dt: undefined })],
      AT
    );
    expect(dispatchText(g!)).toBe('standalone list');
    // And its total is spoken of as the whole list, never as "every run" — no Run wrote it.
    expect(g!.total.kind).toBe('standalone');
  });
});

describe('two Actor versions under one name', () => {
  const MIXED: DatasetInfo[] = [
    ds({ name: 'lame', version: '0.2.0', dt: '2026-08-16T15-52-51', rows: 10 }),
    ds({ name: 'lame', version: '0.1.0', dt: '2026-08-15T18-37-25', rows: 5 }),
  ];

  it('is a real state, reported rather than flattened away', () => {
    const [g] = groupDatasets(MIXED, AT);
    expect(g!.versions).toEqual(['0.2.0', '0.1.0']);
    expect(versionText(g!)).toBe('v0.2.0, v0.1.0');
    expect(g!.total.total.rows).toBe(15);
  });

  it('deduplicates a version that wrote more than once', () => {
    const [g] = groupDatasets(LAME, AT);
    expect(g!.versions).toEqual(['0.1.0']);
  });
});

describe('a temporary Dataset carries its temp-ness and owner through the fold', () => {
  // A temp is framework-named and owned by exactly one Run (temp-datasets slice 01); the listing
  // marks it `temporary` with an `owner`, and the fold must keep both so slice 04's row can show the
  // owner and offer deletion. Temp-ness is read off the member (the server's owner marker), never
  // the `tmp_` name prefix — a durable Dataset that happens to start `tmp_` is not one.
  it('threads temporary + owner onto the group', () => {
    const [g] = groupDatasets(
      [ds({ name: 'tmp_a7f3', dt: '2026-08-18T10-00-00', temporary: true, owner: 'run-42', state: 'open' })],
      AT
    );
    expect(g!.temporary).toBe(true);
    expect(g!.owner).toBe('run-42');
    expect(wholeDataset(g!).temporary).toBe(true);
    expect(wholeDataset(g!).owner).toBe('run-42');
  });

  it('leaves a durable Dataset non-temporary with no owner', () => {
    const [g] = groupDatasets(LAME, AT);
    expect(g!.temporary).toBe(false);
    expect(g!.owner).toBeUndefined();
    expect(wholeDataset(g!).temporary).toBeUndefined();
  });
});

describe('a Dataset record threads onto the group (ADR 0029 §1, §4)', () => {
  // The tag set, the rename and the runId that keys them are read off the NEWEST resolved dispatch —
  // the Run an operator watching a live Dataset is looking at, and the same one `groupDatasetName`
  // names. A group spanning two Runs shows the newer's record; the older is one expansion down.
  it('reads runId, tags and rename off the newest resolved dispatch', () => {
    const [g] = groupDatasets(
      [
        ds({
          name: 'lame',
          dt: '2026-08-16T15-52-51',
          updatedAt: 3,
          runId: 'run-newer',
          datasetName: 'wf-lame-0.1.0--2026-08-16T15-52-51Z--run-ne',
          tags: ['prod', 'nightly'],
          renamedTo: 'the-sweep',
        }),
        ds({
          name: 'lame',
          dt: '2026-08-15T18-37-25',
          updatedAt: 1,
          runId: 'run-older',
          datasetName: 'wf-lame-0.1.0--2026-08-15T18-37-25Z--run-ol',
          tags: ['old'],
        }),
      ],
      AT
    );
    expect(g!.runId).toBe('run-newer');
    expect(g!.tags).toEqual(['prod', 'nightly']);
    expect(g!.renamedTo).toBe('the-sweep');
  });

  it('leaves an untagged, un-renamed Dataset with no runId affordance data', () => {
    const [g] = groupDatasets(LAME, AT);
    // LAME rows carry no runId (the ledger join is server-side); the group reports empty tags and no
    // rename, which is what "only deviation is stored" looks like on the read side.
    expect(g!.runId).toBeUndefined();
    expect(g!.tags).toEqual([]);
    expect(g!.renamedTo).toBeUndefined();
  });
});

describe("the total is one poll's answer", () => {
  it('is stamped with the moment that measured it, not with now', () => {
    // Every row summed here came from the same `/api/datasets` response, so the total is a fact
    // about that response's moment — which is why the moment travels with it.
    const [g] = groupDatasets(LAME, AT);
    expect(g!.total.total.measuredAt).toBe(AT);
    expect(g!.total.total.statement).toContain(String(AT));
  });

  it('is empty for an empty catalog rather than inventing a group', () => {
    expect(groupDatasets([], AT)).toEqual([]);
  });
});

/**
 * WHICH RUN MADE THIS — the question the page could not answer at all. A durable Dataset
 * accumulates across Runs (the three `lame` dispatches above are three of them), so the answer is
 * a SET, and the single `runId` beside it is only the Run whose record the tag/rename affordance
 * addresses. Two fields, two meanings, never merged (ADR 0017).
 */
describe('which Runs contributed to a Dataset', () => {
  it('unions every dispatch\'s Runs, deduplicated and ordered', () => {
    const [g] = groupDatasets(
      [
        ds({ name: 'lame', dt: '2026-08-16T15-52-51', contributingRuns: ['nscheck-3'] }),
        ds({ name: 'lame', dt: '2026-08-16T15-20-54', contributingRuns: ['nscheck-2', 'nscheck-3'] }),
        ds({ name: 'lame', dt: '2026-08-15T18-37-25', contributingRuns: ['nscheck-1'] }),
      ],
      AT
    );
    expect(g!.contributingRuns).toEqual(['nscheck-1', 'nscheck-2', 'nscheck-3']);
    expect(g!.contributingRunsPartial).toBe(false);
    // COUNTED, not named: naming the first of three would be exactly the lie the plural prevents.
    expect(runText(g!)).toBe('from 3 runs');
    expect(runTitle(g!)).toContain('nscheck-2');
  });

  it('NAMES the Run when a Dataset has exactly one', () => {
    // `lame_demo`: 430 rows promoted out of one Run's temp. This is the row that read
    // `runId=None, datasetName=None` on the local controller.
    const [g] = groupDatasets(
      [ds({ name: 'lame_demo', dt: '2026-08-19T14-49-20', contributingRuns: ['nscheck-1787150959'] })],
      AT
    );
    expect(runText(g!)).toBe('from nscheck-1787150959');
  });

  it('says "at least" when a data file spans Runs, because a bound is not a set', () => {
    const [g] = groupDatasets(
      [
        ds({
          name: 'lame',
          dt: '2026-08-16T15-52-51',
          contributingRuns: ['nscheck-1', 'nscheck-9'],
          contributingRunsPartial: true,
        }),
      ],
      AT
    );
    expect(g!.contributingRunsPartial).toBe(true);
    expect(runText(g!)).toBe('from at least 2 runs');
    expect(runTitle(g!)).toContain('at least');
  });

  it('says NOTHING for a Dataset whose rows carry no Run', () => {
    // An operator-loaded list has no `run_id` column. `from 0 runs` would invite the reader to
    // wonder which ones went missing.
    const [g] = groupDatasets([ds({ name: 'domains', kind: 'standalone', dt: undefined })], AT);
    expect(g!.contributingRuns).toEqual([]);
    expect(runText(g!)).toBe('');
    expect(runTitle(g!)).toBe('');
  });
});
