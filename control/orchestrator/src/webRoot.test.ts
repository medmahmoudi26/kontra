/**
 * Where the console's SPA is found, in every shape it can sit in.
 *
 * THE MOVE THAT MADE THIS A TEST, AND IT HAS NOW HAPPENED THREE TIMES. `frontend/` was
 * `orchestrator/web` — a CHILD of the server package, so walking up from the compiled `dist/src`
 * reached `web/dist`. Then it was a SIBLING of `backend/`. Then ADR 0038 moved it out of the
 * repository entirely, into a `kontra-console` checkout beside this one.
 *
 * NONE OF THOSE FAILS LOUDLY. The API still boots, still answers, and serves an empty page —
 * neither suite noticed, because both run against a server told its web root explicitly. That is
 * what this file is for, and it caught the third move only because it was already here: the
 * candidate list in `defaultWebRoot` was edited and this suite was not re-run.
 *
 * The image's shape is the other half and has never moved: the orchestrator image puts the SPA
 * at `orchestrator/web/dist` beside `orchestrator/dist/src`, unpacked from the `web/dist/` member
 * prefix of `kontra-spa.tar.gz` — the same shape the appliance's hydrated install bundle had. So
 * every shape has to resolve, from any depth.
 */
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { describe, expect, it, vi } from 'vitest';

import { defaultWebRoot } from './server';

/** Build `<root>/<rel>/index.html` and return `<root>`. */
function spaAt(rel: string): { root: string; spa: string } {
  const root = mkdtempSync(path.join(tmpdir(), 'webroot-'));
  const spa = path.join(root, rel);
  mkdirSync(spa, { recursive: true });
  writeFileSync(path.join(spa, 'index.html'), '<!doctype html>');
  return { root, spa };
}

describe('defaultWebRoot finds the SPA in both shapes', () => {
  it('finds the CHECKOUT shape — a kontra-console checkout beside this one', () => {
    // `<parent>/kontra-console/dist`, with the server running from `<parent>/kontra/backend/…`.
    // The walk reaches `<parent>/kontra` and looks at its sibling from there.
    const { root, spa } = spaAt('kontra-console/dist');
    const from = path.join(root, 'kontra', 'control', 'orchestrator', 'dist', 'src');
    mkdirSync(from, { recursive: true });
    expect(defaultWebRoot(from)).toBe(path.resolve(spa));
  });

  it('finds the BUNDLE shape — web/dist under the server, which did not move', () => {
    const { root, spa } = spaAt('orchestrator/web/dist');
    const from = path.join(root, 'orchestrator', 'dist', 'src');
    mkdirSync(from, { recursive: true });
    expect(defaultWebRoot(from)).toBe(path.resolve(spa));
  });

  it('finds it from the UNCOMPILED depth too — vitest runs this file from src/', () => {
    const { root, spa } = spaAt('kontra-console/dist');
    const from = path.join(root, 'kontra', 'control', 'orchestrator', 'src');
    mkdirSync(from, { recursive: true });
    expect(defaultWebRoot(from)).toBe(path.resolve(spa));
  });

  it('KONTRA_CONSOLE_DIST wins, and a wrong one is an ANSWER rather than a fallback', () => {
    // A console checked out somewhere the walk will never look. Both exist here, so this is about
    // precedence and not about absence.
    const { root, spa } = spaAt('kontra-console/dist');
    const elsewhere = spaAt('somewhere-else/dist');
    const from = path.join(root, 'kontra', 'control', 'orchestrator', 'dist', 'src');
    mkdirSync(from, { recursive: true });

    vi.stubEnv('KONTRA_CONSOLE_DIST', elsewhere.spa);
    expect(defaultWebRoot(from)).toBe(path.resolve(elsewhere.spa));

    // A NAMED DIRECTORY THAT IS EMPTY MUST NOT FALL THROUGH. Serving the sibling would hand an
    // operator who mistyped a path a different SPA than the one they asked for, and report success.
    vi.stubEnv('KONTRA_CONSOLE_DIST', path.join(root, 'typo'));
    expect(defaultWebRoot(from)).toBeUndefined();
    vi.unstubAllEnvs();

    // …and with the variable gone, the sibling is found again — so the check above proved the
    // override, not a broken fixture.
    expect(defaultWebRoot(from)).toBe(path.resolve(spa));
  });

  it('returns undefined when the SPA is not built, rather than throwing', () => {
    const root = mkdtempSync(path.join(tmpdir(), 'webroot-none-'));
    const from = path.join(root, 'control', 'orchestrator', 'dist', 'src');
    mkdirSync(from, { recursive: true });
    // NOT an error: an API with no SPA degrades to serving no static files rather than failing
    // to boot, which is what lets the server and the console ship on different clocks.
    expect(defaultWebRoot(from)).toBeUndefined();
  });
});
