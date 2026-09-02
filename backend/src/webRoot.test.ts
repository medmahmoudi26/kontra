/**
 * Where the console's SPA is found, in both shapes it can sit in.
 *
 * THE MOVE THAT MADE THIS A TEST: `frontend/` used to be `orchestrator/web`, a CHILD of the server
 * package, so walking up from the compiled `dist/src` reached `web/dist`. It is a SIBLING of
 * `backend/` now and that walk never finds it again — which is not a failure anything reports. The
 * API still boots, still answers, and serves an empty page. Nothing in either suite caught it,
 * because both run against a server told its web root explicitly.
 *
 * The bundle's shape is the other half and did not move: a hydrated appliance bundle puts the SPA
 * at `orchestrator/web/dist` beside `orchestrator/dist/src`, an artifact contract that
 * `handler/internal/hydrate` writes. So both shapes have to resolve, from any depth.
 */
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

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
  it('finds the CHECKOUT shape — frontend/ beside backend/', () => {
    const { root, spa } = spaAt('frontend/dist');
    // Where the compiled server actually runs from in a checkout.
    const from = path.join(root, 'backend', 'dist', 'src');
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
    const { root, spa } = spaAt('frontend/dist');
    const from = path.join(root, 'backend', 'src');
    mkdirSync(from, { recursive: true });
    expect(defaultWebRoot(from)).toBe(path.resolve(spa));
  });

  it('returns undefined when the SPA is not built, rather than throwing', () => {
    const root = mkdtempSync(path.join(tmpdir(), 'webroot-none-'));
    const from = path.join(root, 'backend', 'dist', 'src');
    mkdirSync(from, { recursive: true });
    // NOT an error: an API with no SPA degrades to serving no static files rather than failing
    // to boot, which is what lets the server and the console ship on different clocks.
    expect(defaultWebRoot(from)).toBeUndefined();
  });
});
