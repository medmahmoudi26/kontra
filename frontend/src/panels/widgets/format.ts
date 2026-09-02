/**
 * The few human formatters the generated documents share (ADR 0020, slice 7b).
 *
 * THE WORDS COME FROM `chrome/format.ts`, NOT FROM HERE. Slice 7a wrote that file to be the one place
 * the wall says "how old" and "how many bytes", precisely so a status bar, a tile header and a sidebar
 * cannot report `4s`, `4 sec` and `00:04` for the same measurement — and a drawer that said `2m 5s ago`
 * beside a tile header saying `2m ago` would be the same bug with an extra file. So `formatAge` and
 * `formatBytes` DELEGATE.
 *
 * What they add on top is the one rule the duration-taking API cannot carry: **an absent or zero
 * instant is `never`, not `0s ago`.** `ageWords` takes a duration, so every call site would otherwise
 * have to remember that `lastSnapshotAt === undefined` is not `now - 0`. That rule is
 * `heartbeat.ts`'s — "unknown" and "zero" stay distinguishable — and it belongs in one function rather
 * than in each of the five places a summary prints an age.
 */

import { ageWords, byteWords } from '../chrome/format';

/** `1536` -> `1.5 KiB`. Guards what the chrome's version does not have to: a negative or non-finite
 * byte count is a bug upstream, and `unknown` says so instead of printing `NaN KiB`. */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return 'unknown';
  return byteWords(bytes);
}

/** Thousands-separated, locale-independent: a dashboard read in two places must not report two
 * different numbers because of a browser setting. */
export function formatCount(n: number): string {
  if (!Number.isFinite(n)) return 'unknown';
  const negative = n < 0;
  const digits = Math.abs(Math.round(n)).toString();
  let out = '';
  for (let i = 0; i < digits.length; i += 1) {
    if (i > 0 && (digits.length - i) % 3 === 0) out += ' ';
    out += digits[i];
  }
  return negative ? `-${out}` : out;
}

/**
 * How long ago, in words — and `never` is a first-class answer.
 *
 * `undefined`/`0` means NOTHING HAS BEEN MEASURED, which must not render as "0s ago": the same rule
 * `heartbeat.ts` states and `HealthChips` follows, one layer down. A snapshot age of "0s ago" on a
 * Terminal that has never produced a frame is exactly the lie this project keeps finding.
 */
export function formatAge(at: number | undefined, now: number): string {
  if (at === undefined || at === 0 || !Number.isFinite(at)) return 'never';
  const ms = now - at;
  // A future instant is skew, and `ageWords` would call it `just now` — which is the one reading that
  // hides it. A Machine whose clock is ahead of the Controller's is worth a word of its own, because
  // every age on the page is then wrong by that offset.
  if (ms < 0) return 'in the future (clock skew)';
  return ageWords(ms);
}

/** An epoch-ms instant as ISO-8601 in UTC, or `unknown`. UTC because a fleet spans time zones and
 * every other record in this system (the Manifest, the journal, the lake) is UTC. */
export function formatInstant(at: number | undefined): string {
  if (at === undefined || at === 0 || !Number.isFinite(at)) return 'unknown';
  return new Date(at).toISOString();
}
