/**
 * The bar itself: what the address currently is, how it changes, and how Back reaches the store.
 *
 * A PORT RATHER THAN `window.history` INLINE, for one reason that is worth the interface. The web
 * suite runs in vitest's `node` environment (`vite.config.ts`) — there is no `window`, no
 * `history`, no `popstate` — and the behaviour that most needs pinning here is precisely the
 * history behaviour: that Back reopens the run you closed, and that a two-second poll adds NO
 * entry. Testing that through jsdom would mean a new dependency to assert on a stack jsdom
 * simulates anyway; {@link memoryAddressBar} IS that stack, in twenty lines, with the entries
 * readable.
 *
 * THE INSTALLED BAR IS A MODULE SINGLETON because the store is one. `store.ts` writes through
 * {@link addressBar} without importing the browser or the wiring, which is what keeps the
 * dependency arrow one-way: store → address → nothing. `addressing.ts` is the only module that
 * knows both the store and the browser.
 */

/**
 * Everything the app does to the address bar.
 *
 * `push` adds a history entry — a navigation, something Back should undo. `replace` rewrites the
 * current one — a correction, invisible to Back. Getting that distinction wrong in either
 * direction is the trap this whole seam has: push on something incidental and Back stops working;
 * replace on a real navigation and Back skips a place the operator went.
 */
export interface AddressBar {
  /** Path plus search, exactly as {@link import('./address').formatAddress} writes it. */
  url(): string;
  /** Go somewhere new. Back returns to where we were. */
  push(url: string): void;
  /** Correct where we already are. Back is unaffected — no entry is added or consumed. */
  replace(url: string): void;
  /** Back and Forward. Returns the unsubscribe. */
  listen(onPop: (url: string) => void): () => void;
}

/** The real one. Constructed only by `addressing.ts`, and only in a browser. */
export function browserAddressBar(): AddressBar {
  const here = (): string => window.location.pathname + window.location.search;
  return {
    url: here,
    // `null` state on purpose: the address IS the state. Anything stashed in `history.state` would
    // be a second source of truth that a pasted link does not have, so a pasted link and a Back
    // would land differently — which is the whole class of bug this design is avoiding.
    push: (url) => window.history.pushState(null, '', url),
    replace: (url) => window.history.replaceState(null, '', url),
    listen: (onPop) => {
      const handler = (): void => onPop(here());
      window.addEventListener('popstate', handler);
      return () => window.removeEventListener('popstate', handler);
    },
  };
}

/** A {@link memoryAddressBar}, with the stack exposed so a test can count entries and walk it. */
export interface MemoryAddressBar extends AddressBar {
  /** Every entry, oldest first. Its LENGTH is the assertion that a poll never navigates. */
  readonly entries: readonly string[];
  /** Where in {@link entries} we are. */
  readonly index: number;
  back(): void;
  forward(): void;
}

/**
 * The browser's history stack, in memory, with the same semantics that matter:
 *
 *  - `push` TRUNCATES the forward stack. Going Back and then somewhere new discards what was ahead,
 *    exactly as a browser does, and a test that asserts otherwise is asserting fiction.
 *  - `replace` does not move the index, so Back after a correction goes where it would have gone.
 *  - `back` and `forward` at the ends do nothing rather than throw.
 *
 * Also the default installed bar, so `store.ts` never has to ask whether a browser exists: the
 * store's address writing is always real, and only the visible bar is optional.
 */
export function memoryAddressBar(start = '/'): MemoryAddressBar {
  let entries: string[] = [start];
  let index = 0;
  const listeners = new Set<(url: string) => void>();
  const current = (): string => {
    const url = entries[index];
    // Unreachable — `index` only ever moves to a position that exists. Typed rather than trusted,
    // because a bar that silently answered `undefined` would look exactly like "no navigation".
    if (url === undefined) throw new Error(`address bar at ${index} of ${entries.length}`);
    return url;
  };
  const announce = (): void => {
    for (const fn of listeners) fn(current());
  };
  return {
    get entries() {
      return entries;
    },
    get index() {
      return index;
    },
    url: current,
    push(url) {
      entries = [...entries.slice(0, index + 1), url];
      index = entries.length - 1;
    },
    replace(url) {
      entries = [...entries];
      entries[index] = url;
    },
    back() {
      if (index === 0) return;
      index -= 1;
      announce();
    },
    forward() {
      if (index >= entries.length - 1) return;
      index += 1;
      announce();
    },
    listen(onPop) {
      listeners.add(onPop);
      return () => listeners.delete(onPop);
    },
  };
}

let installed: AddressBar = memoryAddressBar();

/** Point the app at a bar. `addressing.ts` installs the browser's; tests install a memory one. */
export function installAddressBar(bar: AddressBar): void {
  installed = bar;
}

/** The bar the store writes through. */
export function addressBar(): AddressBar {
  return installed;
}
