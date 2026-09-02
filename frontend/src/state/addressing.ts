/**
 * The one module that knows both the store and the browser: cold load, Back, and Forward.
 *
 * THE ADDRESS IS THE SOURCE OF TRUTH ON ARRIVAL. `store.ts` declares `view: 'workflows'` as its
 * initial value, but a session that opened `/runs/nscheck-123` never sees it — {@link
 * startAddressing} runs BEFORE the first render (`main.tsx`) and lands the store on whatever the
 * URL says. That ordering is the fix for the reported bug, not decoration: seeding after mount
 * would paint Workflows for a frame, mount its poller, and then swap, which is the flicker the
 * operator would read as "the state still isn't stable".
 *
 * BACK DRIVES THE STORE THROUGH `setState`, NOT THROUGH THE ACTIONS, and that is deliberate. The
 * actions write the bar (`store.ts`'s `addressed`); calling one from a `popstate` handler would
 * push a new entry for a place the browser had just moved us to, which turns Back into a loop that
 * cannot leave. `setState` is the one path into this store that is silent, which is exactly what a
 * change already reflected in the bar needs.
 */

import { DEFAULT_ADDRESS, formatAddress, parseAddress, stateFor } from './address';
import { addressBar, browserAddressBar, installAddressBar, type AddressBar } from './addressBar';
import { useAppStore } from './store';

/**
 * Land the store on whatever the bar says, and correct the bar if it said it awkwardly.
 *
 * REPLACE, NEVER PUSH. Everything here is a correction to an address already visited — `/` written
 * out as `/workflows`, a trailing slash dropped, a junk `?kind=` removed, a path this app cannot
 * mean falling back to Workflows. Pushing any of them would put the pre-correction URL in the
 * history, so Back would return to the address we just decided was wrong and correct it again.
 *
 * A RETIRED ADDRESS IS CORRECTED BY THE SAME LINE, and that is why there is no redirect code here.
 * `parseAddress` reads `/runs/nscheck-123` as the address that run has now, `formatAddress` prints
 * `/workflows?run=nscheck-123`, and the comparison below sees they differ and replaces. Replace is
 * exactly right for a redirect: Back must not return to a surface that no longer exists.
 */
function land(bar: AddressBar, url: string): void {
  const address = parseAddress(url) ?? DEFAULT_ADDRESS;
  useAppStore.setState(stateFor(address));
  const canonical = formatAddress(address);
  if (canonical !== url) bar.replace(canonical);
}

/**
 * Wire the store to the address bar, and land on the current address.
 *
 * Called once from `main.tsx` before `createRoot(...).render(...)`. Returns the unsubscribe, which
 * the app never uses and a test always does.
 */
export function startAddressing(bar: AddressBar = browserAddressBar()): () => void {
  installAddressBar(bar);
  land(bar, bar.url());
  // Back and Forward. `addressBar()` rather than the closed-over `bar` so that a later install
  // cannot leave this handler writing corrections into a bar nobody is reading.
  return bar.listen((url) => land(addressBar(), url));
}
