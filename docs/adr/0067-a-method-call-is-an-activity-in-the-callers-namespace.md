# 67. A Method call is an activity in the caller's namespace

Date: 2026-10-10

## Status

**Accepted for implementation, in slices.** Implements PRD *the simplified platform* §5 and hardening
Phase 2 ("the in-process worker removes the sidecar"), in the shape the owner chose on 2026-10-09:
**direct activities**, not an in-process copy of today's Nexus path. Supersedes **0001** (dispatch is
one Nexus operation), **0018** §2 (the handler sidecar) and the handler clauses of **0036**, once its
last slice lands.

## Context

A workflow calls `await enrich.enrich(batch, out)` today and four hops follow: a Nexus operation on
the cluster-wide endpoint `kontra-enrich-0-3-0`; the Go handler sidecar's backing workflow
`kontra.v1.ActorService.Run`, in the ACTOR's namespace; a `kontra.fetch_blob` activity when the
units came by reference; the `RunBatch` activity the actor process actually serves; `Close`; and
`kontra.store_blob` for the results. Every actor container carries a second binary to run hops two,
three and six, and every actor version needs a Nexus endpoint, which is cluster state.

Two facts decided the shape:

1. **The actor process already serves `RunBatch` and `Close`** (ADR 0018). The sidecar only
   sequences them, so the sequencing can move to the caller with no new actor-side activity.
2. **Nexus endpoints are cluster-wide and, in open-source Temporal, open to every namespace.** With
   one namespace per workspace (ADR 0051) and one per tenant (PRD D4), a workflow in one tenant can
   dispatch into another tenant's actor workers by naming its endpoint. A direct activity runs in
   the caller's own namespace, where Temporal itself keeps tenants apart.

## Decision

1. **A Method call is `RunBatch` scheduled by the caller workflow**, in the caller's namespace, on
   the actor version's sessions queue (unscoped) or its Session's queue (scoped), followed by
   `Close` for an unscoped call. The caller SDKs (`sdk/python/kontra/catalog.py`,
   `sdk/go/catalog`) make exactly the choices the backing workflow makes today — the actor id, the
   queue, StartToClose, the heartbeat bound and a debugger's relaxation of it, the retry policy, the
   scoped ScheduleToStart — pinned by `shared/conformance/dispatch.json`, which today's handler
   drives first so the move cannot change a dispatch.
2. **The actor keeps bytes out of the caller's history.** `RunBatch` gains two optional request
   fields: `input_ref` (the actor fetches the units itself, where the handler used to) and
   `return_ref` (the actor stores its results and failures in the object store and answers with a
   `BareRef` — `sha256`, `size`, and meta `n`, `done`, `machine`, `isolated`, `failures` — the
   value the backing workflow returned). An actor built before this answers the old shape; the
   caller requires the new one and says so when it does not get it.
3. **`kontra.fetch_blob` and `kontra.store_blob` move to the orchestrator's dataset worker**, on its
   queue in every namespace (`namespacePool.ts`). A workflow reading a Batch's rows
   (`batch.rows()`) no longer needs the actor's worker to be alive.
4. **Observation follows the caller.** A dispatch's progress is the heartbeat of a pending
   `RunBatch` on the caller workflow itself; the transcript reads `ActivityTaskScheduled` events of
   type `RunBatch` instead of Nexus operation events; the per-dispatch summary rides the activity's
   `summary`. The `KontraRunId`/`KontraActor` search attributes on backing workflows go with them.
5. **Deleted at the end:** `runtime/handler` whole, `nexusRegistry.ts` and the endpoint
   derivations in four languages (with `queues.json` §endpoint and §endpoint_in_namespace), the
   worker-base image and the handler half of every launcher. The actor probe dispatches the same
   way as any caller.

## Slices

1. This ADR and `dispatch.json`, driven by today's handler.
2. The actor side, in Python and Go: `input_ref` and `return_ref` on `RunBatch`, both shapes
   answered, with tests.
3. The orchestrator serves the blob pair on its dataset queue in every namespace.
4. The callers switch, in Python and Go, each driving `dispatch.json`; the parity gate and the
   fresh-install e2e prove a run with no handler process.
5. The orchestrator reads progress and the transcript from the caller's own history.
6. The deletions.

## Consequences

- Actor images are rebuilt across the cutover (slice 4): an image that predates slice 2 cannot
  answer `return_ref`. The cutover refuses rather than guessing.
- Cross-namespace dispatch through Nexus (ADR 0001) is gone. A workflow calls the actors deployed in
  its own namespace — its workspace — which is the isolation PRD D4 asks for.
- A caller's history grows by one or two activity events per dispatch, where it held one Nexus
  operation; the payloads stay references either way.
