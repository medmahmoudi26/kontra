import { describe, expect, it } from 'vitest';

import type { DatasetInfo } from '../run/api';
import { localDatasetName } from '../datasets/localName';
import { groupDatasetName } from '../datasets/grouped';

/**
 * The DERIVED, run-grain name on the Datasets page (ADR 0029 §2).
 *
 * The string is the SERVER's — rendered once through `data/datasetName.ts` and shipped on the row —
 * so the page never re-derives it and cannot drift from `kontra dataset ls`. This page only chooses
 * WHICH dispatch's name a group row shows (the newest that has one) and whether to show one at all.
 * Pure, so it is pinned here without mounting the grid.
 */

const dispatch = (o: Partial<DatasetInfo> = {}): DatasetInfo => ({
  kind: 'output',
  name: 'lame',
  version: '0.1.0',
  dt: '2026-08-19T14-32-07',
  rows: 1,
  bytes: 0,
  ...o,
});

describe('groupDatasetName — which derived name a group row shows', () => {
  it('shows the newest dispatch that carries one', () => {
    // Dispatches arrive newest-first; the first with a server-rendered name wins — the Run an
    // operator watching a live Dataset is looking at.
    const g = {
      dispatches: [
        dispatch({ dt: '2026-08-20T09-00-00', datasetName: 'wf-lame-0.1.0--2026-08-20T09-00-00Z--newer0' }),
        dispatch({ dt: '2026-08-19T14-32-07', datasetName: 'wf-lame-0.1.0--2026-08-19T14-32-07Z--older0' }),
      ],
    };
    expect(groupDatasetName(g)).toBe('wf-lame-0.1.0--2026-08-20T09-00-00Z--newer0');
  });

  it('skips a nameless newest dispatch and shows the first that has one', () => {
    // A promoted-in partition carries no name; a real one behind it does, and the row shows that
    // rather than nothing.
    const g = {
      dispatches: [dispatch({ datasetName: undefined }), dispatch({ datasetName: 'wf-lame-0.1.0--2026-08-19T14-32-07Z--a3f9c1' })],
    };
    expect(groupDatasetName(g)).toBe('wf-lame-0.1.0--2026-08-19T14-32-07Z--a3f9c1');
  });

  it('shows nothing for a group whose dispatches all lack a name (a standalone list)', () => {
    const g = { dispatches: [dispatch({ kind: 'standalone', version: undefined, dt: undefined, datasetName: undefined })] };
    expect(groupDatasetName(g)).toBeUndefined();
  });

  it('shows the operator RENAME over the derived default when the record holds one (ADR 0029 §4)', () => {
    // The rename overrides the derived name on the same dispatch; the derived string is still on the
    // row (issue 01's invariant) but the label an operator reads is what they called it.
    const g = {
      dispatches: [
        dispatch({
          datasetName: 'wf-lame-0.1.0--2026-08-19T14-32-07Z--a3f9c1',
          renamedTo: 'the-interesting-sweep',
        }),
      ],
    };
    expect(groupDatasetName(g)).toBe('the-interesting-sweep');
  });
});

/**
 * What the ROW actually draws: the server's name, with its instant in the viewer's zone. The two
 * halves of ADR 0029 §2 meet exactly here — the string stays UTC on the wire and in the CLI, and
 * the page renders local, with the canonical form one hover away.
 */
describe('the name a row draws is the server\'s, read in local time', () => {
  it('localises the derived name and keeps the canonical string beside it', () => {
    const g = {
      dispatches: [
        dispatch({ datasetName: 'wf-nscheck-0.1.0--2026-08-19T14-49-20Z--4e9b1b', runId: 'nscheck-1787150959' }),
      ],
    };
    const shown = localDatasetName(groupDatasetName(g), { locale: 'en-GB', timeZone: 'Europe/Paris' })!;
    expect(shown.text).toBe('wf-nscheck-0.1.0--19 Aug 2026 16:49:20--4e9b1b');
    // The identity never moved: this is what `kontra dataset ls` prints and what a rename replaces.
    expect(shown.canonical).toBe('wf-nscheck-0.1.0--2026-08-19T14-49-20Z--4e9b1b');
    expect(shown.localized).toBe(true);
  });

  it('draws a RENAME verbatim — there is no instant in an operator\'s words', () => {
    const g = {
      dispatches: [
        dispatch({
          datasetName: 'wf-nscheck-0.1.0--2026-08-19T14-49-20Z--4e9b1b',
          renamedTo: 'the-interesting-sweep',
        }),
      ],
    };
    const shown = localDatasetName(groupDatasetName(g))!;
    expect(shown.text).toBe('the-interesting-sweep');
    expect(shown.localized).toBe(false);
  });
});
