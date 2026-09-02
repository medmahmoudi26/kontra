/**
 * The URL, as a value: reading one, printing one, and the two translations between it and the store.
 *
 * THE URL NAMES WHAT YOU ARE LOOKING AT. Until this module the app had no router at all — `view`
 * was in-memory Zustand state and the bar said `/` forever, so every reload landed back on
 * Workflows, Back and Forward did nothing, and no surface could be handed to anybody. The three
 * things an operator most wants to paste to a colleague are a **Run**, a **Dataset** and the
 * workflow a run came from, so those are what carry an id.
 *
 * ADDRESS, NOT ROUTE. `route` is already taken in this codebase and it means an HTTP route on the
 * orchestrator (`run/api.ts`: "the history route", "the caller route"). `address` is the word this
 * repo already uses for the other idea — "a **Run** is addressed by its id and by nothing else",
 * "a Dataset is addressed by name + version + dt" — and an Address is exactly that written down
 * where a browser can hold it.
 *
 * THE STORE IS STILL THE API; THE ADDRESS IS A PROJECTION OF IT. `openRun`, `openWorkflow`,
 * `openDataset`, `watchTerminal` and `setView` keep their names and signatures and every caller
 * across `panels/` is untouched — they additionally write the bar. That is the layering and not an
 * accident: a surface asks the store to go somewhere, and where the store went is what the bar
 * reports. Nothing in `panels/` learns that a URL exists.
 *
 * A RUN IS NO LONGER A SURFACE, IT IS A SEGMENT UNDER ITS WORKFLOW. `/runs/<id>` became
 * `/workflows/<workflow>/<id>` when the global Runs view was retired, because a run is reached
 * through the workflow that produced it. The old address still parses and still means that run —
 * it redirects (see {@link parseAddress}) rather than 404ing, and the id survives the move. What it
 * cannot carry across is which workflow produced the run, which nothing in the browser knows: that
 * lands as `/workflows?run=<id>`, a run whose thread has not been resolved yet.
 *
 * NO ROUTER LIBRARY, and that is an argument rather than an omission. Five surfaces and three
 * entity ids is one `switch` in each direction; react-router would add a dependency, a provider and
 * a second vocabulary for `view` to a bundle already carrying 6.4 MB of JS. What it buys is nested
 * layouts, data loaders and route-level code splitting — this app has no nested layouts, loads
 * through the store, and already splits every surface with `React.lazy` in `App.tsx`. There is
 * nothing left for it to buy.
 *
 * A PATH, NOT A HASH. `backend/src/server.ts` already answers any non-`/api/` GET that is not
 * a real file with `index.html` (its `setNotFoundHandler`), so `/workflows/dnssweep/nscheck-123`
 * serves the SPA with no server change beyond the surface list it copies from `surfaces.ts`. A hash
 * could not 404 either, but `#/runs/x` is a worse thing to paste and it is never sent to the server
 * at all, which forecloses ever answering a **Run** address from the orchestrator.
 */

import { tryParseTerminalId } from '@core/panels/ids';

import { DEFAULT_VIEW, PATHS, RETIRED, type View } from './surfaces';
import type { DatasetFocus } from './store';

/**
 * Where the app is, as the bar says it.
 *
 * Three of the five surfaces address an entity and two do not, which is why this is a union and
 * not a record with three mostly-null fields: `{ view: 'actors', run: null, terminal: null,
 * dataset: null }` invites code that reads `run` on the Actors surface.
 */
export type Address =
  /**
   * The centre. Four readings, and each of them is a real place:
   *
   *  - nothing        `/workflows`            the list, with no thread open
   *  - a workflow     `/workflows/<w>`        the thread, with no conversation open
   *  - both           `/workflows/<w>/<r>`    one run of it — the thing an operator pastes
   *  - a run alone    `/workflows?run=<r>`    a run whose thread nobody has resolved yet
   *
   * The fourth is not a hole in the design, it is what a redirected `/runs/<id>` becomes and what
   * `openRun` produces when it is called from somewhere that does not know the workflow. The run is
   * a QUERY there rather than a segment because it is not addressing a position in the path — there
   * is no workflow above it to be under.
   *
   * `pane` IS A FIFTH READING OF THE SAME PLACE, and it is a QUERY for a mechanical reason as well
   * as a semantic one. Semantically it selects a **Terminal** WITHIN the open run's Monitor tab
   * rather than addressing a fourth position under it. Mechanically, a Terminal id is
   * `<mode>:<node>/<session>/<window>` — a colon, two slashes, and on tmux a dot — and the SPA
   * fallback has already 404'd a dotted id once (`surfaces.ts` records it). A query keeps every one
   * of those bytes out of the path, so what the server has to recognise stays `/workflows`.
   */
  | { view: 'workflows'; workflow: string | null; run: string | null; pane: string | null }
  | { view: 'actors' | 'settings' }
  | { view: 'monitor'; terminal: string | null }
  | { view: 'datasets'; dataset: DatasetFocus | null };

/** Where an unknown or malformed address lands, and where a cold `/` lands. */
export const DEFAULT_ADDRESS: Address = {
  view: DEFAULT_VIEW,
  workflow: null,
  run: null,
  pane: null,
};

const VIEW_BY_SEGMENT = new Map<string, View>(
  Object.entries(PATHS).map(([view, path]) => [path.slice(1), view as View])
);

/** The slice of the store the address is a projection of. Declared here so `store.ts` can hand
 *  itself over without this module knowing anything else about it. */
export interface AddressedState {
  view: View;
  workflowName: string | null;
  runId: string | null;
  /** The **Terminal** the open run's Monitor tab is showing. Named for the run rather than for the
   *  Monitor surface, which has its own {@link AddressedState.focusTerminal} and a different job:
   *  that one is a reveal that is consumed once, this one is a selection that persists. */
  runPane: string | null;
  focusTerminal: string | null;
  datasetFocus: DatasetFocus | null;
}

/**
 * The address of a given store.
 *
 * IT PROJECTS THE CURRENT SURFACE AND ONLY THAT SURFACE'S ENTITY. `setRunId` while the Datasets
 * page is open changes which **Run** the Workflows surface has selected without navigating
 * anywhere — that is the existing behaviour and the bar has to agree with it: the address stays
 * `/datasets`, because that is what is on screen. The id is not lost, it is just not this page's
 * address.
 */
export function addressOf(s: AddressedState): Address {
  switch (s.view) {
    case 'workflows':
      return { view: 'workflows', workflow: s.workflowName, run: s.runId, pane: s.runPane };
    case 'monitor':
      return { view: 'monitor', terminal: s.focusTerminal };
    case 'datasets':
      return { view: 'datasets', dataset: s.datasetFocus };
    default:
      return { view: s.view };
  }
}

/**
 * The store fields an address sets when the address is the thing driving — a cold load, or Back.
 *
 * EACH ADDRESS WRITES ITS OWN SURFACE'S FIELDS AND NOTHING ELSE. Going Back from
 * `/workflows/dnssweep/r1` to `/datasets/lame` must not blank `workflowName`: a Datasets address
 * says nothing about the Workflows surface's selection, and clearing it would lose the operator's
 * place on a page they never left. What an address DOES say about its own surface is total, `null`
 * included — `/workflows` means the list with no thread and no conversation open, which is exactly
 * why Back out of `/workflows/dnssweep` closes it.
 */
export function stateFor(address: Address): Partial<AddressedState> {
  switch (address.view) {
    case 'workflows':
      return {
        view: 'workflows',
        workflowName: address.workflow,
        runId: address.run,
        runPane: address.pane,
      };
    case 'monitor':
      return { view: 'monitor', focusTerminal: address.terminal };
    case 'datasets':
      return { view: 'datasets', datasetFocus: address.dataset };
    default:
      return { view: address.view };
  }
}

/** An address as it should appear in the bar: path plus search, no origin, no hash. */
export function formatAddress(address: Address): string {
  switch (address.view) {
    case 'workflows': {
      // A workflow name is a FOLDER name on somebody's disk and a run id is whatever `--id` was, so
      // both are encoded rather than trusted. `encodeURIComponent` leaves `.` alone, which is why
      // the server matches on the first segment — see `surfaces.ts`.
      const path =
        address.workflow === null
          ? PATHS.workflows
          : `${PATHS.workflows}/${encodeURIComponent(address.workflow)}${
              address.run === null ? '' : `/${encodeURIComponent(address.run)}`
            }`;
      // BUILT WITH `encodeURIComponent`, NOT `URLSearchParams`, and that is not a style choice:
      // `URLSearchParams` writes a space as `+` (form encoding), so `?run=wf%2Fone%20two` would
      // silently become `?run=wf%2Fone+two` — a different string in somebody's notes for the same
      // run. Both read back the same; only one of them is the address this app has always printed.
      const query: string[] = [];
      // `?run=` only where there is no workflow above it to be under. Two spellings of one address
      // is a link that does not round-trip, so the segment wins wherever it exists.
      if (address.workflow === null && address.run !== null) {
        query.push(`run=${encodeURIComponent(address.run)}`);
      }
      // The Terminal the run's Monitor tab is showing. A QUERY, always — the colon and both slashes
      // of `local:main-droplet/kontra-recon/0.1` are escaped here, so the path stays
      // `/workflows/<w>/<r>` and the id's DOT never reaches a path segment. That is the whole
      // defence against the 404 `surfaces.ts` records: on a cold load the server still only has to
      // recognise the FIRST segment, which is `workflows`.
      if (address.pane !== null) query.push(`pane=${encodeURIComponent(address.pane)}`);
      return `${path}${query.length > 0 ? `?${query.join('&')}` : ''}`;
    }
    case 'monitor':
      // A Terminal id carries a colon (`kontra-recon:0.1`) and, on tmux, a dot. Encoded rather
      // than trusted: nothing constrains the id to path-safe bytes, and one `/` in it would
      // otherwise turn into a third segment this module refuses to read back.
      return address.terminal === null
        ? PATHS.monitor
        : `${PATHS.monitor}/${encodeURIComponent(address.terminal)}`;
    case 'datasets': {
      if (address.dataset === null) return PATHS.datasets;
      const query = new URLSearchParams();
      // `kind` is OMITTED rather than defaulted, because absent is a third value and not a synonym
      // for `output`: a focus with no kind matches a Dataset of either kind, which is what a link
      // built from a name alone has to mean. `?kind=output` says output and refuses standalone.
      if (address.dataset.kind !== undefined) query.set('kind', address.dataset.kind);
      // The **Run** whose contribution the operator was reading. It scopes the console; it does not
      // pick the Dataset, so it is a search parameter and not a path segment.
      if (address.dataset.run !== undefined) query.set('run', address.dataset.run);
      const search = query.toString();
      return `${PATHS.datasets}/${encodeURIComponent(address.dataset.name)}${search ? `?${search}` : ''}`;
    }
    default:
      return PATHS[address.view];
  }
}

/**
 * `?pane=<terminal id>`, admitted or dropped.
 *
 * THE EXISTING PARSER, NOT A SECOND ONE. `@core/panels/ids.tryParseTerminalId` is the authority on
 * the id grammar — one colon, exactly two slashes, and a node admitted by the whitelist for the mode
 * it claims — and it never throws. A value it refuses is a value the streamer could not have minted,
 * so it is dropped exactly the way an unrecognised `?kind=` is: the rest of the address still lands
 * you on the run, which is the part somebody actually pasted.
 *
 * A LEGAL ID CONTAINS A COLON AND, ON TMUX, A DOT — `local:main-droplet/kontra-recon/0.1`, whose
 * window is `0.1` — and none of those bytes are in the path, so a cold load and an F5 both come back
 * as the shell. That is the property this being a query buys, and it is what `surfaces.ts`'s 404
 * says a path segment would not.
 */
function paneParam(query: URLSearchParams): string | null {
  const raw = query.get('pane');
  if (raw === null || raw === '') return null;
  return tryParseTerminalId(raw) === null ? null : raw;
}

/**
 * Read an address. `null` means "not one of ours" — the caller lands on {@link DEFAULT_ADDRESS} and
 * rewrites the bar, rather than throwing a blank page at whoever typed it.
 *
 * STRICT ABOUT THE PATH, LENIENT ABOUT THE QUERY. An unknown surface or a fourth path segment is a
 * URL this app cannot mean, so it falls back; an unrecognised `?kind=` is dropped and the rest of
 * the address still lands you on the Dataset. Either way the result round-trips through
 * {@link formatAddress}, which is what lets a cold load canonicalise `/` into `/workflows`, a junk
 * query into no query, and a retired `/runs/<id>` into its new address — each with one `replace`
 * and no history entry.
 *
 * A RETIRED ADDRESS PARSES INTO THE LIVE ONE IT MEANS, and that is the whole redirect. It is here
 * rather than in `addressing.ts` because a redirect IS a reading of a URL, and putting it here
 * makes it a property of the address space that a pure test can pin: `/runs/nscheck-123` is a run,
 * so it becomes the address that names that run; `/scratch` was a drawing surface with no subject,
 * so it becomes the list. Nothing about it is a special case at the browser boundary, and Back
 * cannot land on it again because `land()` replaces rather than pushes.
 *
 * AN ID IN AN ADDRESS IS NOT A PROMISE THAT IT EXISTS, and this module deliberately does not
 * pretend otherwise — it validates syntax and nothing else. A well-formed id for a **Run** Temporal
 * has dropped, or a **Dataset** nobody ever wrote, lands on the right surface WITH THE ID SET and
 * that surface answers for it. Neither spins forever, and neither is a case this module could
 * answer anyway — existence is a question for the server.
 */
export function parseAddress(url: string): Address | null {
  // The hash is not ours. Stripped rather than refused, so a `#anchor` cannot blank the page.
  const [beforeHash = ''] = url.split('#');
  const [path = '', search = ''] = beforeHash.split('?');

  const segments = path.split('/').filter((s) => s !== '');
  if (segments.length === 0) return DEFAULT_ADDRESS;
  if (segments.length > 3) return null;
  const [surface = '', ...rest] = segments;

  // `decodeURIComponent` throws on a malformed escape (`%zz`). That is a URL somebody typed wrong,
  // not an exception an operator should be shown. A trailing slash is no entity, not an empty one.
  let ids: string[];
  try {
    ids = rest.map((seg) => decodeURIComponent(seg));
  } catch {
    return null;
  }
  const [first = null, second = null] = ids;

  const retired = RETIRED[surface];
  if (retired !== undefined) {
    // A retired address has exactly as much depth as it ever had — `/runs/a/b` was never a URL this
    // app could mean and answering it now would be inventing history.
    if (ids.length > 1) return null;
    // A retired surface that never addressed anything still cannot: `/scratch/foo` was not a URL
    // this app could mean before, and a redirect is not a licence to start accepting it.
    if (retired.carries === 'nothing') {
      return first === null ? { view: retired.to, workflow: null, run: null, pane: null } : null;
    }
    // A retired address never carried a pane and is not given one now: a redirect moves an id, it
    // does not invent a selection the old URL could not have expressed.
    return { view: retired.to, workflow: null, run: first, pane: null };
  }

  const view = VIEW_BY_SEGMENT.get(surface);
  if (view === undefined) return null;

  switch (view) {
    case 'workflows': {
      const query = new URLSearchParams(search);
      const pane = paneParam(query);
      // `/workflows?run=<id>` — a run with no thread above it. Only readable when no workflow is
      // named, because `/workflows/<w>/<r>` already says it in the path and two spellings of one
      // address is a link that does not round-trip.
      if (first === null) {
        if (second !== null) return null;
        const run = query.get('run');
        return { view, workflow: null, run: run === null || run === '' ? null : run, pane };
      }
      return { view, workflow: first, run: second, pane };
    }
    case 'monitor':
      return second === null ? { view, terminal: first } : null;
    case 'datasets': {
      if (second !== null) return null;
      if (first === null) return { view, dataset: null };
      const query = new URLSearchParams(search);
      const kind = query.get('kind');
      const run = query.get('run');
      const dataset: DatasetFocus = { name: first };
      if (kind === 'output' || kind === 'standalone') dataset.kind = kind;
      if (run !== null && run !== '') dataset.run = run;
      return { view, dataset };
    }
    default:
      // A surface with no entity to address cannot have a second segment.
      return first === null ? { view } : null;
  }
}
