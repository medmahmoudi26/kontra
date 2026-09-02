/**
 * What a jsdom document does not come with, and the unmount nobody else performs.
 *
 * THE SUITE HAS A DOM NOW (`vite.config.ts`'s `test.environment`), which is what makes an effect
 * run and a handler fire. jsdom is not a browser, though: it implements the document and leaves out
 * everything the layout engine would have had to measure. The three stubs below are exactly the
 * APIs this app calls on a path a test reaches, each of which throws `is not a function` in jsdom —
 * they are not conveniences, and nothing else belongs here. A component that needs a fourth should
 * get it in its own file, where the reader can see what it is pretending.
 *
 * NOTHING HERE FAKES A MEASUREMENT. `ResizeObserver` never fires, `matchMedia` always answers
 * "no", and `scrollIntoView` records nothing — so a test that wanted a real geometry has to say so
 * itself, and one that did not is never handed a plausible-looking number it might assert against.
 */

import { cleanup } from '@testing-library/react';
import { afterEach } from 'vitest';

/**
 * THE UNMOUNT IS NOT AUTOMATIC HERE. `@testing-library/react` installs its own `afterEach` only
 * when the test framework's hooks are GLOBALS; this suite imports `describe`/`it`/`expect` from
 * `vitest` explicitly (there is no `globals: true`), so the auto-cleanup never registers and every
 * `render` would be left mounted in one shared document. That is not merely untidy: a page that
 * polls keeps polling into the next test, a `getByTestId` matches the previous test's markup, and
 * the failure surfaces somewhere other than the test that caused it.
 */
afterEach(() => cleanup());

/** The wall and the tiles observe their own boxes; jsdom has no layout, so it has no observer. */
class NoResizeObserver {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}
globalThis.ResizeObserver ??= NoResizeObserver as unknown as typeof ResizeObserver;

/** `prefers-color-scheme`, asked by the palette. Answers "no" and notifies nobody. */
globalThis.matchMedia ??= ((query: string) => ({
  matches: false,
  media: query,
  onchange: null,
  addListener: () => {},
  removeListener: () => {},
  addEventListener: () => {},
  removeEventListener: () => {},
  dispatchEvent: () => false,
})) as unknown as typeof globalThis.matchMedia;

/** The sidebar's reveal scrolls a tile into view. jsdom has no viewport to scroll. */
Element.prototype.scrollIntoView ??= function scrollIntoView(): void {};
