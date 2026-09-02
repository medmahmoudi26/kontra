/**
 * The Temporal Nexus endpoint a registration owns.
 *
 * ── WHY THIS MOVED ────────────────────────────────────────────────────────────────────────────
 *
 * A Nexus endpoint is the ADDRESS a caller dispatches through: `workflow.NewNexusClient(endpoint,
 * ServiceName).ExecuteOperation(...)` in `sdk/go/catalog/workflows.go`, and its Python
 * peer. It maps a NAME to a namespace and a task queue, and it is cluster state — it outlives every
 * process, and nothing about it is derived at call time.
 *
 * It used to be created on WORKER BOOT (`handler/nexus.go:ensureNexusEndpoint`), which made the
 * route a caller depends on a side effect of somebody having started a process. Three consequences,
 * all of them observed on this cluster:
 *
 *   1. A caller written against an Actor nobody had served yet failed at dispatch, on an endpoint
 *      name that did not exist — long after the code that named it was written, and reported as a
 *      Nexus error rather than as "that Actor was never registered here".
 *   2. Thirty-one endpoints outlived every worker that made them. Nothing owned them, nothing
 *      listed them beside the Actors they addressed, and forgetting a folder left its endpoint
 *      behind for good.
 *   3. Registering, serving and running were one act. You could not declare that an Actor exists
 *      here without also starting it, which is exactly the coupling this file breaks.
 *
 * So: REGISTERING creates the endpoint, forgetting removes it, and the worker's own create stays
 * only as an idempotent fallback for a worker started outside this control plane.
 *
 * ── WHAT IS DELIBERATELY NOT HERE ─────────────────────────────────────────────────────────────
 *
 * NO ENDPOINT PER WORKFLOW. A workflow is a CALLER — it dispatches, nothing dispatches to it — so an
 * endpoint addressed at a workflow's queue would be a route with no service behind it, and the
 * first call through it would fail as a Nexus timeout rather than as the design error it is. A
 * workflow registration records its version, digest and manifest and creates nothing on the cluster.
 *
 * NO FAILURE THAT LOSES THE REGISTRATION. The cluster may be down, or Nexus disabled; the folder is
 * still registered, on this disk, and the row still says so. What is lost is the endpoint, and the
 * row says that too — `endpoint` is absent, which is a state the Actors page can show and a later
 * register can repair.
 */

import { getConnection } from './temporalClient';

/**
 * `kontra-<name>-<version>`, non-alphanumerics collapsed to `-`.
 *
 * A CROSS-LANGUAGE DERIVATION with no shared code and no loud failure mode for a drift — a caller
 * dispatches to a name nobody created and waits. `shared/conformance/queues.json` §endpoint is what holds
 * the four sides to one answer, and `queues.conformance.test.ts` is this package's arm.
 *
 * NOT a comment counting the peers: this one said "THE FIFTH", `panels/pollers.ts` said "a
 * fourth" and `sdk/go/catalog` said "a SIXTH", all at once, all of a different set — which is
 * exactly what a bookkeeping scheme nothing can execute is worth.
 *
 * THIS IS THE DERIVATION THAT SANITISES. `sharedQueue` below takes the same inputs and passes
 * them through verbatim, because Temporal accepts a space in a queue name and the endpoint
 * registry does not. The corpus runs both rules over the same rows for that reason.
 */
export function endpointName(name: string, version: string): string {
  const raw = `kontra-${name}-${version}`;
  const safe = raw.replace(/[^0-9A-Za-z-]/g, '-').replace(/-{2,}/g, '-');
  return safe.replace(/^-+|-+$/g, '');
}

/**
 * The task queue the endpoint points at: the actor's SHARED queue, `<name>-<version>`.
 *
 * NOT the sessions queue. The handler serves its Nexus operation on the shared queue and polls
 * `-sessions` for RunBatch/Close (`handler/internal/identity`), so an endpoint aimed at the latter
 * routes operations to a queue that does not serve them.
 */
export function sharedQueue(name: string, version: string): string {
  return version === '' ? `${name}-shared` : `${name}-${version}`;
}

export interface EndpointResult {
  endpoint: string;
  /** What actually happened, so a caller can report it rather than guess. */
  state: 'created' | 'existed' | 'failed';
  /** Why, when `failed`. The registration still stands — see the header. */
  detail?: string;
}

/** The namespace endpoints are created in. Same variable the rest of the control plane reads. */
function namespace(): string {
  return process.env.KONTRA_NAMESPACE || 'default';
}

/**
 * Create the endpoint for an Actor, idempotently.
 *
 * ALREADY-EXISTS IS SUCCESS, not a race to lose. Two operators registering the same folder, a
 * re-register after an edit, and a worker that booted first all reach this — and the endpoint they
 * would each create is byte-identical, because the name and the target are both functions of
 * `(name, version)`. What it is NOT is a reason to skip recording it: an endpoint somebody else
 * made still addresses this Actor, and the row has to name it or forgetting the folder would leave
 * it behind.
 */
export async function ensureEndpoint(name: string, version: string): Promise<EndpointResult> {
  const endpoint = endpointName(name, version);
  try {
    const connection = await getConnection();
    await connection.operatorService.createNexusEndpoint({
      spec: {
        name: endpoint,
        target: { worker: { namespace: namespace(), taskQueue: sharedQueue(name, version) } },
      },
    });
    return { endpoint, state: 'created' };
  } catch (err) {
    if (isAlreadyExists(err)) return { endpoint, state: 'existed' };
    return { endpoint, state: 'failed', detail: errText(err) };
  }
}

/**
 * Remove the endpoint a registration created.
 *
 * DELETING NEEDS THE ID AND THE VERSION, not the name: the operator API takes an endpoint UUID and
 * a resource version for optimistic concurrency, so this lists to find it. A name that is not there
 * is `already gone`, which is the same end state and must not read as a failure — forgetting a
 * folder twice, or forgetting one whose endpoint was cleaned up by hand, is not an error.
 *
 * A FAILURE HERE MUST NOT BLOCK FORGETTING. The registration is the operator's to drop; an endpoint
 * left behind is a leak to report, not a reason to keep a row they asked to remove.
 */
export async function removeEndpoint(name: string, version: string): Promise<EndpointResult> {
  const endpoint = endpointName(name, version);
  try {
    const connection = await getConnection();
    // BY LISTING, not by name: the operator API deletes by endpoint UUID, and there is no
    // get-by-name RPC to shortcut it. The list is bounded (see `findEndpointId`).
    const id = await findEndpointId(endpoint);
    if (!id) return { endpoint, state: 'existed', detail: 'already gone' };
    const current = await connection.operatorService.getNexusEndpoint({ id });
    await connection.operatorService.deleteNexusEndpoint({ id, version: current.endpoint?.version });
    return { endpoint, state: 'created', detail: 'deleted' };
  } catch (err) {
    if (isNotFound(err)) return { endpoint, state: 'existed', detail: 'already gone' };
    return { endpoint, state: 'failed', detail: errText(err) };
  }
}

/** Find an endpoint's id by name, by paging the list — the portable way when the by-name RPC is
 *  not available on the server version in front of us. */
async function findEndpointId(endpoint: string): Promise<string | undefined> {
  const connection = await getConnection();
  let token: Uint8Array | undefined;
  // BOUNDED. A cluster with a runaway endpoint count must not turn a forget into an infinite loop;
  // ten pages of 100 is far past any real installation and still terminates.
  for (let page = 0; page < 10; page += 1) {
    const res = await connection.operatorService.listNexusEndpoints({ pageSize: 100, nextPageToken: token });
    for (const e of res.endpoints ?? []) {
      if (e.spec?.name === endpoint) return e.id ?? undefined;
    }
    if (!res.nextPageToken || res.nextPageToken.length === 0) return undefined;
    token = res.nextPageToken;
  }
  return undefined;
}

/** Every endpoint the cluster holds, by name. What the Actors page needs to say "this Actor is
 *  addressable" without asking once per card. */
export async function listEndpoints(): Promise<Set<string>> {
  const out = new Set<string>();
  try {
    const connection = await getConnection();
    let token: Uint8Array | undefined;
    for (let page = 0; page < 10; page += 1) {
      const res = await connection.operatorService.listNexusEndpoints({ pageSize: 100, nextPageToken: token });
      for (const e of res.endpoints ?? []) if (e.spec?.name) out.add(e.spec.name);
      if (!res.nextPageToken || res.nextPageToken.length === 0) break;
      token = res.nextPageToken;
    }
  } catch {
    // An empty set from a cluster that could not be asked is indistinguishable from a cluster with
    // no endpoints, so callers are told to treat this as "unknown" — see the route, which does not
    // draw an absence as an answer (ADR 0017).
  }
  return out;
}

/** gRPC codes travel as `code` on the error; the message is the fallback for a transport that
 *  wrapped it. `6` is ALREADY_EXISTS, `5` is NOT_FOUND. */
function isAlreadyExists(err: unknown): boolean {
  const e = err as { code?: number; message?: string };
  return e?.code === 6 || /already exists/i.test(e?.message ?? '');
}

function isNotFound(err: unknown): boolean {
  const e = err as { code?: number; message?: string };
  return e?.code === 5 || /not found/i.test(e?.message ?? '');
}

function errText(err: unknown): string {
  const e = err as { message?: string };
  return e?.message ?? String(err);
}
