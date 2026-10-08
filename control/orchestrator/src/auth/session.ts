/**
 * Console sessions: what signing in gives you, and what it is good for.
 *
 * A browser cannot read `~/.kontra/config.yaml`, which is how the CLI authenticates. Until now the
 * console got in by having a bearer BAKED INTO ITS BUNDLE at build time — a credential in a build
 * artifact, invalidated by every rotation, and one that a build run for an unrelated reason silently
 * replaced with an empty string, so every query answered `query: unauthorized`.
 *
 * So: sign in against the credential on the filesystem, and get a session token to send as
 * `Authorization: Bearer …` from then on. ONE token, for everything the console does — which is the
 * whole point, because the alternative is a browser holding four service tokens with four different
 * scopes and choosing between them per request.
 *
 * A SESSION IS NOT A SERVICE TOKEN, and keeping them separate is what makes this safe to hand a
 * browser. `KONTRA_STATE_TOKEN` can spend money — it admits the infra routes that provision
 * machines. A session is minted here, lives in this process, expires, and can be revoked by a
 * restart. It authorises what the console does and nothing beyond it.
 *
 * IN MEMORY, DELIBERATELY. Sessions do not survive a restart, and that is a feature rather than a
 * gap to close later: the orchestrator restarting is exactly when you want every browser to have to
 * prove itself again, and a session store on disk is one more file holding something worth stealing.
 * The cost is that a redeploy signs everyone out, which for a two-person instance is a fair trade
 * and is stated rather than discovered.
 */

import { randomBytes, timingSafeEqual } from 'node:crypto';

/**
 * How long a session lasts without being used.
 *
 * Twelve hours: longer than a working day so nobody is signed out mid-task, short enough that a
 * laptop left open in a café is not a standing credential. Idle, not absolute — {@link SessionBook.verify}
 * extends a live session, so an operator working all day is not interrupted at hour twelve.
 */
export const SESSION_TTL_MS = 12 * 60 * 60 * 1000;

/**
 * Refuse to hold more than this many live sessions.
 *
 * Minting is authenticated, so this is a leak guard rather than a DoS one: unbounded growth here
 * would be a slow one. The same reasoning and the same number as `panels/tickets.ts`.
 */
export const MAX_SESSIONS = 512;

/**
 * What a session is allowed to reach.
 *
 * A session proves WHO is asking; a scope says WHAT that buys. The two were the same thing while
 * the only credential a browser had admitted everything, and `infra` is the case that shows why
 * they cannot be: signing in to read a run is not a mandate to converge a cloud stack.
 *
 * Deliberately a plain string set rather than a role enum — there are exactly two values and a
 * role table would be a vocabulary before it is a need.
 */
export const CONSOLE_SCOPE = 'console';
export const INFRA_SCOPE = 'infra';

/** What a sign-in mints. NO PATH GRANTS `infra`, which is what keeps it a service-token capability. */
export const DEFAULT_SESSION_SCOPES: readonly string[] = [CONSOLE_SCOPE];

export interface Session {
  token: string;
  /** Which console user signed in. Carried so a future audit line can name somebody. */
  user: string;
  expiresAt: number;
  scopes: readonly string[];
}

/** A live session as the admission code reads it — the user, and what they may reach. */
export interface SessionView {
  user: string;
  scopes: readonly string[];
}

export class SessionBook {
  private live = new Map<string, { user: string; expiresAt: number; scopes: readonly string[] }>();

  constructor(
    private readonly now: () => number = () => Date.now(),
    private readonly ttlMs: number = SESSION_TTL_MS
  ) {}

  get outstanding(): number {
    return this.live.size;
  }

  /** Mint a session for a user who has already proved who they are. */
  mint(user: string, scopes: readonly string[] = DEFAULT_SESSION_SCOPES): Session {
    this.sweep();
    if (this.live.size >= MAX_SESSIONS) {
      // Drop the oldest rather than refuse: the caller authenticated, and a full book must not lock
      // an operator out of their own console.
      const oldest = this.live.keys().next();
      if (!oldest.done) this.live.delete(oldest.value);
    }
    // 32 bytes of CSPRNG, base64url so it survives a header untouched.
    const token = randomBytes(32).toString('base64url');
    const expiresAt = this.now() + this.ttlMs;
    this.live.set(token, { user, expiresAt, scopes });
    return { token, user, expiresAt, scopes };
  }

  /**
   * Is this bearer a live session? Returns the user when it is.
   *
   * CONSTANT-TIME AGAINST EVERY CANDIDATE, not a map lookup. A `Map.get` returns as soon as the
   * hash misses, which leaks through timing whether a guessed prefix was getting warmer — the same
   * reasoning `TicketBook.redeem` and `timingSafeEqualStr` already carry.
   *
   * USING A SESSION EXTENDS IT. The TTL is idle time, so an operator working through the afternoon
   * is not signed out at hour twelve for having been there since the morning.
   */
  verify(candidate: string | undefined | null): string | null {
    return this.look(candidate)?.user ?? null;
  }

  /**
   * The live session behind a bearer — the user AND what it may reach.
   *
   * ONE SCAN, NOT TWO. Admission needs both halves, and calling `verify` then looking the scopes up
   * separately would walk the book twice: two constant-time scans and two TTL slides per request,
   * where the second slide is on a session the first already extended. `verify` is kept as the thin
   * caller of this so the existing readers (audit, logout) do not move.
   */
  look(candidate: string | undefined | null): SessionView | null {
    this.sweep();
    if (!candidate) return null;
    for (const [token, entry] of this.live) {
      if (!timingSafeEqualStr(token, candidate)) continue;
      if (entry.expiresAt <= this.now()) {
        this.live.delete(token);
        return null;
      }
      entry.expiresAt = this.now() + this.ttlMs;
      return { user: entry.user, scopes: entry.scopes };
    }
    return null;
  }

  /** Sign out. Idempotent — signing out twice is not an error worth reporting to a browser. */
  revoke(candidate: string | undefined | null): void {
    if (!candidate) return;
    for (const token of this.live.keys()) {
      if (timingSafeEqualStr(token, candidate)) {
        this.live.delete(token);
        return;
      }
    }
  }

  sweep(): void {
    const t = this.now();
    for (const [token, entry] of this.live) {
      if (entry.expiresAt <= t) this.live.delete(token);
    }
  }
}

/** The process's session book. One per orchestrator, which is one per Tenant. */
export const sessions = new SessionBook();

/**
 * `Bearer <token>` → `<token>`, or undefined. The one place the header shape is parsed.
 *
 * NO REGEX, AND THAT IS THE POINT. This was `/^Bearer\s+(.+)$/i`, which is POLYNOMIAL: `\s+` and
 * `.+` both match a space, so the engine tries every split between them, and `.` does not cross a
 * line terminator — so an input of `bearer`, many spaces, then a tail `(.+)$` cannot match
 * backtracks once per space and rescans from each. The work happens in header parsing, BEFORE any
 * credential is looked at, so it is unauthenticated work. Found by CodeQL, rated high.
 *
 * A prefix compare and two slices cannot backtrack. The accepted shape is unchanged, including the
 * parts that are easy to lose: the scheme is case-insensitive, at least one space is required (so
 * `Bearerabc` is not a bearer header), and a token may not contain a line terminator — which the
 * old `.` gave for free and a plain slice does not.
 */
const BEARER = 'bearer';

export function bearerOf(authorization: string | undefined): string | undefined {
  if (!authorization) return undefined;
  const s = authorization.trim();
  if (s.length <= BEARER.length) return undefined;
  if (s.slice(0, BEARER.length).toLowerCase() !== BEARER) return undefined;

  const rest = s.slice(BEARER.length);
  // ASCII whitespace: space, and tab through carriage return. One character, no quantifier.
  const first = rest.charCodeAt(0);
  if (first !== 0x20 && (first < 0x09 || first > 0x0d)) return undefined;

  const token = rest.trimStart();
  if (token.length === 0) return undefined;
  // `.` excluded line terminators, so `Bearer a\nb` never parsed. Keep that: a header carrying one
  // is malformed, and accepting it here would widen what reaches the session lookup.
  if (/[\n\r\u2028\u2029]/.test(token)) return undefined;
  return token;
}

function timingSafeEqualStr(a: string, b: string): boolean {
  const ab = Buffer.from(a);
  const bb = Buffer.from(b);
  return ab.length === bb.length && timingSafeEqual(ab, bb);
}
