/**
 * Terminal ids, and the whitelist every value crosses before it reaches a shell (ADR 0020).
 *
 * A **Terminal** is a read-only view of one window of one tmux session, named
 * `<mode>:<node>/<session>/<window>`. The first segment is the EXECUTION MODE — the place a Worker
 * runs and therefore the transport that reaches its tmux server — and it is a selector facet, not a
 * constant: a Dashboard slot can select `mode=local` exactly as it selects `role=crawl`.
 *
 *   fleet:<machine>/<session>/<window>      a Machine, reached over SSH
 *   docker:<container>/<session>/<window>   a worker container, reached with `docker exec`
 *   local:<host>/<session>/<window>         this host's tmux server, reached directly
 *
 * Fleet machine names are deterministic (`kf-<role>-NN`), so the id survives a Machine restart and a
 * saved Dashboard can point at it. The same is true of the other two: `cli/scale.go` mints
 * `kontra-<actor>-<version>-<n>` container names, and a host's name does not change under it.
 *
 * WHY THE REGEXES ARE HERE AND NOT AT THE CALL SITE. Every one of these values is interpolated
 * into a command that runs as root on a fleet Machine, through `ssh`. The rule is the one
 * `infra/programs/machine.ts` already states for its own script: a value that reaches `sh`
 * unvalidated is a command, not a string. So parsing an id and admitting a value are the same
 * operation — {@link parseTerminalId} either returns bounded parts or throws, and there is no
 * path that produces an id without going through it.
 *
 * The ids come from a browser. `fleet:kf-crawl-01/kontra-webcrawl/$(id)` is what an attacker
 * sends, and the only reason it cannot become a command is this file.
 */

/** Everything interpolated into a remote command, bounded first. Same stance as `machine.ts`'s
 * `SAFE` — deny by default, and narrow enough that no shell metacharacter can pass. */
export const SAFE = {
  /** `kf-<role>-NN`, minted by `programs/fleet.ts`. Deterministic, which is what makes a saved
   * Dashboard slot able to name a Machine at all. */
  machine: /^kf-[a-z0-9-]{2,16}-\d{2}$/,
  /** tmux session name — ours are `kontra-<actor>`, but an operator's hand-made session is a
   * legitimate Terminal too, so this admits what tmux itself allows minus the shell. */
  session: /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/,
  window: /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/,
  /** An address, never a URL: this is what `root@<host>` is built from, and what a `local:` id
   * carries as its node — this host's own name. */
  host: /^[A-Za-z0-9][A-Za-z0-9.-]{0,253}$/,
  /** A docker container name, as the engine itself bounds them (`[a-zA-Z0-9][a-zA-Z0-9_.-]+`).
   * `cli/scale.go` mints `kontra-<actor>-<version>-<n>`, which is well inside this — but the value
   * arrives from `docker ps` and from a browser, so it is admitted rather than assumed. */
  container: /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/,
  /**
   * A window's command — and it may ONLY follow a systemd journal.
   *
   * Narrower than "no shell metacharacters" on purpose, twice over. A window's command is run as
   * root on a Machine by whoever can start the converge workflow, and Temporal is not the boundary
   * that `POST /api/infra/stacks/:fqn/:op` is; so the authority this admits is "which unit's journal
   * to follow", not "what to run". And ADR 0020 makes it an INVARIANT that a session a Terminal
   * attaches to holds only journals: finding (2) measured a stalled viewer segfaulting a tmux
   * server, which destroys every session on that socket — survivable only because the panes hold
   * nothing but `journalctl`. This regex is that invariant, enforced.
   */
  command: /^journalctl(?: -n \d{1,5})?(?: --no-pager)? -fu [A-Za-z0-9][A-Za-z0-9.@_-]{0,63}\.service$/,
} as const;

export type SafeKind = keyof typeof SAFE;

/** Throw unless `value` matches the whitelist for `kind`. The message names the field and shows
 * the value, because the common cause is a typo and the second-commonest is an attack. */
export function assertSafe(kind: SafeKind, value: string): string {
  if (!SAFE[kind].test(value)) {
    throw new Error(
      `panels: ${kind}=${JSON.stringify(value)} is not safe to interpolate into a remote command`
    );
  }
  return value;
}

/**
 * The three places a Worker runs, which are also the three prefixes an id can carry.
 *
 * ORDER IS THE DEFAULT DISCOVERY ORDER, and `fleet` is first because it is the mode that owns
 * Machines. Nothing may assume `fleet:` — that assumption is what this array replaced.
 */
export const MODES = ['fleet', 'docker', 'local'] as const;
export type ExecutionMode = (typeof MODES)[number];

/**
 * Which whitelist a mode's NODE segment must match — and this table is a security boundary, not a
 * convenience.
 *
 * A fleet node becomes part of a `ControlPath` on disk and of an `ssh` argument, so it is held to
 * the deterministic `kf-<role>-NN` shape `programs/fleet.ts` mints. A docker node becomes an argv
 * element of `docker exec`, so it is held to what the engine itself allows. A local node is
 * interpolated into NO command at all (there is no transport to address) but it is still an id
 * segment and a log line, so it is held to a hostname.
 */
export const NODE_KIND: Record<ExecutionMode, SafeKind> = {
  fleet: 'machine',
  docker: 'container',
  local: 'host',
} as const;

export function isExecutionMode(value: string): value is ExecutionMode {
  return (MODES as readonly string[]).includes(value);
}

/** Admit a node for its mode, or throw. */
export function assertNode(mode: ExecutionMode, node: string): string {
  return assertSafe(NODE_KIND[mode], node);
}

export interface TerminalRef {
  /** The execution mode — which of the three places this Terminal's tmux server is. */
  mode: ExecutionMode;
  /** The node: a Machine, a container, or this host. Named `machine` because the wire keeps that
   * field name (additively) and because the fleet is where the word is literal. */
  machine: string;
  session: string;
  window: string;
}

/** `<mode>:<node>/<session>/<window>`, from already-validated parts. `mode` defaults to `fleet` so
 * a caller that predates slice 6 cannot silently mint an id for a different transport. */
export function terminalId(ref: Omit<TerminalRef, 'mode'> & { mode?: ExecutionMode }): string {
  const mode = ref.mode ?? 'fleet';
  assertNode(mode, ref.machine);
  assertSafe('session', ref.session);
  assertSafe('window', ref.window);
  return `${mode}:${ref.machine}/${ref.session}/${ref.window}`;
}

/**
 * Parse a Terminal id, or throw.
 *
 * Strict on structure as well as on characters: exactly one `:` and exactly two `/`, so
 * `fleet:kf-crawl-01/a/b/../../etc` is a parse failure rather than a traversal, and an id with a
 * second colon cannot smuggle a second mode. The node is admitted by the whitelist for the mode it
 * claims — `fleet:main-droplet/…` is a parse failure even though `main-droplet` is a fine hostname,
 * because a fleet node reaches a `ControlPath` and an `ssh` argument.
 */
export function parseTerminalId(id: string): TerminalRef {
  const colon = id.indexOf(':');
  if (colon < 0) throw new Error(`panels: not a Terminal id: ${JSON.stringify(id)}`);
  const mode = id.slice(0, colon);
  if (!isExecutionMode(mode)) {
    throw new Error(
      `panels: unsupported Terminal mode ${JSON.stringify(mode)} — one of ${MODES.join(', ')}`
    );
  }
  const parts = id.slice(colon + 1).split('/');
  if (parts.length !== 3) {
    throw new Error(`panels: not a Terminal id: ${JSON.stringify(id)}`);
  }
  const [machine, session, window] = parts as [string, string, string];
  assertNode(mode, machine);
  assertSafe('session', session);
  assertSafe('window', window);
  return { mode, machine, session, window };
}

/**
 * The key that identifies one tmux SERVER-plus-session: `<mode>:<node>/<session>`.
 *
 * Slice 1 keyed the streamer's Machine map by the Machine name, which was sound while one node held
 * exactly one session. `local` breaks that — one host runs `kontra-webcrawl` and `kontra-parse` at
 * the same time, and keying by the host would have silently kept ONE of them and dropped the tiles
 * of the other. This is the id with its window removed, so it is derivable from any id a client
 * sends without a second parse.
 */
export function nodeKey(ref: { mode?: ExecutionMode; machine: string; session: string }): string {
  return `${ref.mode ?? 'fleet'}:${ref.machine}/${ref.session}`;
}

/**
 * The session-converge workflow's id for a Machine: `tmux-<machine>`.
 *
 * Lives here rather than beside the workflow so a CLIENT can name a workflow without importing
 * workflow code — and so it is not an exported function inside a workflow module, where Temporal
 * would register it as a workflow of its own.
 */
export function tmuxWorkflowId(machine: string): string {
  assertSafe('machine', machine);
  return `tmux-${machine}`;
}

/** Parse without throwing — for the WS read path, where a bad id is an `error` message to one
 * client and never an exception that takes the socket (or the process) down. */
export function tryParseTerminalId(id: string): TerminalRef | null {
  try {
    return parseTerminalId(id);
  } catch {
    return null;
  }
}
