/**
 * What one run wrote, filtered out of the catalog the app already holds.
 *
 * THE THREE ATTRIBUTIONS ARE PINNED SEPARATELY because the difference decides what a count on the
 * Datasets page means. `runId` is the server saying exactly one Run is behind this partition;
 * `contributingRuns` is the honest plural; `owner` is a temporary Dataset the Run holds. A filter
 * that folded them would send an operator to a total of 1,246 rows believing their run wrote it.
 */

import { describe, expect, it } from 'vitest';

import type { DatasetInfo } from '../run/api';
import { attributionWords, runDatasets } from './runDatasets';

const AT = 1_787_084_868_000;

function ds(over: Partial<DatasetInfo> & { name: string }): DatasetInfo {
  return { kind: 'output', rows: 0, bytes: 0, ...over };
}

describe('the Datasets one run wrote', () => {
  it('says nobody has looked before the catalog has answered', () => {
    // `listedAt` is 0 until the first poll lands. A run drawn as having written nothing before the
    // lake was ever listed is the same lie as a workflow drawn as never run before its type is
    // known.
    const read = runDatasets([], 'r1', 0);
    expect(read.read).toBe(false);
    expect(read.wrote).toEqual([]);
  });

  it('finds the run under each of the three attributions, and says which', () => {
    const catalog = [
      ds({ name: 'lame', runId: 'r1', rows: 623, updatedAt: AT + 3 }),
      ds({ name: 'apexes', contributingRuns: ['r0', 'r1'], rows: 1246, updatedAt: AT + 2 }),
      ds({ name: 'tmp_batch', temporary: true, owner: 'r1', rows: 10, updatedAt: AT + 1 }),
      ds({ name: 'somebody-else', runId: 'r9', rows: 5, updatedAt: AT + 4 }),
    ];
    const read = runDatasets(catalog, 'r1', AT);
    expect(read.read).toBe(true);
    // Newest first: `lame` at +3 leads, and `somebody-else` at +4 is not this run's at all.
    expect(read.wrote.map((w) => [w.info.name, w.how])).toEqual([
      ['lame', 'sole'],
      ['apexes', 'among'],
      ['tmp_batch', 'owner'],
    ]);
  });

  it('marks an empty answer as a floor when a row cannot list its Runs', () => {
    // `contributingRunsPartial` is a data file spanning several Runs, so the list is a lower bound
    // — and a run missing from it is not evidence that it wrote nothing.
    const read = runDatasets(
      [ds({ name: 'shared', contributingRuns: ['r0'], contributingRunsPartial: true })],
      'r1',
      AT
    );
    expect(read.wrote).toEqual([]);
    expect(read.partial).toBe(true);
  });

  it('answers nothing for no run at all rather than everything', () => {
    const read = runDatasets([ds({ name: 'lame', runId: 'r1' })], null, AT);
    expect(read.wrote).toEqual([]);
    expect(read.read).toBe(true);
  });

  it('warns, in words, that a shared total is not this run’s', () => {
    expect(attributionWords('among')).toContain('not this run');
    expect(attributionWords('sole')).toContain('only one');
    expect(attributionWords('owner')).toContain('temporary');
  });
});
