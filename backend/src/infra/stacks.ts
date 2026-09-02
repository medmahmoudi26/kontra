/**
 * Stack FQN → program. The ONE dispatch table for infrastructure (ADR 0019).
 *
 * Keeping this a table rather than letting callers pass arbitrary programs is deliberate: an
 * HTTP route decides *which* stack to act on, never *what* the stack contains. A caller that
 * could supply the program could provision anything the credential allows.
 *
 * SINCE ADR 0034 §4 THE TABLE ALSO DECIDES WHICH CREDENTIAL AND WHICH PROVIDER VARIABLE, and that
 * is the same property one level down: a caller names a credential, and the table decides what
 * that name is *for*. A second provider adds a second project here — a second program, a second
 * `coerce*Args`, and the environment variable that provider's SDK reads — and it does not add a
 * way to pass a program or to point a credential at something the table did not choose.
 *
 * THE CREDENTIAL IS NOT PART OF `FleetArgs`, and that is structural rather than tidy. `FleetArgs`
 * is what reaches `programs/fleet.ts` — the only provider-coupled file, the one that writes
 * cloud-init and the remote install command that runs on every **Machine**. A credential field on
 * that type would be one careless interpolation away from a token in a world-readable Bundle, so
 * the type does not have one: the reference travels beside the args, to `workspace.ts`, and the
 * program never sees it (ADR 0034 §4's fifth negative; `machineSecret.test.ts` sweeps for it).
 */

import { credentialFrom } from './credential';
import { fleetProgram, type FleetArgs, type PlacementArgs } from './programs/fleet';
import { parseFqn } from './workspace';
import type { SecretRef } from '../secrets/types';

/** Projects we know how to build. A stack outside these is refused. */
export const FLEET_PROJECT = 'kontra-fleet';

/** The variable the DigitalOcean SDK reads its token from. One line per provider, here, because
 *  this is the file that already knows which cloud a project is. */
const DO_TOKEN_VAR = 'DIGITALOCEAN_TOKEN';

export interface StackInput {
  /** Arguments for the stack's program, validated by the program itself. */
  args?: Record<string, unknown>;
}

/**
 * Everything one operation needs: what to build, and the NAME of the credential to build it with.
 *
 * The two are produced together, from one parse of one request, so they cannot come apart — a
 * converge that ran the fleet program with some other stack's credential would be a way to
 * provision in an account the caller did not name.
 */
export interface StackPlan {
  program: () => Promise<Record<string, unknown> | void>;
  /** A NAME, and optionally a pinned version. Never a value — see the header. */
  credential: SecretRef;
  /** Where that value goes at the last hop, and nowhere before it. */
  providerEnvVar: string;
}

export function planFor(input: { stackFqn: string } & StackInput): StackPlan {
  const { project } = parseFqn(input.stackFqn);
  switch (project) {
    case FLEET_PROJECT:
      return {
        program: fleetProgram(coerceFleetArgs(input.args ?? {})),
        credential: credentialFrom(input.args),
        providerEnvVar: DO_TOKEN_VAR,
      };
    default:
      throw new Error(
        `unknown infra project ${JSON.stringify(project)}; expected one of: ${FLEET_PROJECT}`
      );
  }
}

/** Just the program, for the call sites that only build one. {@link planFor} is what an operation
 *  uses — a program without the credential it was planned with cannot converge. */
export function programFor(input: { stackFqn: string } & StackInput) {
  return planFor(input).program;
}

/**
 * Narrow untrusted JSON into FleetArgs. The program validates the tag and machine count itself;
 * this only ensures the shapes are right so a string `machines` cannot reach the provider.
 *
 * `credential` IS DELIBERATELY NOT IN THE LIST. It is read by {@link planFor} into the plan and
 * never into the program — see the header for why the type that reaches a Machine has no room for
 * it. A caller that sends `credential` gets it honoured; a caller that sends a credential VALUE
 * under any spelling gets it dropped here, silently, the way `evil: 'rm -rf'` is.
 */
export function coerceFleetArgs(raw: Record<string, unknown>): FleetArgs {
  const tag = typeof raw.tag === 'string' ? raw.tag : '';
  const machines = typeof raw.machines === 'number' ? raw.machines : Number(raw.machines ?? 0);
  const out: FleetArgs = { tag, machines };
  for (const k of [
    'region',
    'size',
    'image',
    'vpcUuid',
    'bundleUrl',
    'bundleSha',
    'actorName',
    'actorVersion',
    'actorEngine',
    'controller',
  ] as const) {
    if (typeof raw[k] === 'string') out[k] = raw[k] as string;
  }
  if (Array.isArray(raw.sshKeyIds)) out.sshKeyIds = raw.sshKeyIds.map(String);
  // A NUMBER, and the loop above would have dropped it — which is exactly how `tmux` was lost
  // (see the note in fleet.ts). `machines` above shows the shape: accept a numeric string too,
  // because JSON off the wire is whatever the client felt like sending. 0 and NaN both mean
  // "not set", so the program leaves the hosts on their own default.
  const density = countOf(raw.maxSessions);
  if (density !== undefined) out.maxSessions = density;
  // ═══ THE PLACEMENTS, NARROWED ONE BY ONE ═══
  //
  // An ARRAY is new here and it is the one shape this reader had no rule for, so it gets the same
  // rule everything else has: entries that are not objects are dropped, and inside each entry only
  // the keys and types below survive. An entry with no `bundleUrl` is dropped ENTIRELY rather than
  // passed on as a placement with nothing to place — the program would build a Worker that fetches
  // "" — and dropping is the same wordless narrowing `evil: 'rm -rf'` gets one level up.
  //
  // AN EMPTY ARRAY IS NOT AN ABSENT ONE, and the difference is a Fleet. `placements: []` says "this
  // Fleet places nothing", which is a real desired state (`fleet.hold()` converges exactly that);
  // absent says "read the single-placement keys instead". `placementsOf` is where that fold lives,
  // so this function's only job is to say whether an array arrived at all.
  if (Array.isArray(raw.placements)) {
    out.placements = raw.placements.flatMap((entry) => coercePlacement(entry) ?? []);
  }
  return out;
}

/** One placement off the wire, or `undefined` when there is nothing placeable in it.
 *
 *  THERE IS NO `Array.isArray` GUARD HERE AND THERE WAS ONE. It could not be made to fail: a JSON
 *  array cannot carry a `bundleUrl` property, so the required-key check below already drops every
 *  array, and the extra line was a guard that passes whatever the code does. Mutation found it —
 *  deleting it changed no test — and an untestable guard is worse than none, because the next reader
 *  believes something is being checked. */
function coercePlacement(raw: unknown): PlacementArgs | undefined {
  if (typeof raw !== 'object' || raw === null) return undefined;
  const src = raw as Record<string, unknown>;
  if (typeof src.bundleUrl !== 'string' || src.bundleUrl === '') return undefined;
  if (typeof src.actorName !== 'string' || src.actorName === '') return undefined;
  const out: PlacementArgs = { actorName: src.actorName, bundleUrl: src.bundleUrl };
  for (const k of ['actorVersion', 'actorEngine', 'bundleSha', 'controller'] as const) {
    if (typeof src[k] === 'string') out[k] = src[k] as string;
  }
  const density = countOf(src.maxSessions);
  if (density !== undefined) out.maxSessions = density;
  const workers = countOf(src.workers);
  if (workers !== undefined) out.workers = workers;
  return out;
}

/** A positive whole count off the wire, or `undefined` for "the desired state has nothing to say".
 *
 *  ONE FUNCTION FOR BOTH COUNTS, because they read their zero the same way and their string the same
 *  way, and two copies would be two chances to drop `workers` the way `tmux` was dropped. `machines`
 *  is deliberately NOT one of them: zero Machines is a real, expressible Fleet size, and reading its
 *  zero as "unset" is the coercion that would make a teardown unsayable. */
function countOf(raw: unknown): number | undefined {
  if (raw === undefined || raw === null) return undefined;
  const n = Number(raw);
  return Number.isFinite(n) && n > 0 ? Math.trunc(n) : undefined;
}
