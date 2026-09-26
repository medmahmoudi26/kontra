/**
 * The `poller` signal: who is actually polling a Temporal task queue.
 *
 * This is `cli/workers.go` transplanted, and the transplant is deliberate rather than
 * convenient — `describeQueue` exists because REGISTRATION SAYS AN ACTOR EXISTS AND ONLY A POLLER
 * SAYS IT CAN RUN, and the distinction that command was written to surface must survive the move:
 *
 *   live     at least one poller identity on the queue
 *   none     the queue is registered and NOTHING is polling it — the classic trap
 *   unknown  we could not ask
 *
 * `unknown` is never `none`. "We could not ask Temporal" and "nothing is polling" are different
 * facts about the world, and `heartbeat.ts` records what conflating them costs: a renamed field
 * defaulting to 0 leaves a monitor showing `0/0` forever while looking like a measurement.
 * `cli/workers.go` holds the same line — a dial failure there is a loud error, never `(none)`.
 *
 * IT USED TO LIVE UNDER `panels/` AND ANSWER A SECOND QUESTION. Alongside the fold below it carried
 * `pollerFor`, `hostIsMachine`, `describeFleetQueues` and `queueForMachine` — per-MACHINE
 * attribution, which existed so a Monitor tile could say whether THIS box's handler was the one
 * polling. The Monitor is gone and that half went with it; what is left is the queue-level fact,
 * which is what every one of this module's callers was already asking for (`routes/runs.ts`,
 * `routes/pollers.ts`, `routes/stuck.ts`, `routes/logsCoverage.ts`, `workflowControl.ts`,
 * `activities/fleet.ts`, `probe.ts`, `server.ts`).
 *
 * The Temporal client is behind `QueueDescriber` and the real one is required LAZILY: `heartbeat.ts`
 * records that importing the client transitively fails to resolve `@temporalio/proto` under vitest,
 * and a test that fakes this seam has no reason to make this process resolve a gRPC stack at all.
 */

// THE PURE HALF LIVES IN @kontra/core (queues.ts): sharedQueue, POLL_FRESH_MS and pollIsFresh have
// no dependencies, and the console needs them too. Re-exported here so this module stays the one
// import site for "everything about pollers".
export { sharedQueue, POLL_FRESH_MS, pollIsFresh, identityHost } from '@kontra/core/queues';

import { temporalConnectOptions, type TemporalConnectOptions } from './temporalTls';


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
