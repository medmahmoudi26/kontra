/**
 * The address, read and written.
 *
 * WHAT IS PINNED HERE IS THE ROUND TRIP, in both directions and for every surface: an address a
 * colleague pasted has to become the store, and the store has to print back the address it came
 * from. Anything that only holds one way round is a link that opens the right page once and then
 * cannot be re-shared from it.
 *
 * AND ONE THING THAT DELIBERATELY DOES NOT ROUND-TRIP: a retired address. `/runs/<id>` still names
 * a run, so it parses — into the address that run has NOW. It prints back as the new one, which is
 * exactly what makes the redirect a fact about the address space rather than a special case at the
 * browser boundary.
 */

import { describe, expect, it } from 'vitest';

import {
  DEFAULT_ADDRESS,
  addressOf,
  formatAddress,
  parseAddress,
  stateFor,
  type Address,
  type AddressedState,
} from './address';
import { SURFACES, type View } from './surfaces';

/** A store with nothing open — what the projections start from. */
const RESTING: AddressedState = {
  view: 'workflows',
  workflowName: null,
  runId: null,
  runPane: null,
  focusTerminal: null,
  datasetFocus: null,
};

/** URL → Address → store → Address → URL. The whole loop, once, for one address. */
function roundTrip(url: string): { state: Partial<AddressedState>; url: string } {
  const address = parseAddress(url);
  expect(address, `${url} did not parse`).not.toBeNull();
  const state = stateFor(address as Address);
  return { state, url: formatAddress(addressOf({ ...RESTING, ...state })) };
}

describe('the address of a surface', () => {
  const VIEWS: View[] = SURFACES.map((s) => s.id);

  it('covers all five and no more', () => {
    expect(VIEWS).toEqual(['workflows', 'actors', 'datasets', 'monitor', 'settings']);
  });

  it.each(VIEWS)('round-trips /%s through the store and back', (view) => {
    const trip = roundTrip(`/${view}`);
    expect(trip.state.view).toBe(view);
    expect(trip.url).toBe(`/${view}`);
  });

  it('lands a cold / on the default surface, and says so in the bar', () => {
    // `/` is a real thing to type and it is not a sixth state: it means Workflows, and the bar is
    // corrected to say Workflows so the address names what is on screen.
    expect(parseAddress('/')).toEqual(DEFAULT_ADDRESS);
    expect(parseAddress('')).toEqual(DEFAULT_ADDRESS);
    expect(formatAddress(DEFAULT_ADDRESS)).toBe('/workflows');
  });

  it('refuses an address this app cannot mean, rather than guessing', () => {
    // `null` is the caller's cue to fall back AND rewrite the bar. Guessing a surface from a near
    // miss would send an operator somewhere they did not ask for and say nothing about it.
    expect(parseAddress('/nope')).toBeNull();
    expect(parseAddress('/workflow')).toBeNull();
    expect(parseAddress('/Runs')).toBeNull();
    expect(parseAddress('/workflows/a/b/c')).toBeNull();
    expect(parseAddress('/datasets/a/b')).toBeNull();
    expect(parseAddress('/monitor/a/b')).toBeNull();
    // A surface with no entity to address cannot carry one.
    expect(parseAddress('/actors/foo')).toBeNull();
    expect(parseAddress('/settings/foo')).toBeNull();
  });

  it('reads a trailing slash as no entity, not as an empty one', () => {
    expect(parseAddress('/workflows/')).toEqual({
      view: 'workflows',
      workflow: null,
      run: null,
      pane: null,
    });
    expect(parseAddress('/datasets/')).toEqual({ view: 'datasets', dataset: null });
  });

  it('survives a hash it did not put there', () => {
    expect(parseAddress('/workflows/dnssweep#anything')).toEqual({
      view: 'workflows',
      workflow: 'dnssweep',
      run: null,
      pane: null,
    });
  });
});

describe('a workflow and its run in the address', () => {
  it('round-trips the thread on its own', () => {
    const trip = roundTrip('/workflows/dnssweep');
    expect(trip.state).toEqual({
      view: 'workflows',
      workflowName: 'dnssweep',
      runId: null,
      runPane: null,
    });
    expect(trip.url).toBe('/workflows/dnssweep');
  });

  it('round-trips a run under the workflow that produced it — the link an operator pastes', () => {
    const trip = roundTrip('/workflows/dnssweep/nightly-sweep-2026-08-14');
    expect(trip.state).toEqual({
      view: 'workflows',
      workflowName: 'dnssweep',
      runId: 'nightly-sweep-2026-08-14',
      runPane: null,
    });
    expect(trip.url).toBe('/workflows/dnssweep/nightly-sweep-2026-08-14');
  });

  it('carries a run whose workflow nobody has resolved, as a query rather than a segment', () => {
    // There is no thread above it to be under, and inventing one would be a URL that reads as a run
    // of a workflow that never produced it.
    const trip = roundTrip('/workflows?run=nscheck-123');
    expect(trip.state).toEqual({
      view: 'workflows',
      workflowName: null,
      runId: 'nscheck-123',
      runPane: null,
    });
    expect(trip.url).toBe('/workflows?run=nscheck-123');
  });

  it('refuses the second spelling of an address it already has', () => {
    // `/workflows/dnssweep?run=r1` would be `/workflows/dnssweep/r1` said twice; two spellings of
    // one place is a link that does not round-trip.
    expect(parseAddress('/workflows?run=r1&x=2')).toEqual({
      view: 'workflows',
      workflow: null,
      run: 'r1',
      pane: null,
    });
    expect(formatAddress({ view: 'workflows', workflow: 'dnssweep', run: 'r1', pane: null })).toBe(
      '/workflows/dnssweep/r1'
    );
  });

  it('closes the thread for /workflows with no name — the list is a place too', () => {
    // Not "leave whatever was open": Back out of `/workflows/dnssweep` has to land on the list, and
    // the only thing that can say so is this address writing `null`.
    expect(stateFor({ view: 'workflows', workflow: null, run: null, pane: null })).toEqual({
      view: 'workflows',
      workflowName: null,
      runId: null,
      runPane: null,
    });
  });

  it('survives a dot in either id — both are names somebody chose, not names we mint', () => {
    // `safeName` admits `.`, a workflow folder is whatever it is called on disk, and a run started
    // with `--id sweep-v1.2` carries one. `encodeURIComponent` does NOT escape a dot, which is why
    // the server matches the FIRST segment rather than sniffing for a file extension.
    const url = '/workflows/nscheck-0.1.0/sweep-v1.2';
    expect(parseAddress(url)).toEqual({
      view: 'workflows',
      workflow: 'nscheck-0.1.0',
      run: 'sweep-v1.2',
      pane: null,
    });
    expect(roundTrip(url).url).toBe(url);
  });

  it('encodes an id that is not path-safe, and reads it back whole', () => {
    const id = 'wf/one two';
    expect(formatAddress({ view: 'workflows', workflow: null, run: id, pane: null })).toBe(
      '/workflows?run=wf%2Fone%20two'
    );
    expect(formatAddress({ view: 'workflows', workflow: id, run: null, pane: null })).toBe(
      '/workflows/wf%2Fone%20two'
    );
    expect(parseAddress('/workflows/wf%2Fone%20two')).toEqual({
      view: 'workflows',
      workflow: id,
      run: null,
      pane: null,
    });
  });

  it('falls back on a malformed escape rather than throwing at the operator', () => {
    // `decodeURIComponent('%zz')` throws. A URL typed wrong is not a blank page.
    expect(parseAddress('/workflows/%zz')).toBeNull();
  });
});

describe('a Terminal in a run’s address', () => {
  /** A real Terminal id: `<mode>:<node>/<session>/<window>`, with a tmux window of `0.1`. One
   *  colon, two slashes and a DOT — every byte the SPA fallback was caught 404ing on. */
  const PANE = 'local:main-droplet/kontra-recon/0.1';

  it('round-trips a Machine under a run, on a cold load and on F5', () => {
    // A cold load and an F5 are the same operation as far as this module is concerned: the bar's
    // string is read, the store is landed on it, and `land()` prints it back — an address that did
    // not round-trip would be REWRITTEN on arrival, which is how a pasted link loses its pane.
    const url = `/workflows/dnssweep/sweep-1?pane=${encodeURIComponent(PANE)}`;
    const trip = roundTrip(url);
    expect(trip.state).toEqual({
      view: 'workflows',
      workflowName: 'dnssweep',
      runId: 'sweep-1',
      runPane: PANE,
    });
    expect(trip.url).toBe(url);
  });

  it('keeps the colon, the slashes and the dot OUT of the path', () => {
    // This is the whole reason it is a query. The server answers a cold load by asking whether the
    // FIRST segment is a surface (`surfaces.ts`), and the path here is `/workflows/dnssweep/sweep-1`
    // whatever the id contains — so an id with a dot in it can never reach the file sniffing that
    // 404'd `/datasets/acme.com`.
    const printed = formatAddress({
      view: 'workflows',
      workflow: 'dnssweep',
      run: 'sweep-1',
      pane: PANE,
    });
    const [path = ''] = printed.split('?');
    expect(path).toBe('/workflows/dnssweep/sweep-1');
    expect(path).not.toContain('.');
    expect(printed).toBe(
      '/workflows/dnssweep/sweep-1?pane=local%3Amain-droplet%2Fkontra-recon%2F0.1'
    );
  });

  it('carries a pane on the thread and on the list too, beside a run with no thread', () => {
    expect(parseAddress(`/workflows?run=r1&pane=${encodeURIComponent(PANE)}`)).toEqual({
      view: 'workflows',
      workflow: null,
      run: 'r1',
      pane: PANE,
    });
    expect(
      formatAddress({ view: 'workflows', workflow: null, run: 'r1', pane: PANE })
    ).toBe('/workflows?run=r1&pane=local%3Amain-droplet%2Fkontra-recon%2F0.1');
  });

  it('drops a value that is not a Terminal id, and still lands you on the run', () => {
    // LENIENT ABOUT THE QUERY, exactly as an unrecognised `?kind=` is dropped. The admission is
    // `@core/panels/ids.tryParseTerminalId` — the one parser — so a value the streamer could not
    // have minted never becomes a subscription id.
    for (const junk of ['nonsense', 'local:a/b', 'nope:main-droplet/s/w', 'fleet:main-droplet/s/w']) {
      expect(parseAddress(`/workflows/dnssweep/sweep-1?pane=${encodeURIComponent(junk)}`), junk).toEqual({
        view: 'workflows',
        workflow: 'dnssweep',
        run: 'sweep-1',
        pane: null,
      });
    }
  });
});

describe('a retired address', () => {
  it('redirects the global run list to the workflows list', () => {
    // RETIRED, NOT DELETED. It parses, so nothing 404s and nothing blanks; it prints back as the
    // address that place has now, so `land()` corrects the bar with one `replace`.
    expect(parseAddress('/runs')).toEqual({
      view: 'workflows',
      workflow: null,
      run: null,
      pane: null,
    });
    expect(formatAddress(parseAddress('/runs') as Address)).toBe('/workflows');
  });

  it('keeps the run id when it redirects one run', () => {
    // The whole reason not to 404: `/runs/nscheck-123` still NAMES something, and dropping the id
    // would land the operator on a list and lose what they pasted.
    expect(parseAddress('/runs/nscheck-123')).toEqual({
      view: 'workflows',
      workflow: null,
      run: 'nscheck-123',
      pane: null,
    });
    expect(formatAddress(parseAddress('/runs/nscheck-123') as Address)).toBe(
      '/workflows?run=nscheck-123'
    );
  });

  it('redirects a run id with a dot in it — the ids this bit us on', () => {
    expect(parseAddress('/runs/sweep-v1.2')).toEqual({
      view: 'workflows',
      workflow: null,
      run: 'sweep-v1.2',
      pane: null,
    });
  });

  it('redirects Scratch to the list, carrying nothing, because it never addressed anything', () => {
    // Scratch is a workflow's own design tab now. It had no entity to hand on: a drawing about
    // nothing is exactly what it was retired for.
    expect(parseAddress('/scratch')).toEqual({
      view: 'workflows',
      workflow: null,
      run: null,
      pane: null,
    });
    expect(formatAddress(parseAddress('/scratch') as Address)).toBe('/workflows');
  });

  it('does not invent a depth a retired address never had', () => {
    expect(parseAddress('/runs/a/b')).toBeNull();
    // `/scratch/foo` was not a URL this app could mean before, and being retired is not a licence
    // to start accepting it.
    expect(parseAddress('/scratch/anything')).toBeNull();
  });
});

describe('a Dataset in the address', () => {
  it('round-trips a bare name', () => {
    const trip = roundTrip('/datasets/lame');
    expect(trip.state).toEqual({ view: 'datasets', datasetFocus: { name: 'lame' } });
    expect(trip.url).toBe('/datasets/lame');
  });

  it('round-trips the kind and the Run it was scoped to — the link a Run hands out', () => {
    const trip = roundTrip('/datasets/lame?kind=output&run=nightly-2026-08-15');
    expect(trip.state).toEqual({
      view: 'datasets',
      datasetFocus: { name: 'lame', kind: 'output', run: 'nightly-2026-08-15' },
    });
    expect(trip.url).toBe('/datasets/lame?kind=output&run=nightly-2026-08-15');
  });

  it('keeps "no kind" distinct from "output"', () => {
    // A focus with no kind matches a Dataset of either kind — that is what a link built from a name
    // alone means, and defaulting it to `output` here would silently refuse a standalone list.
    expect(parseAddress('/datasets/lame')).toEqual({
      view: 'datasets',
      dataset: { name: 'lame' },
    });
    expect(formatAddress({ view: 'datasets', dataset: { name: 'lame' } })).toBe('/datasets/lame');
    expect(formatAddress({ view: 'datasets', dataset: { name: 'lame', kind: 'output' } })).toBe(
      '/datasets/lame?kind=output'
    );
  });

  it('round-trips a standalone list', () => {
    const trip = roundTrip('/datasets/scope?kind=standalone');
    expect(trip.state).toEqual({
      view: 'datasets',
      datasetFocus: { name: 'scope', kind: 'standalone' },
    });
    expect(trip.url).toBe('/datasets/scope?kind=standalone');
  });

  it('round-trips a name with a dot in it', () => {
    // `safeName` (data/parquet.ts) permits `.`, so `acme.com` is a Dataset somebody really has.
    expect(roundTrip('/datasets/acme.com').url).toBe('/datasets/acme.com');
  });

  it('drops a query it does not understand instead of refusing the address', () => {
    // Strict about the path, lenient about the query: a stale `?kind=` from some older link still
    // opens the Dataset, and the bar is corrected on arrival.
    expect(parseAddress('/datasets/lame?kind=banana&run=')).toEqual({
      view: 'datasets',
      dataset: { name: 'lame' },
    });
    expect(parseAddress('/datasets/lame?utm_source=slack')).toEqual({
      view: 'datasets',
      dataset: { name: 'lame' },
    });
  });
});

describe('a Terminal in the address', () => {
  it('round-trips an id with a colon in it', () => {
    const trip = roundTrip('/monitor/kontra-recon%3A0.1');
    expect(trip.state).toEqual({ view: 'monitor', focusTerminal: 'kontra-recon:0.1' });
    expect(trip.url).toBe('/monitor/kontra-recon%3A0.1');
  });
});

describe('the projection of a store', () => {
  it('addresses only the surface on screen', () => {
    // `setRunId` on the Datasets page changes the Workflows surface's selection without navigating
    // — the store's own rule. The bar has to agree: `/datasets`, because that is what is rendered.
    // The id is not lost, it is simply not this page's address.
    const url = formatAddress(
      addressOf({ ...RESTING, view: 'datasets', runId: 'r1', datasetFocus: null })
    );
    expect(url).toBe('/datasets');
  });

  it('writes only its own surface fields, so Back does not blank a neighbour', () => {
    // Back from `/workflows/dnssweep/r1` to `/datasets/lame` must not clear `workflowName`: a
    // Datasets address says nothing about the Workflows surface's selection.
    expect(stateFor({ view: 'datasets', dataset: { name: 'lame' } })).not.toHaveProperty(
      'workflowName'
    );
    expect(stateFor({ view: 'workflows', workflow: 'w', run: 'r1', pane: null })).not.toHaveProperty(
      'datasetFocus'
    );
    expect(stateFor({ view: 'monitor', terminal: null })).not.toHaveProperty('runId');
    expect(stateFor({ view: 'settings' })).toEqual({ view: 'settings' });
  });
});
