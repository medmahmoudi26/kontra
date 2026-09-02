/**
 * Mode `docker`: a worker CONTAINER on this host's engine (ADR 0020, slice 6).
 *
 * `cli/scale.go` turns a built worker image into live worker containers — each one a Temporal poller
 * on the actor's queue, named `kontra-<actor>-<version>-<n>` and labelled `kontra.actor=<a>@<v>` plus
 * `kontra.managed=true`. That label is the reliable filter this file discovers through: it is set by
 * the only code that creates these containers, its value carries the placement a Terminal needs, and
 * `listManaged` already treats it as the definition of "a worker scale owns".
 *
 * THE TRANSPORT IS `docker exec <container> sh -c '<command>'`. Same string, same shell, same tmux
 * commands as the other two modes — the container's `sh` plays the part `sshd` plays on a Machine.
 * No `-t`: the PTY a live attach needs is created INSIDE by `script -q -c … /dev/null`, exactly as it
 * is on a Machine, and asking the engine for a TTY as well would be a second pseudo-terminal with
 * OUR side of it writable. There is no `-i`, ever, for the same reason `stdio[0]` is `'ignore'`.
 *
 * THE GAP THIS MODE SURFACES, and it is a gap in the WORKER, not in the filter. Nothing puts a tmux
 * session inside a worker container today: `worker-entrypoint.sh` runs the actor and the handler
 * directly, and `runScale` creates no session. So a container is discovered, its probe answers
 * `no-tmux` (or `absent`), and the tile SAYS SO with a sentence — which is the honest state and
 * strictly better than a mode that silently discovers nothing. Closing it is a change to the worker
 * image plus a converge that is not `journalctl` (a container has no systemd journal), and both are
 * outside this slice: `ids.ts`'s `SAFE.command` admits only a journal follower, deliberately.
 *
 * THE STAKES ARE THE LOCAL ONES, not the fleet's: if a session ever does hold the container's actor
 * and handler, a viewer that crashes that tmux server costs a running Worker (ADR 0020's finding 2).
 * `Terminal.mode` on the wire is what lets a tile say so.
 */

import { spawn } from 'node:child_process';
import { assertNode, SAFE, type ExecutionMode } from './ids';
import { captureChild, type CommandRunner, type ExecResult, type ExecTarget } from './transport';
import { DEFAULT_WINDOWS } from './converge';
import { sessionNameFor, type MachineTarget } from './discovery';

/** The label `cli/scale.go` marks a managed worker with; its value is `<actor>@<version>`. Kept
 * byte-identical to `managedLabel` there — a divergence would be a Dashboard that sees no workers
 * and no error. */
export const MANAGED_LABEL = 'kontra.actor';

/**
 * `docker ps`, as a fixed argv with no interpolation at all.
 *
 * Tab-separated because a container name cannot contain a tab and neither can a label value the
 * engine accepted, so parsing cannot be confused by either. `-a` includes stopped containers on
 * purpose: a worker that EXITED is exactly the thing an operator is looking for, and its tile says
 * `reachable: fail` with the engine's own sentence rather than vanishing.
 */
export const DOCKER_PS_ARGS: readonly string[] = [
  'ps',
  '-a',
  '--filter',
  `label=${MANAGED_LABEL}`,
  '--format',
  `{{.Names}}\t{{.Label "${MANAGED_LABEL}"}}\t{{.State}}`,
] as const;

/** The argv for one exec. The container name is admitted by `SAFE.container` first — it arrives from
 * the engine and from a browser, and it becomes an argv element either way. */
export function dockerExecArgs(target: ExecTarget, command: string): string[] {
  return ['exec', assertNode('docker', target.machine), 'sh', '-c', command];
}

/** A container target. `host` carries the container name too: on the control-plane network that IS
 * how it is addressed, and nothing in this mode dials an address. */
export function dockerTarget(container: string): ExecTarget {
  return { mode: 'docker', machine: container, host: container };
}

/**
 * The docker transport's one-shot runner.
 *
 * `stdio[0]` is `'ignore'`, so `docker exec` gets no stdin to forward even if a future `-i` slipped
 * in — the read-only boundary is two deep here rather than one.
 */
export const dockerRunner: CommandRunner = {
  async run(target: ExecTarget, command: string, opts): Promise<ExecResult> {
    const child = spawn('docker', dockerExecArgs(target, command), {
      stdio: ['ignore', 'pipe', 'pipe'],
      // Its own process group, so a timeout kills the CLI and anything it forked here — see
      // `local.ts` for the measurement. It does NOT reach the process inside the container: the
      // engine keeps running an exec whose client went away, which is a documented docker behaviour
      // and a real limit of this mode rather than something this flag fixes.
      detached: true,
    });
    const capture: { binary: string; timeoutMs?: number; killGroup: boolean } = {
      binary: 'docker',
      killGroup: true,
    };
    if (opts?.timeoutMs !== undefined) capture.timeoutMs = opts.timeoutMs;
    return await captureChild(child, capture);
  },
};

export interface WorkerContainer {
  container: string;
  actor: string;
  version: string;
  /** The engine's own word: `running`, `exited`, `created`, … */
  state: string;
}

/**
 * Parse `docker ps`'s tab-separated output. PURE, so the shape is pinned without a daemon.
 *
 * A row whose name is not a legal container name, or whose label is not `<actor>@<version>`, is
 * DROPPED rather than repaired — the same rule `machinesFromStack` follows for a machine name. The
 * name becomes an argv element of a command that runs as the container's root.
 */
export function parseWorkerContainers(stdout: string): WorkerContainer[] {
  const out: WorkerContainer[] = [];
  for (const line of stdout.split('\n')) {
    if (!line.trim()) continue;
    const [names = '', label = '', state = ''] = line.split('\t');
    // `.Names` is comma-separated when a container carries several; the first is the one `scale`
    // created it with.
    const container = (names.split(',')[0] ?? '').trim();
    if (!SAFE.container.test(container)) continue;
    const at = label.lastIndexOf('@');
    const actor = at < 0 ? label.trim() : label.slice(0, at).trim();
    const version = at < 0 ? '' : label.slice(at + 1).trim();
    out.push({ container, actor, version, state: state.trim() });
  }
  return out.sort((a, b) => (a.container < b.container ? -1 : a.container > b.container ? 1 : 0));
}

/** One container as a discovery target. */
export function machineFromContainer(c: WorkerContainer): MachineTarget {
  return {
    mode: 'docker',
    machine: c.container,
    host: c.container,
    publicIp: '',
    // Not a fleet tag. A container's placement is its label, and `tag` is a fleet facet — leaving
    // it empty keeps the two vocabularies from bleeding into each other.
    tag: '',
    // A container belongs to no fleet. `state` is not smuggled in here either: it is not a fleet,
    // and the probe reports liveness in the field that means liveness.
    fleet: '',
    actor: c.actor,
    version: c.version,
    // The same rule a Machine's session follows — `<actor>-<version>`. A container HAS a version
    // (`cli/scale.go` puts it in the container name), so unlike a local session there is nothing to
    // guess: two versions of one actor scaled side by side get two session names, which is what
    // makes the wall's tiles distinguishable at all.
    session: sessionNameFor(c.actor, c.version),
    // The windows a worker session WOULD hold, so a container with no session is a visible tile with
    // a sentence instead of an absence — ADR 0020's rule that a node which exists is never missing
    // from the wall. Nothing here promises they exist; the probe decides that.
    windows: DEFAULT_WINDOWS.map((w) => w.name),
  };
}

/**
 * Every managed worker container on this host's engine.
 *
 * Runs `docker ps` directly rather than through {@link dockerRunner} — that runner's job is to reach
 * INTO a container, and discovery is a question about the engine. An engine that is unreachable, or a
 * `docker` binary that is not installed (the streamer's own container may well not have one), comes
 * back as an empty list with a sentence for the caller to log: nothing to show is nothing to show,
 * and the fleet's tiles must not disappear because a docker socket is missing.
 */
export async function discoverWorkerContainers(opts?: {
  timeoutMs?: number;
}): Promise<{ machines: MachineTarget[]; error?: string }> {
  const child = spawn('docker', [...DOCKER_PS_ARGS], {
    stdio: ['ignore', 'pipe', 'pipe'],
    detached: true,
  });
  const res = await captureChild(child, {
    binary: 'docker',
    timeoutMs: opts?.timeoutMs ?? 10_000,
    killGroup: true,
  });
  if (res.code !== 0) {
    const first = res.stderr.trim().split('\n')[0] ?? '';
    return {
      machines: [],
      error: `docker ps exited ${res.code}${res.timedOut ? ' after timing out' : ''}${first ? `: ${first}` : ''}`,
    };
  }
  return { machines: parseWorkerContainers(res.stdout).map(machineFromContainer) };
}

/** Which mode this file implements. */
export const DOCKER_MODE: ExecutionMode = 'docker';
