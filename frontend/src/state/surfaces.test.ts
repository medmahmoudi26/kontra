/**
 * The surface list itself — the one thing three separate places read.
 *
 * These are cheap assertions about a constant, and they are here because the constant has already
 * drifted from its copies once and cost a live 404 on every dataset name with a dot in it. What is
 * pinned is the SHAPE the other two readers depend on: the nav order, that every surface is
 * complete enough to draw and to address, and that the segment list the server copies contains the
 * retired surfaces as well as the live ones.
 */

import { describe, expect, it } from 'vitest';

import { DEFAULT_VIEW, PATHS, RETIRED, SPA_SEGMENTS, SURFACES } from './surfaces';

describe('the five surfaces', () => {
  it('are these five, in this order', () => {
    // The order is the order the work happens, and it is asserted rather than implied because the
    // rail draws it verbatim: reordering this list moves an operator's muscle memory.
    expect(SURFACES.map((s) => s.id)).toEqual([
      'workflows',
      'actors',
      'datasets',
      'monitor',
      'settings',
    ]);
  });

  it('each carry a path, a label and a hint — a half-declared surface draws with a hole in it', () => {
    for (const surface of SURFACES) {
      expect(surface.path, surface.id).toBe(`/${surface.id}`);
      expect(surface.label.length, surface.id).toBeGreaterThan(0);
      expect(surface.hint.length, surface.id).toBeGreaterThan(0);
    }
  });

  it('appear in the nav order AND in the path record — a surface in one but not the other', () => {
    // `PATHS` is a `Record<View, string>`, so a surface with no path fails to compile; nothing
    // makes a surface with a path but no place in the ORDER fail to compile, which is this test.
    expect(Object.keys(PATHS).sort()).toEqual(SURFACES.map((s) => s.id).sort());
  });

  it('start on Workflows, which is the centre', () => {
    expect(DEFAULT_VIEW).toBe('workflows');
    expect(SURFACES[0]?.id).toBe('workflows');
  });
});

describe('the retired surfaces', () => {
  it('are Runs and Scratch, and both land on Workflows', () => {
    expect(Object.keys(RETIRED).sort()).toEqual(['runs', 'scratch']);
    for (const [segment, target] of Object.entries(RETIRED)) {
      expect(target.to, segment).toBe('workflows');
    }
  });

  it('differ in exactly one thing: whether the old address named anything', () => {
    // `/runs/<id>` has a run to hand on; `/scratch` was a drawing about nothing and has nothing.
    expect(RETIRED.runs?.carries).toBe('run');
    expect(RETIRED.scratch?.carries).toBe('nothing');
  });

  it('are not surfaces any more, so nothing can navigate to them', () => {
    for (const segment of Object.keys(RETIRED)) {
      expect(SURFACES.some((s) => s.id === segment), segment).toBe(false);
    }
  });
});

describe('what the server has to serve', () => {
  it('is every live surface plus every retired one', () => {
    // A LIVE segment missing there 404s a cold load of any id containing a dot. A RETIRED segment
    // missing there is worse in a quieter way: the redirect is frontend code, so the shell that
    // would forward `/runs/sweep-v1.2` never loads to forward it.
    expect([...SPA_SEGMENTS].sort()).toEqual(
      ['workflows', 'actors', 'datasets', 'monitor', 'settings', 'runs', 'scratch'].sort()
    );
  });

  it('names the copy the server keeps, so the pair can only be wrong together', () => {
    // `backend/src/server.ts`'s `SPA_SURFACES` restates this list — it is a separate package
    // and cannot import it — and `backend/src/spaFallback.test.ts` asserts the other half.
    expect(SPA_SEGMENTS).toHaveLength(7);
  });
});
