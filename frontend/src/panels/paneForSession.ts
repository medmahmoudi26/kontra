/**
 * Which Terminal is the worker THIS page served, when more than one answers to the name.
 *
 * A SESSION NAME IS NOT UNIQUE ACROSS MACHINES, and that is the whole reason this is a function.
 * `panels/discovery.ts:sessionNameFor` names a fleet Machine's session `<actor>-<version>` — the
 * same rule `@core/panels/tmux:actorSession` uses here — so nine droplets running `probe` 0.1.0 all report a
 * session called `probe-0_1_0`, and so does the local worker the workbench just started. Taking
 * the first match means the pane beside the editor is whichever one the inventory happened to list
 * first, usually a droplet.
 *
 * WHY LOCAL WINS. The console beside the pane prints `tmux attach -t <session>`, and that command
 * run on this host reaches the LOCAL session and nothing else. A pane showing a droplet's screen
 * above a command that attaches to a different machine's worker is two workers under one name, with
 * nothing on screen saying which is which — and the reason the pane exists at all is to be the
 * evidence for what the button just did.
 *
 * WITH NO LOCAL MATCH THE FIRST ONE STANDS. An Actor served from a terminal into a docker session,
 * or one only the fleet is running, still has a pane worth showing; it is labelled with its host in
 * the pane's own header.
 */

import type { Terminal } from './panelsClient';
import { parseTileRef } from './chrome/tileRef';

export function paneForSession(terminals: Terminal[], session: string): Terminal | null {
  if (!session) return null;
  const named = terminals.filter((t) => parseTileRef(t.id).session === session);
  return named.find((t) => parseTileRef(t.id).mode === 'local') ?? named[0] ?? null;
}
