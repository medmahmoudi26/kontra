/**
 * Discovery: the Fleet inventory is the spine (ADR 0020).
 *
 * The Machine list comes from Pulumi's checkpoint, read as a LIBRARY CALL — `listStacks()` /
 * `readStack()` from `infra/state.ts`, in-container, no HTTP, no token. That matters twice: the
 * streamer already runs in the process family that owns the state directory, and the browser never
 * needs a credential that could reach the infra routes to see what Machines exist.
 *
 * A Machine that exists with NO session is a visible tile with a converge, never an absence —
 * `CONTEXT-MAP.md` frames this as Fleet publishing what it has and Execution consuming it, and a
 * missing tile is indistinguishable from a Machine nobody deployed.
 *
 * The `inventory` output is the whole interface Fleet gives the rest of the system
 * (`programs/fleet.ts`), so this file reads exactly that plus the placement echo beside it, and
 * nothing provider-shaped.
 */

import { FLEET_PROJECT } from '../infra/stacks';
import type { StackState } from '../infra/state';
import { SAFE, type ExecutionMode } from './ids';
import { DEFAULT_WINDOWS } from './converge';
import { tmuxSafeName } from './tmux';

/**
 * One node, resolved to everything a Terminal on it needs.
 *
 * Called `MachineTarget` because the fleet is where it started and where the word is literal; since
 * slice 6 a "node" is also a worker container (`docker`) or this host (`local`), discovered by
 * `docker.ts` and `local.ts` into exactly this shape. What the three have in common is precisely what
 * a Terminal needs: something to address, a session on it, and the placement to label it with.
 */
export interface MachineTarget {
  /**
   * Which of the three places this node is, and therefore which transport reaches it.
   *
   * OPTIONAL, and absent means `fleet` — the mode this file discovers. That is not a shortcut: it is
   * what lets every MachineTarget literal written before slice 6 (this repo's tests, and
   * `web/e2e/fakeFleet.ts`, which slice 4 owns and slice 6 must not touch) keep meaning exactly what
   * it meant. Read it through `modeOf()`, never `?? 'fleet'` at a call site.
   */
  mode?: ExecutionMode;
  machine: string;
  /** Private VPC address — `MachineEntry.host`, documented as how the Controller reaches it. */
  host: string;
  publicIp: string;
  /** The fleet's label — a DigitalOcean tag, an inventory group, the `kf-<tag>-NN` name prefix,
   *  and nothing that anything dispatches on. Called `role` until that was corrected. */
  tag: string;
  /**
   * The stack name — which fleet these Machines are.
   *
   * It was `run`, "one bounded period of work", which named a period nothing measured with a
   * string the caller invented. The stack is now named after what it PLACES (`<actor>-<version>`),
   * so this is derivable, typeable into `kontra fleet down`, and means one thing.
   */
  fleet: string;
  /** '' when the stack carries no placement. */
  actor: string;
  version: string;
  /** The tmux session a Terminal on this Machine attaches to — see {@link sessionNameFor}. */
  session: string;
  /** Windows the converge would create. The probe replaces this with what is really there. */
  windows: string[];
}

/** The state seam — an interface so tests never touch a checkpoint on disk. */
export interface StackReader {
  listStacks(): Promise<string[]>;
  readStack(fqn: string): Promise<StackState | null>;
}

/** Which address the streamer SSHes to.
 *
 * `MachineEntry.host` is documented as "how the Controller reaches it", and the private VPC path
 * is the cheaper and less exposed one, so it is the default. It is a knob because the placement
 * path in `programs/fleet.ts` uses the PUBLIC address, and which of the two actually routes from
 * this container has never been measured — no Machine exists in this environment to measure it on.
 */
export function sshAddress(m: MachineTarget): string {
  // Only the fleet dials an address. A container is addressed by its name and this host by nothing at
  // all, so the knob does not apply to them — and reading it for them would let
  // `KONTRA_PANEL_SSH_ADDRESS=public` blank a local node's own label.
  if ((m.mode ?? 'fleet') !== 'fleet') return m.host;
  const address = process.env.KONTRA_PANEL_SSH_ADDRESS ?? 'private';
  const first = address === 'public' ? m.publicIp : m.host;
  return first || m.publicIp || m.host;
}

interface RawInventoryEntry {
  name?: unknown;
  host?: unknown;
  publicIp?: unknown;
  tag?: unknown;
  /** Pre-rename checkpoints. See the read in `machinesFromStack`. */
  role?: unknown;
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

/**
 * The tmux session a Machine's Terminals attach to: **`<actor>-<version>`**.
 *
 * NO `kontra-` PREFIX. It used to be `kontra-<actor>`, and the prefix was doing two jobs badly. It
 * was the Monitor's discovery mechanism for local sessions (`local.ts`), which is now a tmux user
 * option that can also say what KIND a session is; and it namespaced kontra's sessions inside a
 * tmux server, which is not a problem worth a prefix on every name an operator types.
 *
 * THE VERSION IS IN IT NOW, and that is the substantive change rather than the prefix. A Machine's
 * session named `kontra-nscheck` said which Actor was on it and not WHICH BUILD — so two fleets
 * running two versions of one Actor produced two identical session names, and `tmux attach -t
 * kontra-nscheck` was ambiguous the moment a deploy was in flight.
 *
 * Falls back to the tag for a Machine with no placement (a `fleet up` with no actor), and to
 * `fleet` for one with neither. A name the whitelist refuses is replaced rather than repaired: it
 * reaches a remote command, and there is no safe way to rewrite an unsafe one.
 *
 * `fleet` RATHER THAN `actor`, WHICH IS WHAT {@link actorSession} ANSWERS, and the difference is
 * intended rather than a drift: this names a MACHINE, which still has Terminals when no Actor is
 * placed on it, so calling its session `actor` would be a lie about what is running there. The two
 * fallbacks and the reason they differ are rows in `conformance/queues.json` §tmux_session, which
 * `cli/fleet.go:fleetSessionName` executes as well — a difference that is not written down is one
 * nobody can tell from a typo when it goes red.
 */
export function sessionNameFor(actor: string, version: string, tag = ''): string {
  // Sanitised HERE, where the name is minted — tmux rewrites `.` and `:` to `_` at creation, so an
  // unsanitised `nscheck-0.1.0` would be looked for under a name the server does not have. See
  // {@link tmuxSafeName} for what that costs.
  const base = tmuxSafeName(actor ? (version ? `${actor}-${version}` : actor) : tag || 'fleet');
  return SAFE.session.test(base) ? base : 'fleet';
}

/**
 * Machines from one stack's outputs. Pure, so the shape of a checkpoint is pinned by a test rather
 * than by a live fleet.
 *
 * Entries whose machine name is not a Fleet machine name are DROPPED rather than repaired: a name
 * that does not match `kf-<role>-NN` cannot be a Terminal id, and quietly rewriting it would put a
 * value we never validated into a remote command.
 */
export function machinesFromStack(fqn: string, outputs: Record<string, unknown>): MachineTarget[] {
  const fleet = fqn.split('/')[1] ?? fqn;
  const inventory = outputs.inventory;
  if (!inventory || typeof inventory !== 'object') return [];
  const actor = str(outputs.actorName);
  const version = str(outputs.actorVersion);

  const out: MachineTarget[] = [];
  for (const [key, value] of Object.entries(inventory as Record<string, unknown>)) {
    const e = (value ?? {}) as RawInventoryEntry;
    const machine = str(e.name) || key;
    if (!SAFE.machine.test(machine)) continue;
    // `role` is read as a fallback because a stack converged before the rename still has it in
    // its checkpoint, and a live fleet must not lose its label to a field name changing.
    const tag = str(e.tag) || str(outputs.tag) || str(e.role) || str(outputs.role);
    out.push({
      mode: 'fleet',
      machine,
      host: str(e.host),
      publicIp: str(e.publicIp),
      tag,
      fleet,
      actor,
      version,
      session: sessionNameFor(actor, version, tag),
      windows: DEFAULT_WINDOWS.map((w) => w.name),
    });
  }
  return out.sort((a, b) => (a.machine < b.machine ? -1 : a.machine > b.machine ? 1 : 0));
}

/**
 * Every Machine the backend knows about, across every run.
 *
 * Only `kontra-fleet` stacks: the dispatch table in `infra/stacks.ts` is the one place that says
 * what a stack may contain, and a stack outside that project is not a Fleet.
 */
export async function discoverMachines(reader: StackReader): Promise<MachineTarget[]> {
  const out: MachineTarget[] = [];
  const stacks = await reader.listStacks();
  for (const fqn of stacks) {
    if (!fqn.startsWith(`${FLEET_PROJECT}/`)) continue;
    const state = await reader.readStack(fqn);
    if (!state) continue;
    out.push(...machinesFromStack(fqn, state.outputs));
  }
  return out;
}

/** The real reader. A thin adapter so the streamer depends on an interface and the process
 * depends on the module. */
export function pulumiStackReader(): StackReader {
  // Required lazily: `infra/state.ts` reads the state directory, and a test that fakes discovery
  // has no reason to make this process resolve it.
  // eslint-disable-next-line @typescript-eslint/no-var-requires
  const state = require('../infra/state') as {
    listStacks(): Promise<string[]>;
    readStack(fqn: string): Promise<StackState | null>;
  };
  return { listStacks: state.listStacks, readStack: state.readStack };
}
