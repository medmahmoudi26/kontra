/**
 * The BROWSER ARM of `conformance/queues.json` §tmux_session.
 *
 * WHAT THIS FILE USED TO BE. A copy of `backend/src/actorControl.test.ts`'s cases, asserted
 * against `frontend/src/panels/actorSession.ts:actorSessionOf` — which was `actorControl.ts`'s
 * `actorSession` written out a second time, byte-identical including the `'' || '_' -> 'actor'`
 * fallback, one calling the shared sanitiser and the other re-inlining `.replace(/[.:]/g, '_')`.
 * Its own header called itself "a fifth independent derivation" and this one said "the cases here
 * are the same ones `actorControl.test.ts` holds — if one of these two files is edited alone, the
 * other is what says so."
 *
 * Two writers on the same side of a language boundary is not a contract with two writers, it is a
 * copy: the browser imports the server's function now, through the same `@core` alias it already
 * used for `@core/panels/pollers` and `@core/panels/ids`. So this file no longer proves that two
 * implementations agree — there is one — and what it proves instead is the two things a collapse
 * can still get wrong.
 *
 * ONE: THAT THE IMPORT RESOLVES FROM HERE. `@core/panels/tmux` is a path the browser bundle takes,
 * and a module that reached for `node:child_process` two hops down would break the app while
 * leaving every server test green (`.ds-entry.ts` records exactly that trap for
 * `@core/panels/pollers`). TWO: that the shared function still answers what every OTHER language
 * answers, which is what the corpus is.
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { actorSession } from '@core/panels/tmux';

type TmuxCase = {
  why: string;
  actor: string;
  version: string;
  tag: string;
  worker: string;
  machine: string;
};

const corpus = JSON.parse(
  readFileSync(join(__dirname, '../../../conformance/queues.json'), 'utf8')
) as { tmux_session: { cases: TmuxCase[] } };

const CASES = corpus.tmux_session.cases;

describe('the session an Actor’s Worker runs in, derived in the browser', () => {
  it('has a corpus that did not silently shrink', () => {
    // A corpus of nothing passes every case below, and a corpus of only the easy rows is the
    // shape of every guard ADR 0035 found that failed to guard anything.
    expect(CASES.length).toBeGreaterThanOrEqual(8);
    expect(CASES.some((c) => c.version === '')).toBe(true);
    expect(CASES.some((c) => c.actor === '' && c.tag === '')).toBe(true);
    expect(CASES.some((c) => c.actor.includes('.') || c.actor.includes(':') || c.version.includes(':'))).toBe(
      true
    );
  });

  for (const c of CASES) {
    it(c.why, () => {
      expect(actorSession(c.actor, c.version)).toBe(c.worker);
    });
  }

  it('keeps the version, because the version is the half that separates two builds', () => {
    // `probe` alone names the ACTOR and not the BUILD. Two versions served side by side would
    // collide on one session, and the pane beside 0.2.0's code would be 0.1.0's worker. A property
    // rather than a row: it must hold for every pair the corpus carries, not one hand-picked one.
    expect(actorSession('probe', '0.1.0')).not.toBe(actorSession('probe', '0.2.0'));
  });

  it('answers `actor` where a Machine’s derivation answers `fleet`, on purpose', () => {
    // THE ONE DIFFERENCE IN THIS CORPUS THAT IS INTENDED, and the reason it is written down: a
    // Worker's name is derived from an Actor and a version and nothing else, while a Machine's is
    // also given the fleet's tag, because a `fleet up` with no Actor placed on it still has
    // Terminals. Calling such a session `actor` would be a lie about what is running there.
    const differ = CASES.filter((c) => c.worker !== c.machine);
    expect(differ.length).toBeGreaterThan(0);
    for (const c of differ) {
      expect(c.why.length).toBeGreaterThan(0);
      expect(actorSession(c.actor, c.version)).toBe(c.worker);
    }
  });
});
