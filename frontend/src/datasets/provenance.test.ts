/**
 * The three values a reader must tell apart, and the counting rules that follow from them.
 *
 * The bug being fixed reads back as data, not as an exception: `lame` holds 1,246 rows all
 * carrying `('w','0')`, and a console that counts that as "1 Machine" makes a four-Machine run
 * look like a one-Machine run — the same class of lie as a `completed` run that produced nothing.
 */

import { describe, expect, it } from 'vitest';

import {
  PROVENANCE_STATES,
  datasetCount,
  drawsDistribution,
  entriesOf,
  headline,
  isLegacy,
  provenanceBadge,
  recorded,
  runCount,
  runScope,
  type DatasetProvenance,
} from './provenance';
import { share } from './scope';

/**
 * A Dataset's provenance as the server measures it: buckets, and the total from the same scan.
 *
 * The Run defaults to one id, because a Dataset written by exactly one Run is the ordinary case —
 * and it still names it. Tests that care about the multi-Run case say so in the fourth slot.
 */
function measured(
  groups: Array<[string | null, string | null, number, (string | null)?]>
): DatasetProvenance {
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

/** The measured fleet run: three Machines produced rows, one produced none. */
const FLEET = measured([
  ['kf-dns-01', '0.1.0', 600],
  ['kf-dns-02', '0.1.0', 400],
  ['kf-dns-03', '0.1.0', 246],
]);

/** Every row written before provenance travelled with the Batch — `lame`'s real shape today. */
const ALL_LEGACY = measured([['w', '0', 1246]]);

/** Every row from a Batch nothing recorded a Machine for. */
const ALL_UNRECORDED = measured([[null, null, 1246]]);

describe('the three states are never folded into one another', () => {
  it('counts a recorded hostname as a Machine', () => {
    const entries = entriesOf(FLEET, 'machine');
    expect(entries.map((e) => e.value)).toEqual(['kf-dns-01', 'kf-dns-02', 'kf-dns-03']);
    expect(entries.every((e) => e.state === 'recorded')).toBe(true);
    expect(headline(entries, 'Machine')).toBe('3 Machines');
  });

  it('reads a NULL as a gap, never as a value', () => {
    const entries = entriesOf(ALL_UNRECORDED, 'machine');
    expect(entries).toHaveLength(1);
    expect(entries[0]!.state).toBe('unrecorded');
    expect(entries[0]!.value).toBeNull();
    expect(entries[0]!.label).toBe('not recorded');
    // A gap is not a Machine. Counting it would invent one out of the absence of one.
    expect(recorded(entries)).toHaveLength(0);
    expect(headline(entries, 'Machine')).toBe('no Machine recorded');
  });

  it('reads the legacy placeholder as a recorded value that names no Machine', () => {
    // The decision this slice had to make: `('w','0')` is most of the data. It is NOT unrecorded
    // — something really did write it — but it names no Machine, so it cannot be counted as one.
    const entries = entriesOf(ALL_LEGACY, 'machine');
    expect(entries).toHaveLength(1);
    expect(entries[0]!.state).toBe('legacy');
    expect(entries[0]!.value).toBe('w');
    expect(entries[0]!.rows).toBe(1246);
    expect(headline(entries, 'Machine')).toBe('no Machine recorded');
    // …and it says what it is, in words, not only in colour.
    expect(entries[0]!.label).toBe('w (placeholder)');
  });

  it('keeps all three apart in one Dataset', () => {
    const entries = entriesOf(
      measured([
        ['kf-dns-01', '0.1.0', 10],
        ['w', '0', 5],
        [null, null, 2],
      ]),
      'machine'
    );
    expect(entries.map((e) => e.state)).toEqual(['recorded', 'legacy', 'unrecorded']);
    expect(entries.map((e) => e.rows)).toEqual([10, 5, 2]);
    // One Machine, not three: only the measured hostname is one.
    expect(headline(entries, 'Machine')).toBe('1 Machine');
  });

  it('never merges a real value with the placeholder that happens to spell the same', () => {
    // A Machine genuinely named `w` on a real Actor version is not the placeholder — the pair is
    // the fingerprint, and the two stay separate buckets rather than one summed lie.
    const entries = entriesOf(
      measured([
        ['w', '2.0.0', 3],
        ['w', '0', 7],
      ]),
      'machine'
    );
    expect(entries.map((e) => [e.state, e.rows])).toEqual([
      ['legacy', 7],
      ['recorded', 3],
    ]);
    expect(new Set(entries.map((e) => e.key)).size).toBe(2);
  });
});

describe('the legacy fingerprint is the PAIR', () => {
  it('is the exact pair one line of the old publish activity wrote', () => {
    expect(isLegacy({ machine: 'w', version: '0' })).toBe(true);
  });

  it('leaves an Actor whose version really is 0 alone', () => {
    // Requiring both is what keeps a real deployment from being relabelled a dead placeholder.
    expect(isLegacy({ machine: 'kf-dns-01', version: '0' })).toBe(false);
    expect(isLegacy({ machine: 'w', version: '1.0.0' })).toBe(false);
    expect(isLegacy({ machine: null, version: null })).toBe(false);
  });
});

describe('Actor versions are read the same way as Machines', () => {
  it('names every version present, which is the fact before any comparison across them', () => {
    const entries = entriesOf(
      measured([
        ['kf-dns-01', '0.1.0', 10],
        ['kf-dns-01', '0.2.0', 4],
      ]),
      'version'
    );
    expect(entries.map((e) => e.value)).toEqual(['0.1.0', '0.2.0']);
    expect(headline(entries, 'Actor version')).toBe('2 Actor versions');
  });

  it('folds one Machine’s two versions into ONE Machine that contributed the sum', () => {
    const entries = entriesOf(
      measured([
        ['kf-dns-01', '0.1.0', 10],
        ['kf-dns-01', '0.2.0', 4],
      ]),
      'machine'
    );
    expect(entries).toHaveLength(1);
    expect(entries[0]!.rows).toBe(14);
    expect(headline(entries, 'Machine')).toBe('1 Machine');
  });

  it('reads the legacy version `0` as a placeholder, not as a deployed version', () => {
    const entries = entriesOf(ALL_LEGACY, 'version');
    expect(entries[0]!.state).toBe('legacy');
    expect(headline(entries, 'Actor version')).toBe('no Actor version recorded');
  });
});

describe('nothing is drawn that the data does not contain', () => {
  it('draws no distribution over a single bucket', () => {
    // A lone bar at 100% claims a distribution about something that has none — and with one
    // Machine the view says one Machine.
    expect(drawsDistribution(entriesOf(FLEET, 'version'))).toBe(false);
    expect(drawsDistribution(entriesOf(ALL_LEGACY, 'machine'))).toBe(false);
    expect(drawsDistribution(entriesOf(ALL_UNRECORDED, 'machine'))).toBe(false);
    expect(drawsDistribution(entriesOf(FLEET, 'machine'))).toBe(true);
  });

  it('takes every share from the ONE total the server counted', () => {
    // Never `rows / (a second count(*))`: a Dataset a Run is still appending to grows between two
    // queries, and the ratio would be wrong by whatever landed in between.
    const entries = entriesOf(FLEET, 'machine');
    expect(entries.reduce((n, e) => n + e.rows, 0)).toBe(FLEET.rows);
    expect(entries[0]!.share).toBeCloseTo(600 / 1246, 10);
    expect(entries.reduce((n, e) => n + e.share, 0)).toBeCloseTo(1, 10);
  });

  it('has no bucket at all for an empty Dataset', () => {
    const empty: DatasetProvenance = { ...FLEET, rows: 0, groups: [] };
    expect(entriesOf(empty, 'machine')).toEqual([]);
    expect(headline([], 'Machine')).toBe('no Machine recorded');
  });
});

/**
 * WHICH RUN THIS COUNT IS ABOUT — the dimension the whole slice exists for.
 *
 * `SELECT count(*) FROM lame` returns 1,246 (two Runs of one workflow) and the Datasets listing
 * shows 623 (one of them). Both correct, neither labelled, and an operator comparing them
 * concludes something is broken.
 */
describe('a Dataset name spans Runs, and every count says which', () => {
  /** `lame`'s measured shape: two Runs, one Dataset name, the same dead placeholder on both. */
  const TWO_RUNS = measured([
    ['w', '0', 623, 'nightly-2026-08-14'],
    ['w', '0', 623, 'nightly-2026-08-15'],
  ]);

  it('names each Run and what it contributed', () => {
    const entries = entriesOf(TWO_RUNS, 'run');
    expect(entries.map((e) => [e.value, e.rows])).toEqual([
      ['nightly-2026-08-14', 623],
      ['nightly-2026-08-15', 623],
    ]);
    expect(headline(entries, 'Run')).toBe('2 Runs');
  });

  it('counts a Run whose rows carry the dead placeholder as a Run all the same', () => {
    // The placeholder is a claim about the Machine and the Actor version — `run_id` was written
    // for real throughout. Striking these Runs through, or leaving them out of the count, would
    // delete a fact the lake holds: `lame` has NO Machine recorded and TWO Runs.
    const runs = entriesOf(TWO_RUNS, 'run');
    expect(runs.every((e) => e.state === 'recorded')).toBe(true);
    expect(headline(entriesOf(TWO_RUNS, 'machine'), 'Machine')).toBe('no Machine recorded');
  });

  it('folds one Run’s Machines into that Run’s contribution', () => {
    // No bucket answers this alone: a Run's rows are spread across the Machines that served it,
    // and a Machine's rows across the Runs it served. Both dimensions come out of one fold.
    const spread = measured([
      ['kf-dns-01', '0.1.0', 400, 'r1'],
      ['kf-dns-02', '0.1.0', 223, 'r1'],
      ['kf-dns-01', '0.1.0', 623, 'r2'],
    ]);
    expect(entriesOf(spread, 'run').map((e) => [e.value, e.rows])).toEqual([
      ['r1', 623],
      ['r2', 623],
    ]);
    expect(entriesOf(spread, 'machine').map((e) => [e.value, e.rows])).toEqual([
      ['kf-dns-01', 1023],
      ['kf-dns-02', 223],
    ]);
  });

  it('states the scope of a Dataset exactly ONE Run wrote, rather than omitting it', () => {
    // "Unlabelled because it happens to be unambiguous today" is how this bug arrived: the second
    // Run of the same workflow is what turns yesterday's obvious number into today's confusing
    // one, and nothing warns you on the day it happens.
    const entries = entriesOf(FLEET, 'run');
    expect(entries).toHaveLength(1);
    expect(entries[0]!.value).toBe('r1');
    expect(headline(entries, 'Run')).toBe('1 Run');
    // The whole Dataset is that Run's, and the sentence says both numbers anyway.
    expect(runScope(FLEET, 'r1').sentence).toBe('1,246 of 1,246 rows · this run');
  });

  it('reads a Run nothing recorded as a gap, never as a Run', () => {
    const entries = entriesOf(measured([['kf-dns-01', '0.1.0', 5, null]]), 'run');
    expect(entries[0]!.state).toBe('unrecorded');
    expect(headline(entries, 'Run')).toBe('no Run recorded');
  });
});

describe('scoping a count to one Run', () => {
  const TWO_RUNS = measured([
    ['kf-dns-01', '0.1.0', 623, 'r1'],
    ['kf-dns-01', '0.1.0', 623, 'r2'],
  ]);

  it('forms "623 of 1,246 rows · this run" out of ONE statement', () => {
    const scope = runScope(TWO_RUNS, 'r1');
    expect(scope.sentence).toBe('623 of 1,246 rows · this run');
    expect(scope.part.rows).toBe(623);
    expect(scope.whole.rows).toBe(1246);
    // Divisible precisely BECAUSE one scan produced both: same statement, same moment.
    expect(scope.part.statement).toBe(scope.whole.statement);
    expect(scope.share).toBeCloseTo(0.5, 10);
    expect(scope.part.measuredAt).toBe(MEASURED_AT);
  });

  it('scopes each dimension out of the same buckets, so the parts sum to the whole', () => {
    const perRun = entriesOf(TWO_RUNS, 'run').reduce((n, e) => n + e.rows, 0);
    expect(perRun).toBe(datasetCount(TWO_RUNS).rows);
    expect(runCount(TWO_RUNS, 'r1').rows + runCount(TWO_RUNS, 'r2').rows).toBe(TWO_RUNS.rows);
  });

  it('reports zero for a Run that wrote nothing under this name, and never a share of it', () => {
    // A Run that produced no rows is ABSENT from a Dataset, exactly as a Machine that produced
    // none is. Asking about it answers 0 — which is a count, not a missing number, and it is said
    // in the same shape as every other count rather than as a blank.
    const none = runCount(TWO_RUNS, 'never-ran');
    expect(none.rows).toBe(0);
    expect(share(none, datasetCount(TWO_RUNS))).toBe(0);
    expect(runScope(TWO_RUNS, 'never-ran').sentence).toBe('0 of 1,246 rows · this run');
  });

  it('refuses to divide a count from a different statement', () => {
    // The rule the whole vocabulary exists to enforce: two reads of the same Dataset are two
    // moments, so their ratio is not a fact about either.
    const later = { ...TWO_RUNS, measuredAt: MEASURED_AT + 2000, rows: 1300 };
    expect(share(runCount(TWO_RUNS, 'r1'), datasetCount(later))).toBeNull();
  });
});

describe('the states are told apart on sight', () => {
  it('gives each state its own styling, glyph and sentence', () => {
    const drawn = PROVENANCE_STATES.map(provenanceBadge);
    expect(new Set(drawn.map((b) => b.className)).size).toBe(drawn.length);
    expect(new Set(drawn.map((b) => b.glyph)).size).toBe(drawn.length);
    for (const b of drawn) expect(b.title.length).toBeGreaterThan(20);
  });

  it('says outright that no Machine named `w` exists', () => {
    // The whole hazard of showing the placeholder verbatim: a reader must not walk away
    // believing in a host called `w`.
    expect(provenanceBadge('legacy').title).toMatch(/No Machine named `w` exists/);
    // …and it is struck through, so the value reads as naming nothing even before the tooltip.
    expect(provenanceBadge('legacy').valueClassName).toContain('line-through');
  });

  it('draws the gap with a dashed border rather than a paler shade of a value', () => {
    // A lighter grey is how "we don't know" reads as "fine" at a glance.
    expect(provenanceBadge('unrecorded').className).toContain('border-dashed');
    expect(provenanceBadge('recorded').className).toContain('border-solid');
  });
});
