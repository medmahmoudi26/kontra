/**
 * Bearer-token admission for the API's read surfaces.
 *
 * Two routes here mint or expose things that must not be world-readable: raw actor state,
 * and short-lived presigned URLs to a run's typed output. Both FAIL CLOSED — with no token
 * configured they return 503 and serve nothing, rather than defaulting open. Presigning
 * without authentication would make "the token scopes access to one run" false by
 * construction, so there is no unauthenticated fallback to inherit.
 *
 * The rest of this API predates this work and is unauthenticated. That is recorded, not
 * silently inherited: see `.scratch/query-surface/PLAN.md` §3 and `docs/wiki/Orchestrator.md`.
 * A token here does not fix that; it only means the surface this change ADDS is not a new
 * way to read live scan data unauthenticated.
 */

import { timingSafeEqual } from 'node:crypto';

/** Env vars consulted for the explore/presign surface, in order of preference. */
export const EXPLORE_TOKEN_VARS = ['KONTRA_EXPLORE_TOKEN', 'KONTRA_STATE_TOKEN'] as const;

/** Env vars consulted for the raw-state surface. */
export const STATE_TOKEN_VARS = ['KONTRA_STATE_TOKEN'] as const;

/**
 * Constant-time string compare.
 *
 * `a === b` on a secret returns as soon as two bytes differ, so response time leaks how
 * long a guessed prefix was and turns brute force into a per-character search. The length
 * check before `timingSafeEqual` is unavoidable (it throws on mismatched lengths) and
 * leaks only the length, which is not secret.
 */
export function timingSafeEqualStr(a: string, b: string): boolean {
  const ab = Buffer.from(a);
  const bb = Buffer.from(b);
  return ab.length === bb.length && timingSafeEqual(ab, bb);
}

/** The first configured token among `vars`, or undefined when none is set. */
export function configuredToken(vars: readonly string[]): string | undefined {
  for (const v of vars) {
    const value = process.env[v];
    if (value) return value;
  }
  return undefined;
}

/** An admission failure: the exact status and body the route should send. */
export interface AuthFailure {
  code: number;
  body: { error: string };
}

/**
 * Check an `Authorization: Bearer …` header against the configured token.
 * Returns `null` when the caller is admitted, or the reply to send when it is not.
 */
export function checkBearer(
  authorization: string | undefined,
  vars: readonly string[]
): AuthFailure | null {
  const token = configuredToken(vars);
  if (!token) {
    return {
      code: 503,
      body: { error: `disabled: set one of ${vars.join(' or ')} to enable this endpoint` },
    };
  }
  if (!timingSafeEqualStr(authorization ?? '', `Bearer ${token}`)) {
    return { code: 401, body: { error: 'unauthorized' } };
  }
  return null;
}

/**
 * OPT-IN admission: open when no token is configured, enforced when one is.
 *
 * THE OPPOSITE POSTURE OF {@link checkBearer}, deliberately, and it is a separate function rather
 * than a flag so the two can never be confused at a call site — reading `checkBearer` and getting
 * "open by default" is exactly the mistake that would matter.
 *
 * Only the serve/start control surface uses this, because the operator chose it: those routes are
 * open like the rest of the pre-existing API. That choice has teeth — `serve` runs a file from the
 * host's checkout and `start` can start a workflow that provisions cloud machines — so it is
 * stated at boot and shown in the page (`workflowControl.ts:describeExposure`) rather than left to
 * be discovered.
 *
 * Nothing that reads live scan data or spends money on its own may use this. Fail-closed is the
 * default for a reason and this is the documented exception, not a second option.
 */
export function checkOptionalBearer(
  authorization: string | undefined,
  vars: readonly string[]
): AuthFailure | null {
  const token = configuredToken(vars);
  if (!token) return null;
  if (!timingSafeEqualStr(authorization ?? '', `Bearer ${token}`)) {
    return { code: 401, body: { error: 'unauthorized' } };
  }
  return null;
}
