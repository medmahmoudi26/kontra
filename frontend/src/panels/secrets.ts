/**
 * What the Settings surface knows about a secret — all of it derived, none of it fetched twice.
 *
 * PURE, so every state this page can be in is a node test with no DOM: nothing here reads the
 * network, and the component that draws it takes rows as a prop.
 *
 * THE VOCABULARY IS THE POINT. An operator looking at this page cannot read a value back, so the
 * words have to carry the whole of what they can know: which version is CURRENT, which is
 * SUPERSEDED (still usable, which is what makes a rotation safe), and which is REVOKED (destroyed,
 * and not coming back). Collapsing superseded into revoked would make a rotation look like a
 * deletion, and an operator who believes that will not rotate.
 */

import type { Secret, SecretVersion } from '../run/api';

/**
 * MIRRORS `backend/src/secrets/store.ts`'s `SECRET_NAME_RE`, which is the authority — the
 * server refuses anything this lets through, and `secrets.test.ts` reads that file and pins the two
 * together so this copy cannot drift silently. It is copied rather than imported because
 * `backend/src` is a Node package this bundle does not depend on, and the module that owns the
 * rule reaches `node:crypto` two imports down.
 */
export const SECRET_NAME_RE = /^[a-z0-9][a-z0-9._-]{0,63}$/;

/** Whether the secret has any usable version left. */
export type SecretState = 'live' | 'unusable';

/** What one version is, from the point of view of somebody deciding whether to revoke it. */
export type VersionState = 'current' | 'superseded' | 'revoked';

export interface VersionRow extends SecretVersion {
  state: VersionState;
  /** `2026-08-25` — an ISO date, so it reads the same in every timezone a screenshot lands in. */
  created: string;
}

export interface SecretRow {
  name: string;
  owner?: string;
  /**
   * WHO RESOLVES IT, which is the one thing about a secret an operator has to get right. An
   * `actor` secret is fetched by that actor, authenticated as itself; an `operator` secret is
   * resolved in-process at the last hop by a worker, and cannot be fetched over HTTP at all.
   */
  scope: 'operator' | 'actor';
  state: SecretState;
  current?: number;
  versions: VersionRow[];
  /** One line: what version it is on, and when it last changed. */
  detail: string;
}

const isoDate = (ms: number): string => new Date(ms).toISOString().slice(0, 10);

export function versionRows(secret: Secret): VersionRow[] {
  return [...secret.versions]
    .sort((a, b) => b.version - a.version)
    .map((v) => ({
      ...v,
      created: isoDate(v.createdAt),
      state: v.revokedAt ? 'revoked' : v.version === secret.current ? 'current' : 'superseded',
    }));
}

export function secretState(secret: Secret): SecretState {
  return secret.current === undefined ? 'unusable' : 'live';
}

/**
 * The one line under a secret's name.
 *
 * IT NEVER SAYS "SET" AND LEAVES IT AT THAT. "shodan-key: set" is the sentence a page shows when it
 * has nothing to say, and it is exactly the moment an operator wonders whether it is the value they
 * think — so this says which version is live and when it last moved, which are the only facts that
 * can answer that without showing anything.
 */
export function summarise(secret: Secret): string {
  const total = secret.versions.length;
  const versions = `${total} version${total === 1 ? '' : 's'}`;
  if (secret.current === undefined) {
    return `every version revoked · ${versions} · updated ${isoDate(secret.updatedAt)}`;
  }
  return `version ${secret.current} · ${versions} · updated ${isoDate(secret.updatedAt)}`;
}

export function toRows(secrets: readonly Secret[]): SecretRow[] {
  return secrets.map((s) => ({
    name: s.name,
    ...(s.owner ? { owner: s.owner } : {}),
    scope: s.owner ? ('actor' as const) : ('operator' as const),
    state: secretState(s),
    ...(s.current !== undefined ? { current: s.current } : {}),
    versions: versionRows(s),
    detail: summarise(s),
  }));
}

/** `null` when the name is one the store will take; otherwise the sentence to show under it. */
export function nameHint(name: string): string | null {
  if (!name) return null;
  if (SECRET_NAME_RE.test(name)) return null;
  if (/[A-Z]/.test(name)) {
    // The mistake worth naming, because it is the one that makes two secrets look like one.
    return 'lowercase only — DO_TOKEN and do-token would be two different secrets';
  }
  return 'lowercase letters, digits, `.`, `-` and `_`, starting with a letter or digit';
}

/**
 * The confirmation before a version is revoked — NAMING WHAT GOES, the way `deletionConfirm` does.
 *
 * Revocation destroys the stored bytes, so the sentence has to be the strong one; and it says
 * whether anything is left afterwards, because "revoke the version that leaked" and "leave this
 * secret with nothing to resolve" are the same click with very different consequences.
 */
export function revocationConfirm(args: {
  name: string;
  version: number;
  /** The version that will be current afterwards, if any. */
  fallback?: number;
}): string {
  const lines = [
    `Revoke version ${args.version} of "${args.name}"?`,
    'This destroys the stored value for that version. It cannot be undone, and no copy is kept.',
  ];
  lines.push(
    args.fallback === undefined
      ? `"${args.name}" will then have NO usable version — anything resolving it fails until you write a new value.`
      : `"${args.name}" will fall back to version ${args.fallback}, which stays usable.`
  );
  return lines.join('\n');
}

export function destroyConfirm(args: { name: string; versions: number; owner?: string }): string {
  const owned = args.owner ? ` It is owned by ${args.owner}, which will lose access to it.` : '';
  const versions =
    args.versions === 1 ? 'Its only version goes' : `All ${args.versions} versions go`;
  return [
    `Delete the secret "${args.name}"?`,
    `${versions} with it, and cannot be recovered.${owned}`,
    'Anything that names this secret will fail until a secret of the same name exists again.',
  ].join('\n');
}
