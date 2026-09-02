/**
 * ONE ORDERING OF VERSION STRINGS, for the two readers that have to agree about which build is
 * which.
 *
 * The registration gate compares a new version against the one IMMEDIATELY PRECEDING it
 * (`compat.ts`), and the slot store orders an Actor's declarations NEWEST FIRST so a binding is
 * resolved against the current build (`secrets/slotStore.ts`). That is one question asked from two
 * sides, and a second implementation of it would not be a harmless duplicate: the gate would report
 * a breaking change against `0.9.0` while the slot store hands a credential to the declaration it
 * calls newest, and nothing anywhere would look wrong.
 *
 * IT LIVED IN THE SPA FIRST and moved here rather than being copied. The browser's caller was the
 * Scratch palette (`panels/actorVersions.ts`), which went with the page it was the only caller of;
 * a browser reader coming back imports it through the `@core` alias, which is the direction that
 * alias goes (`frontend/vite.config.ts` resolves `@core` to this directory). There is no path from
 * here into `frontend/`, and a server that reached into the SPA's tree to sort a string would be
 * the worse dependency of the two.
 *
 * NUMERIC PER SEGMENT, not lexical, which is the entire reason this function exists: `0.10.0` sorts
 * BEFORE `0.9.0` under string comparison, so a plain `.sort().at(-1)` would offer 0.9.0 as the
 * newest build of an actor that has ten of them. Nothing in the catalog would look wrong — the row
 * would name a real version, of the right actor, that simply is not the current one.
 *
 * Non-numeric segments compare as strings, so `1.0.0-rc.1` and a version like `nightly` still order
 * deterministically instead of collapsing to equal. Nothing enforces semver on an actor's version
 * (it is a free string in actor.json), so this must total-order whatever it is handed.
 */

/** Compare two version strings, newest first. */
export function compareVersionsDesc(a: string, b: string): number {
  const [leftCore, leftPre] = splitPrerelease(a);
  const [rightCore, rightPre] = splitPrerelease(b);
  const core = compareSegments(leftCore, rightCore);
  if (core !== 0) return core;
  // A PRERELEASE IS OLDER THAN ITS RELEASE, and this is the one place where "more segments is
  // newer" must not apply. `1.0.0-rc` and `1.0.0` differ by a trailing `rc`, which reads exactly
  // like the extra segment that makes `1.0.1` newer than `1.0` — so splitting on `.` and `-`
  // together would offer the release candidate as the current build.
  if (!leftPre !== !rightPre) return leftPre ? 1 : -1;
  return compareSegments(leftPre.split('.'), rightPre.split('.'));
}

/** `1.0.0-rc.1` → `[['1','0','0'], 'rc.1']`. No prerelease is an empty string, never undefined. */
function splitPrerelease(v: string): [string[], string] {
  const dash = v.indexOf('-');
  return dash === -1 ? [v.split('.'), ''] : [v.slice(0, dash).split('.'), v.slice(dash + 1)];
}

/** Segment-wise, newest first: numeric where both sides are numbers, lexical where they are not. */
function compareSegments(left: string[], right: string[]): number {
  for (let i = 0; i < Math.max(left.length, right.length); i++) {
    const x = left[i];
    const y = right[i];
    if (x === undefined) return 1; // 1.0 is older than 1.0.1
    if (y === undefined) return -1;
    const nx = Number(x);
    const ny = Number(y);
    const numeric = x !== '' && y !== '' && Number.isFinite(nx) && Number.isFinite(ny);
    if (numeric) {
      if (nx !== ny) return ny - nx;
    } else if (x !== y) {
      return x < y ? 1 : -1;
    }
  }
  return 0;
}
