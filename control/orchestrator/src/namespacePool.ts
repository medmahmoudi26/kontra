/**
 * ONE WORKER PER WORKSPACE NAMESPACE, for the task queues every workspace's runs call into (ADR 0051).
 *
 * A Temporal activity or child workflow always runs in its CALLER'S namespace. A canary in
 * `ws-hello` publishes its rows through `kontra-datasets` and brings its Fleet up through
 * `kontra-infra`, both in `ws-hello`. A materializer or infra worker polling only the legacy
 * namespace leaves that work queued with nobody to take it, and the run waits at RUNNING for ever.
 * So these shared queues are served once PER NAMESPACE, by one process, as §4 asks: a process serves
 * every workspace and does not restart to switch.
 *
 * A WORKSPACE CREATED LATER GETS ITS WORKER WITHIN {@link RESCAN_MS}: the pool re-reads the
 * workspaces folder on that clock. A workspace removed later keeps its worker, which polls an
 * otherwise idle queue, and that is cheaper than proving nothing will ever run there again.
 *
 * ONE FAILURE FAILS THE POOL, which is what the single worker did before. The process exits and its
 * container restarts, rather than limping on with one namespace's runs silently unserved.
 */
import { allNamespaces } from './workspaces';

/** How often the pool looks for a workspace it is not serving yet. */
export const RESCAN_MS = 10_000;

/** The part of a Temporal Worker the pool drives. */
export interface PooledWorker {
  run(): Promise<void>;
}

export interface NamespacePoolOptions {
  /** For log lines: `materializer`, `infra`. */
  label: string;
  /** Make the namespace exist and be usable, before a worker polls it. */
  ensure: (namespace: string) => Promise<void>;
  /** Build this pool's worker for one namespace. */
  make: (namespace: string) => Promise<PooledWorker>;
  /** Defaults to every workspace's namespace, read from the workspaces folder. */
  namespaces?: () => string[];
  rescanMs?: number;
  log?: (line: string) => void;
}

/**
 * Serve every namespace until a worker stops. Resolves when one shuts down cleanly (a SIGTERM drains
 * them all), and rejects, naming the namespace, when one fails.
 */
export async function runPerNamespace(opts: NamespacePoolOptions): Promise<void> {
  const list = opts.namespaces ?? (() => allNamespaces());
  const log = opts.log ?? ((line: string) => console.log(line));
  const serving = new Set<string>();
  let settle!: { resolve: () => void; reject: (err: Error) => void };
  const done = new Promise<void>((resolve, reject) => (settle = { resolve, reject }));

  const add = (namespace: string): void => {
    if (serving.has(namespace)) return;
    serving.add(namespace);
    void (async () => {
      await opts.ensure(namespace);
      const worker = await opts.make(namespace);
      log(`[${opts.label}] serving namespace ${namespace}`);
      await worker.run();
    })().then(
      () => settle.resolve(),
      (err: unknown) =>
        settle.reject(
          new Error(
            `${opts.label} worker for namespace ${namespace} stopped: ${err instanceof Error ? err.message : String(err)}`
          )
        )
    );
  };

  const scan = (): void => {
    try {
      for (const namespace of list()) add(namespace);
    } catch (err) {
      log(`[${opts.label}] could not read the workspaces: ${err instanceof Error ? err.message : String(err)}`);
    }
  };
  scan();
  const timer = setInterval(scan, opts.rescanMs ?? RESCAN_MS);
  timer.unref?.();
  try {
    await done;
  } finally {
    clearInterval(timer);
  }
}
