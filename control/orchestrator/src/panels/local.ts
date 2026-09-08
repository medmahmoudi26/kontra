/**
 * Mode `local`: this host's own tmux server (ADR 0020, slice 6).
 *
 * **Local mode is kontra's dev environment, not a debug hack.** The streamer's fleet path is
 * unreachable on a laptop with no fleet, so before this mode the only way to exercise a Terminal at
 * all was to own DigitalOcean Machines. With `local` the whole feature runs on the box it is written
 * on, which is why it is a peer of the other two modes and gets the same logic: the same
 * `list-panes` probe, the same `capture-pane` snapshot, the same PTY attach.
 *
 * THE TRANSPORT IS THE ABSENCE OF ONE. `sh -c '<command>'` — the same parse `sshd` gives a remote
 * string, which is what makes "identical logic" true rather than aspirational: the command strings
 * `tmux.ts` builds are handed to a shell here exactly as they are handed to a Machine's shell there.
 *
 * DISCOVERY IS THE SESSION LIST, and it is the one thing local mode does differently — there is no
 * Pulumi inventory to read and nothing to converge. A local session exists because
 * `kontra serve --actor <dir> --mode local --tmux` created it, so `kontra-*` sessions ARE the
 * inventory. One host can hold several at once (a crawler and a parser), which is why the streamer
 * keys nodes by `<mode>:<node>/<session>` rather than by node — see `ids.ts`'s `nodeKey`.
 *
 * THE STAKES ARE HIGHER HERE THAN ON THE FLEET, and that must reach the operator rather than a
 * comment. ADR 0020's finding (2) measured a stalled viewer SEGFAULTING a tmux server, which
 * destroys every session on that socket. A fleet pane holds `journalctl -fu`, so that costs the view;
 * a local pane holds the REAL actor and handler processes (`cli/internal/tmux/tmux.go`), so there it costs a
 * running Worker. Same logic, different stakes — hence `Terminal.mode` on the wire, so a tile can say
 * which it is.
 */

import { spawn } from 'node:child_process';
import * as os from 'node:os';
import { SAFE, type ExecutionMode } from './ids';
import { captureChild, type CommandRunner, type ExecResult, type ExecTarget } from './transport';
import {
  isKnownSessionKind,
  kontraSessionKind,
  LIST_PANES_COMMAND,
  parseListPanes,
  SESSION_KINDS,
  type PaneRow,
} from './tmux';
import type { MachineTarget } from './discovery';

/**
 * The name prefix this mode USED to claim sessions by.
 *
 * Kept as a FALLBACK and not as the mechanism. Discovery is `@kontra` now (see
 * {@link KONTRA_SESSION_OPTION}) so that a session can be called `nscheck-0.1.0` or
 * `enumerate_scope` rather than carrying a namespace in the name an operator types. But a Worker
 * that was already running when this build landed has a `kontra-…` name and no tag, and a wall that
 * dropped it would report a live Worker as absent — which is the one thing ADR 0020 says a tile may
 * never do. It costs one `startsWith` per pane.
 */
export const LOCAL_SESSION_PREFIX = 'kontra-';

/**
 * Does this pane belong to a session the wall may show? The tag first, the legacy prefix second.
 *
 * A TAG MUST CLAIM A KIND THIS BUILD KNOWS — ADR 0043, and it is the whole of the change. This used
 * to be `(row.kontra ?? '') !== ''`: any non-empty value admitted a session, and `tmux.ts`'s own
 * header warned that an operator "may have set `@kontra` on a session of their own, with anything in
 * it". So one `tmux set-option` put a session holding anything at all onto a wall whose read-only
 * guarantee says nothing about what the pane CONTAINS, and the grammar that would have refused it
 * (`SAFE.command`, which admits only a journal) runs on the converge path and nowhere else.
 *
 * The three kinds are in `SESSION_KINDS`. Two are kontra's own; `watch:` is the one a human types,
 * and what it asserts is the ADR's rule — the panes hold nothing that exists only there.
 *
 * THE LEGACY PREFIX IS UNCHANGED AND IS A NARROWER DOOR. A session named `kontra-*` with no tag is
 * still admitted, because a Worker that was already running when the tag landed has exactly that
 * shape and a wall that dropped it would report a live Worker as absent — the one thing ADR 0020
 * says a tile may never do. It is a smaller hole than the old one: it needs kontra's own name
 * prefix rather than any value in any option.
 *
 * `kontra` is read defensively because a PaneRow can be built by hand and by a peer that predates
 * the field — an undefined here would throw inside a discovery loop and empty the whole wall.
 */
export function isKontraSession(row: { session: string; kontra?: string }): boolean {
  const tag = row.kontra ?? '';
  if (tag !== '') return isKnownSessionKind(tag);
  return row.session.startsWith(LOCAL_SESSION_PREFIX);
}

/** One session discovery declined to show, and why — the sentence an operator needs. */
export interface RefusedSession {
  session: string;
  tag: string;
  reason: string;
}

/**
 * Sessions on this host that carry a tag of an unknown kind.
 *
 * PURE, and separate from {@link localSessionsFromPanes} so that function stays pure too: this
 * returns what to say and the caller decides whether to say it. A refusal that is silent is
 * indistinguishable from a session that is not there, and an operator who cannot tell those apart
 * concludes the Monitor is unreliable and stops trusting the tiles that are correct.
 *
 * Only TAGGED sessions appear here. An untagged one that fails the prefix was never claiming to be
 * kontra's, and reporting every unrelated tmux session on an operator's laptop would be noise that
 * buries the one line that matters.
 */
export function refusedSessions(rows: readonly PaneRow[]): RefusedSession[] {
  const seen = new Map<string, RefusedSession>();
  for (const row of rows) {
    const tag = row.kontra ?? '';
    if (tag === '' || isKnownSessionKind(tag)) continue;
    if (seen.has(row.session)) continue;
    seen.set(row.session, {
      session: row.session,
      tag,
      reason:
        `@kontra claims kind ${JSON.stringify(kontraSessionKind(tag))}, which this build does not ` +
        `know (${SESSION_KINDS.join(', ')}). A session kontra did not create is shown only when it ` +
        `is tagged watch:<label>, which asserts its panes hold nothing that exists only there — ` +
        `see ADR 0043. To watch something interactive, supervise it and tail its journal.`,
    });
  }
  return [...seen.values()];
}

/**
 * A hostname that is really a container id: 12 or 64 hex characters, which is what Docker writes
 * into `/etc/hostname` when nobody sets one.
 *
 * IT IS NOT A NAME, IT IS A LEASE. The tree drew `54af48ee4c6a` as the Machine holding every local
 * worker, and it identifies nothing an operator can act on: it is not the host the tmux server runs
 * on (that is the machine outside), it is not stable across a `compose up`, and it cannot be
 * pinged, ssh'd or looked up. `localhost` is less specific and true.
 */
const CONTAINER_ID = /^[0-9a-f]{12}$|^[0-9a-f]{64}$/;

/**
 * The node name a `local:` id carries.
 *
 * The host's own name, so an id read out of a saved Dashboard still says WHERE that Terminal was —
 * `local:main-droplet/kontra-webcrawl/actor` is meaningful in a log where `local:localhost/…` is not.
 * Sanitised through `SAFE.host` and falling back to `localhost`, because a hostname is a value this
 * process did not choose.
 *
 * AND `os.hostname()` IS THE CONTAINER'S when this runs in one, which is every deployment that has
 * a Dashboard. The premise above still holds — a real host name beats `localhost` — but a container
 * id is not a real host name, and the mode this labels is the one whose whole meaning is "here".
 * `KONTRA_PANEL_LOCAL_HOST` is the way to say the true name of the machine outside, and it wins;
 * with nothing set, the honest answer is the word the user reads as an answer.
 */
export function localHost(): string {
  const raw = process.env.KONTRA_PANEL_LOCAL_HOST ?? os.hostname();
  if (!SAFE.host.test(raw) || CONTAINER_ID.test(raw)) return 'localhost';
  return raw;
}

/** The local target. There is no address to dial, so `host` carries the same label the id does. */
export function localTarget(host = localHost()): ExecTarget {
  return { mode: 'local', machine: host, host };
}

/** The argv `sh -c '<command>'`, as one array. No shell interpolation happens on this side: the
 * command is a single argv element, exactly as it is for `ssh`. */
export function localShArgs(command: string): string[] {
  return ['-c', command];
}

/**
 * The local transport's one-shot runner.
 *
 * `stdio[0]` is `'ignore'` for the same reason it is in `ssh.ts`: there must be no descriptor a byte
 * could reach a session through. Read-only is a property of this seam in every mode, and local mode
 * is the one where breaking it would type into a running Worker.
 */
export const localRunner: CommandRunner = {
  async run(_target: ExecTarget, command: string, opts): Promise<ExecResult> {
    // `detached: true` puts the shell in its OWN process group so a timeout can kill the group. It is
    // not tidiness: MEASURED on dash, `sh -c 'sleep 30'` forks rather than execs, so SIGKILLing the
    // shell alone leaves the grandchild running AND holding our pipes — on the operator's own box,
    // which in this mode is the box the Dashboard is watching. See `captureChild`'s EXIT_DRAIN_MS.
    const child = spawn('sh', localShArgs(command), {
      stdio: ['ignore', 'pipe', 'pipe'],
      detached: true,
    });
    const capture: { binary: string; timeoutMs?: number; killGroup: boolean } = {
      binary: 'sh',
      killGroup: true,
    };
    if (opts?.timeoutMs !== undefined) capture.timeoutMs = opts.timeoutMs;
    return await captureChild(child, capture);
  },
};

/** The actor a local session is running, read off its name. `kontra-webcrawl` → `webcrawl`. */
export function actorFromSession(session: string): string {
  return session.startsWith(LOCAL_SESSION_PREFIX)
    ? session.slice(LOCAL_SESSION_PREFIX.length)
    : '';
}

/**
 * The actor and version out of an `@kontra` tag — `actor:<name>:<version>`.
 *
 * WHY THIS EXISTS RATHER THAN PARSING THE SESSION NAME. A session is `<actor>-<version>` now, and
 * splitting that back apart is guesswork: `nscheck-0.1.0` is unambiguous and `my-actor-2` is not.
 * The tag was written by the thing that knew both halves, so it is read rather than reconstructed.
 *
 * Both return '' for a tag of any other kind, which is what a workflow session is — it holds no
 * actor, and inventing one would put a caller's workflow on the Actors surface.
 */
export function actorFromTag(tag: string): string {
  const [kind, actor] = tag.split(':');
  return kind === 'actor' ? (actor ?? '') : '';
}

export function versionFromTag(tag: string): string {
  const [kind, , version] = tag.split(':');
  return kind === 'actor' ? (version ?? '') : '';
}

/**
 * The local inventory, from one `list-panes -a`. PURE, so the grouping is pinned by a test rather
 * than by whatever happens to be running on the machine the suite is on.
 *
 * One MachineTarget PER SESSION, all sharing the host as their node: that is the shape the rest of
 * the streamer consumes, and the reason `nodeKey` exists.
 *
 * A session whose name is not `kontra-*` is skipped — an operator's own tmux is not the Dashboard's
 * business, and a Terminal is a view of a Worker. A name that is not a legal id segment is skipped
 * too, never repaired: a value we did not validate must not reach a command.
 */
export function localSessionsFromPanes(rows: readonly PaneRow[], host = localHost()): MachineTarget[] {
  const bySession = new Map<string, { windows: string[]; kontra: string }>();
  for (const row of rows) {
    if (!isKontraSession(row)) continue;
    if (!SAFE.session.test(row.session)) continue;
    if (!SAFE.window.test(row.window)) continue;
    const tag = row.kontra ?? '';
    const found = bySession.get(row.session);
    if (found) {
      if (!found.windows.includes(row.window)) found.windows.push(row.window);
      // The first pane that carries a tag wins. tmux sets the option on the SESSION, so every pane
      // of one reports the same value — but a pane listed before the option was set does not.
      if (found.kontra === '') found.kontra = tag;
    } else {
      bySession.set(row.session, { windows: [row.window], kontra: tag });
    }
  }

  const out: MachineTarget[] = [];
  for (const [session, { windows, kontra }] of bySession) {
    out.push({
      mode: 'local',
      machine: host,
      host,
      // A local Worker has no public address and belongs to no fleet; the honest value is empty,
      // which every renderer already draws as `-`. Inventing `tag: 'local'` would put the mode in
      // two places and let a selector disagree with itself.
      publicIp: '',
      tag: '',
      fleet: '',
      // FROM THE TAG WHEN THERE IS ONE, and only from the name when there is not. `@kontra` carries
      // `actor:<name>:<version>` verbatim, which is how a local Worker finally names the right
      // Temporal queue — `queueForMachine` needs BOTH halves, and a session name never carried the
      // version. An untagged legacy session still resolves its actor from `kontra-<actor>`, and
      // still reports no version rather than guessing one.
      actor: actorFromTag(kontra) || actorFromSession(session),
      version: versionFromTag(kontra),
      session,
      // What is really there, and nothing else: local session existence is owned by `kontra serve`,
      // there is no converge to offer, so there are no windows to promise.
      windows,
    });
  }
  return out.sort((a, b) => (a.session < b.session ? -1 : a.session > b.session ? 1 : 0));
}

/**
 * Every `kontra-*` session on this host's tmux server.
 *
 * `listCommand` is injectable for ONE reason, and it is a safety rule rather than a convenience:
 * ADR 0020's consequence is that a test which touches tmux must use a PRIVATE socket
 * (`tmux -L kontratest`) — establishing the ADR's findings on the default socket segfaulted the
 * operator's server and destroyed a day-old session. So a test points this at its own socket, and
 * production uses the default one.
 */
export async function discoverLocalSessions(
  run: CommandRunner,
  opts?: { host?: string; listCommand?: string; timeoutMs?: number }
): Promise<MachineTarget[]> {
  const host = opts?.host ?? localHost();
  const res = await run.run(localTarget(host), opts?.listCommand ?? LIST_PANES_COMMAND, {
    timeoutMs: opts?.timeoutMs ?? 10_000,
  });
  // A non-zero exit here is "no tmux" or "no server running", which for THIS mode means no local
  // Worker is running in a session — an empty inventory, not an error. The fleet reports the same
  // situation as a tile with a converge because a Machine exists either way; a local host that is
  // not running a Worker has nothing to show, and inventing a tile for it would put a converge
  // affordance in front of an operator that would have to START A WORKER to satisfy it.
  if (res.code !== 0) return [];
  const rows = parseListPanes(res.stdout);
  // SAY WHAT WAS REFUSED, once per poll and only for a session that CLAIMED to be kontra's. ADR
  // 0043's refusal is deliberate and an operator who tagged a session is owed the reason — without
  // this the session simply is not there, which reads as a broken Monitor rather than as a rule.
  for (const refused of refusedSessions(rows)) {
    console.warn(`panels: not showing tmux session ${JSON.stringify(refused.session)} — ${refused.reason}`);
  }
  return localSessionsFromPanes(rows, host);
}

/** Which mode this file implements. Exported so `modes.ts` cannot wire it under the wrong one. */
export const LOCAL_MODE: ExecutionMode = 'local';
