/**
 * The `poller` signal: Temporal pollers on the actor's shared queue (ADR 0020, slice 3).
 *
 * This is `cli/workers.go` transplanted, and the transplant is deliberate rather than
 * convenient — `collectWorkers`/`describeQueue` exist because REGISTRATION SAYS AN ACTOR EXISTS AND
 * ONLY A POLLER SAYS IT CAN RUN, and the distinction that command was written to surface must
 * survive the move to a tile:
 *
 *   live     at least one poller identity, attributable to this Machine
 *   none     the queue is registered and NOTHING is polling it — the classic trap
 *   unknown  we could not ask, or could not attribute the answer to this Machine
 *
 * `unknown` is never `none`. "We could not ask Temporal" and "nothing is polling" are different
 * facts about the world, and `heartbeat.ts` records what conflating them costs: a renamed field
 * defaulting to 0 leaves a monitor showing `0/0` forever while looking like a measurement.
 * `cli/workers.go` holds the same line — a dial failure there is a loud error, never `(none)`.
 *
 * WHAT THE MOVE ADDS, AND WHY IT HAS TO. `kontra workers list` reports per QUEUE; a Terminal is per
 * MACHINE, and every Machine placed with the same actor and version polls ONE shared queue. So a
 * queue-level `live` painted on every tile is exactly the round-3 shape ADR 0020 exists to catch:
 * nine healthy handlers would render the tenth, whose `kontra-handler.service` is dead, green.
 * Attribution comes from the poller identity, which neither `runtime/handler/main.go` (`client.Dial` with no
 * `Identity`) nor the Python host overrides, so both SDK defaults apply and both carry the host:
 * Go `<pid>@<hostname>@<queue>`, Python `<pid>@<hostname>`. When an identity cannot be parsed we
 * report `unknown` rather than guessing in either direction.
 *
 * The Temporal client is behind `QueueDescriber` and the real one is required LAZILY, the way
 * `discovery.ts` requires `infra/state`: `heartbeat.ts` records that importing the client transitively
 * fails to resolve `@temporalio/proto` under vitest, and a test that fakes this seam has no reason to
 * make this process resolve a gRPC stack at all.
 */

// THE PURE HALF MOVED TO @kontra/core (queues.ts): sharedQueue, POLL_FRESH_MS and pollIsFresh
// have no dependencies, and the console needs them while this module reaches infra/stacks.
export { sharedQueue, POLL_FRESH_MS, pollIsFresh, identityHost } from '@kontra/core/queues';
// …and imported as well as re-exported, because this module CALLS it (line ~263). A bare
// `export … from` re-exports without binding the name locally.
import { sharedQueue, identityHost } from '@kontra/core/queues';

import type { MachineTarget } from './discovery';
import type { TerminalHealth } from './types';
import { temporalConnectOptions, type TemporalConnectOptions } from '../temporalTls';


/** `temporal.api.enums.v1.TaskQueueType`. WORKFLOW + ACTIVITY, folded; NEXUS (3) is skipped for
 * the reason `describeQueue` gives — the other two already prove liveness. */
export const TASK_QUEUE_TYPE = { workflow: 1, activity: 2 } as const;
export type TaskQueueType = keyof typeof TASK_QUEUE_TYPE;

/** `temporal.api.enums.v1.TaskQueueKind.TASK_QUEUE_KIND_NORMAL`. */
export const TASK_QUEUE_KIND_NORMAL = 1;

/** The order `describeQueue` folds in. Kept as data so a test can prove the fold stops at the
 * first error rather than asking both and half-reporting. */
export const POLLED_TYPES: readonly TaskQueueType[] = ['workflow', 'activity'];

export interface PollerInfo {
  identity: string;
  /** Epoch ms; 0 when Temporal did not say. */
  lastAccess: number;
}

/** The Temporal seam — an interface so tests never dial anything, the same pattern and the same
 * reason as `cli/workers.go`'s `queueDescriber`. */
export interface QueueDescriber {
  pollers(queue: string, type: TaskQueueType): Promise<PollerInfo[]>;
  close(): Promise<void>;
}

/**
 * One poller, kept whole — an identity AND the moment it was last seen.
 *
 * THE PAIR IS THE POINT. A queue-level "freshest poll" answers "is ANYTHING serving this queue",
 * which is all `pollerFor` ever needed; it cannot answer "is THIS worker serving", and the two come
 * apart exactly when it matters. Temporal lists a poller for about five minutes after it stops, so
 * a queue with one live worker and one killed three minutes ago reports a fresh `lastPoll` and two
 * identities — and a reader that offers both is offering a dead one.
 */
export interface PollerPoll {
  identity: string;
  /** Epoch ms of this identity's freshest poll; 0 when Temporal did not date it. */
  lastPoll: number;
}

/** One queue's folded poller state — the TS peer of Go's `workerRow`. */
export interface QueueState {
  queue: string;
  /** Distinct poller identities, sorted. Empty AND no error means registered-but-nothing-polling. */
  identities: string[];
  /** The same pollers with their own timestamps, in `identities` order. Derived from the same fold,
   *  so `workers.map((w) => w.identity)` is `identities` and cannot drift from it. */
  workers: PollerPoll[];
  /** Freshest poll across identities, epoch ms; 0 when never. */
  lastPoll: number;
  /** Set when the describe itself failed. Its presence is what makes the state `unknown`. */
  error?: string;
}



/**
 * Fold one queue's WORKFLOW + ACTIVITY pollers into distinct identities.
 *
 * A line-for-line mirror of `cli/workers.go:describeQueue`, including the part that looks like a
 * bug and is not: it BREAKS on the first type that errors rather than continuing. Asking the
 * second type after the first failed would produce a count from half the evidence, and a partial
 * count reported as a whole one is the failure this whole signal is about.
 */
export async function describeQueue(d: QueueDescriber, queue: string): Promise<QueueState> {
  const seen = new Map<string, number>();
  let describeErr: string | undefined;

  for (const type of POLLED_TYPES) {
    let ps: PollerInfo[];
    try {
      ps = await d.pollers(queue, type);
    } catch (err) {
      describeErr = (err as Error)?.message || String(err);
      break;
    }
    for (const p of ps) {
      const prev = seen.get(p.identity);
      if (prev === undefined || p.lastAccess > prev) seen.set(p.identity, p.lastAccess);
    }
  }

  if (describeErr !== undefined) {
    // Identities are dropped on an error even if the first type returned some: a half-fold is not
    // a smaller fold, it is an unknown one.
    return { queue, identities: [], workers: [], lastPoll: 0, error: describeErr };
  }

  const identities = [...seen.keys()].sort();
  const workers = identities.map((identity) => ({ identity, lastPoll: seen.get(identity) ?? 0 }));
  let lastPoll = 0;
  for (const t of seen.values()) if (t > lastPoll) lastPoll = t;
  return { queue, identities, workers, lastPoll };
}

/** What a node that is THIS BOX can legitimately be called. `discovery.ts` names the local node
 *  `localhost`; a Worker running on it identifies itself with `os.hostname()`, which is whatever the
 *  box is actually called. Both denote the same machine. */
const LOCAL_LABELS = new Set(['localhost', '127.0.0.1', '::1']);

/**
 * A Machine name matches a poller host when the host's first dot-label equals it, so an FQDN
 * (`kf-crawl-01.internal`) still attributes. DigitalOcean sets a droplet's hostname from its name,
 * and `programs/fleet.ts` names them `kf-<role>-NN` explicitly for exactly this kind of correlation.
 *
 * THE LOCAL NODE IS THE EXCEPTION, and it was a permanent false alarm. A `local` Terminal's node is
 * literally `localhost`, but its Worker identifies itself by `os.hostname()` — on this controller,
 * `main-droplet`. `'main-droplet' === 'localhost'` is false, so a local actor that was polling
 * perfectly reported `poller: NONE — polled by 1 worker(s) (main-droplet) but none from localhost,
 * its kontra-handler.service is probably down`. Every local pane, always, in red, saying a service
 * was down while it was up.
 *
 * This is the same shape as the `loads` and `reachable` bugs beside it: a fleet-shaped assumption
 * applied to a node that is not a Machine. `hostName` is a parameter rather than a call to
 * `os.hostname()` so the rule is testable without depending on what the test host is called.
 */
export function hostIsMachine(host: string, machine: string, hostName?: string): boolean {
  const full = host.trim().toLowerCase();
  const label = (full.split('.')[0] ?? '').toLowerCase();
  const node = machine.trim().toLowerCase();
  if (label === node) return true;
  if (!LOCAL_LABELS.has(node)) return false;
  // The node IS this box, so its own hostname attributes to it — as does another spelling of local.
  // Match the WHOLE host for those: `127.0.0.1` has dots and its first label is `127`.
  if (LOCAL_LABELS.has(full)) return true;
  if (hostName === undefined) return false;
  return label === (hostName.split('.')[0] ?? '').toLowerCase();
}

export interface PollerVerdict {
  poller: TerminalHealth['poller'];
  detail?: string;
}

/**
 * One Machine's verdict on one queue's folded state.
 *
 * The order of these branches IS the contract. Read top to bottom: could we ask, is anything
 * polling at all, is any of it this Machine's, and — only if none of it is — do we understand the
 * identities well enough to say so.
 */
export function pollerFor(
  machine: string,
  q: QueueState | undefined,
  hostName?: string
): PollerVerdict {
  if (!q) {
    return {
      poller: 'unknown',
      detail: `no shared queue for ${machine} — the stack carries no actor placement, so there is nothing to poll`,
    };
  }
  if (q.error !== undefined) {
    return {
      poller: 'unknown',
      detail: `could not ask Temporal about queue ${q.queue}: ${q.error}`,
    };
  }
  if (q.identities.length === 0) {
    // The whole point of `kontra workers list`. Nothing polls this queue from anywhere, so this
    // Machine's handler certainly does not, and no attribution is needed to say it.
    return {
      poller: 'none',
      detail: `queue ${q.queue} is registered but nothing is polling it — no handler is running anywhere in the fleet`,
    };
  }

  const hosts = q.identities.map(identityHost);
  for (const host of hosts) {
    if (host !== undefined && hostIsMachine(host, machine, hostName)) return { poller: 'live' };
  }

  const named = hosts.filter((h): h is string => h !== undefined);
  if (named.length === 0) {
    return {
      poller: 'unknown',
      detail:
        `${q.identities.length} worker(s) poll queue ${q.queue}, but no identity names a host ` +
        `(${q.identities.slice(0, 3).join(', ')}), so ${machine}'s own handler cannot be confirmed`,
    };
  }
  return {
    poller: 'none',
    detail:
      `queue ${q.queue} is polled by ${named.length} worker(s) (${uniqueSorted(named).slice(0, 4).join(', ')}) ` +
      `but none from ${machine} — its kontra-handler.service is probably down`,
  };
}

function uniqueSorted(values: string[]): string[] {
  return [...new Set(values)].sort();
}

/**
 * Describe every queue the given Machines need, ONCE per queue.
 *
 * Twelve Machines placed with one actor share one queue, and one describe answers for all of them.
 * Machines with no placement contribute no queue and get no entry — `pollerFor` reads a missing
 * entry as `unknown`.
 */
export async function describeFleetQueues(
  d: QueueDescriber,
  machines: readonly MachineTarget[]
): Promise<Map<string, QueueState>> {
  const queues = new Set<string>();
  for (const m of machines) {
    if (m.actor === '') continue;
    queues.add(sharedQueue(m.actor, m.version));
  }
  const out = new Map<string, QueueState>();
  await Promise.all(
    [...queues].map(async (queue) => {
      out.set(queue, await describeQueue(d, queue));
    })
  );
  return out;
}

/** The queue a Machine's tile asks about, or undefined when it has no placement. */
export function queueForMachine(m: MachineTarget): string | undefined {
  if (m.actor === '') return undefined;
  // A mode whose session name carries the actor but NOT its version cannot name the shared queue:
  // `sharedQueue(actor, '')` is `<actor>-shared`, a real but DIFFERENT queue, and describing it would
  // report `poller: none` for a healthy local Worker. `health.ts` turns this undefined into `unknown`
  // with the reason. The fleet keeps its behaviour exactly: its placement comes from stack outputs,
  // where an empty version means the same thing it always did.
  if (m.version === '' && (m.mode ?? 'fleet') !== 'fleet') return undefined;
  return sharedQueue(m.actor, m.version);
}

// --- the real describer -------------------------------------------------------------------------

/** The minimal shape of `DescribeTaskQueueResponse` this file reads. Declared locally rather than
 * imported from `@temporalio/proto` so nothing in this module's type graph pulls a gRPC stack into
 * a test run — the resolution failure `heartbeat.ts` documents. */
interface RawDescribeResponse {
  pollers?: Array<{
    identity?: string | null;
    lastAccessTime?: RawTimestamp | null;
  } | null> | null;
}

type RawTimestamp = {
  seconds?: number | string | { low: number; high: number; unsigned?: boolean } | null;
  nanos?: number | null;
} | null;

/**
 * A protobuf `Timestamp` as protobufjs hands it back, in epoch ms.
 *
 * `seconds` is an int64, which arrives as a Long (`{low, high}`), a number, or a decimal string
 * depending on how the client was built. 0 means "Temporal did not say" and is NOT a valid poll
 * time — the same rule the rest of this file follows about zero and unknown.
 */
export function timestampToMs(ts: RawTimestamp): number {
  if (!ts) return 0;
  const s = ts.seconds;
  let seconds = 0;
  if (typeof s === 'number') seconds = s;
  else if (typeof s === 'string') seconds = Number(s);
  else if (s && typeof s === 'object') seconds = s.high * 4294967296 + (s.low >>> 0);
  if (!Number.isFinite(seconds)) return 0;
  const nanos = typeof ts.nanos === 'number' ? ts.nanos : 0;
  return Math.round(seconds * 1000 + nanos / 1e6);
}

/** Pure, so the wire shape is pinned by a test instead of by a running Temporal. */
export function parseDescribeResponse(res: RawDescribeResponse): PollerInfo[] {
  const out: PollerInfo[] = [];
  for (const p of res.pollers ?? []) {
    if (!p) continue;
    out.push({ identity: p.identity ?? '', lastAccess: timestampToMs(p.lastAccessTime ?? null) });
  }
  return out;
}

export interface TemporalDescriberOptions {
  address?: string;
  namespace?: string;
}

/**
 * The real describer: `DescribeTaskQueue` over the same connection `converger.ts` opens, and for
 * the same reasons — lazily connected so the streamer boots and serves `health` with no Temporal
 * running, and a failure is never memoised so a Temporal that was down when the first tile asked
 * does not stay "down" for the life of the process.
 *
 * `require` rather than `import` at module scope: see the file header.
 */
export function temporalQueueDescriber(options?: TemporalDescriberOptions): QueueDescriber {
  const address = options?.address ?? process.env.KONTRA_ADDRESS ?? 'localhost:7233';
  const namespace = options?.namespace ?? process.env.KONTRA_NAMESPACE ?? 'default';

  interface Conn {
    workflowService: {
      describeTaskQueue(req: unknown): Promise<RawDescribeResponse>;
    };
    close(): Promise<void>;
  }

  let connPromise: Promise<Conn> | null = null;
  let conn: Conn | null = null;

  const connect = async (): Promise<Conn> => {
    if (!connPromise) {
      connPromise = (async () => {
        // eslint-disable-next-line @typescript-eslint/no-var-requires
        const client = require('@temporalio/client') as {
          Connection: { connect(opts: TemporalConnectOptions): Promise<Conn> };
        };
        // Through `temporalConnectOptions` like every other site — the require is lazy for module
        // -graph reasons and says nothing about how the connection is configured.
        const c = await client.Connection.connect(temporalConnectOptions({ address }));
        conn = c;
        return c;
      })().catch((err: unknown) => {
        connPromise = null;
        throw err;
      });
    }
    return connPromise;
  };

  return {
    async pollers(queue: string, type: TaskQueueType): Promise<PollerInfo[]> {
      const c = await connect();
      const res = await c.workflowService.describeTaskQueue({
        namespace,
        taskQueue: { name: queue, kind: TASK_QUEUE_KIND_NORMAL },
        taskQueueType: TASK_QUEUE_TYPE[type],
      });
      return parseDescribeResponse(res);
    },
    async close(): Promise<void> {
      const c = conn;
      conn = null;
      connPromise = null;
      if (c) await c.close().catch(() => undefined);
    },
  };
}
