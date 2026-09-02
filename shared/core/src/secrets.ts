/**
 * What a secret may be CALLED — the rule, in the one place both halves can reach.
 *
 * IT IS HERE BECAUSE IT HAD TWO COPIES. `control/orchestrator/src/secrets/store.ts` declared it and refused
 * anything else; `kontra-console`'s `panels/secrets.ts` declared it again, byte-identical, under a
 * header that said "MIRRORS the server, which is the authority — the copy exists because
 * `control/orchestrator/src` is a Node package this bundle does not depend on". A test then read the server's
 * SOURCE and compared the two regex literals as strings, which worked and was a symptom: the only
 * reason to compare two declarations is that there are two.
 *
 * The excuse expired with ADR 0038. The console depends on `@kontra/core` now, so it imports the
 * rule instead of restating it, and the drift the mirror-test was watching for cannot occur.
 *
 * WHAT THE RULE IS FOR. A name is used unescaped in a URL, in a task queue and in a filename, so it
 * is lowercase, bounded, and may not open with punctuation. The browser needs it to tell an
 * operator what is wrong while they are typing; the store needs it to refuse. If those two ever
 * disagreed the form would accept a name and the submit would 400.
 */
export const SECRET_NAME_RE = /^[a-z0-9][a-z0-9._-]{0,63}$/;
