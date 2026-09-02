/**
 * The five surfaces, and the two that used to be surfaces and are not any more.
 *
 * ONE LIST, READ BY THREE THINGS. The nav rail draws it, `address.ts` gives each one a path, and
 * `backend/src/server.ts` keeps a copy so a cold load of any of them comes back as the shell.
 * It was three lists — a `VIEWS` array in `SideNav.tsx`, a `PATHS` record in `address.ts`, and a
 * `SPA_SURFACES` set on the server — and the third one has already drifted from the first two once:
 * `/datasets/acme.com` answered `404 {"error":"not found"}` on a cold load because the server was
 * asking whether the last segment looked like a file rather than whether the FIRST segment was a
 * surface. Two of the three can now only be wrong together.
 *
 * WHAT CHANGED, AND WHY THE RETIRED HALF IS HERE RATHER THAN DELETED.
 *
 *  - **Runs is retired.** A run is reached through the workflow that produced it, because a
 *    conversation belongs to a thread. The global list was a place where a run id and the code that
 *    produced it were two different searches.
 *  - **Scratch is retired as a top level.** It comes back as a workflow's own design tab, where a
 *    drawing has a subject — ADR 0026 survives unchanged and is strengthened by it.
 *  - **Settings is new.** Secrets and configuration, which have had no home at all.
 *
 * A RETIRED SURFACE IS NOT A DELETED URL. `/runs/nscheck-123` is in somebody's tab, somebody's
 * notes and somebody's Slack, and answering it with a 404 or a blank page teaches an operator that
 * links from this console rot. It is an address that still means something — it names a run — so it
 * REDIRECTS to where that run lives now and the id survives the move. {@link RETIRED} is what makes
 * that a fact about the address space rather than a special case buried in a parser, and it is why
 * the server must keep serving `/runs/*` and `/scratch/*`: the shell has to load before it can
 * redirect anything.
 */

/**
 * The surfaces, as a type.
 *
 * The union is what makes {@link SURFACE_DETAIL}'s `Record` a compile-time guard: a sixth surface
 * that nobody gives a path, a label and a hint fails to build the day it is added.
 */
export type View = 'workflows' | 'actors' | 'datasets' | 'monitor' | 'settings';

/** What the rail needs to draw one item, and what the router needs to address it. */
export interface Surface {
  id: View;
  /** The first path segment, with its leading slash. `/workflows`. */
  path: string;
  label: string;
  /** One sentence, for the tooltip. It says what the surface HOLDS, not what it is called. */
  hint: string;
}

/**
 * The order the rail draws them in, which is the order the work happens.
 *
 * Workflows first because it is the centre: a workflow is a thread and each of its runs is a
 * conversation. Actors and Datasets are what a run calls and what it produces. Monitor is the
 * machines under all of it. Settings is last because it is the only one that is not about a
 * running system.
 */
const ORDER: readonly View[] = ['workflows', 'actors', 'datasets', 'monitor', 'settings'];

const SURFACE_DETAIL: Record<View, Omit<Surface, 'id'>> = {
  workflows: {
    path: '/workflows',
    label: 'Workflows',
    hint: 'your caller workflows, each run of one, and everything that run did — a run is reached through the workflow that produced it',
  },
  actors: {
    path: '/actors',
    label: 'Actors',
    hint: 'what is deployed and registered, the Methods each Actor declares, and a form to call one',
  },
  datasets: {
    path: '/datasets',
    label: 'Datasets',
    hint: 'every Dataset ever written, filling as a run writes it — open one to query it',
  },
  monitor: {
    path: '/monitor',
    label: 'Monitor',
    hint: 'the wall of read-only Terminals over tmux',
  },
  settings: {
    path: '/settings',
    label: 'Settings',
    hint: 'secrets and configuration — named, bound to an actor’s slots, and never readable back',
  },
};

/** The five, in nav order. */
export const SURFACES: readonly Surface[] = ORDER.map((id) => ({ id, ...SURFACE_DETAIL[id] }));

/** Every live surface's path, keyed by view. The `Record` is the guard — see {@link View}. */
export const PATHS: Record<View, string> = Object.fromEntries(
  ORDER.map((id) => [id, SURFACE_DETAIL[id].path])
) as Record<View, string>;

/**
 * Where an unknown or malformed address lands, and where a cold `/` lands.
 *
 * `satisfies` RATHER THAN A `View` ANNOTATION, so the type stays the literal. `address.ts` builds
 * its default Address from this, and that address is a member of a union whose Workflows arm also
 * carries a workflow and a run — widened to `View` it stops being assignable to any single arm.
 */
export const DEFAULT_VIEW = 'workflows' satisfies View;

/**
 * A first path segment that WAS a surface, and the surface that answers for it now.
 *
 * `runs` carries an id and `scratch` never did, which is the whole difference between them at this
 * level: `/runs/<id>` has something to hand on and `/scratch` has nothing to say. Both land on
 * Workflows, because that is where a run and a workflow's sketch both live now.
 */
export const RETIRED: Record<string, { to: 'workflows'; carries: 'run' | 'nothing' }> = {
  runs: { to: 'workflows', carries: 'run' },
  scratch: { to: 'workflows', carries: 'nothing' },
};

/**
 * Every first path segment this app answers for — live and retired.
 *
 * THIS IS THE LIST `backend/src/server.ts` COPIES. It is restated there rather than imported
 * because `frontend` is a separate package the server does not depend on, and
 * `spaFallback.test.ts` pins the pair. A segment missing there 404s on cold load for any id with a
 * dot in it, which is exactly the failure that list was introduced to fix — and a RETIRED segment
 * missing there means the redirect never runs, because the shell that would perform it never loads.
 */
export const SPA_SEGMENTS: readonly string[] = [
  ...ORDER.map((id) => SURFACE_DETAIL[id].path.slice(1)),
  ...Object.keys(RETIRED),
];
