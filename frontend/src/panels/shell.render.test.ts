/**
 * The frame, drawn for every surface — the one thing this slice owns end to end.
 *
 * NODE, NO JSDOM, NO TESTING-LIBRARY, as everywhere else here: `renderToStaticMarkup` and string
 * assertions about text and `data-testid`.
 *
 * THE SURFACE IS A PROP, AND THAT IS THE WHOLE REASON `Shell` EXISTS. `App` holds five
 * `React.lazy` surfaces; rendering it here would start five dynamic imports into CodeMirror, xterm
 * and AG Grid — modules that fail a whole test file before a test runs. So the frame takes what it
 * frames, which lets the two states that matter be drawn honestly: a surface that has arrived, and
 * one whose chunk has not.
 *
 * WHAT THIS FILE DOES NOT CLAIM. It does not draw the four pre-existing surfaces' own empty and
 * loaded states. They are unchanged by this slice, they each have their own suites, and their
 * modules cannot be imported in this environment at all. What is asserted here is the boundary:
 * the rail is up for every one of them, the retired two are not reachable from it, and the column
 * beside it holds whatever it is handed.
 */

import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { afterEach, describe, expect, it } from 'vitest';

import { Shell, SurfacePending } from './Shell';
import { SURFACES, type View } from '../state/surfaces';
import { useAppStore } from '../state/store';

const REAL = globalThis.localStorage;

/** The rail reads `localStorage` on its first render; node has none of its own. */
function fakeStorage(seed: Record<string, string> = {}): Storage {
  const map = new Map(Object.entries(seed));
  return {
    get length() {
      return map.size;
    },
    clear: () => map.clear(),
    getItem: (k: string) => map.get(k) ?? null,
    key: (i: number) => [...map.keys()][i] ?? null,
    removeItem: (k: string) => void map.delete(k),
    setItem: (k: string, v: string) => void map.set(k, v),
  } as Storage;
}

afterEach(() => {
  Object.defineProperty(globalThis, 'localStorage', { value: REAL, configurable: true });
  useAppStore.setState({ view: 'workflows' });
});

function draw(surface = createElement('div', { 'data-testid': 'a-surface' }, 'the surface')): string {
  Object.defineProperty(globalThis, 'localStorage', { value: fakeStorage(), configurable: true });
  return renderToStaticMarkup(createElement(Shell, null, surface));
}

describe('the shell', () => {
  it('is a rail and a column, and both are always there', () => {
    const html = draw();
    expect(html).toContain('data-testid="app-shell"');
    expect(html).toContain('data-testid="side-nav"');
    expect(html).toContain('data-testid="a-surface"');
  });

  it('offers all five surfaces whatever is mounted beside it', () => {
    const html = draw();
    for (const surface of SURFACES) {
      expect(html, surface.id).toContain(`data-testid="nav-${surface.id}"`);
      expect(html, surface.id).toContain(`>${surface.label}<`);
    }
  });

  it('offers neither retired surface — those are addresses now, not destinations', () => {
    const html = draw();
    expect(html).not.toContain('data-testid="nav-runs"');
    expect(html).not.toContain('data-testid="nav-scratch"');
    expect(html).not.toContain('>Scratch<');
  });

  it('draws an empty surface without losing the navigation', () => {
    // A surface that renders nothing — the empty state every one of them has — must still leave an
    // operator able to go somewhere else. This is the shell's half of "empty".
    const html = draw(createElement('div', { 'data-testid': 'a-surface' }));
    expect(html).toContain('data-testid="side-nav"');
    expect(html).toContain('data-testid="a-surface"></div>');
  });

  it('keeps the rail up while a surface is still arriving', () => {
    // THE FALLBACK IS INSIDE THE FRAME, not around it. A boundary that replaced the whole page
    // would blank the navigation on every surface change, which reads as the app reloading.
    const pending = renderToStaticMarkup(createElement(SurfacePending));
    expect(pending).toContain('data-testid="surface-pending"');
    expect(pending).toContain('Loading…');
    expect(draw(createElement(SurfacePending))).toContain('data-testid="side-nav"');
  });
});

describe('where the rail says you are', () => {
  /* The container reads the store, and zustand feeds `renderToStaticMarkup` the SERVER snapshot —
     so only the store's INITIAL view is reachable through `Shell`. Every other surface's marked
     state is drawn through `NavRail` directly in `sideNav.render.test.ts`; what is pinned here is
     that the frame's default is the one the address module also defaults to. */

  it('starts on Workflows, which is where a cold load with no address lands', () => {
    const html = draw();
    expect(html).toContain('data-testid="nav-workflows" data-active="true"');
    expect(html.match(/data-active="true"/g)).toHaveLength(1);
  });

  it('marks exactly one of the five, never none', () => {
    const marked = SURFACES.map((s) => s.id).filter((id: View) =>
      draw().includes(`data-testid="nav-${id}" data-active="true"`)
    );
    expect(marked).toHaveLength(1);
  });
});
