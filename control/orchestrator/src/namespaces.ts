/**
 * MAKING A WORKSPACE'S TEMPORAL NAMESPACE EXIST (ADR 0051 §2).
 *
 * Nothing created namespaces before: `default` exists only because the `temporalio/auto-setup` image
 * makes one. With a namespace per workspace, the first thing that touches a workspace registers its
 * namespace — the orchestrator's client and worker pool here, and the CLI on its own side.
 *
 * ── REGISTERED IS NOT USABLE ────────────────────────────────────────────────────────────────────
 *
 * `RegisterNamespace` writes to persistence and returns. The frontend serves calls out of a
 * namespace cache that refreshes on its own clock (ten seconds by default), so for a while after
 * registration a start or a poll in the new namespace is answered `NamespaceNotFound` — by the same
 * server that just said it exists. So this does not return until a real call in the namespace
 * succeeds; a worker that started polling during that window would die on its first poll.
 */
import type { Connection } from '@temporalio/client';

/** How long a freshly registered namespace takes to become usable before this gives up. */
const READY_TIMEOUT_MS = 60_000;
/** History retention for a new namespace, matching what auto-setup gives `default` (24 h). */
const DEFAULT_RETENTION_SECONDS = 24 * 3600;

const NOT_FOUND = 5;
const ALREADY_EXISTS = 6;

const code = (err: unknown): unknown => (err as { code?: unknown } | null)?.code;

/** Namespaces this process has already seen usable, so the check costs one RPC per namespace, once. */
const ready = new Map<string, Promise<void>>();

/**
 * Register `namespace` if it does not exist, and resolve once it is usable. Idempotent, and safe to
 * race: a concurrent registration elsewhere answers ALREADY_EXISTS, which is success.
 */
export function ensureNamespace(
  connection: Pick<Connection, 'workflowService'>,
  namespace: string,
  opts: { timeoutMs?: number; sleep?: (ms: number) => Promise<void>; retentionSeconds?: number } = {}
): Promise<void> {
  let p = ready.get(namespace);
  if (!p) {
    p = ensureOnce(connection, namespace, opts);
    p.catch(() => ready.delete(namespace));
    ready.set(namespace, p);
  }
  return p;
}

async function ensureOnce(
  connection: Pick<Connection, 'workflowService'>,
  namespace: string,
  opts: { timeoutMs?: number; sleep?: (ms: number) => Promise<void>; retentionSeconds?: number }
): Promise<void> {
  const svc = connection.workflowService;
  const sleep = opts.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));
  try {
    await svc.describeNamespace({ namespace });
  } catch (err) {
    if (code(err) !== NOT_FOUND) throw err;
    try {
      await svc.registerNamespace({
        namespace,
        description: 'kontra workspace (ADR 0051): one namespace per workspace',
        workflowExecutionRetentionPeriod: {
          seconds: opts.retentionSeconds ?? DEFAULT_RETENTION_SECONDS,
        } as never,
      });
    } catch (regErr) {
      if (code(regErr) !== ALREADY_EXISTS) throw regErr;
    }
  }
  // Usable, not merely registered: a list in the namespace is what a start or a poll will hit.
  const deadline = Date.now() + (opts.timeoutMs ?? READY_TIMEOUT_MS);
  for (;;) {
    try {
      await svc.listWorkflowExecutions({ namespace, pageSize: 1 });
      return;
    } catch (err) {
      if (code(err) !== NOT_FOUND || Date.now() > deadline) throw err;
      await sleep(1_000);
    }
  }
}

/** Test seam: forget which namespaces were already seen ready. */
export function resetNamespaceCache(): void {
  ready.clear();
}
