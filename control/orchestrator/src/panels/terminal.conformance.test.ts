/**
 * The TYPESCRIPT ARM of `shared/conformance/terminal.json` — and this side is the wire's WRITER.
 *
 * WHY A FIXTURE AND NOT A SOURCE SCRAPE. A TypeScript interface is erased at runtime, so there is
 * nothing to reflect over: `Object.keys(Terminal)` does not exist. The two ways to pin it are to
 * read `types.ts` as text, which ADR 0035 rule two rejects for exactly the reason it rejects any
 * scrape, or to build one real value the compiler must accept. This does the second. `satisfies
 * Terminal` makes `tsc` reject the fixture if the interface renames or adds a required field, and
 * the assertions below make vitest reject it if the corpus and the fixture disagree. A rename in
 * `types.ts` therefore has to break something twice before it can reach a reader.
 *
 * THE ASYMMETRY WITH THE GO ARM IS DELIBERATE, and `shared/conformance/terminal.json` states it under
 * `keys.containment`: the writer's key set is pinned EXACTLY, the reader's only has to be a subset.
 * A reader that ignores `paneCols` is right. A writer that quietly stops sending `fleet` is how
 * `kontra panels` printed `-` in that column on every Fleet for as long as the rename was live.
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import type { Terminal } from './types';

const corpus = JSON.parse(
  readFileSync(join(__dirname, '..', '..', '..', '..', 'shared', 'conformance', 'terminal.json'), 'utf8'),
) as {
  keys: { required: string[]; optional: string[] };
  goldens: { terminal: Record<string, unknown> };
};

/**
 * Every key the streamer sends, as a value the compiler has checked. Optional fields are present
 * here on purpose: the point is to enumerate the whole surface, not the minimum one.
 */
const FIXTURE = {
  id: 'fleet:kf-dns-01:nscheck-0.1.0:actor',
  mode: 'fleet',
  machine: 'kf-dns-01',
  host: '10.124.0.4',
  publicIp: '143.198.1.9',
  tag: 'dns',
  fleet: 'nscheck-0.1.0',
  actor: 'nscheck',
  version: '0.1.0',
  window: 'actor',
  command: 'zsh',
  paneCols: 213,
  paneRows: 51,
  exitStatus: '',
  health: {
    reachable: 'ok',
    session: 'present',
    process: 'running',
    poller: 'live',
    loads: 'ok',
  },
  lastSnapshotAt: 1756555200000,
  telemetry: undefined,
} satisfies Terminal;

describe('the Terminal wire', () => {
  it('is not pinned by an empty corpus', () => {
    // THE GUARD AGAINST A VACUOUS FILE. Every assertion below compares against these lists, and a
    // corpus that parsed to empty ones would make all of them pass over nothing.
    expect(corpus.keys.required.length).toBeGreaterThan(10);
    expect(corpus.keys.optional.length).toBeGreaterThan(0);
    expect(Object.keys(corpus.goldens.terminal).length).toBeGreaterThan(10);
  });

  it('sends exactly the keys the corpus names — no more, no fewer', () => {
    const declared = Object.keys(FIXTURE).sort();
    const pinned = [...corpus.keys.required, ...corpus.keys.optional].sort();

    expect(declared).toEqual(pinned);
  });

  it('names every required key in the golden, so a reader can decode it', () => {
    for (const key of corpus.keys.required) {
      expect(
        corpus.goldens.terminal,
        `the golden Terminal omits the required key \`${key}\`, so the Go arm decodes a zero ` +
          'value for it and proves nothing',
      ).toHaveProperty(key);
    }
  });

  it('agrees with the golden value for the key `kontra panels` prints', () => {
    // `fleet` is the second cell of the table and the one that was blank. It is asserted by value
    // rather than by presence because the bug it guards was never a missing key on this side —
    // this side was always correct, and the reader was asking for the old name.
    expect(corpus.goldens.terminal.fleet).toBe(FIXTURE.fleet);
    expect(corpus.goldens.terminal.tag).toBe(FIXTURE.tag);
  });

  it('carries no key spelled `campaign` or `role`', () => {
    // The word is retired (ADR 0037) and `role` was never sent. Both are named explicitly so that
    // reintroducing either fails here with the reason rather than as an opaque set mismatch.
    const all = [...Object.keys(FIXTURE), ...corpus.keys.required, ...corpus.keys.optional];
    expect(all, 'Campaign retired in ADR 0037; this key is `fleet`').not.toContain('campaign');
    expect(all, 'the Machine tag is `tag`; `role` has never been on this wire').not.toContain('role');
  });
});
